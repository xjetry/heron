package api

import (
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
)

func offlineDraft(channels ...int64) *heronv1.AlertRule {
	return &heronv1.AlertRule{Name: "草稿", Kind: heronv1.AlertKind_ALERT_KIND_OFFLINE, ChannelIds: channels}
}

// 预览按草稿（未保存，作用域为空）与样例节点渲染每个关联渠道的请求，不出网：Webhook 渠道的地址不可达也照样渲染。
// 结果按渠道 id 升序；Telegram 的目标只到 scheme://host，bot token 不回显。
func TestPreviewAlertNotificationRendersDraft(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "", withTelegramBase("http://tg.invalid"))
	h.login(t)
	node, _ := h.createNode(t, "东京")
	hook := webhook("http://127.0.0.1:1/hook?key=secret")
	hook.Webhook.BodyTemplate = `{"s":{{json .Summary}},"r":{{json .Rule}},"n":{{json .Node}},"t":{{json .Transition}}}`
	hook.Webhook.Headers = map[string]string{"Authorization": "Bearer downstream"}
	w := saveChannel(t, h, hook)
	tg := saveChannel(t, h, &heronv1.NotifyChannel{Name: "tg", Kind: heronv1.ChannelKind_CHANNEL_KIND_TELEGRAM, Telegram: &heronv1.TelegramConfig{BotToken: "123:secret", ChatId: "42"}})
	before := rowCounts(t, h.dbPath)
	resp, err := h.admin.PreviewAlertNotification(t.Context(), connect.NewRequest(&heronv1.PreviewAlertNotificationRequest{
		Rule: offlineDraft(tg.Id, w.Id), NodeId: node, Transition: "firing"}))
	if err != nil {
		t.Fatal(err)
	}
	want := []*heronv1.AlertNotificationPreview{
		{ChannelId: w.Id, Method: "POST", UrlHost: "http://127.0.0.1:1", Body: `{"s":"节点 东京 离线 30s（规则 草稿）","r":"草稿","n":"东京","t":"firing"}`},
		{ChannelId: tg.Id, Method: "POST", UrlHost: "http://tg.invalid", Body: `{"chat_id":"42","text":"节点 东京 离线 30s（规则 草稿）"}`},
	}
	if len(resp.Msg.Previews) != len(want) {
		t.Fatalf("previews=%v", resp.Msg.Previews)
	}
	for i := range want {
		if !proto.Equal(resp.Msg.Previews[i], want[i]) {
			t.Fatalf("preview %d = %v, want %v", i, resp.Msg.Previews[i], want[i])
		}
	}
	if after := rowCounts(t, h.dbPath); !maps.Equal(before, after) {
		t.Fatalf("preview wrote rows: before=%v after=%v", before, after)
	}
}

// 模板执行出错只让那个渠道的结果带错误原文，其余渠道照常渲染，整个请求不失败。
func TestPreviewAlertNotificationReportsTemplateErrorPerChannel(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	node, _ := h.createNode(t, "n")
	bad := webhook("http://127.0.0.1:1")
	bad.Webhook.BodyTemplate = `{{if eq .Transition "recovered"}}{{.Missing}}{{end}}ok`
	b := saveChannel(t, h, bad)
	good := saveChannel(t, h, webhook("http://127.0.0.1:2"))
	resp, err := h.admin.PreviewAlertNotification(t.Context(), connect.NewRequest(&heronv1.PreviewAlertNotificationRequest{
		Rule: offlineDraft(b.Id, good.Id), NodeId: node, Transition: "recovered"}))
	if err != nil {
		t.Fatal(err)
	}
	p := resp.Msg.Previews
	if len(p) != 2 || p[0].ChannelId != b.Id || !strings.Contains(p[0].Error, "can't evaluate field Missing") || p[0].Body != "" || p[0].Method != "" {
		t.Fatalf("bad channel preview=%v", p)
	}
	if p[1].Error != "" || !strings.Contains(p[1].Body, `"transition":"recovered"`) || !strings.Contains(p[1].Body, "，告警持续 30 分钟") {
		t.Fatalf("good channel preview=%v", p[1])
	}
}

// 试投递经出站客户端同步发出：摘要冠以 "[测试] "，其余字段是真实形态；失败的渠道逐个给出类别、状态码与原文。
// 不写告警状态、事件与投递记录（任何表的行数都不变）。
func TestTestAlertNotificationSendsPrefixedSampleWithoutRecording(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	node, _ := h.createNode(t, "东京")
	type received struct {
		Rule, Node, Kind, Transition, Summary string
		Value                                 float64
	}
	got := make(chan received, 1)
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m received
		_ = json.NewDecoder(r.Body).Decode(&m)
		got <- m
	}))
	defer ok.Close()
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("bad key"))
	}))
	defer failing.Close()
	good := saveChannel(t, h, webhook(ok.URL))
	bad := saveChannel(t, h, webhook(failing.URL))
	before := rowCounts(t, h.dbPath)
	resp, err := h.admin.TestAlertNotification(t.Context(), connect.NewRequest(&heronv1.TestAlertNotificationRequest{
		Rule: offlineDraft(bad.Id, good.Id), NodeId: node, Transition: "firing"}))
	if err != nil {
		t.Fatal(err)
	}
	want := []*heronv1.AlertNotificationTestResult{
		{ChannelId: good.Id, Ok: true},
		{ChannelId: bad.Id, Failure: heronv1.DeliveryFailure_DELIVERY_FAILURE_HTTP_STATUS, HttpStatus: proto.Uint32(401), Error: "HTTP 401 Unauthorized: bad key"},
	}
	if len(resp.Msg.Results) != 2 || !proto.Equal(resp.Msg.Results[0], want[0]) || !proto.Equal(resp.Msg.Results[1], want[1]) {
		t.Fatalf("results=%v want %v", resp.Msg.Results, want)
	}
	m := <-got
	if m != (received{Rule: "草稿", Node: "东京", Kind: "offline", Transition: "firing", Summary: "[测试] 节点 东京 离线 30s（规则 草稿）", Value: 30}) {
		t.Fatalf("received=%+v", m)
	}
	if after := rowCounts(t, h.dbPath); !maps.Equal(before, after) {
		t.Fatalf("test send wrote rows: before=%v after=%v", before, after)
	}
}

// 两个入口共用的输入校验：规则字段与 SaveAlertRule 同样以 rule.<字段> 点名；渠道只能是已保存的；节点须存在；阶段只有两种。
func TestAlertNotificationValidation(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	node, _ := h.createNode(t, "n")
	c := saveChannel(t, h, webhook("http://127.0.0.1:1"))
	for _, tc := range []struct {
		name string
		edit func(*heronv1.PreviewAlertNotificationRequest)
		code connect.Code
		text string
	}{
		{"rule_name", func(r *heronv1.PreviewAlertNotificationRequest) { r.Rule.Name = "" }, connect.CodeInvalidArgument, "rule.name must contain"},
		{"rule_kind", func(r *heronv1.PreviewAlertNotificationRequest) { r.Rule.Kind = 0 }, connect.CodeInvalidArgument, "rule.kind must be"},
		{"no_rule", func(r *heronv1.PreviewAlertNotificationRequest) { r.Rule = nil }, connect.CodeInvalidArgument, "rule.kind must be"},
		{"no_channels", func(r *heronv1.PreviewAlertNotificationRequest) { r.Rule.ChannelIds = nil }, connect.CodeInvalidArgument, "rule.channel_ids must name between 1 and 16 saved notify channels"},
		{"unsaved_channel", func(r *heronv1.PreviewAlertNotificationRequest) { r.Rule.ChannelIds = []int64{c.Id, 99} }, connect.CodeNotFound, "rule.channel_ids: notify channel 99 does not exist"},
		{"node", func(r *heronv1.PreviewAlertNotificationRequest) { r.NodeId = 99 }, connect.CodeNotFound, "node 99 does not exist"},
		{"transition", func(r *heronv1.PreviewAlertNotificationRequest) { r.Transition = "pending" }, connect.CodeInvalidArgument, `transition must be one of firing, recovered; got "pending"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := &heronv1.PreviewAlertNotificationRequest{Rule: offlineDraft(c.Id), NodeId: node, Transition: "firing"}
			tc.edit(req)
			_, err := h.admin.PreviewAlertNotification(t.Context(), connect.NewRequest(req))
			if codeOf(err) != tc.code || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("preview err=%v want %s %q", err, tc.code, tc.text)
			}
			_, err = h.admin.TestAlertNotification(t.Context(), connect.NewRequest(&heronv1.TestAlertNotificationRequest{Rule: req.Rule, NodeId: req.NodeId, Transition: req.Transition}))
			if codeOf(err) != tc.code || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("test err=%v want %s %q", err, tc.code, tc.text)
			}
		})
	}
}

// 回显可能含下游密钥的渲染结果与接收方原文，两个入口都只对会话开放：API token 一律 PermissionDenied，且不出网。
func TestAlertNotificationRejectsAPITokens(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	node, _ := h.createNode(t, "n")
	hits := make(chan struct{}, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits <- struct{}{} }))
	defer srv.Close()
	c := saveChannel(t, h, webhook(srv.URL))
	_, token := createToken(t, h, "automation")
	body := `{"rule":{"name":"x","kind":"ALERT_KIND_OFFLINE","channelIds":["` + strconv.FormatInt(c.Id, 10) + `"]},"nodeId":"` + strconv.FormatInt(node, 10) + `","transition":"firing"}`
	for _, method := range []string{"PreviewAlertNotification", "TestAlertNotification"} {
		if got := rawCall(t, h, method, body, bearer(token)); got.status != 403 || got.code != "permission_denied" {
			t.Errorf("%s token must be PermissionDenied: %+v", method, got)
		}
	}
	if len(hits) != 0 {
		t.Fatal("token call reached the channel")
	}
	// 对照：同一个请求体经会话可以调用，说明上面的拒绝来自凭据而不是请求体。
	if got := rawCall(t, h, "TestAlertNotification", body, map[string][]string{"Cookie": {sessionCookieHeader(t, h)}}); got.status != 200 {
		t.Fatalf("session call: %+v", got)
	}
	if len(hits) != 1 {
		t.Fatalf("session call hits=%d", len(hits))
	}
}
