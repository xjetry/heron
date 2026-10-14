package store

import (
	"bytes"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"google.golang.org/protobuf/proto"
)

func httpsProbe() *heronv1.ProbeTask {
	return &heronv1.ProbeTask{Kind: heronv1.ProbeKind_PROBE_KIND_HTTP, Target: "https://example.com/", IntervalS: 60, TimeoutMs: 1000}
}

func TestSaveProbeTaskRegeneratesConfigIDOnlyWhenContentChanges(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	node, _, err := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	pin := bytes.Repeat([]byte{1}, 32)
	saved, _, err := s.SaveProbeTask(ctx, httpsProbe(), NodeSelector{NodeIDs: []int64{node}}, WithCertPin(pin))
	if err != nil || len(saved.Task.ConfigId) != 16 || !bytes.Equal(saved.Task.CertSpkiSha256, pin) {
		t.Fatalf("create = %+v %v", saved.Task, err)
	}
	if _, err := s.UpsertProbeCert(ctx, node, saved.Task.Id, 100, 10, saved.Task.ConfigId); err != nil {
		t.Fatal(err)
	}
	// 只改分配，内容不变：身份留下，证书行还在。
	kept, _, err := s.SaveProbeTask(ctx, saved.Task, NodeSelector{AllNodes: true}, WithExpectedConfigID(saved.Task.ConfigId))
	if err != nil || !bytes.Equal(kept.Task.ConfigId, saved.Task.ConfigId) || !bytes.Equal(kept.Task.CertSpkiSha256, pin) {
		t.Fatalf("selector-only = %x %x %v", kept.Task.ConfigId, kept.Task.CertSpkiSha256, err)
	}
	certs, err := s.ProbeCertsByTask(ctx, saved.Task.Id)
	if err != nil || certs[node] != 100 {
		t.Fatalf("cert after selector edit = %v %v", certs, err)
	}
	// 改目标：新身份，旧证书行删除。
	changed := proto.Clone(kept.Task).(*heronv1.ProbeTask)
	changed.Target = "https://other.example/"
	next, _, err := s.SaveProbeTask(ctx, changed, NodeSelector{AllNodes: true}, WithExpectedConfigID(kept.Task.ConfigId))
	if err != nil || bytes.Equal(next.Task.ConfigId, kept.Task.ConfigId) {
		t.Fatalf("content change kept identity: %x %v", next.Task.ConfigId, err)
	}
	certs, err = s.ProbeCertsByTask(ctx, saved.Task.Id)
	if err != nil || len(certs) != 0 {
		t.Fatalf("old cert survived identity change: %v %v", certs, err)
	}
}

func TestCertWriteRejectsStaleIdentityAndUnassignedNode(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	node, _, err := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := s.CreateNode(ctx, NewNode{Name: "o"}, hash(2))
	if err != nil {
		t.Fatal(err)
	}
	saved, _, err := s.SaveProbeTask(ctx, httpsProbe(), NodeSelector{NodeIDs: []int64{node}})
	if err != nil {
		t.Fatal(err)
	}
	stale := bytes.Repeat([]byte{9}, 16)
	if changed, err := s.UpsertProbeCert(ctx, node, saved.Task.Id, 100, 10, stale); err != nil || changed {
		t.Fatalf("stale cert write changed=%v err=%v", changed, err)
	}
	if err := s.write(ctx, func(tx *sql.Tx) error {
		return upsertPresentedTx(tx, other, saved.Task.Id, saved.Task.ConfigId, bytes.Repeat([]byte{1}, 32), 100, 10, int32(heronv1.PresentedReason_PRESENTED_REASON_PIN_MISMATCH))
	}); err != nil {
		t.Fatal(err)
	}
	if changed, err := s.UpsertProbeCert(ctx, other, saved.Task.Id, 100, 10, saved.Task.ConfigId); err != nil || changed {
		t.Fatalf("unassigned node wrote cert changed=%v err=%v", changed, err)
	}
	certs, err := s.ProbeCertsByTask(ctx, saved.Task.Id)
	if err != nil || len(certs) != 0 {
		t.Fatalf("certs = %v %v, want none", certs, err)
	}
	if err := s.write(ctx, func(tx *sql.Tx) error {
		return upsertPresentedTx(tx, node, saved.Task.Id, nil, bytes.Repeat([]byte{4}, 32), 50, 14, int32(heronv1.PresentedReason_PRESENTED_REASON_CA_VERIFY_FAILED))
	}); err != nil {
		t.Fatal(err)
	}
	var emptyPresented int
	if err := s.r.QueryRow(`SELECT count(*) FROM probe_cert_presented WHERE task_id = ? AND (config_id IS NULL OR length(config_id) = 0)`, int64(saved.Task.Id)).Scan(&emptyPresented); err != nil || emptyPresented != 0 {
		t.Fatalf("empty identity candidate rows=%d err=%v", emptyPresented, err)
	}
	// 空身份无法绑定观测到配置，未钉任务也必须拒绝。
	if changed, err := s.UpsertProbeCert(ctx, node, saved.Task.Id, 200, 11, nil); err != nil || changed {
		t.Fatalf("empty identity write changed=%v err=%v", changed, err)
	}
	pinned, _, err := s.SaveProbeTask(ctx, saved.Task, NodeSelector{NodeIDs: []int64{node}}, WithCertPin(bytes.Repeat([]byte{3}, 32)), WithExpectedConfigID(saved.Task.ConfigId))
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := s.UpsertProbeCert(ctx, node, pinned.Task.Id, 300, 12, nil); err != nil || changed {
		t.Fatalf("empty identity on a pinned task changed=%v err=%v", changed, err)
	}
	if changed, err := s.UpsertProbeCert(ctx, node, pinned.Task.Id, 400, 13, pinned.Task.ConfigId); err != nil || !changed {
		t.Fatalf("current identity changed=%v err=%v", changed, err)
	}
}

func TestCertWriteRejectsEmptyIdentity(t *testing.T) {
	s, _ := open(t)
	node, _, err := s.CreateNode(t.Context(), NewNode{Name: "n"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	saved, _, err := s.SaveProbeTask(t.Context(), httpsProbe(), NodeSelector{AllNodes: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range [][]byte{nil, {}} {
		if err := s.write(t.Context(), func(tx *sql.Tx) error {
			ok, err := certWriteOK(tx, node, saved.Task.Id, id)
			if err != nil {
				return err
			}
			if ok {
				t.Error("transaction guard accepted empty identity")
			}
			// 非空是独立约束，不能只靠与当前身份不相等来拒绝；损坏的当前身份也不得放宽准入。
			if _, err := tx.Exec("UPDATE probe_task SET config_id=X'' WHERE id=?", saved.Task.Id); err != nil {
				return err
			}
			ok, err = certWriteOK(tx, node, saved.Task.Id, id)
			if err != nil {
				return err
			}
			if ok {
				t.Error("transaction guard accepted empty identity matching empty current identity")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCertObservationRequiresConfigID(t *testing.T) {
	s, _ := open(t)
	node, _, err := s.CreateNode(t.Context(), NewNode{Name: "n"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	saved, _, err := s.SaveProbeTask(t.Context(), httpsProbe(), NodeSelector{AllNodes: true})
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := s.UpsertProbeCert(t.Context(), node, saved.Task.Id, 100, 10, nil); err != nil || changed {
		t.Fatalf("empty identity changed=%v err=%v", changed, err)
	}
	if rows := identityCertRows(t, s.r); len(rows) != 0 {
		t.Fatalf("empty identity wrote rows: %+v", rows)
	}
	if changed, err := s.UpsertProbeCert(t.Context(), node, saved.Task.Id, 200, 20, saved.Task.ConfigId); err != nil || !changed {
		t.Fatalf("current identity changed=%v err=%v", changed, err)
	}
	var noPin bool
	if err := s.r.QueryRow("SELECT cert_spki_sha256 IS NULL FROM probe_task WHERE id=?", saved.Task.Id).Scan(&noPin); err != nil {
		t.Fatal(err)
	}
	if !noPin {
		t.Fatal("unpinned task fingerprint must be SQL NULL")
	}
	assertIdentityCerts(t, s.w, []identityCertRow{{node, int64(saved.Task.Id), 200, 20, saved.Task.ConfigId}})
}

func TestRestoreForgetsIdentitiesCreatedAfterTheSnapshot(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	ctx := t.Context()
	node, _, err := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	saved, version, err := s.SaveProbeTask(ctx, httpsProbe(), NodeSelector{NodeIDs: []int64{node}})
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "config.db")
	if err := s.SnapshotConfig(ctx, config); err != nil {
		t.Fatal(err)
	}
	changed := proto.Clone(saved.Task).(*heronv1.ProbeTask)
	changed.Target = "https://later.example/"
	later, _, err := s.SaveProbeTask(ctx, changed, NodeSelector{NodeIDs: []int64{node}}, WithExpectedConfigID(saved.Task.ConfigId))
	if err != nil || bytes.Equal(later.Task.ConfigId, saved.Task.ConfigId) {
		t.Fatal(err)
	}
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
	if err != nil || len(tasks) != 1 || !bytes.Equal(tasks[0].Task.ConfigId, saved.Task.ConfigId) {
		t.Fatalf("restored tasks=%v %v", tasks, err)
	}
	if got, _, err := restored.LoadProbeTasks(ctx); err != nil || got <= version {
		t.Fatalf("restored version=%d snapshot version=%d err=%v, want a bump", got, version, err)
	}
	if changed, err := restored.UpsertProbeCert(ctx, node, saved.Task.Id, 100, 10, later.Task.ConfigId); err != nil || changed {
		t.Fatalf("post-snapshot identity was accepted changed=%v err=%v", changed, err)
	}
	_, _, err = restored.SaveProbeTask(ctx, tasks[0].Task, NodeSelector{NodeIDs: []int64{node}}, WithCertPin(bytes.Repeat([]byte{1}, 32)), WithExpectedConfigID(later.Task.ConfigId))
	if err == nil || !errors.Is(err, ErrPrecondition) {
		t.Fatalf("stale precondition after restore: %v", err)
	}
}

// 证书观测与候选按解析出的 scheme 判断 https：目标写成 "HTTPS://" 的任务同样落库，与 agent 取证书的判据一致。
func TestCertObservationsFollowTheParsedScheme(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	node, _, err := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	task := httpsProbe()
	task.Target = "HTTPS://example.com/"
	saved, _, err := s.SaveProbeTask(ctx, task, NodeSelector{NodeIDs: []int64{node}})
	if err != nil {
		t.Fatal(err)
	}
	id, cfg := saved.Task.Id, saved.Task.ConfigId
	if _, err := s.UpsertProbeCert(ctx, node, id, 100, 10, cfg); err != nil {
		t.Fatal(err)
	}
	if err := s.write(ctx, func(tx *sql.Tx) error {
		return upsertPresentedTx(tx, node, id, cfg, bytes.Repeat([]byte{3}, 32), 200, 10, int32(heronv1.PresentedReason_PRESENTED_REASON_CA_VERIFY_FAILED))
	}); err != nil {
		t.Fatal(err)
	}
	view, err := s.ListProbeCertificates(ctx, id)
	if err != nil || len(view.Visible) != 1 {
		t.Fatalf("view = %+v, err = %v", view, err)
	}
	if n := view.Visible[0]; n.Current == nil || n.Candidate == nil {
		t.Fatalf("uppercase-scheme https task: current=%v candidate=%v, want both written", n.Current != nil, n.Candidate != nil)
	}
}
