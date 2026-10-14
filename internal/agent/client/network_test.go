package client

import (
	"context"
	"errors"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"google.golang.org/protobuf/proto"
	"testing"
	"time"
)

type networkSnapshot struct {
	value *heronv1.NetworkInfo
	skips []*heronv1.NetworkDetection
}

func (n *networkSnapshot) Snapshot() *heronv1.NetworkInfo {
	return proto.Clone(n.value).(*heronv1.NetworkInfo)
}

func (n *networkSnapshot) Skip(d *heronv1.NetworkDetection) { n.skips = append(n.skips, d) }

func TestNetworkChangesReconcileWithoutRestart(t *testing.T) {
	hub := &fakeHub{interval: 5000, reconcile: true}
	r, _ := newRunner(t, hub)
	n := &networkSnapshot{value: &heronv1.NetworkInfo{Ipv4: &heronv1.AddressDetection{State: 1, Address: "8.8.4.4", CheckedAt: 100}}}
	r.Network = n
	round := 0
	r.Sleep = func(context.Context, time.Duration) error {
		round++
		if round == 1 {
			n.value.Ipv4 = &heronv1.AddressDetection{State: 3, CheckedAt: 200}
		}
		if round == 3 {
			return context.Canceled
		}
		return nil
	}
	if err := r.Run(t.Context()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	reports := hub.received()
	if reports[0].Facts.GetNetwork().GetIpv4().GetAddress() != "8.8.4.4" {
		t.Fatalf("initial=%v", reports[0])
	}
	if reports[1].FactsHash == reports[0].FactsHash || reports[1].Facts != nil {
		t.Fatal("network change did not use hash reconciliation")
	}
	if got := reports[2].Facts.GetNetwork().GetIpv4(); got.GetState() != 3 || got.GetAddress() != "" || got.GetCheckedAt() != 200 {
		t.Fatalf("requested network=%v", got)
	}
}

// hub 的逐族开关每次成功应答都交给探测器。应答没带 detection（与各项为 false 相同，清空手填之后也是这样）时交 nil，探测器恢复探测。
// 失败的上报没有应答，不交。
func TestRunnerHandsDetectionToNetworkAfterEverySuccessfulReport(t *testing.T) {
	hub := &fakeHub{interval: 5000, detection: &heronv1.NetworkDetection{SkipIpv4: true}}
	r, _ := newRunner(t, hub)
	n := &networkSnapshot{value: &heronv1.NetworkInfo{}}
	r.Network = n
	round := 0
	r.Sleep = func(context.Context, time.Duration) error {
		round++
		hub.mu.Lock()
		defer hub.mu.Unlock()
		switch round {
		case 1:
			hub.detection = nil
		case 2:
			hub.fail = true
		case 3:
			return context.Canceled
		}
		return nil
	}
	if err := r.Run(t.Context()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(n.skips) != 2 || !n.skips[0].GetSkipIpv4() || n.skips[0].GetSkipIpv6() || n.skips[1] != nil {
		t.Fatalf("detections handed to network = %v, want [skip_ipv4, nil] and nothing for the failed report", n.skips)
	}
}
