package store

import (
	"bytes"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"google.golang.org/protobuf/proto"
)

// schemaV52 冻结 v52 的完整 DDL：v51 加上 probe_task 的 HTTP 状态码判据两列。
var schemaV52 = append(slices.Clone(schemaV51),
	`ALTER TABLE probe_task ADD COLUMN http_status_policy INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE probe_task ADD COLUMN http_expected_status INTEGER NOT NULL DEFAULT 0`,
)

// removeV52Config 把当前配置库或快照退回 v51 的 probe_task 结构，供从旧版本起步的迁移与恢复测试使用。
func removeV52Config(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec("ALTER TABLE probe_task DROP COLUMN http_status_policy; ALTER TABLE probe_task DROP COLUMN http_expected_status"); err != nil {
		t.Fatal(err)
	}
}

func strictHTTPProbe() *heronv1.ProbeTask {
	t := httpsProbe()
	t.HttpStatusPolicy = heronv1.HttpStatusPolicy_HTTP_STATUS_POLICY_SUCCESS
	return t
}

// 状态码判据是任务内容：随任务落库、在全部读取入口原样读回；判据或精确码变了就换配置身份，没变就保留。
func TestProbeTaskHTTPStatusPolicyIsTaskContent(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	exact := httpsProbe()
	exact.Target, exact.HttpStatusPolicy, exact.HttpExpectedStatus = "https://example.net/health", heronv1.HttpStatusPolicy_HTTP_STATUS_POLICY_EXACT, 204
	savedStrict, _, err := s.SaveProbeTask(ctx, strictHTTPProbe(), NodeSelector{AllNodes: true})
	if err != nil {
		t.Fatal(err)
	}
	savedExact, _, err := s.SaveProbeTask(ctx, exact, NodeSelector{AllNodes: true})
	if err != nil {
		t.Fatal(err)
	}
	if savedStrict.Task.GetHttpStatusPolicy() != heronv1.HttpStatusPolicy_HTTP_STATUS_POLICY_SUCCESS || savedExact.Task.GetHttpExpectedStatus() != 204 {
		t.Fatalf("save returned %v / %v", savedStrict.Task, savedExact.Task)
	}
	_, records, err := s.LoadProbeTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || !proto.Equal(records[0].Task, savedStrict.Task) || !proto.Equal(records[1].Task, savedExact.Task) {
		t.Fatalf("loaded %v, want %v and %v", records, savedStrict.Task, savedExact.Task)
	}
	rec, ok, _, err := s.ListComparisonNodes(ctx, savedExact.Task.Id, ComparisonPublic)
	if err != nil || !ok || !proto.Equal(rec.Task, savedExact.Task) {
		t.Fatalf("comparison read = %v ok=%v err=%v, want %v", rec.Task, ok, err, savedExact.Task)
	}

	// 内容不变（只换分配）：身份保留。
	kept, _, err := s.SaveProbeTask(ctx, savedExact.Task, NodeSelector{})
	if err != nil || !bytes.Equal(kept.Task.ConfigId, savedExact.Task.ConfigId) {
		t.Fatalf("selector-only save changed identity: %x -> %x err=%v", savedExact.Task.ConfigId, kept.Task.ConfigId, err)
	}
	// 只改精确码：新身份。
	other := proto.Clone(kept.Task).(*heronv1.ProbeTask)
	other.HttpExpectedStatus = 200
	changed, _, err := s.SaveProbeTask(ctx, other, NodeSelector{})
	if err != nil || bytes.Equal(changed.Task.ConfigId, kept.Task.ConfigId) || changed.Task.GetHttpExpectedStatus() != 200 {
		t.Fatalf("expected status change = %v (identity %x -> %x) err=%v", changed.Task, kept.Task.ConfigId, changed.Task.ConfigId, err)
	}
	// 只改判据（严格 2xx 退回缺省）：新身份。
	loose := proto.Clone(savedStrict.Task).(*heronv1.ProbeTask)
	loose.HttpStatusPolicy = heronv1.HttpStatusPolicy_HTTP_STATUS_POLICY_UNSPECIFIED
	changedPolicy, _, err := s.SaveProbeTask(ctx, loose, NodeSelector{AllNodes: true})
	if err != nil || bytes.Equal(changedPolicy.Task.ConfigId, savedStrict.Task.ConfigId) || changedPolicy.Task.GetHttpStatusPolicy() != heronv1.HttpStatusPolicy_HTTP_STATUS_POLICY_UNSPECIFIED {
		t.Fatalf("policy change = %v (identity %x -> %x) err=%v", changedPolicy.Task, savedStrict.Task.ConfigId, changedPolicy.Task.ConfigId, err)
	}
}

// 非 HTTP 任务带判据在保存时就被拒绝，不落库。
func TestSaveProbeTaskRejectsHTTPStatusPolicyOnOtherKinds(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	tcp := &heronv1.ProbeTask{Kind: heronv1.ProbeKind_PROBE_KIND_TCP, Target: "example.com:443", IntervalS: 60, TimeoutMs: 1000, HttpStatusPolicy: heronv1.HttpStatusPolicy_HTTP_STATUS_POLICY_SUCCESS}
	var invalid InvalidTaskError
	if _, _, err := s.SaveProbeTask(t.Context(), tcp, NodeSelector{AllNodes: true}); !errors.As(err, &invalid) || !strings.Contains(err.Error(), "http_status_policy only applies to an HTTP task") {
		t.Fatalf("save TCP task with a status policy: err=%v, want InvalidTaskError", err)
	}
	if _, records, err := s.LoadProbeTasks(t.Context()); err != nil || len(records) != 0 {
		t.Fatalf("records after a rejected save = %v err=%v", records, err)
	}
}

// 只做指纹动作的保存沿用库里的任务字段，状态码判据也在其中：不能因请求里留了零值就退回缺省判据。
func TestPinOnlySaveKeepsStoredHTTPStatusPolicy(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	task := httpsProbe()
	task.HttpStatusPolicy, task.HttpExpectedStatus = heronv1.HttpStatusPolicy_HTTP_STATUS_POLICY_EXACT, 204
	saved, _, err := s.SaveProbeTask(ctx, task, NodeSelector{AllNodes: true})
	if err != nil {
		t.Fatal(err)
	}
	pinned, _, err := s.SaveProbeTask(ctx, &heronv1.ProbeTask{Id: saved.Task.Id}, NodeSelector{}, WithCertPin(bytes.Repeat([]byte{7}, 32)), WithExpectedConfigID(saved.Task.ConfigId))
	if err != nil {
		t.Fatal(err)
	}
	if pinned.Task.GetHttpStatusPolicy() != heronv1.HttpStatusPolicy_HTTP_STATUS_POLICY_EXACT || pinned.Task.GetHttpExpectedStatus() != 204 {
		t.Fatalf("pin-only save = %v, want the stored exact 204 policy", pinned.Task)
	}
}

// 两列在配置快照里随任务表搬运，恢复后原样读回；旧快照经迁移补列，旧任务取缺省（< 400）。
func TestProbeTaskHTTPStatusPolicySurvivesConfigSnapshot(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	ctx := t.Context()
	task := httpsProbe()
	task.HttpStatusPolicy, task.HttpExpectedStatus = heronv1.HttpStatusPolicy_HTTP_STATUS_POLICY_EXACT, 401
	saved, _, err := s.SaveProbeTask(ctx, task, NodeSelector{AllNodes: true})
	if err != nil {
		t.Fatal(err)
	}
	restore := func(t *testing.T, config string) []ProbeTaskRecord {
		t.Helper()
		target := filepath.Join(t.TempDir(), "restored.db")
		if _, err := Restore(ctx, target, config, "", "", clk.Now(), s.log); err != nil {
			t.Fatal(err)
		}
		restored, err := Open(target, clk, s.log, RequireCurrentSchema)
		if err != nil {
			t.Fatal(err)
		}
		defer restored.Close()
		_, tasks, err := restored.LoadProbeTasks(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return tasks
	}
	config := filepath.Join(t.TempDir(), "config.db")
	if err := s.SnapshotConfig(ctx, config); err != nil {
		t.Fatal(err)
	}
	if tasks := restore(t, config); len(tasks) != 1 || !proto.Equal(tasks[0].Task, saved.Task) {
		t.Fatalf("restored %v, want %v", tasks, saved.Task)
	}

	// 同一份快照退回 v51 的结构：恢复时迁移补列，取缺省判据。
	old := filepath.Join(t.TempDir(), "config-v51.db")
	if err := s.SnapshotConfig(ctx, old); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", old)
	if err != nil {
		t.Fatal(err)
	}
	removeV56Config(t, db)
	removeV52Config(t, db)
	if _, err := db.Exec("UPDATE snapshot_meta SET schema_version = 51"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	tasks := restore(t, old)
	if len(tasks) != 1 || tasks[0].Task.GetHttpStatusPolicy() != heronv1.HttpStatusPolicy_HTTP_STATUS_POLICY_UNSPECIFIED || tasks[0].Task.GetHttpExpectedStatus() != 0 || tasks[0].Task.GetTarget() != task.GetTarget() {
		t.Fatalf("restored v51 snapshot %v, want the default status policy", tasks)
	}
}

// 迁移 52 只补两列，旧任务一律取缺省：升级前的 HTTP 任务按 < 400 判定，迁出严格 2xx 会让已有任务悄悄改判。
func TestMigrationFromV51AddsHTTPStatusPolicyColumns(t *testing.T) {
	t.Parallel()
	migrated := migrateFrom(t, 51, func(t *testing.T, db *sql.DB) {
		if _, err := db.Exec("INSERT INTO probe_task (kind, target, interval_s, timeout_ms, created_at, config_id) VALUES (3, 'https://legacy.example/', 60, 1000, 0, x'01010101010101010101010101010101')"); err != nil {
			t.Fatal(err)
		}
	})
	_, tasks, err := migrated.LoadProbeTasks(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].Task.GetHttpStatusPolicy() != heronv1.HttpStatusPolicy_HTTP_STATUS_POLICY_UNSPECIFIED || tasks[0].Task.GetHttpExpectedStatus() != 0 {
		t.Fatalf("legacy task after migration = %v, want the default status policy", tasks)
	}
}
