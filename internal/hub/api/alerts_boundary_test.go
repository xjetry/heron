package api

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/alert"
	"google.golang.org/protobuf/proto"
)

func TestAlertEnumGotUsesProtocolVocabulary(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	resp, err := h.http.Post(h.srv.URL+"/heron.v1.AdminService/SaveAlertRule", "application/json", strings.NewReader(`{"rule":{"name":"offline","kind":"offline","allNodes":true}}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 400 || !strings.Contains(string(body), `got \"ALERT_KIND_UNSPECIFIED\"`) {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
}

func TestWebhookHeaderNamesAreUnambiguous(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	for _, tc := range []struct {
		name    string
		headers map[string]string
		remove  []string
		want    []string
	}{
		{"duplicate", map[string]string{"x-a": "one", "X-A": "two"}, nil, []string{"channel.webhook.headers", "x-a", "X-A"}},
		{"remove", nil, []string{"bad key"}, []string{"channel.webhook.remove_headers", "bad key"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := webhook("http://host")
			c.Webhook.Headers, c.Webhook.RemoveHeaders = tc.headers, tc.remove
			_, err := h.admin.SaveNotifyChannel(t.Context(), connect.NewRequest(&heronv1.SaveNotifyChannelRequest{Channel: c}))
			if codeOf(err) != connect.CodeInvalidArgument {
				t.Fatalf("err=%v", err)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err=%v lacks %s", err, want)
				}
			}
		})
	}
}

func TestUpdateNodeRequiresExplicitGrace(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, _ := h.createNode(t, "n")
	_, err := h.admin.UpdateNode(t.Context(), connect.NewRequest(&heronv1.UpdateNodeRequest{Id: id, Name: "n", TrafficResetDay: 1}))
	if codeOf(err) != connect.CodeInvalidArgument || err.Error() != "invalid_argument: offline_grace_s: required; 0 clears it" {
		t.Fatalf("err=%v", err)
	}
}

func TestNewRejectsNonpositiveTTL(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	for _, ttl := range []time.Duration{0, -time.Second} {
		t.Run(ttl.String(), func(t *testing.T) {
			defer func() {
				if r := recover(); r != "api.Config.TTL must be positive" {
					t.Fatalf("panic=%v", r)
				}
			}()
			cfg := h.svc.cfg
			cfg.TTL = ttl
			New(cfg, h.deps())
		})
	}
}

func TestTaskIDsMustFitStorage(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	for _, tc := range []struct {
		name  string
		call  func(uint64) error
		field string
	}{
		{"rule", func(id uint64) error {
			r := offlineRule()
			r.TaskId = id
			_, err := h.admin.SaveAlertRule(t.Context(), connect.NewRequest(&heronv1.SaveAlertRuleRequest{Rule: r}))
			return err
		}, "rule.task_id"},
		{"delete", func(id uint64) error {
			_, err := h.admin.DeleteProbeTask(t.Context(), connect.NewRequest(&heronv1.DeleteProbeTaskRequest{Id: id}))
			return err
		}, "id"},
		{"save", func(id uint64) error {
			task := validProbeTask()
			task.Id = id
			_, err := h.admin.SaveProbeTask(t.Context(), connect.NewRequest(&heronv1.SaveProbeTaskRequest{Task: task}))
			return err
		}, "task.id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call(math.MaxInt64 + 1)
			if codeOf(err) != connect.CodeInvalidArgument || err.Error() != "invalid_argument: "+tc.field+": out of range" {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestAlertFieldErrorsUseProtocolVocabulary(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	for _, tc := range []struct {
		name string
		call func() error
		want []string
	}{
		{"rule_kind", func() error {
			r := offlineRule()
			r.Kind = 0
			_, err := h.admin.SaveAlertRule(t.Context(), connect.NewRequest(&heronv1.SaveAlertRuleRequest{Rule: r}))
			return err
		}, []string{"rule.kind", "ALERT_KIND_OFFLINE", "ALERT_KIND_PROBE"}},
		{"metric", func() error {
			r := offlineRule()
			r.Kind = heronv1.AlertKind_ALERT_KIND_PROBE
			r.TaskId = 1
			_, err := h.admin.SaveAlertRule(t.Context(), connect.NewRequest(&heronv1.SaveAlertRuleRequest{Rule: r}))
			return err
		}, []string{"rule.metric", "PROBE_METRIC_LOSS_PCT", "PROBE_METRIC_RTT_MS"}},
		{"channel_kind", func() error {
			c := webhook("http://host")
			c.Kind = 0
			_, err := h.admin.SaveNotifyChannel(t.Context(), connect.NewRequest(&heronv1.SaveNotifyChannelRequest{Channel: c}))
			return err
		}, []string{"channel.kind", "CHANNEL_KIND_TELEGRAM", "CHANNEL_KIND_WEBHOOK"}},
		{"url", func() error {
			_, err := h.admin.SaveNotifyChannel(t.Context(), connect.NewRequest(&heronv1.SaveNotifyChannelRequest{Channel: webhook("bad")}))
			return err
		}, []string{"channel.webhook.url"}},
		{"method", func() error {
			c := webhook("http://host")
			c.Webhook.Method = "GET"
			_, err := h.admin.SaveNotifyChannel(t.Context(), connect.NewRequest(&heronv1.SaveNotifyChannelRequest{Channel: c}))
			return err
		}, []string{"channel.webhook.method", "POST", "PUT", "PATCH"}},
		{"chat", func() error {
			_, err := h.admin.SaveNotifyChannel(t.Context(), connect.NewRequest(&heronv1.SaveNotifyChannelRequest{Channel: &heronv1.NotifyChannel{Name: "tg", Kind: heronv1.ChannelKind_CHANNEL_KIND_TELEGRAM, Telegram: &heronv1.TelegramConfig{BotToken: "secret"}}}))
			return err
		}, []string{"channel.telegram.chat_id"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if codeOf(err) != connect.CodeInvalidArgument {
				t.Fatalf("err=%v", err)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err=%v lacks %s", err, want)
				}
			}
		})
	}
	// 包装层可以补上下文，但不能改变结构化字段定位。
	wrapped := fmt.Errorf("outer: %w", alert.FieldError{Path: "webhook.url", Constraint: "must be absolute"})
	if got := h.svc.operationError(wrapped, "channel", "save"); got.Error() != "invalid_argument: channel.webhook.url must be absolute" {
		t.Fatalf("wrapped=%v", got)
	}
}

func TestTestNotifyChannelClassifiesFailures(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	for _, status := range []int{400, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status); fmt.Fprint(w, "reason") }))
			defer srv.Close()
			c := saveChannel(t, h, webhook(srv.URL))
			_, err := h.admin.TestNotifyChannel(t.Context(), connect.NewRequest(&heronv1.TestNotifyChannelRequest{Id: c.Id}))
			want := connect.CodeUnavailable
			if status == 400 {
				want = connect.CodeFailedPrecondition
			}
			if codeOf(err) != want || err.Error() != fmt.Sprintf("%s: HTTP %d %s: reason", want, status, http.StatusText(status)) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestWebhookCredentialsAreWriteOnlyAndMerged(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	type request struct{ path, auth, keep, replaced string }
	requests := make(chan request, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- request{r.URL.RequestURI(), r.Header.Get("Authorization"), r.Header.Get("X-Keep"), r.Header.Get("X-Replace")}
	}))
	defer srv.Close()
	c := webhook(strings.Replace(srv.URL, "://", "://user:password@", 1) + "/secret?key=hidden")
	c.Webhook.Headers = map[string]string{"authorization": "Bearer hidden", "X-Keep": "kept", "X-Replace": "old"}
	c = saveChannel(t, h, c)
	check := func(c *heronv1.NotifyChannel) {
		t.Helper()
		want := &heronv1.WebhookConfig{Method: "POST", HasUrl: true, UrlHost: srv.URL, HeaderNames: []string{"Authorization", "X-Keep", "X-Replace"}}
		if !proto.Equal(c.Webhook, want) {
			t.Fatalf("credentials response=%v", c.Webhook)
		}
	}
	check(c)
	list, err := h.admin.ListNotifyChannels(t.Context(), connect.NewRequest(&heronv1.ListNotifyChannelsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	check(list.Msg.Channels[0])
	c.Name = "renamed"
	c = saveChannel(t, h, c)
	check(c)
	send := func() request {
		t.Helper()
		if _, err := h.admin.TestNotifyChannel(t.Context(), connect.NewRequest(&heronv1.TestNotifyChannelRequest{Id: c.Id})); err != nil {
			t.Fatal(err)
		}
		return <-requests
	}
	if got := send(); got != (request{"/secret?key=hidden", "Bearer hidden", "kept", "old"}) {
		t.Fatalf("preserved=%+v", got)
	}
	c.Webhook.RemoveHeaders = []string{"AUTHORIZATION", "x-replace"}
	c.Webhook.Headers = map[string]string{"x-replace": "new"}
	c = saveChannel(t, h, c)
	if got := send(); got != (request{"/secret?key=hidden", "Basic dXNlcjpwYXNzd29yZA==", "kept", "new"}) {
		t.Fatalf("merged=%+v", got)
	}
	if cfg := h.alerts.Channels()[0].Config; strings.Contains(cfg, "remove_headers") {
		t.Fatalf("removal command persisted=%s", cfg)
	}
	if len(c.Webhook.RemoveHeaders) != 0 {
		t.Fatalf("removal command echoed=%v", c)
	}
	c = saveChannel(t, h, c)
	if got := send(); got.replaced != "new" {
		t.Fatalf("removal persisted=%+v", got)
	}
	c.Webhook.Url = srv.URL + "/replacement"
	c = saveChannel(t, h, c)
	if got := send(); got.path != "/replacement" {
		t.Fatalf("URL not replaced=%+v", got)
	}
}
