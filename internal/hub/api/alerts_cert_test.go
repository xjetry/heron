package api

import (
	"testing"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
)

// 证书到期规则经协议保存并回显任务与提前天数；任务必须是 https:// 的 HTTP 任务，由 store 在事务内裁决，
// 错误以请求路径写明字段与约束；探测专用与资源专用字段照常被种类字段表拒绝。
func TestSaveAlertRuleCertExpiryKind(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	httpTask := func(target string) uint64 {
		t.Helper()
		task := probeTask(target)
		task.Kind = heronv1.ProbeKind_PROBE_KIND_HTTP
		resp, err := h.admin.SaveProbeTask(t.Context(), connect.NewRequest(&heronv1.SaveProbeTaskRequest{Task: task, AllNodes: true}))
		if err != nil {
			t.Fatal(err)
		}
		return resp.Msg.GetTask().GetTask().GetId()
	}
	https := httpTask("https://example.com/")
	plainHTTP := httpTask("http://example.com/")
	icmp := h.saveAllNodesTask(t, "192.0.2.1").GetTask().GetId()

	certRule := func() *heronv1.AlertRule {
		return &heronv1.AlertRule{Name: "证书到期", Kind: heronv1.AlertKind_ALERT_KIND_CERT_EXPIRY, Enabled: true, AllNodes: true, TaskId: https, DaysBefore: 7}
	}
	saved := saveRule(t, h, certRule())
	if saved.GetKind() != heronv1.AlertKind_ALERT_KIND_CERT_EXPIRY || saved.GetTaskId() != https || saved.GetDaysBefore() != 7 {
		t.Fatalf("saved %v", saved)
	}
	list, err := h.admin.ListAlertRules(t.Context(), connect.NewRequest(&heronv1.ListAlertRulesRequest{}))
	if err != nil || len(list.Msg.GetRules()) != 1 || list.Msg.GetRules()[0].GetTaskId() != https {
		t.Fatalf("listed %v %v", list, err)
	}

	// 非 HTTPS 任务：http:// 的 HTTP 任务与 ICMP 任务同样被拒，指向协议词 task_id。
	for _, id := range []uint64{plainHTTP, icmp} {
		r := certRule()
		r.TaskId = id
		_, err := h.admin.SaveAlertRule(t.Context(), connect.NewRequest(&heronv1.SaveAlertRuleRequest{Rule: r}))
		want := "rule.task_id must reference an https:// HTTP probe task for cert_expiry rules"
		if codeOf(err) != connect.CodeInvalidArgument || err.Error() != "invalid_argument: "+want {
			t.Errorf("task %d: err = %v, want %s", id, err, want)
		}
	}

	// 提前天数越界、任务缺失、带探测或资源专用字段，都被拒绝，什么也不保存。
	for _, c := range []struct {
		change func(*heronv1.AlertRule)
		want   string
	}{
		{func(r *heronv1.AlertRule) { r.DaysBefore = 0 }, "rule.days_before must be between 1 and 365"},
		{func(r *heronv1.AlertRule) { r.DaysBefore = 366 }, "rule.days_before must be between 1 and 365"},
		{func(r *heronv1.AlertRule) { r.TaskId = 0 }, "rule.task_id must not be 0"},
		{func(r *heronv1.AlertRule) { r.Metric = heronv1.ProbeMetric_PROBE_METRIC_LOSS_PCT }, "rule.metric must be unspecified unless kind is probe"},
		{func(r *heronv1.AlertRule) { r.Threshold = 1 }, "rule.threshold must be 0 unless kind is probe, resource or traffic"},
		{func(r *heronv1.AlertRule) { r.ForMinutes = 1 }, "rule.for_minutes must be 0 unless kind is probe or resource"},
		{func(r *heronv1.AlertRule) { r.ResourceMetric = heronv1.ResourceMetric_RESOURCE_METRIC_MEMORY_USED_PCT }, "rule.resource_metric must be unspecified unless kind is resource"},
		{func(r *heronv1.AlertRule) { r.RecoveryThreshold = 1 }, "rule.recovery_threshold must be 0 unless kind is resource"},
	} {
		r := certRule()
		c.change(r)
		_, err := h.admin.SaveAlertRule(t.Context(), connect.NewRequest(&heronv1.SaveAlertRuleRequest{Rule: r}))
		if codeOf(err) != connect.CodeInvalidArgument || err.Error() != "invalid_argument: "+c.want {
			t.Errorf("%v: err = %v, want %s", r, err, c.want)
		}
	}
	rules, err := h.admin.ListAlertRules(t.Context(), connect.NewRequest(&heronv1.ListAlertRulesRequest{}))
	if err != nil || len(rules.Msg.GetRules()) != 1 {
		t.Fatalf("rejected rules saved something: %v %v", rules, err)
	}
}

// scheme 写成大写的 https 目标同样是 https 任务（hub 与 agent 都按解析出的 scheme 判断，probelimit.IsHTTPSTarget）：
// 可以挂证书到期规则；被规则引用之后照常可以编辑，不会被当成"改成了非 https"拒绝。
func TestCertExpiryRuleAcceptsUppercaseHTTPSScheme(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	task := probeTask("HTTPS://example.com/upper")
	task.Kind = heronv1.ProbeKind_PROBE_KIND_HTTP
	resp, err := h.admin.SaveProbeTask(t.Context(), connect.NewRequest(&heronv1.SaveProbeTaskRequest{Task: task, AllNodes: true}))
	if err != nil {
		t.Fatal(err)
	}
	id := resp.Msg.GetTask().GetTask().GetId()
	saveRule(t, h, &heronv1.AlertRule{Name: "证书到期", Kind: heronv1.AlertKind_ALERT_KIND_CERT_EXPIRY, Enabled: true, AllNodes: true, TaskId: id, DaysBefore: 7})
	edited := probeTask("HTTPS://example.com/upper")
	edited.Kind, edited.Id, edited.IntervalS = heronv1.ProbeKind_PROBE_KIND_HTTP, id, 120
	if _, err := h.admin.SaveProbeTask(t.Context(), connect.NewRequest(&heronv1.SaveProbeTaskRequest{Task: edited, AllNodes: true})); err != nil {
		t.Fatalf("editing an uppercase-scheme https task referenced by a cert rule: %v", err)
	}
}
