package alert

import (
	"bytes"
	"database/sql"
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/store"
)

// 证书到期规则挂在 https:// 的 HTTP 任务上；夹具的今天是东八区 2026-09-24。
func certRule(task uint64) store.AlertRule {
	return store.AlertRule{Name: "证书到期", Kind: store.KindCertExpiry, Enabled: true, AllNodes: true, TaskID: task, DaysBefore: 7}
}

func (f *fixture) httpsTask(t *testing.T) uint64 {
	t.Helper()
	p, _, err := f.st.SaveProbeTask(t.Context(), &heronv1.ProbeTask{Kind: heronv1.ProbeKind_PROBE_KIND_HTTP, Target: "https://example.com/", IntervalS: 30, TimeoutMs: 1000}, store.NodeSelector{AllNodes: true})
	must(t, err)
	return p.Task.Id
}

// cert 写一份 (节点, 任务) 的证书观测：notAfter 取当地正午，Today 之后就是 expiresOn 那一天。
func (f *fixture) cert(t *testing.T, node int64, task uint64, expiresOn string) {
	t.Helper()
	d := date(expiresOn)
	notAfter := time.Date(d.Year(), d.Month(), d.Day(), 12, 0, 0, 0, f.loc)
	_, err := f.st.UpsertProbeCert(t.Context(), node, task, notAfter.Unix(), f.clk.Now().Unix(), f.certIdentity(t, task))
	must(t, err)
}

func (f *fixture) certIdentity(t *testing.T, task uint64) []byte {
	t.Helper()
	view, err := f.st.ListProbeCertificates(t.Context(), task)
	must(t, err)
	if !view.Found {
		t.Fatal("certificate task missing")
	}
	return view.Task.Task.ConfigId
}

func TestCertAlertStateAfterIdentityCleanup(t *testing.T) {
	f := newFixture(t)
	task := f.httpsTask(t)
	r := f.rule(t, certRule(task))
	f.cert(t, f.ids[0], task, "2026-09-28")
	f.sweepExpiry(t)
	wantState(t, f.e, r.ID, f.ids[0], store.StateFiring)
	must(t, f.st.Close())
	db, err := sql.Open("sqlite", f.path)
	must(t, err)
	// 回退到 44 前撤掉之后新增的结构，并重建可空身份列，构造迁移前的持久化状态。
	// 节点资料版本触发器挂在更早的表上，按名字前缀全部删除；只删版本表会让再迁移撞上同名对象。
	var triggers []string
	found, err := db.Query(`SELECT name FROM sqlite_schema WHERE type = 'trigger' AND name LIKE 'node\_list\_revision\_%' ESCAPE '\'`)
	must(t, err)
	for found.Next() {
		var name string
		must(t, found.Scan(&name))
		triggers = append(triggers, name)
	}
	must(t, found.Close())
	if len(triggers) == 0 {
		t.Fatal("no node_list_revision triggers found; the downgrade below no longer matches the schema")
	}
	for _, name := range triggers {
		_, err = db.Exec("DROP TRIGGER " + name)
		must(t, err)
	}
	_, err = db.Exec(`DROP TABLE node_list_revision;
		DROP TABLE silence_rule;
		ALTER TABLE silence DROP COLUMN all_rules;
		ALTER TABLE traffic DROP COLUMN observed_at;
		ALTER TABLE api_token DROP COLUMN expires_at;
		CREATE TABLE probe_cert_old (node_id INTEGER NOT NULL, task_id INTEGER NOT NULL, not_after INTEGER NOT NULL, observed_at INTEGER NOT NULL, config_id BLOB, PRIMARY KEY (node_id, task_id)) WITHOUT ROWID;
		INSERT INTO probe_cert_old SELECT node_id, task_id, not_after, observed_at, NULL FROM probe_cert;
		DROP TABLE probe_cert;
		ALTER TABLE probe_cert_old RENAME TO probe_cert;
		PRAGMA user_version=44`)
	must(t, err)
	must(t, db.Close())
	f.st, err = store.Open(f.path, f.clk, f.log, store.MigrateSchema)
	must(t, err)
	f.restart(t)
	rows, err := f.st.ProbeCertsByTask(t.Context(), task)
	must(t, err)
	if len(rows) != 0 {
		t.Fatalf("identity cleanup left observations: %v", rows)
	}
	f.sweepExpiry(t)
	wantState(t, f.e, r.ID, f.ids[0], store.StateFiring)
	if events := f.events(t); len(events) != 1 {
		t.Fatalf("cleanup changed alert events: %+v", events)
	}
	f.cert(t, f.ids[0], task, "2027-09-28")
	f.sweepExpiry(t)
	wantState(t, f.e, r.ID, f.ids[0], store.StateOK)
	if events := f.events(t); len(events) != 2 || events[0].Transition != store.TransitionRecovered {
		t.Fatalf("new identity observation did not recover: %+v", events)
	}
}

// days_left 恰等于 days_before 触发，多一天不触发；触发文案与到期规则同一形状，值是剩余天数。
func TestSweepCertExpiryFiresAtTheWindowEdge(t *testing.T) {
	f := newFixture(t)
	task := f.httpsTask(t)
	r := f.rule(t, certRule(task))
	f.cert(t, f.ids[0], task, "2026-10-01") // 剩 7 天
	f.cert(t, f.ids[1], task, "2026-10-02") // 剩 8 天
	f.sweepExpiry(t)
	f.sweepExpiry(t)
	wantState(t, f.e, r.ID, f.ids[0], store.StateFiring)
	wantState(t, f.e, r.ID, f.ids[1], "")
	events := f.events(t)
	if len(events) != 1 || events[0].NodeID != f.ids[0] ||
		events[0].Summary != "节点 node1 的证书将于 2026-10-01 到期（剩 7 天，规则 证书到期）" || events[0].Value != 7 {
		t.Fatalf("events = %+v, want exactly one firing at the window edge", events)
	}
}

// 已过期按负的剩余天数触发；到期时刻按 hub 时区换算日历日：东八区 2026-10-01 00:30 的证书对 UTC 还是 9 月 30 日，
// 剩余天数按 10-01 算。
func TestSweepCertExpiryCountsDaysInTheHubZone(t *testing.T) {
	f := newFixture(t)
	task := f.httpsTask(t)
	r := f.rule(t, certRule(task))
	f.cert(t, f.ids[0], task, "2026-09-20") // 已过期 4 天
	f.sweepExpiry(t)
	wantState(t, f.e, r.ID, f.ids[0], store.StateFiring)
	events := f.events(t)
	if len(events) != 1 || events[0].Summary != "节点 node1 的证书已于 2026-09-20 到期（已过期 4 天，规则 证书到期）" || events[0].Value != -4 {
		t.Fatalf("events = %+v", events)
	}
}

// 没有 probe_cert 行的 (节点, 任务) 是无读数：不评估、不产生状态与事件；同任务有行的节点照常评估。
func TestSweepCertExpirySkipsNodesWithoutAnObservation(t *testing.T) {
	f := newFixture(t)
	task := f.httpsTask(t)
	r := f.rule(t, certRule(task))
	f.cert(t, f.ids[0], task, "2026-09-28")
	f.sweepExpiry(t)
	wantState(t, f.e, r.ID, f.ids[0], store.StateFiring)
	wantState(t, f.e, r.ID, f.ids[1], "")
	if events := f.events(t); len(events) != 1 {
		t.Fatalf("events = %+v, want only the node with an observation", events)
	}
}

// 证书更换后到期日变远，下一轮扫描恢复；文案写"证书到期日已更新为"，与到期规则的续期恢复同一形状。
func TestSweepCertExpiryRecoversWhenTheCertIsReplaced(t *testing.T) {
	f := newFixture(t)
	task := f.httpsTask(t)
	r := f.rule(t, certRule(task))
	f.cert(t, f.ids[0], task, "2026-09-28")
	f.sweepExpiry(t)
	wantState(t, f.e, r.ID, f.ids[0], store.StateFiring)
	f.cert(t, f.ids[0], task, "2027-09-28")
	f.sweepExpiry(t)
	wantState(t, f.e, r.ID, f.ids[0], store.StateOK)
	events := f.events(t)
	if len(events) != 2 || events[0].Transition != store.TransitionRecovered ||
		events[0].Summary != "节点 node1 的证书到期日已更新为 2027-09-28（规则 证书到期），告警持续 不到 1 分钟" {
		t.Fatalf("events = %+v, want a renewal recovery after the firing", events)
	}
}

// 维护静默不约束日历类规则：维护中的节点触发证书到期照常投递，事件不带静默标记。
func TestSweepCertExpiryIsNotSilencedByMaintenance(t *testing.T) {
	f := newFixture(t)
	task := f.httpsTask(t)
	r := f.rule(t, certRule(task))
	f.setMaintenance(t, f.ids[0], true)
	f.cert(t, f.ids[0], task, "2026-09-28")
	f.sweepExpiry(t)
	wantState(t, f.e, r.ID, f.ids[0], store.StateFiring)
	events := f.events(t)
	if len(events) != 1 || events[0].Silenced {
		t.Fatalf("events = %+v, want an unsilenced firing under maintenance", events)
	}
}

// ingest 的变化触发走同一入口：写入变化的观测后调 SweepExpiry（装配在 serve.go 的 CertObserved 钩子里），
// 续期不必等到日界即恢复；同值重写不触发（changed 为假，调用方不扫），状态原样。
func TestSweepCertExpiryTriggeredByAChangedObservation(t *testing.T) {
	f := newFixture(t)
	task := f.httpsTask(t)
	r := f.rule(t, certRule(task))
	f.cert(t, f.ids[0], task, "2026-09-28")
	f.sweepExpiry(t)
	wantState(t, f.e, r.ID, f.ids[0], store.StateFiring)
	notAfter := date("2027-09-28")
	notAfterUnix := time.Date(notAfter.Year(), notAfter.Month(), notAfter.Day(), 12, 0, 0, 0, f.loc).Unix()
	changed, err := f.st.UpsertProbeCert(t.Context(), f.ids[0], task, notAfterUnix, f.clk.Now().Unix(), f.certIdentity(t, task))
	must(t, err)
	if !changed {
		t.Fatal("a renewed cert must report changed so ingest triggers the sweep")
	}
	f.sweepExpiry(t) // serve.go 的 CertObserved 钩子在 changed 时做的事
	wantState(t, f.e, r.ID, f.ids[0], store.StateOK)
	again, err := f.st.UpsertProbeCert(t.Context(), f.ids[0], task, notAfterUnix, f.clk.Now().Unix(), f.certIdentity(t, task))
	must(t, err)
	if again {
		t.Fatal("rewriting the same not_after must not report changed")
	}
}

// 节点删除经 RemoveNode 清掉 probe_cert 行，状态随候选集撤销；任务删除先被规则引用检查拦住。
func TestSweepCertExpiryDropsStatesOfNodesDeletedElsewhere(t *testing.T) {
	f := newFixture(t)
	task := f.httpsTask(t)
	r := f.rule(t, certRule(task))
	f.cert(t, f.ids[0], task, "2026-09-28")
	f.sweepExpiry(t)
	wantState(t, f.e, r.ID, f.ids[0], store.StateFiring)
	must(t, f.st.DeleteNode(t.Context(), f.ids[0]))
	f.sweepExpiry(t)
	wantState(t, f.e, r.ID, f.ids[0], "")
	if _, err := f.st.DeleteProbeTask(t.Context(), task); err == nil {
		t.Fatal("deleting a task referenced by a cert_expiry rule must be refused")
	}
}

// 保存启用的证书到期规则即评估一次，不等日界；把 days_before 调小重新保存立即按新窗口评估。
func TestSaveRuleEvaluatesEnabledCertExpiryRules(t *testing.T) {
	f := newFixture(t)
	task := f.httpsTask(t)
	f.cert(t, f.ids[0], task, "2026-09-27")
	r := f.rule(t, certRule(task))
	wantState(t, f.e, r.ID, f.ids[0], store.StateFiring)
	r.DaysBefore = 2
	f.rule(t, r)
	wantState(t, f.e, r.ID, f.ids[0], store.StateOK)
	events := f.events(t)
	if len(events) != 2 || events[0].Transition != store.TransitionRecovered ||
		events[0].Summary != "节点 node1 的证书已不在提醒窗口内（规则 证书到期），告警持续 不到 1 分钟" {
		t.Fatalf("events = %+v", events)
	}
}

// 列表与告警只消费带当前身份的观测，空身份不能在任何一面产生读数。
func TestCertIdentityAgreesOnTheListAndTheExpiryAlert(t *testing.T) {
	f := newFixture(t)
	saved, _, err := f.st.SaveProbeTask(t.Context(), &heronv1.ProbeTask{Kind: heronv1.ProbeKind_PROBE_KIND_HTTP, Target: "https://example.com/", IntervalS: 30, TimeoutMs: 1000}, store.NodeSelector{AllNodes: true})
	must(t, err)
	task, cfg := saved.Task.GetId(), append([]byte(nil), saved.Task.GetConfigId()...)
	notAfter := time.Date(2026, 9, 28, 12, 0, 0, 0, f.loc).Unix()
	now := f.clk.Now().Unix()
	_, err = f.st.UpsertProbeCert(t.Context(), f.ids[0], task, notAfter, now, cfg)
	must(t, err)
	_, err = f.st.UpsertProbeCert(t.Context(), f.ids[1], task, notAfter, now, nil)
	must(t, err)

	view, err := f.st.ListProbeCertificates(t.Context(), task)
	must(t, err)
	byNode := map[int64]store.ProbeCertNodeView{}
	for _, n := range view.Visible {
		byNode[n.NodeID] = n
	}
	if byNode[f.ids[0]].Current == nil {
		t.Fatal("current identity missing from certificate list")
	}
	if byNode[f.ids[1]].Current != nil {
		t.Fatal("empty identity appears as a current observation")
	}

	r := f.rule(t, certRule(task))
	f.sweepExpiry(t)
	wantState(t, f.e, r.ID, f.ids[0], store.StateFiring)
	wantState(t, f.e, r.ID, f.ids[1], "")

	pin := bytes.Repeat([]byte{7}, 32)
	_, _, err = f.st.SaveProbeTask(t.Context(), saved.Task, store.NodeSelector{AllNodes: true}, store.WithCertPin(pin), store.WithExpectedConfigID(cfg))
	must(t, err)
	left, err := f.st.ProbeCertsByTask(t.Context(), task)
	must(t, err)
	if len(left) != 0 {
		t.Fatalf("certs after pin = %v, want no rows", left)
	}
	changed, err := f.st.UpsertProbeCert(t.Context(), f.ids[1], task, notAfter, now, nil)
	must(t, err)
	if changed {
		t.Fatal("empty identity was written onto a pinned task")
	}
	after, err := f.st.ListProbeCertificates(t.Context(), task)
	must(t, err)
	for _, n := range after.Visible {
		if n.NodeID == f.ids[1] && n.Current != nil {
			t.Fatal("empty identity appears after the task was pinned")
		}
	}
}
