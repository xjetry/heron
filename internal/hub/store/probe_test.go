package store

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/metric"
	"github.com/xjetry/heron/internal/probelimit"
	"google.golang.org/protobuf/proto"
)

func probeRow(nodeID int64, ts int64, task uint64, rtts []uint32, lost, errs uint32) metric.ProbeRow {
	b := &metric.ProbeBucket{}
	for _, us := range rtts {
		b.Add(&heronv1.ProbeResult{TaskId: task, Outcome: &heronv1.ProbeResult_RttUs{RttUs: us}})
	}
	for range lost {
		b.Add(&heronv1.ProbeResult{TaskId: task, Outcome: &heronv1.ProbeResult_Timeout{Timeout: &heronv1.Timeout{}}})
	}
	for range errs {
		b.Add(&heronv1.ProbeResult{TaskId: task, Outcome: &heronv1.ProbeResult_Error{Error: &heronv1.ProbeError{Message: "x"}}})
	}
	return metric.ProbeRow{NodeID: nodeID, TS: ts, TaskID: task, Bucket: b}
}

// 不并行：它临时改包级的 family.states（metricFamily、probeFamily 被所有 Store 共用），并行用例的
// WriteMinuteBatch 与上卷会按改过的水位键去查 rollup_state，查不到行。
func TestMinuteBatchUsesFamilyWatermarkKeys(t *testing.T) {
	for _, f := range families {
		t.Run(f.name, func(t *testing.T) {
			s, _ := open(t)
			seedProbeTasks(t, s, 7)
			id, _, err := s.CreateNode(t.Context(), NewNode{Name: "n"}, hash(1))
			if err != nil {
				t.Fatal(err)
			}
			original := f.states[1]
			f.states[1] = original + "_renamed"
			defer func() { f.states[1] = original }()
			if err := s.write(t.Context(), func(tx *sql.Tx) error {
				_, err := tx.Exec("UPDATE rollup_state SET level = ?, upto_ts = 1200 WHERE level = ?", f.states[1], original)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			n, err := s.WriteMinuteBatch(t.Context(), metric.Batch{
				Rows:   minuteRows(id, 600, 660),
				Probes: []metric.ProbeRow{probeRow(id, 600, 7, []uint32{5}, 0, 0)},
			})
			if err != nil || n != 1 {
				t.Fatalf("family watermark: rejected=%d err=%v, want 1 and nil", n, err)
			}
			for _, other := range families {
				want := 1
				if other == f {
					want = 0
				}
				var count int
				if err := s.r.QueryRow("SELECT COUNT(*) FROM " + other.tables[0]).Scan(&count); err != nil || count != want {
					t.Fatalf("%s count=%d err=%v, want %d", other.name, count, err, want)
				}
			}
		})
	}
}

func TestProbeRowsMergeAdditivelyAndKeepNullRtt(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	seedProbeTasks(t, s, 7, 8)
	ctx := t.Context()
	id, _, _ := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	// 先写一个只有丢包的桶：rtt_min/max 必须是 NULL 而不是 0。
	if _, err := s.WriteMinuteBatch(ctx, metric.Batch{Probes: []metric.ProbeRow{probeRow(id, 600, 7, nil, 2, 0)}}); err != nil {
		t.Fatal(err)
	}
	var mn sql.NullInt64
	if err := s.r.QueryRow("SELECT rtt_min_us FROM probe_1m WHERE node_id = ? AND ts = 600 AND task_id = 7", id).Scan(&mn); err != nil || mn.Valid {
		t.Fatalf("rtt_min_us should be NULL without samples; got valid=%v err=%v", mn.Valid, err)
	}
	// 再并入有 rtt 的半桶：min 不能被 NULL 或 0 污染。
	if _, err := s.WriteMinuteBatch(ctx, metric.Batch{Probes: []metric.ProbeRow{probeRow(id, 600, 7, []uint32{300, 100}, 0, 1)}}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.QueryProbes(ctx, id, 600, 660, levels[0], 60, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%s err=%v", formatProbeRows(rows), err)
	}
	b := rows[0].Bucket
	if b.Sent != 5 || b.Lost != 2 || b.Errors != 1 || b.RttN != 2 || b.RttMinUs != 100 || b.RttMaxUs != 300 || b.RttSumUs != 400 {
		t.Fatalf("merged bucket %+v", *b)
	}

	for _, tc := range []struct {
		name string
		rtts []uint32
		lost uint32
		want metric.ProbeBucket
	}{
		{"inside", []uint32{200}, 0, metric.ProbeBucket{Sent: 6, Lost: 2, Errors: 1, RttN: 3, RttSumUs: 600, RttMinUs: 100, RttMaxUs: 300}},
		{"outside", []uint32{50, 400}, 0, metric.ProbeBucket{Sent: 8, Lost: 2, Errors: 1, RttN: 5, RttSumUs: 1050, RttMinUs: 50, RttMaxUs: 400}},
		{"loss_after_samples", nil, 1, metric.ProbeBucket{Sent: 9, Lost: 3, Errors: 1, RttN: 5, RttSumUs: 1050, RttMinUs: 50, RttMaxUs: 400}},
	} {
		if _, err := s.WriteMinuteBatch(ctx, metric.Batch{Probes: []metric.ProbeRow{probeRow(id, 600, 7, tc.rtts, tc.lost, 0)}}); err != nil {
			t.Fatal(err)
		}
		t.Run(tc.name, func(t *testing.T) {
			rows, err := s.QueryProbes(ctx, id, 600, 660, levels[0], 60, 0)
			if err != nil || len(rows) != 1 || *rows[0].Bucket != tc.want {
				t.Fatalf("merged %s: rows=%s err=%v want=%+v", tc.name, formatProbeRows(rows), err, tc.want)
			}
		})
	}
	// 双方都无样本的冲突合并仍然必须保持 SQL NULL。
	for range 2 {
		if _, err := s.WriteMinuteBatch(ctx, metric.Batch{Probes: []metric.ProbeRow{probeRow(id, 600, 8, nil, 1, 0)}}); err != nil {
			t.Fatal(err)
		}
	}
	var mx sql.NullInt64
	if err := s.r.QueryRow("SELECT rtt_min_us, rtt_max_us FROM probe_1m WHERE node_id=? AND ts=600 AND task_id=8", id).Scan(&mn, &mx); err != nil || mn.Valid || mx.Valid {
		t.Fatalf("NULL/NULL merge min=%v max=%v err=%v", mn, mx, err)
	}
}

func TestProbeWriterRejectsRowsBeforeProbeWatermarkOnly(t *testing.T) {
	t.Parallel()
	for _, frozen := range []string{"probe_5m", "5m"} {
		t.Run(frozen, func(t *testing.T) {
			s, _ := open(t)
			seedProbeTasks(t, s, 7)
			ctx := t.Context()
			id, _, _ := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
			if err := s.setRollupWatermark(t.Context(), frozen, 1200); err != nil {
				t.Fatal(err)
			}
			n, err := s.WriteMinuteBatch(ctx, metric.Batch{Rows: minuteRows(id, 600, 660), Probes: []metric.ProbeRow{probeRow(id, 600, 7, []uint32{5}, 0, 0)}})
			if err != nil || n != 1 {
				t.Fatalf("rejected=%d err=%v, want 1", n, err)
			}
			counts := rowCounts(t, s)
			wantMetric, wantProbe := int64(1), int64(0)
			if frozen == "5m" {
				wantMetric, wantProbe = 0, 1
			}
			if counts["metric_1m"] != wantMetric || counts["probe_1m"] != wantProbe {
				t.Fatalf("independent freeze: metric=%d probe=%d, want %d/%d", counts["metric_1m"], counts["probe_1m"], wantMetric, wantProbe)
			}
		})
	}
}

func TestQueryProbesRebucketsPerTask(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	seedProbeTasks(t, s, 3, 9)
	id, _, _ := s.CreateNode(t.Context(), NewNode{Name: "n"}, hash(1))
	var input []metric.ProbeRow
	for ts := int64(600); ts < 1200; ts += 60 {
		for _, task := range []uint64{9, 3} {
			input = append(input, probeRow(id, ts, task, []uint32{100}, 1, 1))
		}
	}
	input = append(input, probeRow(id, 1200, 3, []uint32{200}, 0, 0))
	if _, err := s.WriteMinuteBatch(t.Context(), metric.Batch{Probes: input}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.QueryProbes(t.Context(), id, 630, 1201, levels[0], 300, 0)
	want := []metric.ProbeRow{
		{NodeID: id, TS: 600, TaskID: 3, Bucket: &metric.ProbeBucket{Sent: 15, Lost: 5, Errors: 5, RttN: 5, RttSumUs: 500, RttMinUs: 100, RttMaxUs: 100}},
		{NodeID: id, TS: 900, TaskID: 3, Bucket: &metric.ProbeBucket{Sent: 15, Lost: 5, Errors: 5, RttN: 5, RttSumUs: 500, RttMinUs: 100, RttMaxUs: 100}},
		{NodeID: id, TS: 1200, TaskID: 3, Bucket: &metric.ProbeBucket{Sent: 1, RttN: 1, RttSumUs: 200, RttMinUs: 200, RttMaxUs: 200}},
		{NodeID: id, TS: 600, TaskID: 9, Bucket: &metric.ProbeBucket{Sent: 15, Lost: 5, Errors: 5, RttN: 5, RttSumUs: 500, RttMinUs: 100, RttMaxUs: 100}},
		{NodeID: id, TS: 900, TaskID: 9, Bucket: &metric.ProbeBucket{Sent: 15, Lost: 5, Errors: 5, RttN: 5, RttSumUs: 500, RttMinUs: 100, RttMaxUs: 100}},
	}
	if err != nil || !reflect.DeepEqual(rows, want) {
		t.Fatalf("rebucketed rows=%s err=%v, want %s", formatProbeRows(rows), err, formatProbeRows(want))
	}
	for _, step := range []int64{0, 30, 90} {
		if _, err := s.QueryProbes(t.Context(), id, 600, 1200, levels[0], step, 0); err == nil {
			t.Fatalf("invalid step %d accepted", step)
		}
	}
	for _, lv := range []Level{{Name: "bad", Bucket: 60}, {Name: "1m", Bucket: 0}} {
		if _, err := s.QueryProbes(t.Context(), id, 600, 1200, lv, 60, 0); err == nil {
			t.Fatalf("invalid level %+v accepted", lv)
		}
	}
}

func taskForTest() *heronv1.ProbeTask {
	return &heronv1.ProbeTask{Kind: heronv1.ProbeKind_PROBE_KIND_ICMP, Target: "127.0.0.1", IntervalS: 5, TimeoutMs: 100}
}

// dns_server 随任务落库并原样读回；更新整体替换该列，其他任务行互不影响。
func TestProbeTaskDNSServerRoundTripsThroughStore(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	task := &heronv1.ProbeTask{Kind: heronv1.ProbeKind_PROBE_KIND_DNS, Target: "example.com", IntervalS: 5, TimeoutMs: 1000, DnsServer: "[2001:4860:4860::8888]:53"}
	saved, _, err := s.SaveProbeTask(ctx, task, NodeSelector{AllNodes: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SaveProbeTask(ctx, taskForTest(), NodeSelector{AllNodes: true}); err != nil {
		t.Fatal(err)
	}
	_, records, err := s.LoadProbeTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].Task.GetDnsServer() != "[2001:4860:4860::8888]:53" || records[1].Task.GetDnsServer() != "" {
		t.Fatalf("loaded dns_server = %q, %q", records[0].Task.GetDnsServer(), records[1].Task.GetDnsServer())
	}
	saved.Task.DnsServer = "1.1.1.1:53"
	if _, _, err := s.SaveProbeTask(ctx, saved.Task, NodeSelector{AllNodes: true}); err != nil {
		t.Fatal(err)
	}
	_, records, err = s.LoadProbeTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if records[0].Task.GetDnsServer() != "1.1.1.1:53" {
		t.Fatalf("updated dns_server = %q", records[0].Task.GetDnsServer())
	}
}

func TestSaveProbeTaskAssignsAndBumpsVersion(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	a, _, _ := s.CreateNode(ctx, NewNode{Name: "a"}, hash(1))
	b, created, _ := s.CreateNode(ctx, NewNode{Name: "b"}, hash(2))
	base := created.Version
	saved, version, err := s.SaveProbeTask(ctx, taskForTest(), NodeSelector{AllNodes: false, NodeIDs: []int64{b, a}})
	if err != nil || saved.Task.Id != 1 || !reflect.DeepEqual(saved.NodeIDs, []int64{a, b}) || version != base+1 {
		t.Fatalf("save=%v version=%d err=%v", saved, version, err)
	}
	assertTasks(t, s, base+1, []ProbeTaskRecord{saved})
	saved.Task.Target = "example.com"
	saved.Task.IntervalS, saved.Task.TimeoutMs = 10, 500
	saved, version, err = s.SaveProbeTask(ctx, saved.Task, NodeSelector{AllNodes: false, NodeIDs: []int64{b}})
	if err != nil || version != base+2 {
		t.Fatalf("replace version=%d err=%v", version, err)
	}
	assertTasks(t, s, base+2, []ProbeTaskRecord{{Task: saved.Task, NodeIDs: []int64{b}}})
	absent := taskForTest()
	absent.Id = 99
	if _, _, err := s.SaveProbeTask(ctx, absent, NodeSelector{AllNodes: false, NodeIDs: nil}); !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "probe task 99 does not exist") {
		t.Fatalf("missing task error=%v", err)
	}
	if _, _, err := s.SaveProbeTask(ctx, taskForTest(), NodeSelector{AllNodes: false, NodeIDs: []int64{a, 42}}); !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "node 42 does not exist") {
		t.Fatalf("missing node error=%v", err)
	}
	assertTasks(t, s, base+2, []ProbeTaskRecord{saved})
	if version, err := s.DeleteProbeTask(ctx, 1); err != nil || version != base+3 {
		t.Fatalf("delete version=%d err=%v", version, err)
	}
	assertTasks(t, s, base+3, nil)
	if _, err := s.DeleteProbeTask(ctx, 1); !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "probe task 1 does not exist") {
		t.Fatalf("missing delete error=%v", err)
	}
	assertTasks(t, s, base+3, nil)
}

func TestProbeVersionUsesClockAndIncreases(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	var previous uint64
	for i := range 3 {
		if i == 2 {
			clk.Advance(time.Hour)
		}
		_, version, err := s.SaveProbeTask(t.Context(), taskForTest(), NodeSelector{AllNodes: false, NodeIDs: nil})
		if err != nil || version <= previous || version < uint64(clk.Now().Unix()) {
			t.Fatalf("save version=%d previous=%d now=%d err=%v", version, previous, clk.Now().Unix(), err)
		}
		previous = version
	}
	clk.Advance(time.Hour)
	version, err := s.DeleteProbeTask(t.Context(), 1)
	if err != nil || version <= previous || version < uint64(clk.Now().Unix()) {
		t.Fatalf("delete version=%d previous=%d now=%d err=%v", version, previous, clk.Now().Unix(), err)
	}
}

func assertTasks(t *testing.T, s *Store, wantVersion uint64, want []ProbeTaskRecord) {
	t.Helper()
	version, got, err := s.LoadProbeTasks(t.Context())
	if err != nil || version != wantVersion || len(got) != len(want) {
		t.Fatalf("tasks=%v version=%d err=%v, want %v/%d", got, version, err, want, wantVersion)
	}
	for i := range want {
		if !proto.Equal(got[i].Task, want[i].Task) || got[i].AllNodes != want[i].AllNodes || !reflect.DeepEqual(got[i].NodeIDs, want[i].NodeIDs) {
			t.Fatalf("task %d=%v/%v/%v, want %v/%v/%v", i, got[i].Task, got[i].AllNodes, got[i].NodeIDs, want[i].Task, want[i].AllNodes, want[i].NodeIDs)
		}
	}
	var assignments int
	if err := s.r.QueryRow("SELECT COUNT(*) FROM probe_task_node").Scan(&assignments); err != nil {
		t.Fatal(err)
	}
	wantAssignments := 0
	for _, rec := range want {
		if !rec.AllNodes {
			wantAssignments += len(rec.NodeIDs)
		}
	}
	if assignments != wantAssignments {
		t.Fatalf("assignment count=%d, want %d", assignments, wantAssignments)
	}
}

func TestSaveProbeTaskEnforcesPerNodeLimit(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	id, created, _ := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	base := created.Version
	for range probelimit.MaxTasksPerNode {
		if _, _, err := s.SaveProbeTask(ctx, taskForTest(), NodeSelector{AllNodes: false, NodeIDs: []int64{id}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.SaveProbeTask(ctx, taskForTest(), NodeSelector{AllNodes: false, NodeIDs: []int64{id}}); !errors.Is(err, ErrNodeLimit) || !strings.Contains(err.Error(), fmt.Sprintf("node %d would have 65 probe tasks (maximum 64)", id)) {
		t.Fatalf("65th task error=%v, want ErrNodeLimit with node", err)
	}
	version, tasks, err := s.LoadProbeTasks(ctx)
	if err != nil || version != base+64 || len(tasks) != 64 {
		t.Fatalf("limit rollback: version=%d tasks=%d err=%v", version, len(tasks), err)
	}

	changed := proto.Clone(tasks[0].Task).(*heronv1.ProbeTask)
	changed.Target = "example.com"
	before := append([]byte(nil), changed.GetConfigId()...)
	saved, version, err := s.SaveProbeTask(ctx, changed, NodeSelector{AllNodes: false, NodeIDs: []int64{id}})
	if err != nil || version != base+65 {
		t.Fatalf("editing full node: version=%d err=%v", version, err)
	}
	// 目标变了，配置身份必须换掉；输入里带的旧身份是只输出字段。
	if bytes.Equal(saved.Task.GetConfigId(), before) || len(saved.Task.GetConfigId()) != 16 {
		t.Fatalf("config id = %x, want a new 16-byte id", saved.Task.GetConfigId())
	}
	changed.ConfigId = append([]byte(nil), saved.Task.GetConfigId()...)
	if !proto.Equal(saved.Task, changed) {
		t.Fatalf("editing full node: task=%v, want %v", saved.Task, changed)
	}
	tasks[0].Task = changed
	assertTasks(t, s, base+65, tasks)
}

func TestDuplicateProbeAssignmentRollsBackReplacement(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	id, _, _ := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	saved, version, err := s.SaveProbeTask(ctx, taskForTest(), NodeSelector{AllNodes: false, NodeIDs: []int64{id}})
	if err != nil {
		t.Fatal(err)
	}
	changed := proto.Clone(saved.Task).(*heronv1.ProbeTask)
	changed.Target = "changed"
	if _, _, err := s.SaveProbeTask(ctx, changed, NodeSelector{AllNodes: false, NodeIDs: []int64{id, id}}); err == nil || !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		t.Fatalf("duplicate assignment error=%v", err)
	}
	assertTasks(t, s, version, []ProbeTaskRecord{saved})
}

func TestDeleteNodeRemovesProbeRowsAndAssignments(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	a, _, _ := s.CreateNode(ctx, NewNode{Name: "a"}, hash(1))
	b, _, _ := s.CreateNode(ctx, NewNode{Name: "b"}, hash(2))
	saved, version, err := s.SaveProbeTask(ctx, taskForTest(), NodeSelector{AllNodes: false, NodeIDs: []int64{a, b}})
	if err != nil {
		t.Fatal(err)
	}
	seedProbeLevels(t, s, []int64{a, b})
	if err := s.DeleteNode(ctx, a); err != nil {
		t.Fatal(err)
	}
	assertTasks(t, s, version, []ProbeTaskRecord{{Task: saved.Task, NodeIDs: []int64{b}}})
	// 分配随删除事务消失；时序行留给清理作业，作业跑完才少。
	counts := rowCounts(t, s)
	if counts["probe_task"] != 1 || counts["probe_task_node"] != 1 || counts["probe_1m"] != 2 {
		t.Fatalf("right after delete: counts=%v, want one task, one assignment and both nodes' history", counts)
	}
	if _, err := s.CleanupDeleted(ctx); err != nil {
		t.Fatal(err)
	}
	counts = rowCounts(t, s)
	for _, table := range append(append([]string{}, probeTables...), "probe_task", "probe_task_node") {
		if counts[table] != 1 {
			t.Fatalf("%s count=%d, want 1", table, counts[table])
		}
	}
	for _, table := range probeTables {
		var node int64
		if err := s.r.QueryRow("SELECT node_id FROM " + table).Scan(&node); err != nil || node != b {
			t.Fatalf("%s remaining node=%d err=%v, want %d", table, node, err, b)
		}
	}
	n, err := s.WriteMinuteBatch(ctx, metric.Batch{Probes: []metric.ProbeRow{probeRow(a, 600, 1, []uint32{5}, 0, 0)}})
	if err != nil || n != 1 {
		t.Fatalf("deleted node probe accepted: rejected=%d err=%v", n, err)
	}
	var count int
	if err := s.r.QueryRow("SELECT COUNT(*) FROM probe_1m WHERE node_id = ?", a).Scan(&count); err != nil || count != 0 {
		t.Fatalf("deleted history revived: count=%d err=%v", count, err)
	}
}

func seedProbeLevels(t *testing.T, s *Store, ids []int64) {
	t.Helper()
	if err := s.write(t.Context(), func(tx *sql.Tx) error {
		for _, table := range probeTables {
			for _, id := range ids {
				if _, err := tx.Exec("INSERT INTO "+table+" (node_id,ts,task_id) VALUES (?,600,1)", id); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// 删除任务的事务不碰历史，历史由清理作业删光；任务 id 不复用，新任务不会接上旧任务的历史。
func TestDeleteProbeTaskCleansHistoryAndNeverReusesID(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	id, _, _ := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	saved, _, err := s.SaveProbeTask(ctx, taskForTest(), NodeSelector{AllNodes: false, NodeIDs: []int64{id}})
	if err != nil {
		t.Fatal(err)
	}
	seedProbeLevels(t, s, []int64{id})
	if _, err := s.DeleteProbeTask(ctx, saved.Task.Id); err != nil {
		t.Fatal(err)
	}
	next, _, err := s.SaveProbeTask(ctx, taskForTest(), NodeSelector{AllNodes: false, NodeIDs: nil})
	if err != nil || next.Task.Id <= saved.Task.Id {
		t.Fatalf("task id reused: next=%v old=%v err=%v", next, saved, err)
	}
	counts := rowCounts(t, s)
	for _, table := range probeTables {
		if counts[table] != 1 {
			t.Fatalf("task deletion touched %s history inside its own transaction: %d rows, want 1", table, counts[table])
		}
	}
	if _, err := s.CleanupDeleted(ctx); err != nil {
		t.Fatal(err)
	}
	counts = rowCounts(t, s)
	for _, table := range append(slices.Clone(probeTables), "cleanup_job") {
		if counts[table] != 0 {
			t.Fatalf("after cleanup %s has %d rows, want 0", table, counts[table])
		}
	}
}

func TestMinuteBatchRollsBackBothFamilies(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	seedProbeTasks(t, s, 1)
	ctx := t.Context()
	id, _, _ := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	if err := s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec("CREATE TRIGGER reject_probe BEFORE INSERT ON probe_1m BEGIN SELECT RAISE(ABORT, 'probe rejected'); END")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_, err := s.WriteMinuteBatch(ctx, metric.Batch{Rows: minuteRows(id, 600, 660), Probes: []metric.ProbeRow{probeRow(id, 600, 1, []uint32{1}, 0, 0)}})
	if err == nil || !strings.Contains(err.Error(), "probe rejected") {
		t.Fatalf("batch error=%v", err)
	}
	counts := rowCounts(t, s)
	if counts["metric_1m"] != 0 || counts["probe_1m"] != 0 {
		t.Fatalf("partial batch committed: counts=%v", counts)
	}
}

func TestProbeBucketMergeMeanAndBatchEmpty(t *testing.T) {
	t.Parallel()
	b := &metric.ProbeBucket{}
	if mean, ok := b.RttMean(); mean != 0 || ok {
		t.Fatalf("empty mean=%d/%v", mean, ok)
	}
	b.Merge(probeRow(1, 0, 1, nil, 2, 1).Bucket)
	b.Merge(probeRow(1, 0, 1, []uint32{300, 100}, 0, 0).Bucket)
	b.Merge(probeRow(1, 0, 1, nil, 1, 0).Bucket)
	b.Merge(probeRow(1, 0, 1, []uint32{50, 500}, 0, 0).Bucket)
	want := metric.ProbeBucket{Sent: 8, Lost: 3, Errors: 1, RttSumUs: 950, RttN: 4, RttMinUs: 50, RttMaxUs: 500}
	if *b != want {
		t.Fatalf("merged=%+v want=%+v", *b, want)
	}
	if mean, ok := b.RttMean(); mean != 237 || !ok {
		t.Fatalf("mean=%d/%v, want 237/true", mean, ok)
	}
	for _, tc := range []struct {
		batch metric.Batch
		empty bool
	}{
		{metric.Batch{}, true},
		{metric.Batch{Rows: []metric.Row{{}}}, false},
		{metric.Batch{Probes: []metric.ProbeRow{{}}}, false},
		{metric.Batch{Rows: []metric.Row{{}}, Probes: []metric.ProbeRow{{}}}, false},
	} {
		if got := tc.batch.Empty(); got != tc.empty {
			t.Fatalf("batch %+v empty=%v, want %v", tc.batch, got, tc.empty)
		}
	}
}

func formatProbeRows(rows []metric.ProbeRow) string {
	var out strings.Builder
	out.WriteByte('[')
	for i, row := range rows {
		if i > 0 {
			out.WriteString(", ")
		}
		fmt.Fprintf(&out, "{NodeID:%d TS:%d TaskID:%d Bucket:", row.NodeID, row.TS, row.TaskID)
		if row.Bucket == nil {
			out.WriteString("<nil>")
		} else {
			fmt.Fprintf(&out, "%+v", *row.Bucket)
		}
		out.WriteByte('}')
	}
	out.WriteByte(']')
	return out.String()
}

func TestProbeBucketIgnoresMissingOutcome(t *testing.T) {
	t.Parallel()
	for _, input := range []*heronv1.ProbeResult{nil, {TaskId: 7}} {
		b := probeRow(1, 0, 7, []uint32{100}, 1, 1).Bucket
		want := *b
		b.Add(input)
		if *b != want || b.RttN != b.Sent-b.Lost-b.Errors {
			t.Fatalf("missing outcome changed bucket: got=%+v want=%+v", *b, want)
		}
	}
}
