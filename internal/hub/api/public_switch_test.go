package api

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"google.golang.org/protobuf/proto"
)

func TestPublicSwitchSettings(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	if got := currentSettings(t, h); got.PublicEnabled == nil || !got.GetPublicEnabled() {
		t.Fatal("never saved: public_enabled must be present and true")
	}
	for _, enabled := range []bool{false, true, false} {
		in := validSettings()
		in.PublicEnabled = proto.Bool(enabled)
		if got := saveSettings(t, h, in); got.PublicEnabled == nil || got.GetPublicEnabled() != enabled {
			t.Fatalf("saved public_enabled must be present and %v: %v", enabled, got)
		}
		if got := currentSettings(t, h); got.PublicEnabled == nil || got.GetPublicEnabled() != enabled {
			t.Fatalf("read public_enabled must be present and %v: %v", enabled, got)
		}
	}
}

func TestPublicSwitchOmittedSettings(t *testing.T) {
	t.Parallel()
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			h := newHarness(t, "")
			h.login(t)
			in := validSettings()
			in.PublicEnabled = proto.Bool(enabled)
			saveSettings(t, h, in)
			out := saveSettings(t, h, &heronv1.Settings{Title: "旧客户端改标题", Theme: "auto"})
			if out.PublicEnabled == nil || out.GetPublicEnabled() != enabled {
				t.Errorf("omitted gate echo must be present and %v: %v", enabled, out)
			}
			if got := currentSettings(t, h); got.PublicEnabled == nil || got.GetPublicEnabled() != enabled {
				t.Errorf("omitted gate persisted must be present and %v: %v", enabled, got)
			}
			want := http.StatusNotFound
			if enabled {
				want = http.StatusOK
			}
			if got := pubGet(t, h, "GetSite", jsonQuery("{}"), nil); got.status != want {
				t.Errorf("omitted gate public status = %d, want %d", got.status, want)
			}
		})
	}
}

// 总闸自成一组：只带 public_enabled 的请求照常生效，外观与国家查询原样保留；一组都没给出的请求被拒，总闸不变。
func TestPublicSwitchAloneIsAGroup(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	before := saveSettings(t, h, validSettings())
	want := proto.Clone(before).(*heronv1.Settings)
	want.PublicEnabled = proto.Bool(false)
	if got := saveSettings(t, h, &heronv1.Settings{PublicEnabled: proto.Bool(false)}); !proto.Equal(got, want) {
		t.Fatalf("gate-only echo = %v, want %v", got, want)
	}
	if got := pubGet(t, h, "GetSite", jsonQuery("{}"), nil); got.status != http.StatusNotFound {
		t.Fatalf("gate-only close: GetSite status = %d, want %d", got.status, http.StatusNotFound)
	}
	_, err := h.admin.UpdateSettings(t.Context(), connect.NewRequest(&heronv1.UpdateSettingsRequest{Settings: &heronv1.Settings{}}))
	if codeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), noGroup) {
		t.Fatalf("empty update err = %v, want InvalidArgument containing %q", err, noGroup)
	}
	if got := currentSettings(t, h); !proto.Equal(got, want) || h.store.PublicEnabled() {
		t.Fatalf("rejected empty update changed settings to %v (gate in memory %v)", got, h.store.PublicEnabled())
	}
}

func TestPublicSwitchAllMethodsAndNodePreservation(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, _ := h.createNode(t, "public")
	h.setPublic(t, id, "public", true)
	private, _ := h.createNode(t, "private")
	h.setPublic(t, private, "private", false)
	// 对比入口需要任务与节点清单；其余方法共享 nodeId/from/to/maxPoints。
	saved, err := h.admin.SaveProbeTask(t.Context(), connect.NewRequest(&heronv1.SaveProbeTaskRequest{Task: &heronv1.ProbeTask{Kind: heronv1.ProbeKind_PROBE_KIND_ICMP, Target: "192.0.2.1", IntervalS: 60, TimeoutMs: 1000}, AllNodes: true}))
	if err != nil {
		t.Fatal(err)
	}
	task := saved.Msg.Task.Task.Id
	window := fmt.Sprintf(`"from":"%d","to":"%d","maxPoints":100`, h.clk.Now().Unix()-3600, h.clk.Now().Unix())
	bodies := map[string]string{
		"ListProbeComparisonNodes": fmt.Sprintf(`{"taskId":"%d"}`, task),
		"QueryProbeComparison":     fmt.Sprintf(`{"taskId":"%d","nodeIds":["%d"],%s}`, task, id, window),
	}
	methods := heronv1.File_heron_v1_public_proto.Services().ByName("PublicService").Methods()
	if methods.Len() == 0 {
		t.Fatal("PublicService has no methods")
	}
	for _, enabled := range []bool{false, true} {
		in := validSettings()
		in.PublicEnabled = proto.Bool(enabled)
		saveSettings(t, h, in)
		for i := 0; i < methods.Len(); i++ {
			m := methods.Get(i)
			// 未经特判的方法共享这些查询字段；空消息忽略未知字段，新增方法也自动经过总闸断言。
			body, ok := bodies[string(m.Name())]
			if !ok {
				body = fmt.Sprintf(`{"nodeId":"%d",%s}`, id, window)
			}
			got := pubPost(t, h, string(m.Name()), body, nil)
			want := http.StatusNotFound
			if enabled {
				want = http.StatusOK
			}
			if got.status != want || (!enabled && !strings.Contains(string(got.body), `"code":"not_found"`)) {
				t.Errorf("%s enabled=%v: status=%d body=%s, want %d", m.Name(), enabled, got.status, got.body, want)
			}
		}
		for node, want := range map[int64]bool{id: true, private: false} {
			got, err := h.store.NodeIsPublic(t.Context(), node)
			if err != nil || got != want {
				t.Fatalf("node %d public changed: %v %v, want %v", node, got, err, want)
			}
		}
	}
	snap, err := h.publicClient().GetSnapshot(t.Context(), connect.NewRequest(&heronv1.PublicServiceGetSnapshotRequest{}))
	if err != nil || len(snap.Msg.Nodes) != 1 || snap.Msg.Nodes[0].Id != id {
		t.Fatalf("reopened snapshot lost public node: %v %v", snap, err)
	}
}

func TestPublicSwitchSnapshotCacheWindow(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	first := pubGet(t, h, "GetSnapshot", jsonQuery("{}"), nil)
	if first.status != 200 {
		t.Fatalf("prime snapshot: %d %s", first.status, first.body)
	}
	h.clk.Advance(500 * time.Millisecond)
	saveSettings(t, h, &heronv1.Settings{PublicEnabled: proto.Bool(false)})
	cached := pubGet(t, h, "GetSnapshot", jsonQuery("{}"), nil)
	if cached.status != 200 || !bytes.Equal(cached.body, first.body) {
		t.Fatalf("snapshot inside 1s cache window: %d %s", cached.status, cached.body)
	}
	h.clk.Advance(time.Second)
	expired := pubGet(t, h, "GetSnapshot", jsonQuery("{}"), nil)
	if expired.status != 404 || expired.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("expired snapshot: %d %s %v", expired.status, expired.body, expired.header)
	}
}

func TestPublicSwitchStillRateLimits(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	saveSettings(t, h, &heronv1.Settings{PublicEnabled: proto.Bool(false)})
	for i := 0; i <= publicBurst; i++ {
		got := pubGet(t, h, "GetSite", jsonQuery("{}"), nil)
		want := 404
		if i == publicBurst {
			want = 429
		}
		if got.status != want || got.header.Get("Cache-Control") != "no-store" {
			t.Fatalf("closed request %d: %d %s %v, want %d", i, got.status, got.body, got.header, want)
		}
		if i == publicBurst && !strings.Contains(string(got.body), "resource_exhausted") {
			t.Fatalf("limit response: %s", got.body)
		}
	}
}

func TestPublicSwitchDoesNotReadDatabase(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	saveSettings(t, h, &heronv1.Settings{PublicEnabled: proto.Bool(false)})
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}
	got := pubGet(t, h, "GetSite", jsonQuery("{}"), nil)
	if got.status != 404 {
		t.Fatalf("gate needs database: %d %s", got.status, got.body)
	}
}
