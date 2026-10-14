package metric

import (
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"google.golang.org/protobuf/proto"
	"testing"
)

func TestResourceRatiosUsePairedReadings(t *testing.T) {
	b := NewBucket()
	b.Add(&heronv1.Metrics{MemUsed: proto.Uint64(10), MemTotal: proto.Uint64(20), DiskUsed: proto.Uint64(25), DiskTotal: proto.Uint64(100)})
	b.Add(&heronv1.Metrics{MemUsed: proto.Uint64(90), MemTotal: proto.Uint64(100), DiskUsed: proto.Uint64(30), DiskTotal: proto.Uint64(40)})
	b.Add(&heronv1.Metrics{MemUsed: proto.Uint64(999), DiskUsed: proto.Uint64(99), DiskTotal: proto.Uint64(0)})
	b.Add(&heronv1.Metrics{MemTotal: proto.Uint64(1000), DiskTotal: proto.Uint64(100)})
	for name, want := range map[string]float64{"memory_used_pct": 70, "disk_used_pct": 50} {
		i := Index(name)
		got, ok := b.Mean(i)
		if !ok || got != want || b.N[i] != 2 {
			t.Fatalf("%s = %v,%v n=%d, want %v,true n=2", name, got, ok, b.N[i], want)
		}
	}
}
