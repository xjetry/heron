package ingest

import (
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/store"
	"google.golang.org/protobuf/proto"
)

// Report 每次应答都带逐族探测开关：没有手填时是两族都探测的零值（字段在、各项为 false），agent 才能在手填清空后收到
// "恢复探测"；SetAddressPins 之后该节点的下一次应答即按手填给出；Forget 清掉。
func TestReportCarriesNetworkDetectionFromPins(t *testing.T) {
	h := newHub(t)
	id, tok := h.node(t)
	other, otherTok := h.node(t)
	detection := func(tok string) *heronv1.NetworkDetection {
		t.Helper()
		h.clk.Advance(h.svc.Interval())
		resp, err := h.client.Report(t.Context(), report(tok, &heronv1.Metrics{}))
		if err != nil {
			t.Fatal(err)
		}
		if resp.Msg.Detection == nil {
			t.Fatal("ReportResponse without detection")
		}
		return resp.Msg.Detection
	}
	none := &heronv1.NetworkDetection{}
	if d := detection(tok); !proto.Equal(d, none) {
		t.Fatalf("detection without pins = %v, want both families probed", d)
	}
	h.svc.SetAddressPins(id, "8.8.8.8", "")
	if d := detection(tok); !proto.Equal(d, &heronv1.NetworkDetection{SkipIpv4: true}) {
		t.Fatalf("detection after pinning IPv4 = %v", d)
	}
	if d := detection(otherTok); !proto.Equal(d, none) {
		t.Fatalf("node %d got another node's detection %v", other, d)
	}
	h.svc.SetAddressPins(id, "", "2606:4700::1111")
	if d := detection(tok); !proto.Equal(d, &heronv1.NetworkDetection{SkipIpv6: true}) {
		t.Fatalf("detection after moving the pin to IPv6 = %v", d)
	}
	h.svc.SetAddressPins(id, "", "")
	if d := detection(tok); !proto.Equal(d, none) {
		t.Fatalf("detection after clearing = %v, want both families probed", d)
	}
	h.svc.SetAddressPins(id, "8.8.8.8", "2606:4700::1111")
	h.svc.Forget(id)
	if d := h.svc.detectionFor(id); !proto.Equal(d, none) {
		t.Fatalf("Forget kept detection %v", d)
	}
}

// 启动加载按库里的手填建立开关：hub 重启后不必等管理员再保存一次，agent 照旧停用被手填的族。
func TestLoadRestoresNetworkDetectionFromStore(t *testing.T) {
	h := newHub(t)
	id, _ := h.node(t)
	if _, err := h.store.UpdateNode(t.Context(), id, store.NodeEdit{Name: "n", TrafficResetDay: 1, IPv6Pin: "2606:4700::1111"}); err != nil {
		t.Fatal(err)
	}
	restarted := New(Config{TTL: 30 * time.Second}, h.deps())
	if err := restarted.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	if d := restarted.detectionFor(id); !proto.Equal(d, &heronv1.NetworkDetection{SkipIpv6: true}) {
		t.Fatalf("detection after load = %v, want IPv6 skipped", d)
	}
}
