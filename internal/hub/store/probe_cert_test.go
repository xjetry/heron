package store

import (
	"context"
	"errors"
	"testing"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
)

// 建一个 https:// 的 HTTP 任务并把它分给节点，返回任务 id。
func certTask(t *testing.T, ctx context.Context, s *Store, target string, kind heronv1.ProbeKind) uint64 {
	t.Helper()
	rec, _, err := s.SaveProbeTask(ctx, &heronv1.ProbeTask{Kind: kind, Target: target, IntervalS: 60, TimeoutMs: 1000}, NodeSelector{AllNodes: true})
	if err != nil {
		t.Fatal(err)
	}
	return rec.Task.GetId()
}

func certTaskIdentity(t *testing.T, s *Store, task uint64) []byte {
	t.Helper()
	view, err := s.ListProbeCertificates(t.Context(), task)
	if err != nil {
		t.Fatal(err)
	}
	if !view.Found {
		t.Fatal("certificate task missing")
	}
	return view.Task.Task.ConfigId
}

func TestUpsertProbeCert(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := context.Background()
	node, _, err := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	task := certTask(t, ctx, s, "https://a.example", heronv1.ProbeKind_PROBE_KIND_HTTP)

	changed, err := s.UpsertProbeCert(ctx, node, task, 1893456000, 1000, certTaskIdentity(t, s, task))
	if err != nil || !changed {
		t.Fatalf("first upsert changed=%v err=%v, want changed", changed, err)
	}
	// 同值覆盖不是变化：重复上报同一观测不触发重新评估。
	changed, err = s.UpsertProbeCert(ctx, node, task, 1893456000, 2000, certTaskIdentity(t, s, task))
	if err != nil || changed {
		t.Fatalf("same-value upsert changed=%v err=%v, want unchanged", changed, err)
	}
	changed, err = s.UpsertProbeCert(ctx, node, task, 1895000000, 3000, certTaskIdentity(t, s, task))
	if err != nil || !changed {
		t.Fatalf("new-value upsert changed=%v err=%v, want changed", changed, err)
	}
	certs, err := s.ProbeCertsByTask(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	if len(certs) != 1 || certs[node] != 1895000000 {
		t.Fatalf("certs = %v, want {%d: 1895000000}", certs, node)
	}
}

// 删任务同事务删掉它的证书观测；观测不是历史，任务没了"最新值"也就没有载体。
func TestDeleteProbeTaskRemovesProbeCert(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := context.Background()
	node, _, err := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	task := certTask(t, ctx, s, "https://a.example", heronv1.ProbeKind_PROBE_KIND_HTTP)
	if _, err := s.UpsertProbeCert(ctx, node, task, 1893456000, 1000, certTaskIdentity(t, s, task)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteProbeTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	certs, err := s.ProbeCertsByTask(ctx, task)
	if err != nil || len(certs) != 0 {
		t.Fatalf("certs after task deletion = %v err=%v, want empty", certs, err)
	}
}

// 证书观测随节点消失（nodeDependentTables），删除节点不留下孤儿观测。
func TestDeleteNodeRemovesProbeCert(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := context.Background()
	node, _, err := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	task := certTask(t, ctx, s, "https://a.example", heronv1.ProbeKind_PROBE_KIND_HTTP)
	if _, err := s.UpsertProbeCert(ctx, node, task, 1893456000, 1000, certTaskIdentity(t, s, task)); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteNode(ctx, node); err != nil {
		t.Fatal(err)
	}
	certs, err := s.ProbeCertsByTask(ctx, task)
	if err != nil || len(certs) != 0 {
		t.Fatalf("certs after node deletion = %v err=%v, want empty", certs, err)
	}
}

func TestCheckKindFieldsCertExpiry(t *testing.T) {
	t.Parallel()
	base := AlertRule{Kind: KindCertExpiry, TaskID: 7, DaysBefore: 30}
	if err := CheckKindFields(base); err != nil {
		t.Fatalf("valid cert_expiry rule rejected: %v", err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*AlertRule)
		want   string
	}{
		{"metric", func(r *AlertRule) { r.Metric = MetricRttMs }, "metric must be unspecified unless kind is probe"},
		{"threshold", func(r *AlertRule) { r.Threshold = 1 }, "threshold must be 0 unless kind is probe, resource or traffic"},
		{"for_minutes", func(r *AlertRule) { r.ForMinutes = 5 }, "for_minutes must be 0 unless kind is probe or resource"},
		{"resource_metric", func(r *AlertRule) { r.ResourceMetric = MetricCpuPct }, "resource_metric must be unspecified unless kind is resource"},
		{"recovery_threshold", func(r *AlertRule) { r.RecoveryThreshold = 1 }, "recovery_threshold must be 0 unless kind is resource"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := base
			tc.mutate(&r)
			err := CheckKindFields(r)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	// 探测与资源规则不能带 days_before；证书到期与到期可以。
	if err := CheckKindFields(AlertRule{Kind: KindProbe, TaskID: 7, DaysBefore: 30, Metric: MetricRttMs, Threshold: 1, ForMinutes: 1}); err == nil {
		t.Fatal("probe rule with days_before accepted")
	}
}

func TestSaveAlertRuleCertExpiry(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := context.Background()
	node, _, err := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	https := certTask(t, ctx, s, "https://a.example", heronv1.ProbeKind_PROBE_KIND_HTTP)
	rule := AlertRule{Kind: KindCertExpiry, Name: "cert", TaskID: https, DaysBefore: 30, NodeIDs: []int64{node}}
	saved, err := s.SaveAlertRule(ctx, rule)
	if err != nil {
		t.Fatalf("https cert rule rejected: %v", err)
	}
	if saved.TaskID != https || saved.DaysBefore != 30 {
		t.Fatalf("saved = %+v", saved)
	}

	// 非 https:// 任务（http:// 与非 HTTP 种类）与缺失任务都被拒。
	http := certTask(t, ctx, s, "http://a.example", heronv1.ProbeKind_PROBE_KIND_HTTP)
	tcp := certTask(t, ctx, s, "a.example:443", heronv1.ProbeKind_PROBE_KIND_TCP)
	for name, taskID := range map[string]uint64{"http": http, "tcp": tcp, "missing": 9999} {
		r := rule
		r.ID, r.TaskID = 0, taskID
		err := error(nil)
		if _, err = s.SaveAlertRule(ctx, r); err == nil {
			t.Errorf("%s task accepted for cert_expiry rule", name)
		} else {
			var nf NotFoundError
			var kf KindFieldError
			switch name {
			case "missing":
				if !errors.As(err, &nf) {
					t.Errorf("missing task err = %v, want NotFoundError", err)
				}
			default:
				if !errors.As(err, &kf) {
					t.Errorf("%s task err = %v, want KindFieldError", name, err)
				}
			}
		}
	}
	// 带探测字段被拒（种类字段表）。
	r := rule
	r.ID, r.Threshold = 0, 1
	if _, err := s.SaveAlertRule(ctx, r); err == nil {
		t.Error("cert_expiry rule with threshold accepted")
	}

	// 被证书到期规则引用的任务不能改成非 https://；仍是 https:// 的改动照常放行。
	task := &heronv1.ProbeTask{Id: https, Kind: heronv1.ProbeKind_PROBE_KIND_HTTP, Target: "http://a.example", IntervalS: 60, TimeoutMs: 1000}
	if _, _, err := s.SaveProbeTask(ctx, task, NodeSelector{AllNodes: true}); err == nil {
		t.Error("referenced task retargeted to http://")
	}
	task.Target = "https://b.example"
	if _, _, err := s.SaveProbeTask(ctx, task, NodeSelector{AllNodes: true}); err != nil {
		t.Errorf("referenced task retargeted to another https:// rejected: %v", err)
	}
}
