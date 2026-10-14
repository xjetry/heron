package metric

import (
	"testing"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"google.golang.org/protobuf/proto"
)

func TestNetworkPeaksKeepSamplesSeparateFromTraffic(t *testing.T) {
	rx, tx := idx(t, "net_rx_bps"), idx(t, "net_tx_bps")
	b := NewBucket()
	b.Add(&heronv1.Metrics{NetRxBps: proto.Uint64(800), NetTxBps: proto.Uint64(0)})
	b.Add(&heronv1.Metrics{NetRxBps: proto.Uint64(20)})
	b.Add(&heronv1.Metrics{})
	b.AddSum(RxBytes, 1200)
	other := NewBucket()
	other.Add(&heronv1.Metrics{NetRxBps: proto.Uint64(100), NetTxBps: proto.Uint64(50)})
	other.AddSum(RxBytes, 300)
	b.Merge(other)
	if b.Max[rx] != 800 || b.N[rx] != 3 || b.Max[tx] != 50 || b.N[tx] != 2 {
		t.Fatalf("rate max/count = %v/%d %v/%d", b.Max[rx], b.N[rx], b.Max[tx], b.N[tx])
	}
	if b.Sum[RxBytes] != 1500 || b.N[RxBytes] != 2 || b.N[TxBytes] != 0 {
		t.Fatalf("sampling rates changed traffic: %+v", b)
	}
	empty := NewBucket()
	empty.Add(&heronv1.Metrics{NetRxTotal: proto.Uint64(800)})
	if empty.N[rx] != 0 || empty.N[tx] != 0 {
		t.Fatal("a cumulative counter is not a sampled rate")
	}
	empty.Add(&heronv1.Metrics{NetRxBps: proto.Uint64(0)})
	if empty.N[rx] != 1 || empty.Max[rx] != 0 || empty.N[tx] != 0 {
		t.Fatal("zero and missing rates must stay distinct")
	}
}
