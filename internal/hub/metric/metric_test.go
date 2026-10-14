package metric

import (
	"testing"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"google.golang.org/protobuf/proto"
)

func idx(t *testing.T, name string) int {
	t.Helper()
	for i, c := range Columns {
		if c.Name == name {
			return i
		}
	}
	t.Fatalf("no column %q", name)
	return -1
}

func TestAddCountsOnlyPresentReadings(t *testing.T) {
	b := NewBucket()
	b.Add(&heronv1.Metrics{CpuPct: proto.Float64(10), MemUsed: proto.Uint64(100)})
	b.Add(&heronv1.Metrics{CpuPct: proto.Float64(30)}) // 无 mem_used 读数
	cpu, mem := idx(t, "cpu"), idx(t, "mem_used")
	if got, ok := b.Mean(cpu); !ok || got != 20 {
		t.Fatalf("cpu mean = %v,%v want 20,true", got, ok)
	}
	if b.N[mem] != 1 {
		t.Fatalf("mem_used n = %d, want 1: a missing reading must not enter the count", b.N[mem])
	}
	if got, ok := b.Mean(mem); !ok || got != 100 {
		t.Fatalf("mem_used mean = %v,%v want 100,true", got, ok)
	}
	if b.Max[cpu] != 30 {
		t.Fatalf("cpu max = %v, want 30", b.Max[cpu])
	}
}

func TestMeanOfEmptyColumnIsNoData(t *testing.T) {
	b := NewBucket()
	b.Add(&heronv1.Metrics{CpuPct: proto.Float64(1)})
	if _, ok := b.Mean(idx(t, "swap_used")); ok {
		t.Fatal("swap_used had no readings; mean must report no-data, not 0")
	}
}

func TestMergeIsAdditive(t *testing.T) {
	a, b := NewBucket(), NewBucket()
	a.Add(&heronv1.Metrics{CpuPct: proto.Float64(10)})
	b.Add(&heronv1.Metrics{CpuPct: proto.Float64(20)})
	b.Add(&heronv1.Metrics{CpuPct: proto.Float64(60)})
	a.Merge(b)
	cpu := idx(t, "cpu")
	if a.Sum[cpu] != 90 || a.N[cpu] != 3 || a.Max[cpu] != 60 {
		t.Fatalf("merged sum/n/max = %v/%d/%v, want 90/3/60", a.Sum[cpu], a.N[cpu], a.Max[cpu])
	}
}

func TestColumnsCoverSpecifiedMetrics(t *testing.T) {
	want := map[string]struct {
		kind Kind
		typ  Type
	}{
		"cpu": {MeanMax, Float}, "mem_used": {MeanMax, Int}, "swap_used": {Mean, Int},
		"disk_used": {Mean, Int}, "load1": {Mean, Float}, "tcp": {Mean, Int},
		"udp": {Mean, Int}, "procs": {Mean, Int}, "rx_bytes": {Sum, Int}, "tx_bytes": {Sum, Int},
		"memory_used_pct": {Mean, Float}, "disk_used_pct": {Mean, Float},
		"net_rx_bps": {MeanMax, Int}, "net_tx_bps": {MeanMax, Int},
		"disk_read_bps": {MeanMax, Int}, "disk_write_bps": {MeanMax, Int},
		"cpu_steal_pct": {MeanMax, Float}, "cpu_iowait_pct": {MeanMax, Float},
		"load1_per_core":      {Mean, Float},
		"tcp_active_opens_ps": {MeanMax, Float}, "tcp_passive_opens_ps": {MeanMax, Float},
		"tcp_out_segs_ps": {Mean, Float}, "tcp_retrans_segs_ps": {Mean, Float},
	}
	if len(Columns) != len(want) {
		t.Fatalf("%d columns, want %d", len(Columns), len(want))
	}
	for _, c := range Columns {
		w, ok := want[c.Name]
		if !ok {
			t.Fatalf("unexpected column %q", c.Name)
		}
		if c.Kind != w.kind || c.Type != w.typ {
			t.Fatalf("column %q kind/type = %v/%v, want %v/%v", c.Name, c.Kind, c.Type, w.kind, w.typ)
		}
	}
}

func TestEveryColumnDeclaresItsUnit(t *testing.T) {
	allowed := map[string]bool{"percent": true, "bytes": true, "bytes/s": true, "count": true, "count/s": true, "": true}
	for _, c := range Columns {
		if !allowed[c.Unit] {
			t.Fatalf("%s: unit %q is not one of percent/bytes/bytes/s/count/count/s/\"\"", c.Name, c.Unit)
		}
		if c.Unit == "" && c.Name != "load1" && c.Name != "load1_per_core" {
			t.Fatalf("%s: only load readings have no unit", c.Name)
		}
	}
}

func TestSumColumnsAreFedByAddSumNotByMetrics(t *testing.T) {
	b := NewBucket()
	b.Add(&heronv1.Metrics{NetRxTotal: proto.Uint64(5), NetTxTotal: proto.Uint64(7)})
	if b.N[RxBytes] != 0 || b.N[TxBytes] != 0 {
		t.Fatalf("Sum columns took a value from Metrics: n = %d/%d, want 0/0", b.N[RxBytes], b.N[TxBytes])
	}
	b.AddSum(RxBytes, 1500)
	b.AddSum(RxBytes, 500)
	if b.Sum[RxBytes] != 2000 || b.N[RxBytes] != 2 {
		t.Fatalf("rx_bytes = %v/%d, want 2000/2", b.Sum[RxBytes], b.N[RxBytes])
	}
	o := NewBucket()
	o.AddSum(RxBytes, 1)
	b.Merge(o)
	if b.Sum[RxBytes] != 2001 || b.N[RxBytes] != 3 {
		t.Fatalf("merged rx_bytes = %v/%d, want 2001/3", b.Sum[RxBytes], b.N[RxBytes])
	}
	if _, ok := b.Mean(TxBytes); ok {
		t.Fatal("tx_bytes without AddSum must read as no data")
	}
}

func TestIndexLocatesColumnsByName(t *testing.T) {
	if Index("rx_bytes") != 8 || Index("tx_bytes") != 9 {
		t.Fatalf("rx_bytes/tx_bytes at %d/%d, want stable indices 8/9", Index("rx_bytes"), Index("tx_bytes"))
	}
	if Index("nope") != -1 {
		t.Fatal("unknown name must be -1")
	}
	if Columns[RxBytes].Kind != Sum || Columns[TxBytes].Kind != Sum || Columns[RxBytes].Unit != "bytes" {
		t.Fatal("traffic columns must be Sum kind in bytes")
	}
}
