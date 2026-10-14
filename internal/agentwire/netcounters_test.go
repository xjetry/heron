package agentwire

import (
	"fmt"
	"math"
	"strings"
	"testing"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
)

func counter(name string, rx, tx uint64) *heronv1.NetInterfaceCounter {
	return &heronv1.NetInterfaceCounter{Name: name, RxBytes: rx, TxBytes: tx}
}

// 摘要必须与引入逐网卡列表之前 agent 填的 net_counter_epoch 逐字节相同：hub 拿列表推出的摘要与落盘的合计基线比较，
// 差一个字节就让每台升级的节点在第一次上报时丢掉一整段区间。期望值是 '["eth0","wlan0"]' 的 SHA-256，独立于实现算出。
func TestNetInterfaceSummaryMatchesLegacyEpoch(t *testing.T) {
	t.Parallel()
	got := NetInterfaceSummary([]*heronv1.NetInterfaceCounter{counter("eth0", 10, 20), counter("wlan0", 1, 2)})
	want := NetSummary{Rx: 11, Tx: 22, Epoch: "8bfc281484d99b5647ac7699d7d16ffa675399f904324d89f53979d040771c03"}
	if got != want {
		t.Fatalf("summary = %+v, want %+v", got, want)
	}
	if err := ValidateCounterEpoch(got.Epoch); err != nil {
		t.Fatalf("derived epoch rejected by the admission rule: %v", err)
	}
}

// 合计是逐项 uint64 相加，到 2^64 回绕：与 agent 一直以来填 net_rx_total 的算术相同，hub 的一致性校验按同一算术比较。
func TestNetInterfaceSummaryWrapsAtUint64(t *testing.T) {
	t.Parallel()
	got := NetInterfaceSummary([]*heronv1.NetInterfaceCounter{counter("a", math.MaxUint64, 3), counter("b", 2, math.MaxUint64)})
	if got.Rx != 1 || got.Tx != 2 {
		t.Fatalf("sum = %d/%d, want 1/2 (uint64 wraparound)", got.Rx, got.Tx)
	}
}

func TestValidateNetInterfaces(t *testing.T) {
	t.Parallel()
	full := make([]*heronv1.NetInterfaceCounter, 0, MaxNetInterfaces+1)
	for i := range MaxNetInterfaces + 1 {
		full = append(full, counter(fmt.Sprintf("%03d%s", i, strings.Repeat("x", MaxInterfaceNameBytes-3)), math.MaxUint64, math.MaxUint64))
	}
	for _, tc := range []struct {
		name string
		list []*heronv1.NetInterfaceCounter
		err  string
	}{
		{"absent", nil, ""},
		{"full", full[:MaxNetInterfaces], ""},
		{"over_limit", full, "at most 64 interfaces; got 65"},
		{"unsorted", []*heronv1.NetInterfaceCounter{counter("eth1", 0, 0), counter("eth0", 0, 0)}, "unique and sorted"},
		{"duplicate", []*heronv1.NetInterfaceCounter{counter("eth0", 0, 0), counter("eth0", 0, 0)}, "unique and sorted"},
		{"empty_name", []*heronv1.NetInterfaceCounter{counter("", 0, 0)}, "names must be non-empty"},
		{"control", []*heronv1.NetInterfaceCounter{counter("eth\n0", 0, 0)}, "names must be non-empty"},
		{"long_name", []*heronv1.NetInterfaceCounter{counter(strings.Repeat("x", MaxInterfaceNameBytes+1), 0, 0)}, "names must be non-empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateNetInterfaces(tc.list)
			if tc.err == "" {
				if err != nil {
					t.Fatalf("valid list rejected: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.err) || !strings.HasPrefix(err.Error(), "net_interfaces: ") {
				t.Fatalf("error = %v, want net_interfaces: ... %s", err, tc.err)
			}
		})
	}
}
