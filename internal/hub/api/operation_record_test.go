package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/gen/heron/v1/heronv1connect"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/hub/theme"
	. "github.com/xjetry/heron/internal/hub/theme/themetest"
)

// auditNoneMethods 是 ACCESS_SESSION 里声明为不写操作记录的方法：凭据与可能含密钥内容的只读，向外部的只读查询，以及只在
// 本会话进程内留下短期授权的交互预览。把一个方法挪进这里就是让它的调用不再留痕，必须同时改这份清单。
var auditNoneMethods = []string{
	"GetSecurity", "ListSessions", "GetAlertDeliveryError", "ListNotifyChannels", "PreviewAlertNotification",
	"ListThemes", "GetThemePreview", "ListThemeReleases", "PreviewTheme", "GetThemePackage", "ListApiTokens",
}

// 每个方法的操作记录口径与准入口径相符：SESSION 不在 auditNoneMethods 里的都是 AUDIT_WRITE，CHANGE 是 AUDIT_WRITE，
// READ 与 LOGIN 不声明。
func TestAuditTableMatchesDeclaredPolicy(t *testing.T) {
	t.Parallel()
	svc := adminService()
	access := accessTable(svc)
	audit := auditTable(svc, access)
	none := map[string]bool{}
	for _, m := range auditNoneMethods {
		none[m] = true
	}
	for i := 0; i < svc.Methods().Len(); i++ {
		name := string(svc.Methods().Get(i).Name())
		path := "/heron.v1.AdminService/" + name
		want := heronv1.Audit_AUDIT_UNSPECIFIED
		switch access[path] {
		case heronv1.Access_ACCESS_SESSION:
			want = heronv1.Audit_AUDIT_WRITE
			if none[name] {
				want = heronv1.Audit_AUDIT_NONE
			}
		case heronv1.Access_ACCESS_CHANGE:
			want = heronv1.Audit_AUDIT_WRITE
		}
		if got := audit[path]; got != want {
			t.Errorf("%s: audit %v, want %v", name, got, want)
		}
		delete(none, name)
	}
	if len(none) != 0 {
		t.Errorf("auditNoneMethods names methods that do not exist: %v", none)
	}
}

// 没有声明、声明了不合准入口径的值，hub 都在构造处理器时拒绝启动。
func TestAuditTableRefusesMisclassification(t *testing.T) {
	t.Parallel()
	options := func(access heronv1.Access, audit heronv1.Audit) *descriptorpb.MethodOptions {
		o := &descriptorpb.MethodOptions{}
		proto.SetExtension(o, heronv1.E_Access, access)
		if audit != heronv1.Audit_AUDIT_UNSPECIFIED {
			proto.SetExtension(o, heronv1.E_Audit, audit)
		}
		return o
	}
	for _, tc := range []struct {
		access heronv1.Access
		audit  heronv1.Audit
		ok     bool
	}{
		{heronv1.Access_ACCESS_SESSION, heronv1.Audit_AUDIT_UNSPECIFIED, false},
		{heronv1.Access_ACCESS_SESSION, heronv1.Audit(99), false},
		{heronv1.Access_ACCESS_SESSION, heronv1.Audit_AUDIT_NONE, true},
		{heronv1.Access_ACCESS_SESSION, heronv1.Audit_AUDIT_WRITE, true},
		{heronv1.Access_ACCESS_CHANGE, heronv1.Audit_AUDIT_UNSPECIFIED, false},
		{heronv1.Access_ACCESS_CHANGE, heronv1.Audit_AUDIT_NONE, false},
		{heronv1.Access_ACCESS_CHANGE, heronv1.Audit_AUDIT_WRITE, true},
		{heronv1.Access_ACCESS_READ, heronv1.Audit_AUDIT_WRITE, false},
		{heronv1.Access_ACCESS_READ, heronv1.Audit_AUDIT_NONE, false},
		{heronv1.Access_ACCESS_READ, heronv1.Audit_AUDIT_UNSPECIFIED, true},
		{heronv1.Access_ACCESS_LOGIN, heronv1.Audit_AUDIT_WRITE, false},
		{heronv1.Access_ACCESS_LOGIN, heronv1.Audit_AUDIT_UNSPECIFIED, true},
	} {
		svc := syntheticService(t, options(tc.access, tc.audit))
		build := func() { auditTable(svc, accessTable(svc)) }
		if tc.ok {
			build()
			continue
		}
		expectPanic(t, "synthetic.S.Bare", build)
	}
}

// operations 读库里全部操作记录（按提交时刻倒序），不经 API：会话登出之后仍能核对。
func operations(t *testing.T, h *harness) []store.Operation {
	t.Helper()
	list, err := h.store.ListOperations(t.Context(), store.OperationFilter{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	return list
}

// newOperations 是 after 里不在 before 里的记录。
func newOperations(before, after []store.Operation) []store.Operation {
	seen := map[string]bool{}
	for _, o := range before {
		seen[o.ID] = true
	}
	var out []store.Operation
	for _, o := range after {
		if !seen[o.ID] {
			out = append(out, o)
		}
	}
	return out
}

// currentSessionID 是 h 的会话在 ListSessions 里的 id。
func currentSessionID(t *testing.T, h *harness) string {
	t.Helper()
	list, err := h.admin.ListSessions(t.Context(), connect.NewRequest(&heronv1.ListSessionsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range list.Msg.Sessions {
		if s.Current {
			return s.Id
		}
	}
	t.Fatal("no current session")
	return ""
}

type releaseStub struct{ content []byte }

func (r releaseStub) Releases(context.Context, string) ([]theme.GitHubRelease, error) {
	return nil, nil
}
func (r releaseStub) Download(_ context.Context, repository, tag string, assetID int64) ([]byte, theme.GitHubSource, error) {
	return r.content, theme.GitHubSource{Repository: repository, Release: tag, Asset: "theme.zip"}, nil
}

// auditCase 是一次审计写方法的成功调用：setup 做准备（它留下的记录不计），act 只做被测的那一次调用，want 给出它应留下的
// 那一条记录的动作与对象。同一个方法可以有几条（不同的成功路径）。
type auditCase struct {
	method string
	setup  func(t *testing.T)
	act    func(t *testing.T) error
	want   func() (action string, id int64, key string)
}

// 每个 ACCESS_SESSION 的 AUDIT_WRITE 方法在成功路径上恰好写一条记录：主体是会话、会话 id 是这次请求的会话、来源与方法
// 是这次请求的、动作与对象是方法改的那一个；直调记录的 request_id 不在 ExecuteChange 接受的字母表里。业务拒绝与 AUDIT_NONE
// 的方法不写。最后在全部记录里扫一遍：这次用例里出现过的每一个凭据都不在任何一条记录里。
func TestEveryAuditedSessionMethodRecordsExactlyOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "", withConfig(func(c *Config) { c.HubVersion = "v0.1.0" }))
	h.login(t)
	session := currentSessionID(t, h)
	ctx := t.Context()
	nodeA, _ := h.createNode(t, "audit-a")
	nodeB, _ := h.createNode(t, "audit-b")
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer receiver.Close()
	const canary = "LEAKCANARY"
	secrets := []string{password, canary}
	var created, probe, channel, rule, silence, tokenID, tagID int64
	var updateTask, themeV1, otherSession string
	must := func(t *testing.T, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	upload := func(t *testing.T, id string, extra ...Entry) string {
		t.Helper()
		th, err := h.upload(t, Minimal(t, id, extra...), "")
		must(t, err)
		return th.Digest
	}
	enable := func(t *testing.T, id, digest string) {
		t.Helper()
		_, err := h.admin.EnableTheme(ctx, connect.NewRequest(&heronv1.EnableThemeRequest{Id: id, Digest: digest}))
		must(t, err)
	}
	cases := []auditCase{
		{method: "CreateNode", act: func(t *testing.T) error {
			r, err := h.admin.CreateNode(ctx, connect.NewRequest(&heronv1.CreateNodeRequest{Name: "audit-created"}))
			if err == nil {
				created, secrets = r.Msg.Node.Id, append(secrets, r.Msg.Token)
			}
			return err
		}, want: func() (string, int64, string) { return "create_node", created, "" }},
		{method: "UpdateNode", act: func(t *testing.T) error {
			_, err := h.admin.UpdateNode(ctx, connect.NewRequest(&heronv1.UpdateNodeRequest{Id: nodeA, Name: "audit-a2", TrafficResetDay: 1, OfflineGraceS: proto.Uint32(0),
				Billing: &heronv1.Billing{BillingCycle: heronv1.BillingCycle_BILLING_CYCLE_MONTHLY, ExpiresOn: "2027-01-01"}}))
			return err
		}, want: func() (string, int64, string) { return "update_node", nodeA, "" }},
		{method: "RenewNodeBilling", act: func(t *testing.T) error {
			_, err := h.admin.RenewNodeBilling(ctx, connect.NewRequest(&heronv1.RenewNodeBillingRequest{NodeId: nodeA}))
			return err
		}, want: func() (string, int64, string) { return "renew_node_billing", nodeA, "" }},
		{method: "RotateNodeToken", act: func(t *testing.T) error {
			r, err := h.admin.RotateNodeToken(ctx, connect.NewRequest(&heronv1.RotateNodeTokenRequest{Id: nodeA}))
			if err == nil {
				secrets = append(secrets, r.Msg.Token)
			}
			return err
		}, want: func() (string, int64, string) { return "rotate_node_token", nodeA, "" }},
		{method: "BatchUpdateNodeTags", act: func(t *testing.T) error {
			_, err := h.admin.BatchUpdateNodeTags(ctx, connect.NewRequest(&heronv1.BatchUpdateNodeTagsRequest{NodeIds: []int64{nodeB, nodeA}, AddTags: []string{"audit-tag"}}))
			return err
		}, want: func() (string, int64, string) {
			return "batch_update_node_tags", 0, fmt.Sprintf("[%d,%d]", nodeA, nodeB)
		}},
		{method: "DeleteTag", setup: func(t *testing.T) {
			var err error
			tagID, err = h.store.TagID(ctx, "audit-tag")
			must(t, err)
		}, act: func(t *testing.T) error {
			_, err := h.admin.DeleteTag(ctx, connect.NewRequest(&heronv1.DeleteTagRequest{Name: "audit-tag"}))
			return err
		}, want: func() (string, int64, string) { return "delete_tag", tagID, "" }},
		{method: "ReorderNodes", act: func(t *testing.T) error {
			_, err := h.admin.ReorderNodes(ctx, connect.NewRequest(&heronv1.ReorderNodesRequest{Ids: []int64{created, nodeB, nodeA}}))
			return err
		}, want: func() (string, int64, string) { return "reorder_nodes", 0, "" }},
		{method: "MoveNodes", act: func(t *testing.T) error {
			_, err := h.admin.MoveNodes(ctx, connect.NewRequest(&heronv1.MoveNodesRequest{Ids: []int64{nodeA}, Position: 1}))
			return err
		}, want: func() (string, int64, string) { return "move_nodes", 0, fmt.Sprintf("[%d]", nodeA) }},
		{method: "OpenRegisterWindow", act: func(t *testing.T) error {
			r, err := h.admin.OpenRegisterWindow(ctx, connect.NewRequest(&heronv1.OpenRegisterWindowRequest{TtlS: 600, MaxNodes: 1}))
			if err == nil {
				secrets = append(secrets, r.Msg.Key)
			}
			return err
		}, want: func() (string, int64, string) { return "open_register_window", 0, "" }},
		{method: "CloseRegisterWindow", act: func(t *testing.T) error {
			_, err := h.admin.CloseRegisterWindow(ctx, connect.NewRequest(&heronv1.CloseRegisterWindowRequest{}))
			return err
		}, want: func() (string, int64, string) { return "close_register_window", 0, "" }},
		{method: "AdjustTraffic", act: func(t *testing.T) error {
			_, err := h.admin.AdjustTraffic(ctx, connect.NewRequest(&heronv1.AdjustTrafficRequest{NodeId: nodeA, PeriodRx: 10, PeriodTx: 20}))
			return err
		}, want: func() (string, int64, string) { return "adjust_traffic", nodeA, "" }},
		{method: "SaveProbeTask", act: func(t *testing.T) error {
			r, err := h.admin.SaveProbeTask(ctx, connect.NewRequest(&heronv1.SaveProbeTaskRequest{Task: validProbeTask(), NodeIds: []int64{nodeA}}))
			if err == nil {
				probe = int64(r.Msg.Task.Task.Id)
			}
			return err
		}, want: func() (string, int64, string) { return "save_probe_task", probe, "" }},
		{method: "SaveProbeTask", act: func(t *testing.T) error {
			task := validProbeTask()
			task.Id, task.IntervalS = uint64(probe), 10
			_, err := h.admin.SaveProbeTask(ctx, connect.NewRequest(&heronv1.SaveProbeTaskRequest{Task: task, NodeIds: []int64{nodeA}}))
			return err
		}, want: func() (string, int64, string) { return "save_probe_task", probe, "" }},
		{method: "ReorderProbeTasks", act: func(t *testing.T) error {
			_, err := h.admin.ReorderProbeTasks(ctx, connect.NewRequest(&heronv1.ReorderProbeTasksRequest{Ids: []uint64{uint64(probe)}}))
			return err
		}, want: func() (string, int64, string) { return "reorder_probe_tasks", 0, "" }},
		{method: "DeleteProbeTask", act: func(t *testing.T) error {
			_, err := h.admin.DeleteProbeTask(ctx, connect.NewRequest(&heronv1.DeleteProbeTaskRequest{Id: uint64(probe)}))
			return err
		}, want: func() (string, int64, string) { return "delete_probe_task", probe, "" }},
		{method: "SaveNotifyChannel", act: func(t *testing.T) error {
			c := webhook(receiver.URL + "/" + canary)
			c.Webhook.Headers = map[string]string{"Authorization": canary}
			c.Webhook.BodyTemplate = `{"k":"` + canary + `"}`
			r, err := h.admin.SaveNotifyChannel(ctx, connect.NewRequest(&heronv1.SaveNotifyChannelRequest{Channel: c}))
			if err == nil {
				channel = r.Msg.Channel.Id
			}
			return err
		}, want: func() (string, int64, string) { return "save_notify_channel", channel, "" }},
		{method: "TestNotifyChannel", act: func(t *testing.T) error {
			_, err := h.admin.TestNotifyChannel(ctx, connect.NewRequest(&heronv1.TestNotifyChannelRequest{Id: channel}))
			return err
		}, want: func() (string, int64, string) { return "test_notify_channel", channel, "" }},
		{method: "SaveAlertRule", act: func(t *testing.T) error {
			r := offlineRule()
			r.ChannelIds = []int64{channel}
			resp, err := h.admin.SaveAlertRule(ctx, connect.NewRequest(&heronv1.SaveAlertRuleRequest{Rule: r}))
			if err == nil {
				rule = resp.Msg.Rule.Id
			}
			return err
		}, want: func() (string, int64, string) { return "save_alert_rule", rule, "" }},
		{method: "TestAlertNotification", act: func(t *testing.T) error {
			_, err := h.admin.TestAlertNotification(ctx, connect.NewRequest(&heronv1.TestAlertNotificationRequest{Sample: &heronv1.AlertNotificationSample{Rule: offlineDraft(channel), NodeId: nodeA, Transition: "firing"}}))
			return err
		}, want: func() (string, int64, string) { return "test_alert_notification", 0, "" }},
		{method: "DeleteAlertRule", act: func(t *testing.T) error {
			_, err := h.admin.DeleteAlertRule(ctx, connect.NewRequest(&heronv1.DeleteAlertRuleRequest{Id: rule}))
			return err
		}, want: func() (string, int64, string) { return "delete_alert_rule", rule, "" }},
		{method: "DeleteNotifyChannel", act: func(t *testing.T) error {
			_, err := h.admin.DeleteNotifyChannel(ctx, connect.NewRequest(&heronv1.DeleteNotifyChannelRequest{Id: channel}))
			return err
		}, want: func() (string, int64, string) { return "delete_notify_channel", channel, "" }},
		{method: "SaveSilence", act: func(t *testing.T) error {
			r, err := h.admin.SaveSilence(ctx, connect.NewRequest(&heronv1.SaveSilenceRequest{Silence: &heronv1.Silence{Name: "维护", Enabled: true, AllNodes: true, AllRules: true,
				Kind: heronv1.SilenceKind_SILENCE_KIND_DAILY, StartHhmm: "01:00", EndHhmm: "02:00"}}))
			if err == nil {
				silence = r.Msg.Silence.Id
			}
			return err
		}, want: func() (string, int64, string) { return "save_silence", silence, "" }},
		{method: "DeleteSilence", act: func(t *testing.T) error {
			_, err := h.admin.DeleteSilence(ctx, connect.NewRequest(&heronv1.DeleteSilenceRequest{Id: silence}))
			return err
		}, want: func() (string, int64, string) { return "delete_silence", silence, "" }},
		{method: "UpdateSettings", act: func(t *testing.T) error {
			_, err := h.admin.UpdateSettings(ctx, connect.NewRequest(&heronv1.UpdateSettingsRequest{Settings: &heronv1.Settings{
				Backup:    &heronv1.BackupSettings{Endpoint: "https://s3.example", Bucket: "bucket", AccessKey: "ak", Secret: proto.String(canary + "-s3")},
				Heartbeat: &heronv1.Heartbeat{Url: "https://hb.example/" + canary, IntervalS: 60, Method: heronv1.HeartbeatMethod_HEARTBEAT_METHOD_POST},
			}}))
			return err
		}, want: func() (string, int64, string) { return "update_settings", 0, "" }},
		{method: "UploadTheme", act: func(t *testing.T) error {
			th, err := h.upload(t, Minimal(t, "aud"), "")
			if err == nil {
				themeV1 = th.Digest
			}
			return err
		}, want: func() (string, int64, string) { return "install_theme", 0, "aud" }},
		{method: "EnableTheme", act: func(t *testing.T) error {
			_, err := h.admin.EnableTheme(ctx, connect.NewRequest(&heronv1.EnableThemeRequest{Id: "aud", Digest: themeV1}))
			return err
		}, want: func() (string, int64, string) { return "enable_theme", 0, "aud" }},
		{method: "DeleteThemeVersion", setup: func(t *testing.T) {
			// 第一个版本既不是当前也不是回滚版本时才能删：再装两版并依次启用。
			enable(t, "aud", upload(t, "aud", File("v2.txt", "2")))
			enable(t, "aud", upload(t, "aud", File("v3.txt", "3")))
		}, act: func(t *testing.T) error {
			_, err := h.admin.DeleteThemeVersion(ctx, connect.NewRequest(&heronv1.DeleteThemeVersionRequest{Id: "aud", Digest: themeV1}))
			return err
		}, want: func() (string, int64, string) { return "delete_theme_version", 0, "aud" }},
		{method: "InstallThemeRelease", setup: func(t *testing.T) {
			h.svc.github = releaseStub{content: Minimal(t, "rel")}
		}, act: func(t *testing.T) error {
			_, err := h.admin.InstallThemeRelease(ctx, connect.NewRequest(&heronv1.InstallThemeReleaseRequest{Repository: "owner/repo", Tag: "v1", AssetId: 1}))
			return err
		}, want: func() (string, int64, string) { return "install_theme", 0, "rel" }},
		{method: "DeleteTheme", act: func(t *testing.T) error {
			_, err := h.admin.DeleteTheme(ctx, connect.NewRequest(&heronv1.DeleteThemeRequest{Id: "rel"}))
			return err
		}, want: func() (string, int64, string) { return "delete_theme", 0, "rel" }},
		{method: "CreateApiToken", act: func(t *testing.T) error {
			r, err := h.admin.CreateApiToken(ctx, connect.NewRequest(&heronv1.CreateApiTokenRequest{Name: "audit-token",
				Grant: &heronv1.TokenGrant{Permissions: []heronv1.TokenPermission{heronv1.TokenPermission_TOKEN_PERMISSION_CONFIGURE}, AllNodes: true}}))
			if err == nil {
				tokenID = r.Msg.ApiToken.Id
				digest := sha256.Sum256([]byte(r.Msg.Token))
				secrets = append(secrets, r.Msg.Token, hex.EncodeToString(digest[:]), strings.ToUpper(hex.EncodeToString(digest[:])))
			}
			return err
		}, want: func() (string, int64, string) { return "create_api_token", tokenID, "" }},
		{method: "UpdateApiToken", act: func(t *testing.T) error {
			_, err := h.admin.UpdateApiToken(ctx, connect.NewRequest(&heronv1.UpdateApiTokenRequest{Id: tokenID, Name: "renamed", ExpectedVersion: listedVersion(t, h, tokenID)}))
			return err
		}, want: func() (string, int64, string) { return "update_api_token", tokenID, "" }},
		{method: "DeleteApiToken", act: func(t *testing.T) error {
			_, err := h.admin.DeleteApiToken(ctx, connect.NewRequest(&heronv1.DeleteApiTokenRequest{Id: tokenID}))
			return err
		}, want: func() (string, int64, string) { return "delete_api_token", tokenID, "" }},
		{method: "StartUpdate", setup: func(t *testing.T) {
			must(t, h.store.SaveNodeUpdate(ctx, nodeA, &heronv1.UpdateStatus{Supported: true, Version: "v0.1.0"}))
			boundManager(t, h, "v0.2.0")
		}, act: func(t *testing.T) error {
			r, err := h.admin.StartUpdate(ctx, connect.NewRequest(&heronv1.StartUpdateRequest{NodeId: nodeA, Version: "v0.2.0"}))
			if err == nil {
				updateTask = r.Msg.Task.Id
			}
			return err
		}, want: func() (string, int64, string) { return "start_update", nodeA, "" }},
		{method: "CancelUpdate", act: func(t *testing.T) error {
			_, err := h.admin.CancelUpdate(ctx, connect.NewRequest(&heronv1.CancelUpdateRequest{NodeId: nodeA, Id: updateTask}))
			return err
		}, want: func() (string, int64, string) { return "cancel_update", nodeA, "" }},
		{method: "StartUpdate", setup: func(t *testing.T) {
			h.svc.updateLocal = &updateAPIFake{}
		}, act: func(t *testing.T) error {
			_, err := h.admin.StartUpdate(ctx, connect.NewRequest(&heronv1.StartUpdateRequest{NodeId: 0, Version: "v0.2.0"}))
			return err
		}, want: func() (string, int64, string) { return "start_update", 0, "" }},
		{method: "SecurityAction", act: func(t *testing.T) error {
			r, err := h.admin.SecurityAction(ctx, connect.NewRequest(&heronv1.SecurityActionRequest{Action: heronv1.SecurityActionKind_SECURITY_ACTION_KIND_TOTP_BEGIN, Password: password}))
			if err == nil {
				secrets = append(secrets, r.Msg.TotpSecret)
			}
			return err
		}, want: func() (string, int64, string) { return "security_totp_begin", 0, "" }},
		{method: "RevokeSession", setup: func(t *testing.T) {
			jar, _ := cookiejar.New(nil)
			other := heronv1connect.NewAdminServiceClient(&http.Client{Jar: jar, Transport: h.srv.Client().Transport}, h.srv.URL)
			_, err := other.Login(ctx, connect.NewRequest(&heronv1.LoginRequest{Password: password}))
			must(t, err)
			list, err := h.admin.ListSessions(ctx, connect.NewRequest(&heronv1.ListSessionsRequest{}))
			must(t, err)
			for _, s := range list.Msg.Sessions {
				if !s.Current {
					otherSession = s.Id
				}
			}
		}, act: func(t *testing.T) error {
			_, err := h.admin.RevokeSession(ctx, connect.NewRequest(&heronv1.RevokeSessionRequest{Id: otherSession}))
			return err
		}, want: func() (string, int64, string) { return "revoke_session", 0, otherSession }},
		{method: "DeleteNode", act: func(t *testing.T) error {
			_, err := h.admin.DeleteNode(ctx, connect.NewRequest(&heronv1.DeleteNodeRequest{Id: nodeB}))
			return err
		}, want: func() (string, int64, string) { return "delete_node", nodeB, "" }},
	}

	// 业务拒绝与只读：一条都不写。放在登出之前，会话仍有效。
	t.Run("rejections and reads", func(t *testing.T) {
		// 预览告警通知要引用一个已保存的渠道；建渠道本身的记录不计入。
		saved, err := h.admin.SaveNotifyChannel(ctx, connect.NewRequest(&heronv1.SaveNotifyChannelRequest{Channel: webhook(receiver.URL)}))
		if err != nil {
			t.Fatal(err)
		}
		before := operations(t, h)
		for name, call := range map[string]func() error{
			"UpdateNode missing": func() error {
				_, err := h.admin.UpdateNode(ctx, connect.NewRequest(&heronv1.UpdateNodeRequest{Id: 999999, Name: "x", TrafficResetDay: 1}))
				return err
			},
			"AdjustTraffic missing": func() error {
				_, err := h.admin.AdjustTraffic(ctx, connect.NewRequest(&heronv1.AdjustTrafficRequest{NodeId: 999999}))
				return err
			},
			"UpdateSettings empty": func() error {
				_, err := h.admin.UpdateSettings(ctx, connect.NewRequest(&heronv1.UpdateSettingsRequest{Settings: &heronv1.Settings{}}))
				return err
			},
			"DeleteTag missing": func() error {
				_, err := h.admin.DeleteTag(ctx, connect.NewRequest(&heronv1.DeleteTagRequest{Name: "no-such-tag"}))
				return err
			},
			"DeleteNotifyChannel missing": func() error {
				_, err := h.admin.DeleteNotifyChannel(ctx, connect.NewRequest(&heronv1.DeleteNotifyChannelRequest{Id: 999999}))
				return err
			},
			"DeleteSilence missing": func() error {
				_, err := h.admin.DeleteSilence(ctx, connect.NewRequest(&heronv1.DeleteSilenceRequest{Id: 999999}))
				return err
			},
			"EnableTheme missing": func() error {
				_, err := h.admin.EnableTheme(ctx, connect.NewRequest(&heronv1.EnableThemeRequest{Id: "nope", Digest: "x"}))
				return err
			},
			"MoveNodes missing": func() error {
				_, err := h.admin.MoveNodes(ctx, connect.NewRequest(&heronv1.MoveNodesRequest{Ids: []int64{999999}, Position: 1}))
				return err
			},
			"CreateApiToken invalid": func() error {
				_, err := h.admin.CreateApiToken(ctx, connect.NewRequest(&heronv1.CreateApiTokenRequest{Name: ""}))
				return err
			},
			"SecurityAction wrong password": func() error {
				_, err := h.admin.SecurityAction(ctx, connect.NewRequest(&heronv1.SecurityActionRequest{Action: heronv1.SecurityActionKind_SECURITY_ACTION_KIND_TOTP_BEGIN, Password: "wrong"}))
				return err
			},
		} {
			if err := call(); err == nil {
				t.Errorf("%s succeeded", name)
			}
		}
		for name, call := range map[string]func() error{
			"ListApiTokens": func() error {
				_, err := h.admin.ListApiTokens(ctx, connect.NewRequest(&heronv1.ListApiTokensRequest{}))
				return err
			},
			"GetSecurity": func() error {
				_, err := h.admin.GetSecurity(ctx, connect.NewRequest(&heronv1.GetSecurityRequest{}))
				return err
			},
			"ListNotifyChannels": func() error {
				_, err := h.admin.ListNotifyChannels(ctx, connect.NewRequest(&heronv1.ListNotifyChannelsRequest{}))
				return err
			},
			"ListThemes": func() error {
				_, err := h.admin.ListThemes(ctx, connect.NewRequest(&heronv1.ListThemesRequest{}))
				return err
			},
			"PreviewAlertNotification": func() error {
				_, err := h.admin.PreviewAlertNotification(ctx, connect.NewRequest(&heronv1.PreviewAlertNotificationRequest{Sample: &heronv1.AlertNotificationSample{Rule: offlineDraft(saved.Msg.Channel.Id), NodeId: nodeA, Transition: "firing"}}))
				return err
			},
		} {
			if err := call(); err != nil {
				t.Errorf("%s: %v", name, err)
			}
		}
		if added := newOperations(before, operations(t, h)); len(added) != 0 {
			t.Fatalf("rejections or reads wrote records: %+v", added)
		}
	})

	// 登出放在最后：它结束这个会话。
	cases = append(cases, auditCase{method: "Logout", act: func(t *testing.T) error {
		_, err := h.admin.Logout(ctx, connect.NewRequest(&heronv1.LogoutRequest{}))
		return err
	}, want: func() (string, int64, string) { return "logout", 0, session }})

	covered := map[string]bool{}
	for i, c := range cases {
		covered[c.method] = true
		t.Run(fmt.Sprintf("%02d_%s", i, c.method), func(t *testing.T) {
			if c.setup != nil {
				c.setup(t)
			}
			before := operations(t, h)
			if err := c.act(t); err != nil {
				t.Fatalf("call: %v", err)
			}
			added := newOperations(before, operations(t, h))
			if len(added) != 1 {
				t.Fatalf("%d records, want exactly 1: %+v", len(added), added)
			}
			o := added[0]
			action, id, key := c.want()
			if o.Principal != store.PrincipalSession || o.SessionID != session || o.Source != "127.0.0.1" || o.Method != c.method || o.OwnerID != 0 ||
				o.Action != action || o.ResourceID != id || o.ResourceKey != key || o.CommittedAt == 0 {
				t.Fatalf("record %+v, want action %s resource %d %q by session %s via %s", o, action, id, key, session, c.method)
			}
			if requestIDPattern.MatchString(o.RequestID) {
				t.Fatalf("generated request_id %q is a valid ExecuteChange request_id", o.RequestID)
			}
		})
	}
	svc := adminService()
	for i := 0; i < svc.Methods().Len(); i++ {
		name := string(svc.Methods().Get(i).Name())
		path := "/heron.v1.AdminService/" + name
		if h.svc.access[path] == heronv1.Access_ACCESS_SESSION && h.svc.audit[path] == heronv1.Audit_AUDIT_WRITE && !covered[name] {
			t.Errorf("%s is AUDIT_WRITE but has no case here", name)
		}
	}

	db, err := sql.Open("sqlite", h.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query("SELECT action, request_id, before_json, after_json, session_id, source, method, resource_key FROM operation")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var fields [8]string
		if err := rows.Scan(&fields[0], &fields[1], &fields[2], &fields[3], &fields[4], &fields[5], &fields[6], &fields[7]); err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(fields[:], "\x00")
		for _, s := range secrets {
			if s != "" && strings.Contains(joined, s) {
				t.Errorf("record %s carries a credential %q", fields[0], s)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

// 会话直调与 API token 经 ExecuteChange 改同一个节点：两条记录在同一个查询面上，同一个动作名，主体各自；会话看得到两条
// 并能按主体过滤，token 只看得到自己的，指定别人的 owner 被拒绝。
func TestSessionAndTokenRecordsShareOneChain(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	node, _ := h.createNode(t, "shared")
	client, tokenID, _ := grantedClient(t, h, &heronv1.TokenGrant{Permissions: []heronv1.TokenPermission{heronv1.TokenPermission_TOKEN_PERMISSION_CONFIGURE}, AllNodes: true})
	change := &heronv1.ExecuteChangeRequest{RequestId: "by-token", UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"note"}},
		Change: &heronv1.ExecuteChangeRequest_UpdateNode{UpdateNode: &heronv1.UpdateNodeRequest{Id: node, Note: "token"}}}
	previewChange(t, client, change)
	if _, err := client.ExecuteChange(t.Context(), connect.NewRequest(change)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.admin.UpdateNode(t.Context(), connect.NewRequest(&heronv1.UpdateNodeRequest{Id: node, Name: "shared", Note: "session", TrafficResetDay: 1, OfflineGraceS: proto.Uint32(0)})); err != nil {
		t.Fatal(err)
	}
	session := currentSessionID(t, h)
	list := func(c heronv1connect.AdminServiceClient, req *heronv1.ListOperationsRequest) ([]string, error) {
		req.Action = "update_node"
		r, err := c.ListOperations(t.Context(), connect.NewRequest(req))
		if err != nil {
			return nil, err
		}
		var out []string
		for _, o := range r.Msg.Operations {
			out = append(out, fmt.Sprintf("%s/%s/%d/%s/%d", o.Principal, o.Method, o.OwnerId, o.SessionId, o.ResourceId))
		}
		slices.Sort(out)
		return out, nil
	}
	bySession := fmt.Sprintf("OPERATION_PRINCIPAL_SESSION/UpdateNode/0/%s/%d", session, node)
	byToken := fmt.Sprintf("OPERATION_PRINCIPAL_API_TOKEN/ExecuteChange/%d//%d", tokenID, node)
	for _, tc := range []struct {
		name   string
		client heronv1connect.AdminServiceClient
		req    *heronv1.ListOperationsRequest
		want   []string
	}{
		{"session sees both", h.admin, &heronv1.ListOperationsRequest{}, []string{byToken, bySession}},
		{"session filters by principal", h.admin, &heronv1.ListOperationsRequest{Principal: heronv1.OperationPrincipal_OPERATION_PRINCIPAL_API_TOKEN}, []string{byToken}},
		{"session filters by owner", h.admin, &heronv1.ListOperationsRequest{OwnerId: tokenID}, []string{byToken}},
		{"token sees its own", client, &heronv1.ListOperationsRequest{}, []string{byToken}},
		{"token cannot see sessions", client, &heronv1.ListOperationsRequest{Principal: heronv1.OperationPrincipal_OPERATION_PRINCIPAL_SESSION}, nil},
	} {
		got, err := list(tc.client, tc.req)
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v %v, want %v", tc.name, got, err, tc.want)
		}
	}
	if _, err := list(client, &heronv1.ListOperationsRequest{OwnerId: tokenID + 1}); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("token asking for another owner: %v, want PermissionDenied", err)
	}
}

// 会话改计费触发的自动续期记系统主体：一次 UpdateNode 留两条记录，改节点的算会话的，推后到期日的算 hub 自己的。
func TestAutoRenewDuringUpdateNodeIsSystem(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	node, _ := h.createNode(t, "renewing")
	before := operations(t, h)
	// 夹具时钟在 2026-01-01；到期日已过、开着自动续期，UpdateNode 返回前的到期扫描把它推后。
	got := h.update(t, &heronv1.UpdateNodeRequest{Id: node, Name: "renewing", TrafficResetDay: 1, OfflineGraceS: proto.Uint32(0),
		Billing: &heronv1.Billing{BillingCycle: heronv1.BillingCycle_BILLING_CYCLE_MONTHLY, ExpiresOn: "2025-11-15", AutoRenew: true}})
	if got.Billing.ExpiresOn != "2026-01-15" {
		t.Fatalf("expiry after the sweep: %s", got.Billing.ExpiresOn)
	}
	added := newOperations(before, operations(t, h))
	byPrincipal := map[store.PrincipalKind]store.Operation{}
	for _, o := range added {
		byPrincipal[o.Principal] = o
	}
	if len(added) != 2 || byPrincipal[store.PrincipalSession].Action != "update_node" || byPrincipal[store.PrincipalSystem].Action != "auto_renew_node" {
		t.Fatalf("records: %+v", added)
	}
	if sys := byPrincipal[store.PrincipalSystem]; sys.ResourceID != node || sys.Method != "" || sys.Source != "" || sys.SessionID != "" ||
		!strings.Contains(sys.AfterJSON, "2026-01-15") {
		t.Fatalf("system record: %+v", sys)
	}
}

// 方法成功返回却没有留下记录（主写没经 store 的写边界，或库外副作用之后漏了记录）：回答 Internal 并记 Error 日志点名方法，
// 不把一次没留痕的写报告为成功。方法失败时原样返回它的错误。
func TestAuditedCallWithoutRecordAnswersInternal(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	var logs bytes.Buffer
	h.svc.log = slog.New(slog.NewTextHandler(&logs, nil))
	ctx := store.WithCaller(t.Context(), store.Caller{Kind: store.PrincipalSession, Session: strings.Repeat("ab", 32), Source: "192.0.2.1", Method: "UpdateNode"})
	silent := func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
		return connect.NewResponse(&heronv1.UpdateNodeResponse{}), nil
	}
	if _, err := h.svc.audited(ctx, silent, connect.NewRequest(&heronv1.UpdateNodeRequest{}), "UpdateNode"); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("handler without a record: %v, want Internal", err)
	}
	if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "method=UpdateNode") {
		t.Fatalf("missing record not logged with the method: %s", logs.String())
	}
	recorded := func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		return connect.NewResponse(&heronv1.UpdateNodeResponse{}), h.store.ReorderNodes(ctx, nil)
	}
	if _, err := h.svc.audited(ctx, recorded, connect.NewRequest(&heronv1.UpdateNodeRequest{}), "UpdateNode"); err != nil {
		t.Fatalf("handler with a record: %v", err)
	}
	failing := errors.New("business rejection")
	rejected := func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) { return nil, failing }
	if _, err := h.svc.audited(ctx, rejected, connect.NewRequest(&heronv1.UpdateNodeRequest{}), "UpdateNode"); !errors.Is(err, failing) {
		t.Fatalf("handler error: %v, want it passed through", err)
	}
}
