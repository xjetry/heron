package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// 直调审计：主写与记录在同一事务里提交，记录带上 Caller 给的主体、会话、来源与方法，写前写后快照取自白名单。业务写
// 返回错误时记录随之回滚；同一次调用的第二次主写被拒绝，不开事务；没有 Caller、或 Caller 与 Principal 对不上时整个
// 事务回滚，业务写也不落库。
func TestDirectAuditRecordsWithTheWrite(t *testing.T) {
	t.Parallel()
	f := newTargetFixture(t)
	base := sessionContext(t.Context())

	a := &Audit{}
	if _, err := f.s.UpdateNode(WithAudit(base, a), f.node, rename("audited")); err != nil {
		t.Fatal(err)
	}
	if !a.Recorded() {
		t.Fatal("primary write committed without a record")
	}
	o := a.Operation
	if o.Principal != PrincipalSession || o.SessionID != testSessionID || o.Source != "192.0.2.1" || o.Method != "Test" || o.Action != "update_node" ||
		o.ResourceID != f.node || o.OwnerID != 0 || o.RequestHash != "" || !strings.HasPrefix(o.RequestID, "op:") {
		t.Fatalf("record: %+v", o)
	}
	if !strings.Contains(o.BeforeJSON, `"original"`) || !strings.Contains(o.AfterJSON, `"audited"`) {
		t.Fatalf("snapshots: before=%s after=%s", o.BeforeJSON, o.AfterJSON)
	}
	stored, err := f.s.ListOperations(base, OperationFilter{Limit: 10})
	if err != nil || len(stored) != 1 || stored[0] != o {
		t.Fatalf("stored record differs from the returned one: %+v %v", stored, err)
	}

	// 同一次调用的第二次主写：不开事务，名字不变，记录不增。
	if _, err := f.s.UpdateNode(WithAudit(base, a), f.node, rename("second")); !errors.Is(err, ErrChangeTarget) {
		t.Fatalf("second primary write: %v, want ErrChangeTarget", err)
	}
	if name, _, receipts := f.state(t); name != "audited" || receipts != 1 {
		t.Fatalf("after a refused second write: name=%q records=%d", name, receipts)
	}

	// 业务拒绝：节点不存在，事务回滚，没有记录。
	rejected := &Audit{}
	if _, err := f.s.UpdateNode(WithAudit(base, rejected), f.node+100, rename("missing")); err == nil {
		t.Fatal("update of a missing node succeeded")
	}
	if rejected.Recorded() {
		t.Fatal("rejected write left a record")
	}
	if _, _, receipts := f.state(t); receipts != 1 {
		t.Fatalf("rejected write changed the record count to %d", receipts)
	}

	// 装配错误：没有 Caller，或会话 Caller 带着 token 主体。业务写随记录一起回滚。
	for name, ctx := range map[string]context.Context{
		"no caller":          WithAudit(t.Context(), &Audit{}),
		"principal mismatch": WithAudit(WithPrincipal(base, APIToken{ID: 9, Identity: "X"}), &Audit{}),
	} {
		if _, err := f.s.UpdateNode(ctx, f.node, rename("unrecorded")); err == nil {
			t.Errorf("%s: write committed", name)
		}
		if n, _, receipts := f.state(t); n != "audited" || receipts != 1 {
			t.Errorf("%s: name=%q records=%d, want the write rolled back", name, n, receipts)
		}
	}
}

// 自动续期记系统主体：即使由某个会话请求顺带触发，记录也不算在那个请求名下，也不用掉它的主写机会。条件不成立的
// 推后连同记录一起回滚。
func TestAutoRenewRecordsSystemPrincipal(t *testing.T) {
	t.Parallel()
	f := newTargetFixture(t)
	mustExec(t, f.s, "UPDATE node SET billing_cycle='monthly', expires_on='2025-11-15', auto_renew=1 WHERE id=?", f.node)
	request := &Audit{}
	ctx := WithAudit(sessionContext(t.Context()), request)
	renewed, err := f.s.RenewExpiry(ctx, f.node, CycleMonthly, "2025-11-15", "2026-01-15")
	if err != nil || !renewed {
		t.Fatalf("renew: %v %v", renewed, err)
	}
	if request.claimed {
		t.Fatal("the system renewal used the request's primary write")
	}
	list, err := f.s.ListOperations(sessionContext(t.Context()), OperationFilter{Principal: PrincipalSystem, Limit: 10})
	if err != nil || len(list) != 1 {
		t.Fatalf("system records: %+v %v", list, err)
	}
	o := list[0]
	if o.Action != "auto_renew_node" || o.ResourceID != f.node || o.SessionID != "" || o.Source != "" || o.Method != "" || o.OwnerID != 0 ||
		!strings.Contains(o.BeforeJSON, "2025-11-15") || !strings.Contains(o.AfterJSON, "2026-01-15") {
		t.Fatalf("system record: %+v", o)
	}
	// 读到的旧到期日已过时：不推后、不留记录。
	if renewed, err := f.s.RenewExpiry(ctx, f.node, CycleMonthly, "2025-11-15", "2026-01-15"); err != nil || renewed {
		t.Fatalf("stale renew: %v %v", renewed, err)
	}
	if _, _, receipts := f.state(t); receipts != 1 {
		t.Fatalf("a renewal that did not happen left a record (records=%d)", receipts)
	}
}

// 会话看得到全部记录；API token 只看得到自己的，按 owner 过滤也越不过去。过滤条件各自生效。
func TestListOperationsVisibilityAndFilters(t *testing.T) {
	t.Parallel()
	f := newTargetFixture(t)
	mine := APIToken{ID: 1, Identity: "MINE"}
	theirs := APIToken{ID: 2, Identity: "THEIRS"}
	write := func(ctx context.Context, name string) {
		t.Helper()
		if _, err := f.s.UpdateNode(WithAudit(ctx, &Audit{}), f.node, rename(name)); err != nil {
			t.Fatal(err)
		}
	}
	write(sessionContext(t.Context()), "by-session")
	write(tokenContext(t.Context(), mine), "by-mine")
	write(tokenContext(t.Context(), theirs), "by-theirs")
	if err := f.s.ReorderNodes(WithAudit(sessionContext(t.Context()), &Audit{}), []int64{f.node}); err != nil {
		t.Fatal(err)
	}
	actions := func(ctx context.Context, filter OperationFilter) []string {
		t.Helper()
		filter.Limit = 10
		list, err := f.s.ListOperations(ctx, filter)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, o := range list {
			out = append(out, string(o.Principal)+"/"+o.Action+"/"+o.SessionID[:min(2, len(o.SessionID))])
		}
		slices.Sort(out)
		return out
	}
	session := sessionContext(t.Context())
	for _, tc := range []struct {
		name   string
		ctx    context.Context
		filter OperationFilter
		want   []string
	}{
		{"session sees all", session, OperationFilter{}, []string{"api_token/update_node/", "api_token/update_node/", "session/reorder_nodes/00", "session/update_node/00"}},
		{"session by owner", session, OperationFilter{OwnerID: mine.ID}, []string{"api_token/update_node/"}},
		{"session by principal", session, OperationFilter{Principal: PrincipalSession}, []string{"session/reorder_nodes/00", "session/update_node/00"}},
		{"session by action", session, OperationFilter{Action: "reorder_nodes"}, []string{"session/reorder_nodes/00"}},
		{"token sees its own", tokenContext(t.Context(), mine), OperationFilter{}, []string{"api_token/update_node/"}},
		{"token cannot widen by owner", tokenContext(t.Context(), mine), OperationFilter{OwnerID: theirs.ID}, nil},
		{"token cannot see sessions", tokenContext(t.Context(), mine), OperationFilter{Principal: PrincipalSession}, nil},
	} {
		if got := actions(tc.ctx, tc.filter); !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// leakCanary 出现在夹具里每一处凭据或可换出凭据的内容中；canaryHash 是夹具里每一个凭据哈希的字节模式。
const leakCanary = "LEAKCANARY"

var canaryHash = bytes.Repeat([]byte{0xC0, 0xFF, 0xEE, 0x11}, 8)

// forbiddenSnapshotNames 是快照里不能出现的字段名：它们是凭据、凭据的摘要或装着凭据的整块配置的列名或 JSON 键。
var forbiddenSnapshotNames = map[string]bool{
	"token_hash": true, "key_hash": true, "password": true, "password_hash": true, "secret": true, "config": true,
	"credential": true, "recovery": true, "user_id": true, "publickey": true, "public_key": true, "url": true,
	"headers": true, "body": true, "bot_token": true, "template": true,
}

// 每一种动作的快照都在一个装满凭据的库上读一遍：输出里不得有凭据原文、凭据哈希的十六进制，也不得有凭据类的字段名。
// 每次都核对对象确实读到了（主行非空），扫描不会因为读空而白过。
func TestSnapshotsCarryNoSecrets(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	ctx := t.Context()
	node, _, err := s.CreateNode(ctx, NewNode{Name: "n"}, canaryHash)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetRegisterWindow(ctx, canaryHash, clk.Now().Add(time.Hour), 1); err != nil {
		t.Fatal(err)
	}
	token, err := s.CreateAPIToken(ctx, "t", [32]byte(canaryHash), clk.Now(), 10, &TokenGrant{NodeIDs: []int64{node}}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	channel, err := s.SaveNotifyChannel(ctx, NotifyChannel{Name: "c", Kind: ChannelWebhook,
		Config: `{"url":"https://` + leakCanary + `.example/hook","headers":{"Authorization":"` + leakCanary + `"},"body":"` + leakCanary + `","bot_token":"` + leakCanary + `"}`})
	if err != nil {
		t.Fatal(err)
	}
	secret := leakCanary + "-s3"
	if _, err := s.SaveSettings(ctx, SettingsUpdate{
		Backup:    &BackupSettingsUpdate{Endpoint: "https://s3.example", Bucket: "b", AccessKey: "ak", Secret: &secret},
		Heartbeat: &HeartbeatUpdate{URL: "https://hb.example/" + leakCanary, IntervalS: 60, Method: HeartbeatPost},
	}); err != nil {
		t.Fatal(err)
	}
	session := bytes.Repeat([]byte{0xAB}, 32)
	data := `{"secret":"` + leakCanary + `","last_step":1,"recovery":["` + leakCanary + `"],"user_id":"` + leakCanary + `","origin":"https://admin.example","rp_id":"admin.example",` +
		`"passkeys":[{"name":"pk","credential":{"id":"` + leakCanary + `","publicKey":"` + leakCanary + `","attestationType":"` + leakCanary + `"}}]}`
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{"UPDATE admin_security SET data=? WHERE id=1", []any{data}},
		{"INSERT INTO admin_session (token_hash,created_at,last_used_at,expires_at,source) VALUES (?,1,1,9999999999,'192.0.2.9')", []any{session}},
		{"INSERT INTO probe_task (id,kind,target,interval_s,timeout_ms,created_at,all_nodes,sort_order,config_id) VALUES (1,1,'example.net',60,1000,0,1,0,x'01')", nil},
		{"INSERT INTO alert_rule (id,name,kind,all_nodes,created_at) VALUES (1,'r','offline',1,1)", nil},
		{"INSERT INTO silence (id,name,kind,all_nodes,from_at,until_at,created_at,all_rules) VALUES (1,'m','once',1,1,100,1,0)", nil},
		{"INSERT INTO tag (id,name,name_fold) VALUES (1,'t','t')", nil},
		{"INSERT INTO node_tag (node_id,tag_id) VALUES (?,1)", []any{node}},
		{"INSERT INTO traffic (node_id,boot_id,total_rx,total_tx,period_rx,period_tx,period_start,updated_at) VALUES (?,'b',1,2,3,4,5,6)", []any{node}},
		{"INSERT INTO node_update (node_id,data) VALUES (?,'{}')", []any{node}},
		{"INSERT INTO theme (id) VALUES ('th')", nil},
		{"INSERT INTO theme_version (theme_id,digest,name,version,preview,uploaded_at,sdk,published) VALUES ('th','d1','T','1','',1,1,1)", nil},
		{"UPDATE theme_selection SET current_id='th',current_digest='d1' WHERE id=1", nil},
	} {
		mustExec(t, s, q.sql, q.args...)
	}
	byID := map[ChangeKind]int64{ChangeNode: node, ChangeProbe: 1, ChangeAlert: 1, ChangeWindow: 0, ChangeUpdate: node, ChangeTag: 1,
		ChangeTraffic: node, ChangeChannel: channel.ID, ChangeSilence: 1, ChangeAPIToken: token.ID}
	byKey := map[ChangeKind]string{ChangeNodeTags: idsKey([]int64{node}), ChangeTheme: "th", ChangeSession: hex.EncodeToString(session)}
	canaryHex := strings.ToUpper(hex.EncodeToString(canaryHash[:4]))
	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for action, spec := range changeActions {
		kind := changeKinds[spec.kind]
		id, key := byID[spec.kind], byKey[spec.kind]
		if _, ok := byID[spec.kind]; !ok && kind.arg == byResourceID {
			t.Errorf("%s: no fixture object of kind %s", action, spec.kind)
			continue
		}
		if _, ok := byKey[spec.kind]; !ok && kind.arg == byResourceKey {
			t.Errorf("%s: no fixture object of kind %s", action, spec.kind)
			continue
		}
		out, _, err := snapshot(tx, action, id, key)
		if err != nil {
			t.Errorf("%s: %v", action, err)
			continue
		}
		var parsed map[string][]map[string]any
		if err := json.Unmarshal([]byte(out), &parsed); err != nil {
			t.Errorf("%s: %v", action, err)
			continue
		}
		if len(parsed[spec.kind.String()]) == 0 {
			t.Errorf("%s: the fixture object was not read, the scan proves nothing: %s", action, out)
		}
		upper := strings.ToUpper(out)
		if strings.Contains(upper, leakCanary) || strings.Contains(upper, canaryHex) {
			t.Errorf("%s: snapshot carries a credential: %s", action, out)
		}
		for name, rows := range parsed {
			for _, row := range rows {
				for column := range row {
					if forbiddenSnapshotNames[strings.ToLower(column)] || forbiddenSnapshotNames[strings.ToLower(name)] {
						t.Errorf("%s: snapshot has credential field %s.%s", action, name, column)
					}
				}
			}
		}
	}
}

// 设置快照的白名单与只写表恰好覆盖 SaveSettings 能写出的每一个键：新增的设置键不登记就红，登记了却写不出来的陈旧键
// 也红。只写的键在快照里只有"是否已设置"。
func TestSettingSnapshotClassifiesEveryKey(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	channel, err := s.SaveNotifyChannel(ctx, NotifyChannel{Name: "c", Kind: ChannelWebhook, Config: "{}"})
	if err != nil {
		t.Fatal(err)
	}
	on, url, secret := true, "https://geo.example/{ip}", leakCanary
	interval, keep := uint32(3600), uint32(10)
	channels := []int64{channel.ID}
	if _, err := s.SaveSettings(ctx, SettingsUpdate{
		Appearance:    &SiteAppearance{Title: "t", Theme: "dark", AccentColor: "#123456", Logo: "data:image/png;base64,AA==", CustomCSS: "a{}"},
		PublicEnabled: &on,
		Geo:           GeoUpdate{Enabled: &on, URL: &url},
		Backup: &BackupSettingsUpdate{Endpoint: "https://s3.example", Bucket: "b", Region: "r", AccessKey: "ak", Prefix: "p", Secret: &secret,
			ConfigIntervalS: &interval, MetricsIntervalS: &interval, ConfigKeep: &keep, MetricsKeep: &keep, Channels: &channels},
		LoginChannels: &channels,
		Heartbeat:     &HeartbeatUpdate{URL: "https://hb.example/" + leakCanary, IntervalS: 60, Method: HeartbeatPost},
		TrafficReport: &TrafficReportUpdate{Enabled: true, Daily: true, Weekly: true, Monthly: true, Hour: 9, Channels: channels},
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.r.QueryContext(ctx, "SELECT key FROM setting ORDER BY key")
	if err != nil {
		t.Fatal(err)
	}
	var written []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		written = append(written, k)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	for _, k := range written {
		if slices.Contains(settingSnapshotKeys, k) == slices.Contains(settingWriteOnlyKeys, k) {
			t.Errorf("setting %s is in neither or both of settingSnapshotKeys and settingWriteOnlyKeys", k)
		}
	}
	for _, k := range append(slices.Clone(settingSnapshotKeys), settingWriteOnlyKeys...) {
		if !slices.Contains(written, k) {
			t.Errorf("setting %s is classified but SaveSettings never writes it", k)
		}
	}
	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	out, _, err := snapshot(tx, ActionUpdateSettings, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, leakCanary) || !strings.Contains(out, `{"is_set":1,"key":"backup.secret"}`) || !strings.Contains(out, `{"is_set":1,"key":"heartbeat.url"}`) || !strings.Contains(out, `"value":"https://geo.example/{ip}"`) {
		t.Fatalf("settings snapshot: %s", out)
	}
}
