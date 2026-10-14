package client

import (
	"fmt"
	"strconv"
	"testing"
	"testing/fstest"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/agent/collect"
	"github.com/xjetry/heron/internal/agentwire"
	"github.com/xjetry/heron/internal/clock"
)

// netHost 是 Runner 的采集器看到的 /sys/class/net：每轮之前可以增删网卡、改计数；采集器的时钟每轮前进一个间隔，
// 第二次采样起本地速率有值。
type netHost struct {
	fs  fstest.MapFS
	clk *clock.Fake
}

func newNetRunner(t *testing.T) (*Runner, *netHost) {
	t.Helper()
	r, _ := newRunner(t, &fakeHub{})
	h := &netHost{fs: fstest.MapFS{}, clk: clock.NewFake(time.Unix(0, 0))}
	r.Collector = &collect.Collector{Host: hostProcFS(h.fs, func() (uint64, uint64, error) { return 1, 1, nil }), Clock: h.clk, Version: "t"}
	return r, h
}

func (h *netHost) put(name string, rx, tx uint64) {
	h.fs["sys/class/net/"+name+"/statistics/rx_bytes"] = &fstest.MapFile{Data: []byte(strconv.FormatUint(rx, 10) + "\n")}
	h.fs["sys/class/net/"+name+"/statistics/tx_bytes"] = &fstest.MapFile{Data: []byte(strconv.FormatUint(tx, 10) + "\n")}
	h.fs["sys/class/net/"+name+"/type"] = &fstest.MapFile{Data: []byte("1\n")}
	h.fs["sys/class/net/"+name+"/uevent"] = &fstest.MapFile{Data: []byte("INTERFACE=" + name + "\n")}
}

// counters 让 eth0、eth1 在第 round 轮各收 1000·round、发 500·round 字节；之前的网卡全部移除。
func (h *netHost) counters(round int) {
	h.clear()
	h.put("eth0", uint64(1000*round), uint64(500*round))
	h.put("eth1", uint64(1000*round), uint64(500*round))
	h.clk.Advance(10 * time.Second)
}

// overLimit 让计入的网卡多于 agentwire.MaxNetInterfaces：采集器不给列表、只给合计。
func (h *netHost) overLimit() {
	h.clear()
	for i := range agentwire.MaxNetInterfaces + 1 {
		h.put(fmt.Sprintf("ens%03d", i), 1, 2)
	}
	h.clk.Advance(10 * time.Second)
}

// onlyExcluded 只留下默认排除的 lo：没有计入任何网卡，采集器按网络采集失败处理，列表与合计都缺席。
func (h *netHost) onlyExcluded() {
	h.clear()
	h.put("lo", 7, 7)
	h.clk.Advance(10 * time.Second)
}

// failed 去掉 /sys/class/net：网络采集失败，既没有列表也没有合计。
func (h *netHost) failed() {
	h.clear()
	h.clk.Advance(10 * time.Second)
}

func (h *netHost) clear() {
	for k := range h.fs {
		if len(k) > len("sys/class/net/") && k[:len("sys/class/net/")] == "sys/class/net/" {
			delete(h.fs, k)
		}
	}
}

// listOnly 判定一次上报只带列表：列表非空、合计与摘要三项都缺席。
func listOnly(r *heronv1.ReportRequest) bool {
	m := r.GetMetrics()
	return len(r.NetInterfaces) > 0 && m.NetRxTotal == nil && m.NetTxTotal == nil && m.NetCounterEpoch == ""
}

// 从首次上报起，列表提供合计的全部输入，metrics 不重复携带。
func TestRunnerOmitsTotalsWhenListPresent(t *testing.T) {
	r, h := newNetRunner(t)
	reports := driveScript(t, r, []*heronv1.ReportResponse{{}, {}, {}}, map[int]func(){0: func() { h.counters(0) }, 1: func() { h.counters(1) }, 2: func() { h.counters(2) }})
	for i, rep := range reports {
		if !listOnly(rep) || len(rep.NetInterfaces) != 2 {
			t.Fatalf("report %d carries totals or lost list: %v", i, rep)
		}
		if i > 0 && (rep.Metrics.GetNetRxBps() != 200 || rep.Metrics.GetNetTxBps() != 100) {
			t.Fatalf("local rates lost: %v", rep.Metrics)
		}
	}
}

func TestRunnerKeepsTotalsWithoutList(t *testing.T) {
	r, h := newNetRunner(t)
	reports := driveScript(t, r, []*heronv1.ReportResponse{{}, {}, {}, {}}, map[int]func(){0: func() { h.counters(0) }, 1: h.overLimit, 2: h.onlyExcluded, 3: h.failed})
	if rep := reports[1]; len(rep.NetInterfaces) != 0 || rep.Metrics.GetNetRxTotal() != agentwire.MaxNetInterfaces+1 || rep.Metrics.GetNetTxTotal() != 2*(agentwire.MaxNetInterfaces+1) || rep.Metrics.NetCounterEpoch == "" {
		t.Fatalf("over limit lost metrics totals: %v", rep)
	}
	for _, i := range []int{2, 3} {
		if rep := reports[i]; len(rep.NetInterfaces) != 0 || rep.Metrics.NetRxTotal != nil || rep.Metrics.NetTxTotal != nil || rep.Metrics.NetCounterEpoch != "" {
			t.Fatalf("failed collection gained counters: %v", rep)
		}
	}
}
