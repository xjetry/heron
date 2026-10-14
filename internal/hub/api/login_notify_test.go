package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/testwait"
	"google.golang.org/protobuf/proto"
)

func putNotifySettings(t *testing.T, h *harness, body string, status int) map[string]any {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.srv.URL+"/heron.v1.AdminService/UpdateSettings", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != status {
		t.Fatalf("settings HTTP status=%d body=%s, want %d", resp.StatusCode, raw, status)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func chooseLoginChannels(t *testing.T, h *harness, ids ...int64) *heronv1.Settings {
	t.Helper()
	return saveSettings(t, h, &heronv1.Settings{LoginNotify: &heronv1.LoginNotify{ChannelIds: ids}})
}

func TestLoginNotifyFieldNumberAndPresence(t *testing.T) {
	t.Parallel()
	field := (&heronv1.Settings{}).ProtoReflect().Descriptor().Fields().ByName("login_notify")
	if field == nil || field.Number() != 12 || !field.HasPresence() || field.Message() == nil {
		t.Fatalf("settings.login_notify must be a message with presence at field 12: %v", field)
	}
}

func loginEvents(t *testing.T, h *harness) []*heronv1.AlertEvent {
	t.Helper()
	r, err := h.admin.ListAlertEvents(t.Context(), connect.NewRequest(&heronv1.ListAlertEventsRequest{Limit: 100}))
	if err != nil {
		t.Fatal(err)
	}
	return r.Msg.Events
}

func TestLoginNotifySettingsPresenceReferencesAndDeletion(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	a := saveChannel(t, h, webhook("https://example.invalid/a")).Id
	b := saveChannel(t, h, webhook("https://example.invalid/b")).Id
	chooseLoginChannels(t, h, b, a, b)
	appearance := validSettings()
	got := saveSettings(t, h, appearance)
	if !slices.Equal(got.GetLoginNotify().GetChannelIds(), []int64{a, b}) || !slices.Equal(currentSettings(t, h).GetLoginNotify().GetChannelIds(), []int64{a, b}) {
		t.Fatalf("appearance update cleared or failed to canonicalize login channels: %v", got)
	}
	for _, id := range []int64{0, -1, 987654} {
		bad := proto.Clone(appearance).(*heronv1.Settings)
		bad.Title = "不得保存"
		bad.LoginNotify = &heronv1.LoginNotify{ChannelIds: []int64{a, id}}
		rejected(t, h, bad, fmt.Sprintf("settings.login_notify.channel_ids: channel %d does not exist", id), got)
	}
	if _, err := h.admin.DeleteNotifyChannel(t.Context(), connect.NewRequest(&heronv1.DeleteNotifyChannelRequest{Id: a})); err != nil {
		t.Fatal(err)
	}
	if ids := currentSettings(t, h).GetLoginNotify().GetChannelIds(); !slices.Equal(ids, []int64{b}) {
		t.Fatalf("deleted channel remains in login settings: %v", ids)
	}
	chooseLoginChannels(t, h)
	if ids := currentSettings(t, h).GetLoginNotify().GetChannelIds(); len(ids) != 0 {
		t.Fatalf("explicit empty login channels did not disable notifications: %v", ids)
	}
}

// 列表按请求里的原始条数计，重复也算：满上限的重复合法并合并成一个，多一条整次拒绝、什么都不写。
func TestLoginNotifyChannelListCountsRawEntries(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	a := saveChannel(t, h, webhook("https://example.invalid/a")).Id
	full := make([]int64, maxNotifyChannels)
	for i := range full {
		full[i] = a
	}
	before := chooseLoginChannels(t, h, full...)
	if ids := before.GetLoginNotify().GetChannelIds(); !slices.Equal(ids, []int64{a}) {
		t.Fatalf("%d copies of one channel saved as %v, want [%d]", len(full), ids, a)
	}
	over := proto.Clone(before).(*heronv1.Settings)
	over.Title = "不得保存"
	over.LoginNotify = &heronv1.LoginNotify{ChannelIds: append(full, a)}
	rejected(t, h, over, fmt.Sprintf("settings.login_notify.channel_ids must list at most %d channel IDs, duplicates included; got %d", maxNotifyChannels, maxNotifyChannels+1), before)
}

// 响应总带 login_notify：关闭时是空 message，与"hub 不认识这个字段"可以区分。读到的整份设置原样写回，
// 开着的保持原渠道，关着的仍关着。
func TestLoginNotifyResponseAlwaysCarriesTheField(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	raw := func(path, body string) map[string]any {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.srv.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := h.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out struct {
			Settings map[string]any `json:"settings"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d, %v", path, resp.StatusCode, err)
		}
		return out.Settings
	}
	if got, ok := raw("/heron.v1.AdminService/GetSettings", "{}")["loginNotify"]; !ok || !reflect.DeepEqual(got, map[string]any{}) {
		t.Fatalf("GetSettings with login notification off: loginNotify = %v (present %v), want {}", got, ok)
	}
	if got, ok := raw("/heron.v1.AdminService/UpdateSettings", `{"settings":{"theme":"dark"}}`)["loginNotify"]; !ok || !reflect.DeepEqual(got, map[string]any{}) {
		t.Fatalf("UpdateSettings echo with login notification off: loginNotify = %v (present %v), want {}", got, ok)
	}
	off := currentSettings(t, h)
	if off.LoginNotify == nil || saveSettings(t, h, off).LoginNotify == nil || len(currentSettings(t, h).GetLoginNotify().GetChannelIds()) != 0 {
		t.Fatalf("writing back the settings read while off: %v", currentSettings(t, h))
	}
	c := saveChannel(t, h, webhook("https://example.invalid/a")).Id
	chooseLoginChannels(t, h, c)
	on := currentSettings(t, h)
	if ids := saveSettings(t, h, on).GetLoginNotify().GetChannelIds(); !slices.Equal(ids, []int64{c}) {
		t.Fatalf("writing back the settings read while on: %v, want [%d]", ids, c)
	}
}

func TestLoginNotifySuccessDeliversAndUsesTrustedSource(t *testing.T) {
	t.Parallel()
	for _, trusted := range []bool{false, true} {
		t.Run(fmt.Sprint(trusted), func(t *testing.T) {
			prefix := ""
			wantIP := "127.0.0.1"
			if trusted {
				prefix, wantIP = "127.0.0.0/8", "203.0.113.7"
			}
			h := newHarness(t, prefix)
			h.login(t)
			var mu sync.Mutex
			var received []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				mu.Lock()
				received = append(received, string(b))
				mu.Unlock()
				w.WriteHeader(http.StatusNoContent)
			}))
			defer srv.Close()
			var ids []int64
			for range 2 {
				c := webhook(srv.URL)
				c.Webhook.BodyTemplate = "{{.Rule}}|{{.Node}}|{{.Kind}}|{{.Transition}}|{{.Summary}}|{{.At.Unix}}"
				ids = append(ids, saveChannel(t, h, c).Id)
			}
			chooseLoginChannels(t, h, ids...)
			req := connect.NewRequest(&heronv1.LoginRequest{Password: password})
			req.Header().Set("X-Forwarded-For", "203.0.113.7")
			if _, err := h.admin.Login(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			events := loginEvents(t, h)
			wantSummary := fmt.Sprintf("管理员登录成功（密码）：来源 %s，时间 %s", wantIP, h.clk.Now().Format(time.RFC3339))
			if len(events) != 2 {
				t.Fatalf("successful logins wrote %d events, want 2", len(events))
			}
			if initial := events[1]; initial.Transition != "login_success" || len(initial.Deliveries) != 0 {
				t.Fatalf("login without channels must retain audit without deliveries: %v", initial)
			}
			ev := events[0]
			if ev.RuleId != 0 || ev.NodeId != 0 || ev.Transition != "login_success" || ev.At != h.clk.Now().Unix() || ev.Summary != wantSummary || len(ev.Deliveries) != 2 {
				t.Fatalf("login event envelope or deliveries: %v", ev)
			}
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan struct{})
			go func() { defer close(done); h.svc.notifier.Run(ctx) }()
			defer func() { cancel(); <-done }()
			testwait.Until(t, time.Millisecond, func() bool {
				evs := loginEvents(t, h)
				return len(evs) == 2 && len(evs[0].Deliveries) == 2 && evs[0].Deliveries[0].Done && evs[0].Deliveries[1].Done
			}, "login deliveries did not reach terminal state")
			var delivered []int64
			for _, d := range loginEvents(t, h)[0].Deliveries {
				if !d.Ok || d.Attempts != 1 || d.GetDeliveredAt() != h.clk.Now().Unix() {
					t.Errorf("login delivery result: %v", d)
				}
				delivered = append(delivered, d.ChannelId)
			}
			slices.Sort(delivered)
			mu.Lock()
			bodies := slices.Clone(received)
			mu.Unlock()
			body := fmt.Sprintf("系统事件|Hub|login|login_success|%s|%d", wantSummary, h.clk.Now().Unix())
			if !slices.Equal(delivered, ids) || !reflect.DeepEqual(bodies, []string{body, body}) {
				t.Fatalf("webhook delivery mismatch: channels=%v bodies=%q", delivered, bodies)
			}
		})
	}
}

func TestLoginNotifyLockThresholdOnlyOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "127.0.0.0/8")
	h.login(t)
	c := saveChannel(t, h, webhook("https://example.invalid/hook"))
	chooseLoginChannels(t, h, c.Id)
	var locked *heronv1.AlertEvent
	for i := 1; i <= 8; i++ {
		req := connect.NewRequest(&heronv1.LoginRequest{Password: "wrong password"})
		req.Header().Set("X-Forwarded-For", "2001:db8:1::"+fmt.Sprint(i))
		_, err := h.admin.Login(t.Context(), req)
		if codeOf(err) != connect.CodeUnauthenticated {
			t.Fatalf("failed login %d: %v", i, err)
		}
		evs := loginEvents(t, h)
		wantLocked := 0
		if i >= 5 {
			wantLocked = 1
		}
		counts := make(map[string]int)
		for _, ev := range evs {
			counts[ev.Transition]++
			if ev.Transition == "login_locked" {
				locked = ev
			} else if len(ev.Deliveries) != 0 {
				t.Fatalf("ordinary failure or initial login must not notify: %v", ev)
			}
		}
		if counts["login_success"] != 1 || counts["login_failed"] != min(i, 5) || counts["login_locked"] != wantLocked || len(evs) != 1+min(i, 5)+wantLocked {
			t.Fatalf("attempt %d audit counts=%v total=%d, want success=1 failed=%d locked=%d", i, counts, len(evs), min(i, 5), wantLocked)
		}
	}
	ev := locked
	if ev == nil || ev.RuleId != 0 || ev.NodeId != 0 || ev.Summary != "登录失败达到锁定阈值：来源 2001:db8:1::5，时间 "+h.clk.Now().Format(time.RFC3339) || len(ev.Deliveries) != 1 || ev.Deliveries[0].ChannelId != c.Id {
		t.Fatalf("lock notification content: %v", ev)
	}
}

// 两条摘要都写进事件时刻，按 hub 的 --timezone 写成带偏移的 RFC 3339：投递会重试、重启后续投，接收方
// 看到的送达时刻不是登录时刻，而 Telegram 只发摘要。从登录入口一直看到 Telegram 收到的正文。
func TestLoginNotifySummaryCarriesZonedTime(t *testing.T) {
	t.Parallel()
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var texts []string
	tg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		mu.Lock()
		texts = append(texts, body.Text)
		mu.Unlock()
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer tg.Close()
	h := newZonedHarness(t, "127.0.0.0/8", shanghai, store.DefaultRetention, withTelegramBase(tg.URL))
	h.login(t)
	initial := loginEvents(t, h)
	if len(initial) != 1 || initial[0].Transition != "login_success" || len(initial[0].Deliveries) != 0 {
		t.Fatalf("initial login audit: %v", initial)
	}
	c := saveChannel(t, h, &heronv1.NotifyChannel{Name: "tg", Kind: heronv1.ChannelKind_CHANNEL_KIND_TELEGRAM, Telegram: &heronv1.TelegramConfig{BotToken: "1:x", ChatId: "chat"}})
	chooseLoginChannels(t, h, c.Id)
	at := time.Date(2026, 9, 28, 2, 5, 7, 0, time.UTC)
	h.clk.SetWall(at)
	const stamp = "2026-09-28T10:05:07+08:00"
	login := func(pw, from string) error {
		req := connect.NewRequest(&heronv1.LoginRequest{Password: pw})
		req.Header().Set("X-Forwarded-For", from)
		_, err := h.admin.Login(t.Context(), req)
		return err
	}
	if err := login(password, "203.0.113.7"); err != nil {
		t.Fatal(err)
	}
	// 5 是 auth 的登录失败上限，第 5 次失败设下锁定。
	for range 5 {
		if err := login("wrong password", "198.51.100.9"); codeOf(err) != connect.CodeUnauthenticated {
			t.Fatalf("wrong password: %v", err)
		}
	}
	want := []string{
		"登录失败达到锁定阈值：来源 198.51.100.9，时间 " + stamp,
		"管理员登录成功（密码）：来源 203.0.113.7，时间 " + stamp,
	}
	var summaries []string
	events := loginEvents(t, h)
	if len(events) != 8 || !proto.Equal(events[len(events)-1], initial[0]) {
		t.Fatalf("login audits must retain initial login and all new attempts: %v", events)
	}
	failed := 0
	for _, ev := range events[:len(events)-1] {
		if ev.At != at.Unix() {
			t.Errorf("event %d at %d, want %d", ev.Id, ev.At, at.Unix())
		}
		if ev.Transition == "login_failed" {
			failed++
			if len(ev.Deliveries) != 0 || ev.Summary != "管理员认证失败：来源 198.51.100.9，时间 "+stamp {
				t.Errorf("ordinary failure audit: %v", ev)
			}
			continue
		}
		if len(ev.Deliveries) != 1 || ev.Deliveries[0].ChannelId != c.Id {
			t.Errorf("login notification target: %v", ev)
		}
		summaries = append(summaries, ev.Summary)
	}
	if failed != 5 || !slices.Equal(summaries, want) {
		t.Errorf("login summaries = %q, failures=%d, want %q and 5 failures", summaries, failed, want)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); h.svc.notifier.Run(ctx) }()
	defer func() { cancel(); <-done }()
	testwait.Until(t, time.Millisecond, func() bool {
		evs := loginEvents(t, h)
		delivered := 0
		for _, ev := range evs {
			for _, d := range ev.Deliveries {
				if d.Done && d.Ok {
					delivered++
				}
			}
		}
		return len(evs) == 8 && delivered == 2
	}, "telegram deliveries did not reach terminal state")
	mu.Lock()
	got := slices.Sorted(slices.Values(texts))
	mu.Unlock()
	if !slices.Equal(got, want) {
		t.Fatalf("telegram texts = %q, want %q", got, want)
	}
}

func TestLoginNotifyTokenReadsAndCleanup(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	node, _ := h.createNode(t, "remove me")
	rule := saveRule(t, h, offlineRule())
	c := saveChannel(t, h, webhook("https://example.invalid/hook"))
	_, token := createToken(t, h, "reader")
	chooseLoginChannels(t, h, c.Id)
	beforeRead := loginEvents(t, h)
	req := connect.NewRequest(&heronv1.GetSettingsRequest{})
	req.Header().Set("Authorization", "Bearer "+token)
	if _, err := h.admin.GetSettings(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if evs := loginEvents(t, h); !reflect.DeepEqual(evs, beforeRead) {
		t.Fatalf("API token read emitted login events: %v", evs)
	}
	if _, err := h.admin.Login(t.Context(), connect.NewRequest(&heronv1.LoginRequest{Password: password})); err != nil {
		t.Fatal(err)
	}
	before := loginEvents(t, h)
	if len(before) != 2 {
		t.Fatalf("cleanup fixture has %d login events, want 2", len(before))
	}
	if _, err := h.admin.DeleteNode(t.Context(), connect.NewRequest(&heronv1.DeleteNodeRequest{Id: node})); err != nil {
		t.Fatal(err)
	}
	if after := loginEvents(t, h); !reflect.DeepEqual(after, before) {
		t.Fatalf("DeleteNode/Forget changed system events: %v", after)
	}
	if _, err := h.admin.DeleteAlertRule(t.Context(), connect.NewRequest(&heronv1.DeleteAlertRuleRequest{Id: rule.Id})); err != nil {
		t.Fatal(err)
	}
	if after := loginEvents(t, h); !reflect.DeepEqual(after, before) {
		t.Fatalf("DeleteAlertRule changed system events: %v", after)
	}
	states, err := h.store.ListAlertStates(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 0 {
		t.Fatalf("login created rule/node state: %v", states)
	}
	filtered, err := h.store.ListAlertEvents(t.Context(), node, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 0 {
		t.Fatalf("node filter included system events: %v", filtered)
	}
	// 不依赖 API 的零 ID 映射，直接回读表确认真实存储也保留系统事件。
	stored, err := h.store.GetAlertEvent(t.Context(), before[0].Id)
	if err != nil || stored.Transition != store.TransitionLoginSuccess {
		t.Fatalf("stored system event missing: %+v, %v", stored, err)
	}
}

func TestLoginNotifyOnlyUpdatePreservesAppearance(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	before := saveSettings(t, h, validSettings())
	// 外观任一项非空就按整体替换校验，即使请求里同时有 login_notify：只给标题、缺了 theme 的请求整次拒绝。
	partial := &heronv1.Settings{Title: "只改标题", LoginNotify: &heronv1.LoginNotify{}}
	rejected(t, h, partial, `settings.theme must be one of auto, light, dark; got ""`, before)
	putNotifySettings(t, h, `{"settings":{"loginNotify":{}}}`, http.StatusOK)
	after := currentSettings(t, h)
	if after.GetTitle() != before.GetTitle() || after.GetTheme() != before.GetTheme() || after.GetAccentColor() != before.GetAccentColor() || after.GetLogo() != before.GetLogo() || after.GetCustomCss() != before.GetCustomCss() {
		t.Fatalf("login-only update changed appearance: got %v, before %v", after, before)
	}
	_, err := h.admin.Login(t.Context(), connect.NewRequest(&heronv1.LoginRequest{Password: password}))
	if err != nil {
		t.Fatal(err)
	}
	events, err := h.store.ListAlertEvents(t.Context(), 0, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("disabled login notification must retain both audits: %v", events)
	}
	for _, ev := range events {
		if ev.Transition != store.TransitionLoginSuccess || len(ev.Deliveries) != 0 {
			t.Fatalf("disabled login notification must retain audit without deliveries: %v", ev)
		}
	}
}
