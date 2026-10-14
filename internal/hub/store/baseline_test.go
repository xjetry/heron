package store

import (
	"database/sql"
	"errors"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
)

// validRelative 是一条合法的相对判定规则（自适应基线），矩阵用例在它上面逐项改坏。
func validRelative() AlertRule {
	return AlertRule{Name: "rtt", Kind: KindProbe, Enabled: true, AllNodes: true, TaskID: 1, Metric: MetricRttMs, ForMinutes: 5,
		RttMode: RttRelative, BaselineMode: BaselineAdaptive, BaselineWindowS: 3600, BaselineMinSamples: 3,
		UpperDeviationPct: 100, LowerDeviationPct: 50, CooldownS: 600}
}

func validFixed() AlertRule {
	r := validRelative()
	r.BaselineMode, r.BaselineWindowS, r.BaselineMinSamples, r.FixedBaselineMs = BaselineFixed, 0, 0, 40
	return r
}

// 零值矩阵（规则见 alert.CheckRule）的每一格：非 rtt 规则与固定阈值下八个字段全部为零值，相对判定下必填项与两种基线来源各自的字段，
// 以及每个区间的两端。合法的一侧与非法的一侧都钉住，矩阵放宽或收紧都会让其中一侧红。
func TestCheckKindFieldsRttMatrix(t *testing.T) {
	t.Parallel()
	threshold := AlertRule{Name: "t", Kind: KindProbe, TaskID: 1, Metric: MetricRttMs, RttMode: RttThreshold, Threshold: 80, ForMinutes: 3}
	loss := AlertRule{Name: "l", Kind: KindProbe, TaskID: 1, Metric: MetricLossPct, Threshold: 20, ForMinutes: 3}
	offline := AlertRule{Name: "o", Kind: KindOffline}
	invalid := []struct {
		name  string
		rule  AlertRule
		field string
	}{
		{"loss_rtt_mode", func() AlertRule { r := loss; r.RttMode = RttThreshold; return r }(), "rtt_mode"},
		{"loss_cooldown", func() AlertRule { r := loss; r.CooldownS = 60; return r }(), "cooldown_s"},
		{"offline_baseline_mode", func() AlertRule { r := offline; r.BaselineMode = BaselineFixed; return r }(), "baseline_mode"},
		{"offline_fixed", func() AlertRule { r := offline; r.FixedBaselineMs = 1; return r }(), "fixed_baseline_ms"},
		{"rtt_mode_empty", func() AlertRule { r := threshold; r.RttMode = ""; return r }(), "rtt_mode"},
		{"rtt_mode_other", func() AlertRule { r := threshold; r.RttMode = "other"; return r }(), "rtt_mode"},
		{"threshold_upper", func() AlertRule { r := threshold; r.UpperDeviationPct = 10; return r }(), "upper_deviation_pct"},
		{"threshold_lower", func() AlertRule { r := threshold; r.LowerDeviationPct = 10; return r }(), "lower_deviation_pct"},
		{"threshold_window", func() AlertRule { r := threshold; r.BaselineWindowS = 3600; return r }(), "baseline_window_s"},
		{"threshold_min", func() AlertRule { r := threshold; r.BaselineMinSamples = 1; return r }(), "baseline_min_samples"},
		{"threshold_baseline_mode", func() AlertRule { r := threshold; r.BaselineMode = BaselineAdaptive; return r }(), "baseline_mode"},
		{"relative_threshold", func() AlertRule { r := validRelative(); r.Threshold = 80; return r }(), "threshold"},
		{"upper_zero", func() AlertRule { r := validRelative(); r.UpperDeviationPct = 0; return r }(), "upper_deviation_pct"},
		{"upper_high", func() AlertRule { r := validRelative(); r.UpperDeviationPct = 1000.001; return r }(), "upper_deviation_pct"},
		{"upper_nan", func() AlertRule { r := validRelative(); r.UpperDeviationPct = math.NaN(); return r }(), "upper_deviation_pct"},
		{"lower_zero", func() AlertRule { r := validRelative(); r.LowerDeviationPct = 0; return r }(), "lower_deviation_pct"},
		{"lower_over_hundred", func() AlertRule { r := validRelative(); r.LowerDeviationPct = 100.001; return r }(), "lower_deviation_pct"},
		{"lower_nan", func() AlertRule { r := validRelative(); r.LowerDeviationPct = math.NaN(); return r }(), "lower_deviation_pct"},
		{"cooldown_low", func() AlertRule { r := validRelative(); r.CooldownS = 59; return r }(), "cooldown_s"},
		{"cooldown_high", func() AlertRule { r := validRelative(); r.CooldownS = 604801; return r }(), "cooldown_s"},
		{"baseline_mode_missing", func() AlertRule { r := validRelative(); r.BaselineMode = ""; return r }(), "baseline_mode"},
		{"window_below_judgment", func() AlertRule { r := validRelative(); r.BaselineWindowS = r.ForMinutes*60 - 1; return r }(), "baseline_window_s"},
		{"window_over_30d", func() AlertRule { r := validRelative(); r.BaselineWindowS = 30*24*3600 + 1; return r }(), "baseline_window_s"},
		{"min_samples_zero", func() AlertRule { r := validRelative(); r.BaselineMinSamples = 0; return r }(), "baseline_min_samples"},
		{"adaptive_fixed", func() AlertRule { r := validRelative(); r.FixedBaselineMs = 40; return r }(), "fixed_baseline_ms"},
		{"fixed_zero", func() AlertRule { r := validFixed(); r.FixedBaselineMs = 0; return r }(), "fixed_baseline_ms"},
		{"fixed_high", func() AlertRule { r := validFixed(); r.FixedBaselineMs = 60000.5; return r }(), "fixed_baseline_ms"},
		{"fixed_window", func() AlertRule { r := validFixed(); r.BaselineWindowS = 3600; return r }(), "baseline_window_s"},
		{"fixed_min", func() AlertRule { r := validFixed(); r.BaselineMinSamples = 1; return r }(), "baseline_min_samples"},
	}
	for _, c := range invalid {
		var kf KindFieldError
		if err := CheckKindFields(c.rule); err == nil || !errors.As(err, &kf) || kf.Field != c.field {
			t.Errorf("%s: CheckKindFields = %v, want a KindFieldError on %s", c.name, err, c.field)
		}
	}
	valid := []struct {
		name string
		rule AlertRule
	}{
		{"loss", loss}, {"offline", offline}, {"threshold", threshold}, {"adaptive", validRelative()}, {"fixed", validFixed()},
		{"upper_max", func() AlertRule { r := validRelative(); r.UpperDeviationPct = 1000; return r }()},
		{"lower_near_hundred", func() AlertRule { r := validRelative(); r.LowerDeviationPct = 99.9; return r }()},
		{"lower_hundred", func() AlertRule { r := validRelative(); r.LowerDeviationPct = 100; return r }()},
		{"cooldown_min", func() AlertRule { r := validRelative(); r.CooldownS = 60; return r }()},
		{"cooldown_max", func() AlertRule { r := validRelative(); r.CooldownS = 604800; return r }()},
		{"window_equals_judgment", func() AlertRule { r := validRelative(); r.BaselineWindowS = r.ForMinutes * 60; return r }()},
		{"window_30d", func() AlertRule { r := validRelative(); r.BaselineWindowS = 30 * 24 * 3600; return r }()},
		{"min_samples_one", func() AlertRule { r := validRelative(); r.BaselineMinSamples = 1; return r }()},
		{"fixed_max", func() AlertRule { r := validFixed(); r.FixedBaselineMs = 60000; return r }()},
	}
	for _, c := range valid {
		if err := CheckKindFields(c.rule); err != nil {
			t.Errorf("%s: CheckKindFields = %v, want nil", c.name, err)
		}
	}
}

// 两种基线来源的规则经存储往返不变；用不到的列落 NULL（直接读库核对），存储层对非法组合报错而不改写。
func TestSaveAlertRuleRoundTripsRelativeFields(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	task, _, err := s.SaveProbeTask(t.Context(), taskForTest(), NodeSelector{AllNodes: true})
	if err != nil {
		t.Fatal(err)
	}
	adaptive, fixed := validRelative(), validFixed()
	adaptive.TaskID, fixed.TaskID = task.Task.Id, task.Task.Id
	a, b := saveRule(t, s, adaptive), saveRule(t, s, fixed)
	got, err := s.ListAlertRules(t.Context())
	if err != nil || len(got) != 2 {
		t.Fatalf("rules=%+v err=%v", got, err)
	}
	for i, want := range []AlertRule{a, b} {
		if got[i].RttMode != want.RttMode || got[i].BaselineMode != want.BaselineMode || got[i].BaselineWindowS != want.BaselineWindowS ||
			got[i].BaselineMinSamples != want.BaselineMinSamples || got[i].UpperDeviationPct != want.UpperDeviationPct ||
			got[i].LowerDeviationPct != want.LowerDeviationPct || got[i].CooldownS != want.CooldownS || got[i].FixedBaselineMs != want.FixedBaselineMs {
			t.Errorf("rule %d listed %+v, saved %+v", i, got[i], want)
		}
	}
	var nulls string
	if err := s.r.QueryRow(`SELECT (fixed_baseline_ms IS NULL) FROM alert_rule WHERE id = ?`, a.ID).Scan(&nulls); err != nil || nulls != "1" {
		t.Errorf("adaptive rule: fixed_baseline_ms NULL = %q (%v); want 1", nulls, err)
	}
	if err := s.r.QueryRow(`SELECT (baseline_window_s IS NULL) || (baseline_min_samples IS NULL) FROM alert_rule WHERE id = ?`, b.ID).Scan(&nulls); err != nil || nulls != "11" {
		t.Errorf("fixed rule: window and min samples NULL = %q (%v); want 11", nulls, err)
	}
	bad := adaptive
	bad.BaselineMode = ""
	if _, err := s.SaveAlertRule(t.Context(), bad); err == nil || !strings.Contains(err.Error(), "baseline_mode") {
		t.Fatalf("saving a relative rule without baseline_mode: %v", err)
	}
}

// baselineFixture 建两个节点、一个任务与一条覆盖全部节点的自适应基线规则。
func baselineFixture(t *testing.T) (*Store, AlertRule, []int64) {
	t.Helper()
	s, _ := open(t)
	var nodes []int64
	for i := byte(1); i <= 2; i++ {
		id, _, err := s.CreateNode(t.Context(), NewNode{Name: "n"}, hash(i))
		if err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, id)
	}
	task, _, err := s.SaveProbeTask(t.Context(), taskForTest(), NodeSelector{AllNodes: true})
	if err != nil {
		t.Fatal(err)
	}
	r := validRelative()
	r.TaskID = task.Task.Id
	return s, saveRule(t, s, r), nodes
}

func baselineRows(t *testing.T, s *Store, ruleID int64) []int64 {
	t.Helper()
	ids, err := scanIDs(s.r.Query("SELECT node_id FROM alert_baseline WHERE rule_id = ? ORDER BY node_id", ruleID))
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func fingerprintOf(t *testing.T, s *Store, taskID uint64) string {
	t.Helper()
	b, err := s.ReadAlertBaselines(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	fp, ok := b.Fingerprint(taskID)
	if !ok {
		t.Fatalf("task %d has no fingerprint", taskID)
	}
	return fp
}

// 删除规则与它的基线行同事务提交；只删这条规则的行，别的规则的行不动。
func TestDeleteAlertRuleRemovesBaselines(t *testing.T) {
	t.Parallel()
	s, r, nodes := baselineFixture(t)
	other := r
	other.ID, other.Name = 0, "other"
	other = saveRule(t, s, other)
	fp := fingerprintOf(t, s, r.TaskID)
	for _, rule := range []int64{r.ID, other.ID} {
		if ok, err := s.SaveAlertBaseline(t.Context(), r.TaskID, AlertBaseline{RuleID: rule, NodeID: nodes[0], BaselineUs: 1, Buckets: 1, ComputedAt: s.clk.Now(), TaskFingerprint: fp}); err != nil || !ok {
			t.Fatalf("seed baseline for rule %d: %v %v", rule, ok, err)
		}
	}
	if err := s.DeleteAlertRule(t.Context(), r.ID); err != nil {
		t.Fatal(err)
	}
	if rows := baselineRows(t, s, r.ID); len(rows) != 0 {
		t.Fatalf("baseline rows of the deleted rule survived: %v", rows)
	}
	if rows := baselineRows(t, s, other.ID); !slices.Equal(rows, []int64{nodes[0]}) {
		t.Fatalf("baseline rows of the other rule = %v, want [%d]", rows, nodes[0])
	}
}

// 写事务里的核对：规则、节点、任务指纹任一已变，写被丢弃，不留孤儿行，也不把旧目标的基线挂到新目标上；
// 规则身份变化或不再用自适应基线时，SaveAlertRule 同事务删掉它的行。
func TestSaveAlertBaselineGuards(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, s *Store, r *AlertRule, nodes []int64, fp *string)
		write  bool
	}{
		{"current", func(*testing.T, *Store, *AlertRule, []int64, *string) {}, true},
		{"rule_deleted", func(t *testing.T, s *Store, r *AlertRule, _ []int64, _ *string) {
			if err := s.DeleteAlertRule(t.Context(), r.ID); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"rule_now_fixed", func(t *testing.T, s *Store, r *AlertRule, _ []int64, _ *string) {
			r.BaselineMode, r.BaselineWindowS, r.BaselineMinSamples, r.FixedBaselineMs = BaselineFixed, 0, 0, 40
			saveRule(t, s, *r)
		}, false},
		{"node_deleted", func(t *testing.T, s *Store, _ *AlertRule, nodes []int64, _ *string) {
			if err := s.DeleteNode(t.Context(), nodes[0]); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"stale_fingerprint", func(_ *testing.T, _ *Store, _ *AlertRule, _ []int64, fp *string) {
			*fp = TaskFingerprint(1, "elsewhere")
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, r, nodes := baselineFixture(t)
			fp := fingerprintOf(t, s, r.TaskID)
			tc.mutate(t, s, &r, nodes, &fp)
			ok, err := s.SaveAlertBaseline(t.Context(), r.TaskID, AlertBaseline{RuleID: r.ID, NodeID: nodes[0], BaselineUs: 5, Buckets: 3, ComputedAt: s.clk.Now(), TaskFingerprint: fp})
			if err != nil || ok != tc.write {
				t.Fatalf("SaveAlertBaseline = %v, %v; want %v", ok, err, tc.write)
			}
			if n := len(baselineRows(t, s, r.ID)); (n == 1) != tc.write {
				t.Fatalf("baseline rows after the write: %d, want written=%v", n, tc.write)
			}
		})
	}
	t.Run("identity_change_deletes", func(t *testing.T) {
		t.Parallel()
		s, r, nodes := baselineFixture(t)
		fp := fingerprintOf(t, s, r.TaskID)
		if ok, err := s.SaveAlertBaseline(t.Context(), r.TaskID, AlertBaseline{RuleID: r.ID, NodeID: nodes[0], Buckets: 1, ComputedAt: s.clk.Now(), TaskFingerprint: fp}); err != nil || !ok {
			t.Fatal(ok, err)
		}
		// 基线窗口不是身份：行沿用。
		r.BaselineWindowS = 7200
		saveRule(t, s, r)
		if n := len(baselineRows(t, s, r.ID)); n != 1 {
			t.Fatalf("baseline rows after changing the window: %d, want 1", n)
		}
		r.RttMode, r.BaselineMode, r.BaselineWindowS, r.BaselineMinSamples, r.UpperDeviationPct, r.LowerDeviationPct, r.CooldownS, r.Threshold = RttThreshold, "", 0, 0, 0, 0, 0, 80
		saveRule(t, s, r)
		if n := len(baselineRows(t, s, r.ID)); n != 0 {
			t.Fatalf("baseline rows after switching to a fixed threshold: %d, want 0", n)
		}
	})
}

// fired_at 是一段历史：从非 firing 写成 firing 记当下，其余写入（含恢复、再次 pending）沿用；没有上一行时为 0。
func TestFiredAtFollowsFiringTransitions(t *testing.T) {
	t.Parallel()
	s, r, nodes := baselineFixture(t)
	at := func(sec int64) time.Time { return time.Unix(sec, 0).UTC() }
	for _, step := range []struct {
		state AlertState
		since int64
		want  int64
	}{
		{StatePending, 100, 0},
		{StateFiring, 200, 200},
		{StateFiring, 250, 200},
		{StateOK, 300, 200},
		{StatePending, 400, 200},
		{StateFiring, 500, 500},
	} {
		if err := s.write(t.Context(), func(tx *sql.Tx) error {
			return setAlertState(tx, r.ID, nodes[0], step.state, at(step.since), "", time.Time{}, false)
		}); err != nil {
			t.Fatal(err)
		}
		states, err := s.ListAlertStates(t.Context())
		if err != nil || len(states) != 1 {
			t.Fatalf("states=%+v err=%v", states, err)
		}
		want := time.Time{}
		if step.want != 0 {
			want = at(step.want)
		}
		if !states[0].FiredAt.Equal(want) {
			t.Fatalf("after writing %s at %d: fired_at=%v want %v", step.state, step.since, states[0].FiredAt, want)
		}
	}
}

// probe5m 直接写 5 分钟桶：基线只读这一级，测试不必经上卷。
func probe5m(t *testing.T, s *Store, node int64, task uint64, ts int64, sent, lost, errs, rttSumUs int64) {
	t.Helper()
	if err := s.write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.Exec("INSERT INTO probe_5m (node_id, ts, task_id, sent, lost, errors, rtt_sum_us) VALUES (?, ?, ?, ?, ?, ?, ?)", node, ts, int64(task), sent, lost, errs, rttSumUs)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// 桶均值 = rtt_sum_us / (sent − lost − errors)：errors 不计入分母；没有 rtt 样本的桶不出现；只取这一个任务在这个节点上
// 整段落在 [from, to) 内的桶，按时间升序。
func TestProbeBucketMeans(t *testing.T) {
	t.Parallel()
	s, r, nodes := baselineFixture(t)
	other, _, err := s.SaveProbeTask(t.Context(), &heronv1.ProbeTask{Kind: heronv1.ProbeKind_PROBE_KIND_ICMP, Target: "10.0.0.1", IntervalS: 5, TimeoutMs: 100}, NodeSelector{AllNodes: true})
	if err != nil {
		t.Fatal(err)
	}
	const from, to = 3000, 6000
	probe5m(t, s, nodes[0], r.TaskID, from-300, 10, 0, 0, 10*99_000)    // 早于窗口
	probe5m(t, s, nodes[0], r.TaskID, from, 10, 2, 3, 5*40_000)         // RttN = 5
	probe5m(t, s, nodes[0], r.TaskID, from+300, 10, 10, 0, 0)           // 全丢，没有 rtt 样本
	probe5m(t, s, nodes[0], r.TaskID, to-600, 4, 0, 0, 4*20_000)        // 窗口内
	probe5m(t, s, nodes[0], r.TaskID, to-300, 2, 0, 0, 2*30_000)        // 恰好在 to 结束
	probe5m(t, s, nodes[0], r.TaskID, to, 2, 0, 0, 2*99_000)            // 晚于窗口
	probe5m(t, s, nodes[0], other.Task.Id, from+600, 2, 0, 0, 2*99_000) // 别的任务
	probe5m(t, s, nodes[1], r.TaskID, from+600, 2, 0, 0, 2*99_000)      // 别的节点
	means, err := s.Evaluation().ProbeBucketMeans(t.Context(), nodes[0], r.TaskID, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if want := []float64{40_000, 20_000, 30_000}; !slices.Equal(means, want) {
		t.Fatalf("bucket means = %v, want %v", means, want)
	}
	// 删除节点只删配置层，它的桶留到清理作业完成；这段时间里桶均值读不到它们，与其余历史读一致（family.live）。
	if err := s.DeleteNode(t.Context(), nodes[0]); err != nil {
		t.Fatal(err)
	}
	if means, err = s.Evaluation().ProbeBucketMeans(t.Context(), nodes[0], r.TaskID, from, to); err != nil || len(means) != 0 {
		t.Fatalf("bucket means of a deleted node = %v, %v; want none", means, err)
	}
	if _, err := s.Evaluation().ProbeBucketMeans(t.Context(), nodes[0], r.TaskID, 0, MaxBaselineWindowS+1); err == nil {
		t.Fatal("a window longer than MaxBaselineWindowS was accepted")
	}
}

// 桶均值的读取计划走 (task_id, node_id, ts) 索引前缀：主键按节点定位会读出该节点全部任务的行。
func TestProbeBucketMeansUsesByTaskIndex(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	rows, err := s.r.Query("EXPLAIN QUERY PLAN "+probeBucketMeansSQL, 1, 1, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	joined := strings.Join(plan, "; ")
	if !strings.Contains(joined, "SEARCH probe_5m USING INDEX probe_5m_by_task (task_id=? AND node_id=? AND ts>? AND ts<?)") {
		t.Fatalf("plan = %q, want a SEARCH on probe_5m_by_task with task_id, node_id and a ts range", joined)
	}
}
