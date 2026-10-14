package api

import (
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/gen/heron/v1/heronv1connect"
	"github.com/xjetry/heron/internal/hub/heartbeat"
)

func TestHeartbeatFieldNumberAndPresence(t *testing.T) {
	t.Parallel()
	field := (&heronv1.Settings{}).ProtoReflect().Descriptor().Fields().ByName("heartbeat")
	if field == nil || field.Number() != 13 || !field.HasPresence() || field.Message() == nil {
		t.Fatalf("settings.heartbeat must be a message with presence at field 13: %v", field)
	}
}

// 从未配置过就不带这一组；配置过后（哪怕随后清空 url）一直带着它，has_url=false 表示停用。
// url 是只写设置：任何响应里都不回显原文，只给 has_url 与 url_host。
func TestHeartbeatSettingsPresenceAndEcho(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	before := saveSettings(t, h, validSettings())
	if before.GetHeartbeat() != nil {
		t.Fatalf("never configured heartbeat must be absent from the echo: %v", before.GetHeartbeat())
	}
	const secret = "https://hc.example/ping/sekrit?token=abc"
	got := saveSettings(t, h, &heronv1.Settings{Heartbeat: &heronv1.Heartbeat{
		Url: secret, IntervalS: 120, Method: heronv1.HeartbeatMethod_HEARTBEAT_METHOD_POST}})
	want := &heronv1.Heartbeat{IntervalS: 120, Method: heronv1.HeartbeatMethod_HEARTBEAT_METHOD_POST, HasUrl: true, UrlHost: "hc.example"}
	if hb := got.GetHeartbeat(); !proto.Equal(hb, want) {
		t.Fatalf("heartbeat echo = %v, want %v", hb, want)
	}
	if strings.Contains(got.String(), "sekrit") {
		t.Fatalf("echo leaked the write-only url: %v", got)
	}
	// 这一组缺席的更新不动它，回显照旧。
	again := saveSettings(t, h, &heronv1.Settings{Title: "改标题", Theme: "auto"})
	if hb := again.GetHeartbeat(); !proto.Equal(hb, want) {
		t.Fatalf("appearance-only update changed heartbeat: %v", hb)
	}
	if hb := currentSettings(t, h).GetHeartbeat(); !proto.Equal(hb, want) {
		t.Fatalf("GetSettings heartbeat = %v, want %v", hb, want)
	}
	// 给出即整体替换：url 空串是清空并停用，method 与 interval_s 也一起换。
	cleared := saveSettings(t, h, &heronv1.Settings{Heartbeat: &heronv1.Heartbeat{
		Url: "", IntervalS: 60, Method: heronv1.HeartbeatMethod_HEARTBEAT_METHOD_HEAD}})
	wantCleared := &heronv1.Heartbeat{IntervalS: 60, Method: heronv1.HeartbeatMethod_HEARTBEAT_METHOD_HEAD}
	if hb := cleared.GetHeartbeat(); !proto.Equal(hb, wantCleared) {
		t.Fatalf("cleared heartbeat = %v, want %v", hb, wantCleared)
	}
}

// interval_s 的范围由 store 写事务裁决，api 把它映射成点名字段与范围的 InvalidArgument；0 不是"取默认"。
// method 的 UNSPECIFIED 与 url 的形态由 api 直接拒绝；被拒的更新一项都不写。
func TestHeartbeatSettingsValidates(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	before := saveSettings(t, h, validSettings())
	base := func(mutate func(*heronv1.Heartbeat)) *heronv1.Settings {
		hb := &heronv1.Heartbeat{Url: "https://hc.example/ping", IntervalS: 60, Method: heronv1.HeartbeatMethod_HEARTBEAT_METHOD_POST}
		mutate(hb)
		return &heronv1.Settings{Heartbeat: hb}
	}
	for _, c := range []struct {
		name string
		in   *heronv1.Settings
		want string
	}{
		{"interval below", base(func(h *heronv1.Heartbeat) { h.IntervalS = 59 }), "heartbeat.interval_s must be in [60, 3600]; got 59"},
		{"interval above", base(func(h *heronv1.Heartbeat) { h.IntervalS = 3601 }), "heartbeat.interval_s must be in [60, 3600]; got 3601"},
		{"interval zero is not the default", base(func(h *heronv1.Heartbeat) { h.IntervalS = 0 }), "heartbeat.interval_s must be in [60, 3600]; got 0"},
		{"method unspecified", base(func(h *heronv1.Heartbeat) { h.Method = heronv1.HeartbeatMethod_HEARTBEAT_METHOD_UNSPECIFIED }),
			"settings.heartbeat.method must be GET, POST or HEAD; HEARTBEAT_METHOD_UNSPECIFIED is not allowed and is not taken as a default"},
		{"url not absolute", base(func(h *heronv1.Heartbeat) { h.Url = "hc.example/ping" }), "settings.heartbeat.url must be empty"},
		{"url wrong scheme", base(func(h *heronv1.Heartbeat) { h.Url = "ftp://hc.example/ping" }), "settings.heartbeat.url must be empty"},
		{"url no host", base(func(h *heronv1.Heartbeat) { h.Url = "https:///ping" }), "settings.heartbeat.url must be empty"},
		{"url too long", base(func(h *heronv1.Heartbeat) { h.Url = "https://hc.example/" + strings.Repeat("a", maxHeartbeatURLBytes) }),
			"settings.heartbeat.url must be at most 2048 bytes"},
	} {
		t.Run(c.name, func(t *testing.T) { rejected(t, h, c.in, c.want, before) })
	}
}

func readHeartbeatStatus(t *testing.T, client heronv1connect.AdminServiceClient, h *harness, token string) *heronv1.GetHeartbeatStatusResponse {
	t.Helper()
	req := connect.NewRequest(&heronv1.GetHeartbeatStatusRequest{})
	c := h.admin
	if token != "" {
		req.Header().Set("Authorization", "Bearer "+token)
		c = client
	}
	r, err := c.GetHeartbeatStatus(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	return r.Msg
}

func TestGetHeartbeatStatusAccessAndState(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	client := heronv1connect.NewAdminServiceClient(h.srv.Client(), h.srv.URL)
	if _, err := client.GetHeartbeatStatus(t.Context(), connect.NewRequest(&heronv1.GetHeartbeatStatusRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("anonymous status=%v", err)
	}
	h.login(t)
	// 从未跑过、也未配置：enabled 为假，两个时刻与 next_at 都是 0，类别为空。
	initial := readHeartbeatStatus(t, client, h, "")
	if initial.Enabled || initial.LastSuccessAt != 0 || initial.LastFailureAt != 0 || initial.FailureCategory != "" || initial.FailureHttpStatus != 0 || initial.NextAt != 0 {
		t.Fatalf("initial status=%v", initial)
	}
	saveSettings(t, h, &heronv1.Settings{Heartbeat: &heronv1.Heartbeat{
		Url: "https://hc.example/ping", IntervalS: 60, Method: heronv1.HeartbeatMethod_HEARTBEAT_METHOD_POST}})
	h.heartbeat.set(heartbeat.Status{
		LastSuccessAt: time.Unix(1000, 0), LastFailureAt: time.Unix(2000, 0),
		FailureCategory: "http_status", FailureHTTPStatus: 503, NextAt: time.Unix(3000, 0),
	})
	want := &heronv1.GetHeartbeatStatusResponse{Enabled: true, LastSuccessAt: 1000, LastFailureAt: 2000, FailureCategory: "http_status", FailureHttpStatus: 503, NextAt: 3000}
	if got := readHeartbeatStatus(t, client, h, ""); !proto.Equal(got, want) {
		t.Fatalf("session status = %v, want %v", got, want)
	}
	// API token 与其它 ACCESS_READ 方法一样放行；状态是全局的，不需要 all_nodes 范围。
	_, token := createToken(t, h, "reader")
	if got := readHeartbeatStatus(t, client, h, token); !proto.Equal(got, want) {
		t.Fatalf("token status = %v, want %v", got, want)
	}
	// 清空 url 即停用：enabled 转假，其余仍是上次跑过的值。
	saveSettings(t, h, &heronv1.Settings{Heartbeat: &heronv1.Heartbeat{
		Url: "", IntervalS: 60, Method: heronv1.HeartbeatMethod_HEARTBEAT_METHOD_POST}})
	if got := readHeartbeatStatus(t, client, h, ""); got.Enabled || got.LastSuccessAt != 1000 {
		t.Fatalf("disabled status = %v", got)
	}
}
