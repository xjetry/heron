package collect

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/agentwire"
	"github.com/xjetry/heron/internal/clock"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// 本地速率逐网卡求差：只计两次采样都在、收发都不倒退的网卡。新出现的网卡已有的计数、消失的网卡、倒退的网卡都不进
// 分子，其余网卡照常计；没有任何网卡满足时速率缺失。过滤规则换掉的网卡按出现与消失处理。
func TestNetRatePerInterface(t *testing.T) {
	clk := clock.NewFake(time.Unix(0, 0))
	h := &ifacesOnly{list: []ifaceCounters{{"eth0", 100, 200}}}
	c := &Collector{Host: h, Clock: clk}
	if m, _ := c.Metrics(c.Identify()); m.NetRxBps != nil || m.NetTxBps != nil {
		t.Fatalf("first sample created rate: %v", m)
	}
	step := func(name string, list []ifaceCounters, rx, tx uint64, present bool) {
		t.Helper()
		clk.Advance(time.Second)
		h.list = list
		m, _ := c.Metrics(c.Identify())
		if !present {
			if m.NetRxBps != nil || m.NetTxBps != nil {
				t.Fatalf("%s: rate = %d/%d, want missing", name, m.GetNetRxBps(), m.GetNetTxBps())
			}
			return
		}
		if m.NetRxBps == nil || m.NetTxBps == nil || m.GetNetRxBps() != rx || m.GetNetTxBps() != tx {
			t.Fatalf("%s: rate = %d/%d (present %v/%v), want %d/%d", name, m.GetNetRxBps(), m.GetNetTxBps(), m.NetRxBps != nil, m.NetTxBps != nil, rx, tx)
		}
	}
	step("added interface counts only eth0", []ifaceCounters{{"eth0", 110, 220}, {"eth1", 5000, 9000}}, 10, 20, true)
	step("reordered stable set", []ifaceCounters{{"eth1", 5020, 9040}, {"eth0", 120, 240}}, 30, 60, true)
	step("eth0 counter went back: eth1 only", []ifaceCounters{{"eth0", 5, 5}, {"eth1", 5030, 9050}}, 10, 10, true)
	step("rx fine, tx back on eth1: eth0 only", []ifaceCounters{{"eth0", 7, 8}, {"eth1", 5040, 1}}, 2, 3, true)
	step("removed interface", []ifaceCounters{{"eth1", 5045, 11}}, 5, 10, true)
	step("every interface went back", []ifaceCounters{{"eth1", 1, 1}}, 0, 0, false)
	step("whole set replaced", []ifaceCounters{{"wlan0", 1000, 1000}}, 0, 0, false)
	c.NetInclude = []string{"eth1"}
	step("filter swapped the counted set", []ifaceCounters{{"wlan0", 1100, 1100}, {"eth1", 10000, 20000}}, 0, 0, false)
	step("excluded interfaces do not reset the set", []ifaceCounters{{"eth1", 10005, 20010}, {"docker0", 999999, 999999}}, 5, 10, true)
}

// 逐网卡列表与合计、摘要出自同一次采样：列表按名称字节序，合计与摘要由 agentwire 的同一个推导得出。计入网卡多于
// MaxNetInterfaces 时只省列表，合计与摘要照旧取全集，越过与回落各记一行日志。
func TestNetInterfacesListFollowsCountedSet(t *testing.T) {
	var logs strings.Builder
	h := &ifacesOnly{list: []ifaceCounters{{"wlan0", 1, 2}, {"eth0", 10, 20}, {"lo", 7, 7}}}
	c := &Collector{Host: h, Clock: clock.NewFake(time.Unix(0, 0)), Log: slog.New(slog.NewTextHandler(&logs, nil))}
	m, _ := c.Metrics(c.Identify())
	list := c.NetInterfaces()
	want := []*heronv1.NetInterfaceCounter{{Name: "eth0", RxBytes: 10, TxBytes: 20}, {Name: "wlan0", RxBytes: 1, TxBytes: 2}}
	if len(list) != len(want) || !proto.Equal(list[0], want[0]) || !proto.Equal(list[1], want[1]) {
		t.Fatalf("list = %v, want %v", list, want)
	}
	if err := agentwire.ValidateNetInterfaces(list); err != nil {
		t.Fatalf("collector built a list the hub rejects: %v", err)
	}
	if sum := agentwire.NetInterfaceSummary(list); m.GetNetRxTotal() != sum.Rx || m.GetNetTxTotal() != sum.Tx || m.NetCounterEpoch != sum.Epoch {
		t.Fatalf("metrics %d/%d %s disagree with the list summary %+v", m.GetNetRxTotal(), m.GetNetTxTotal(), m.NetCounterEpoch, sum)
	}

	setCount := func(n int) {
		h.list = nil
		for i := range n {
			h.list = append(h.list, ifaceCounters{fmt.Sprintf("veth%03d", i), 1, 2})
		}
	}
	setCount(agentwire.MaxNetInterfaces)
	c.Metrics(c.Identify())
	if len(c.NetInterfaces()) != agentwire.MaxNetInterfaces || strings.Contains(logs.String(), "per-interface") {
		t.Fatalf("at the limit: list %d entries, logs %q", len(c.NetInterfaces()), logs.String())
	}
	for round := range 2 {
		setCount(agentwire.MaxNetInterfaces + 1)
		m, _ = c.Metrics(c.Identify())
		if c.NetInterfaces() != nil || m.GetNetRxTotal() != agentwire.MaxNetInterfaces+1 || m.NetCounterEpoch == "" {
			t.Fatalf("round %d over the limit: list %v, totals %d, epoch %q", round, c.NetInterfaces(), m.GetNetRxTotal(), m.NetCounterEpoch)
		}
		c.Metrics(c.Identify())
		if got := strings.Count(logs.String(), "reporting totals only"); got != round+1 {
			t.Fatalf("round %d: %d over-limit log lines, want one per crossing:\n%s", round, got, logs.String())
		}
		setCount(agentwire.MaxNetInterfaces)
		c.Metrics(c.Identify())
		if len(c.NetInterfaces()) != agentwire.MaxNetInterfaces || strings.Count(logs.String(), "back within the per-interface limit") != round+1 {
			t.Fatalf("round %d back at the limit: list %d entries, logs:\n%s", round, len(c.NetInterfaces()), logs.String())
		}
	}

	h.list = []ifaceCounters{{"eth0", 1, 1}, {"eth0", 2, 2}}
	m, err := c.Metrics(c.Identify())
	if m.NetRxTotal != nil || m.NetCounterEpoch != "" || c.NetInterfaces() != nil || err == nil || !strings.Contains(err.Error(), "listed twice") {
		t.Fatalf("duplicate names must fail network collection: totals %v epoch %q list %v err %v", m.NetRxTotal, m.NetCounterEpoch, c.NetInterfaces(), err)
	}
}

func TestCollectionDiagnosticsReflectEffectiveSampling(t *testing.T) {
	c := fixture(t)
	c.NetInclude = []string{"eth*"}
	c.NetExclude = []string{"eth0"}
	c.Metrics(c.Identify())
	d := c.Facts(c.Identify()).GetDiagnostics()
	if d == nil || !slices.Equal(d.NetInclude, []string{"eth*"}) || len(d.NetExclude) != 0 || !slices.Equal(d.NetInterfaces, []string{"eth0"}) || d.NetInterfacesTotal != 1 || len(d.FailedCollectors) != 0 {
		t.Fatalf("effective collection diagnostics = %v", d)
	}
	fixtureOf(c.Host.(*ProcFS)).UsageFunc = func() (uint64, uint64, error) { return 0, 0, errors.New("secret raw filesystem error") }
	c.Metrics(c.Identify())
	d = c.Facts(c.Identify()).GetDiagnostics()
	if d == nil || !slices.Equal(d.FailedCollectors, []heronv1.CollectionComponent{heronv1.CollectionComponent_COLLECTION_COMPONENT_DISK}) {
		t.Fatalf("failed collector not identified: %v", d)
	}
	encoded, err := protojson.Marshal(d)
	if err != nil || strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), "filesystem") {
		t.Fatalf("raw failure escaped into diagnostics: %s %v", encoded, err)
	}
	fixtureOf(c.Host.(*ProcFS)).UsageFunc = func() (uint64, uint64, error) { return 100, 50, nil }
	c.Metrics(c.Identify())
	if d = c.Facts(c.Identify()).GetDiagnostics(); len(d.GetFailedCollectors()) != 0 {
		t.Fatalf("recovered collector retained a failure: %v", d)
	}
}

// 磁盘 I/O 计数器读不到是独立失败类别：它不是 statfs 的磁盘用量失败，面板不能把它显示成"磁盘"。
func TestDiskCounterFailureHasItsOwnCollectorCategory(t *testing.T) {
	c := fixture(t)
	c.Host = &diskIOFail{ProcFS: c.Host.(*ProcFS)}
	m, err := c.Metrics(c.Identify())
	if m == nil || err == nil {
		t.Fatalf("disk counter failure must be reported: m=%v err=%v", m, err)
	}
	d := c.Facts(c.Identify()).GetDiagnostics()
	if !slices.Contains(d.GetFailedCollectors(), heronv1.CollectionComponent_COLLECTION_COMPONENT_DISK_IO) {
		t.Fatalf("disk I/O counter failure not identified: %v", d)
	}
	if slices.Contains(d.GetFailedCollectors(), heronv1.CollectionComponent_COLLECTION_COMPONENT_DISK) {
		t.Fatalf("disk usage category reused for an I/O counter failure: %v", d)
	}
	if err := agentwire.ValidateDiagnostics(d); err != nil {
		t.Fatalf("new component must pass the shared whitelist: %v", err)
	}
	if m.GetDiskTotal() != 1000 || m.GetDiskUsed() != 400 {
		t.Fatalf("disk usage must be unaffected: %+v", m)
	}
}

type diskIOFail struct{ *ProcFS }

func (h *diskIOFail) diskCounters() ([]diskCounters, error) {
	return nil, errors.New("no whole-disk device")
}

func TestDiagnosticsTruncationDoesNotTruncateCounterScope(t *testing.T) {
	h := &ifacesOnly{}
	for i := range agentwire.MaxDiagnosticInterfaces + 1 {
		h.list = append(h.list, ifaceCounters{fmt.Sprintf("eth%03d", i), 10, 20})
	}
	c := &Collector{Host: h, Clock: clock.NewFake(time.Unix(0, 0))}
	m, _ := c.Metrics(c.Identify())
	d := c.Facts(c.Identify()).Diagnostics
	if len(d.NetInterfaces) != 128 || d.NetInterfacesTotal != 129 || m.GetNetRxTotal() != 1290 {
		t.Fatalf("truncation lost counter members: %v / %v", d, m)
	}
	if err := agentwire.ValidateDiagnostics(d); err != nil {
		t.Fatal(err)
	}
	epoch := m.NetCounterEpoch
	h.list[128].name = "renamed-last-interface"
	m, _ = c.Metrics(c.Identify())
	if m.NetCounterEpoch == epoch {
		t.Fatal("scope hash ignored members beyond the displayed list")
	}
	h.list = nil
	c.Metrics(c.Identify())
	d = c.Facts(c.Identify()).Diagnostics
	if d.NetInterfacesTotal != 0 || len(d.NetInterfaces) != 0 || !slices.Contains(d.FailedCollectors, heronv1.CollectionComponent_COLLECTION_COMPONENT_NET) {
		t.Fatalf("failed network retained old scope: %v", d)
	}
}
