package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/outbound"
	"github.com/xjetry/heron/internal/probelimit"
)

type AlertKind string

const (
	KindOffline    AlertKind = "offline"
	KindProbe      AlertKind = "probe"
	KindExpiry     AlertKind = "expiry"
	KindResource   AlertKind = "resource"
	KindCertExpiry AlertKind = "cert_expiry"
	KindTraffic    AlertKind = "traffic"
)

// IsCalendar 表示按日历日评估的到期提醒；告警引擎对这两类规则豁免维护静默。
func (k AlertKind) IsCalendar() bool { return k == KindExpiry || k == KindCertExpiry }

type ProbeMetric string

const (
	MetricLossPct ProbeMetric = "loss_pct"
	MetricRttMs   ProbeMetric = "rtt_ms"
)

// RttMode 是 rtt 规则（探测、指标 rtt_ms）的判定方式；其余规则为空串。rtt 规则只取两个值之一，库里显式落值，
// 不以空串代表固定阈值：迁移 39 把已有 rtt 规则写成 threshold，协议层把未指定收成 threshold。
type RttMode string

const (
	RttThreshold RttMode = "threshold"
	RttRelative  RttMode = "relative"
)

// BaselineMode 是相对判定的基线来源；非相对判定的规则为空串。
type BaselineMode string

const (
	BaselineAdaptive BaselineMode = "adaptive"
	BaselineFixed    BaselineMode = "fixed"
)

// 相对判定字段的取值界。MaxBaselineWindowS 同时是 ProbeBucketMeans 一次读取的窗口上限，
// 5 分钟桶下至多 8640 个。
const (
	MaxBaselineWindowS   = 30 * 24 * 3600
	MaxUpperDeviationPct = 1000
	// 下偏差取到 100 即下界为 0 ms：正的分钟均值不可能落到带下，等于只按上偏差判定（alert.rttBand）。
	MaxLowerDeviationPct = 100
	MinCooldownS         = 60
	MaxCooldownS         = 7 * 24 * 3600
	MaxFixedBaselineMs   = 60000
)

type ResourceMetric string

// 常量值即 metric 列名时取值可直接按名查列。cpu_pct 是语义名，到列名 cpu 的映射在 alert 包。
// load1_per_core 就是列名：按核负载已经由 agent 算好，告警读这一列的分钟均值，不再除以 Facts 的核数。
const (
	MetricMemoryUsedPct ResourceMetric = "memory_used_pct"
	MetricDiskUsedPct   ResourceMetric = "disk_used_pct"
	MetricCpuPct        ResourceMetric = "cpu_pct"
	MetricLoad1PerCore  ResourceMetric = "load1_per_core"
	MetricNetRxBps      ResourceMetric = "net_rx_bps"
	MetricNetTxBps      ResourceMetric = "net_tx_bps"
)

type AlertRule struct {
	ID      int64
	Name    string
	Kind    AlertKind
	Enabled bool
	// AllNodes 为真时不存选择关联；SelectorTags 非空时 NodeIDs 是当前标签交集的展开结果，否则是显式集合。
	// 空覆盖不代表全部节点，DeleteNode 只删关联、不改模式，因此删除最后一个作用域节点不会放宽。
	AllNodes          bool
	NodeIDs           []int64
	SelectorTags      []string
	ChannelIDs        []int64
	TaskID            uint64
	Metric            ProbeMetric
	Threshold         float64
	ForMinutes        int
	ResourceMetric    ResourceMetric
	RecoveryThreshold float64
	// DaysBefore 只属于到期规则。它与 Threshold、ForMinutes 一样不是规则身份：改它保留状态（见 SaveAlertRule）。
	DaysBefore int
	CreatedAt  time.Time
	// 以下只属于 rtt 规则，取值矩阵由 CheckKindFields 裁决（规则见 alert.CheckRule）。RttMode 为 relative 时其余字段才可能非零。
	RttMode            RttMode
	BaselineMode       BaselineMode
	BaselineWindowS    int
	BaselineMinSamples int
	UpperDeviationPct  float64
	LowerDeviationPct  float64
	CooldownS          int
	FixedBaselineMs    float64
}

// AdaptiveBaseline 报告规则是否用自适应基线：只有这样的规则有 alert_baseline 行。
func (r AlertRule) AdaptiveBaseline() bool {
	return r.Kind == KindProbe && r.Metric == MetricRttMs && r.RttMode == RttRelative && r.BaselineMode == BaselineAdaptive
}

type NotifyChannel struct {
	ID        int64
	Name      string
	Kind      ChannelKind
	Config    string
	CreatedAt time.Time
	// RatePerMinute 是这个渠道每分钟至多发出的请求数（含重试），0 表示不限；上限属于接收方。
	RatePerMinute int
}

type ChannelKind string

const (
	ChannelTelegram ChannelKind = "telegram"
	ChannelWebhook  ChannelKind = "webhook"
)

type AlertState string

const (
	StateOK      AlertState = "ok"
	StatePending AlertState = "pending"
	StateFiring  AlertState = "firing"
)

type StateRow struct {
	RuleID, NodeID int64
	State          AlertState
	SinceAt        time.Time
	// FiredExpiresOn 只属于到期规则：进入 firing 时节点的到期日。恢复时拿它与节点当前的到期日比较，相等说明日期没变，
	// 恢复文案写"已不在提醒窗口内"而不是"到期日已更新"。这个日期随状态存，而不是从触发事件里读回：告警事件
	// 按保留期清理，一个过期节点却可以一直处在 firing。其余种类的状态、以及不经 RecordTransition 写入的状态，这一项为空。
	FiredExpiresOn string
	// RecoveredAt 只属于离线规则：这一对规则与节点上一次从 firing 恢复的时刻，零值表示从未恢复过（库里是 NULL）。
	// 它不是当前状态的属性，而是一段历史：状态再变（恢复后再次离线写成 pending）时由写入方沿用，只有下一次恢复
	// 才改写它。抖动抑制按它判断这次离线是否落在恢复后的窗口里；状态行被删除（规则不再适用）时一起消失。
	RecoveredAt time.Time
	// FiredAt 是这一对规则与节点最近一次进入 firing 的时刻，零值表示从未触发过（库里是 0）。与 RecoveredAt 一样是一段
	// 历史而不是当前状态的属性，由 setAlertState 维护：写入 firing 记当下，其余写入沿用库里的值，调用方不给出它。
	// 冷却（rtt 相对判定的 cooldown_s）按它判断新的触发是否落在上一次触发之后的冷却期内；状态行被删除时一起消失。
	FiredAt time.Time
	// FiredSilenced 表示当前 firing 进入时处于维护静默覆盖内、没有投递：恢复是否投递只看它，
	// 与恢复时刻是否静默无关。随触发转换写入（见 RecordTransition），恢复转换把它写回 0。
	FiredSilenced bool
}

type Transition string

const (
	TransitionFiring         Transition = "firing"
	TransitionRecovered      Transition = "recovered"
	TransitionLoginSuccess   Transition = "login_success"
	TransitionLoginLocked    Transition = "login_locked"
	TransitionLoginFailed    Transition = "login_failed"
	TransitionAuthChanged    Transition = "auth_changed"
	TransitionBackupSuccess  Transition = "backup_success"
	TransitionBackupRestored Transition = "backup_restored"
	// 配置层备份失败与恢复，同一故障只在首次失败与恢复时各写一条。
	TransitionBackupFailed    Transition = "backup_failed"
	TransitionBackupRecovered Transition = "backup_recovered"
	// TransitionBackupDisabled 收尾一段已通知、但因备份被停用而不会再有恢复的配置层故障。它不是恢复：停用前的故障
	// 可能仍在，只是 hub 不再观察。
	TransitionBackupDisabled Transition = "backup_disabled"
	// TransitionTrafficReport 是一条周期流量报告：一个事件覆盖这一次到期的各种周期，摘要是整条报告的原文。
	TransitionTrafficReport Transition = "traffic_report"
)

// SystemEventKind 判定 transition 是否属于系统事件并给出它的种类。系统事件是 hub 自身的事件，不属于任何
// 规则×节点，rule_id 与 node_id 都是 0；0/0 只说明它是系统事件，说明不了是哪一种，种类由 transition 决定。
// 表外的 transition 不是系统事件。
//
// 写侧据此把关：RecordLoginEvent 只收登录的 transition、RecordBackupEvent 只收备份的 transition、
// RecordTrafficReportEvent 只写流量报告的 transition，三者都把 rule_id、node_id 写成 0；RecordTransition 不收系统事件的 transition，且经 setAlertState 要求规则与节点存在，
// 两张表的 id 都从 1 起，它写的行不会是 0/0。所以库里 0/0 的行都带系统事件的 transition，规则事件的行都不带。
// 读侧（投递队列、面板）按 transition 给出标签与种类，不按 0/0 推断。
func SystemEventKind(t Transition) (kind string, ok bool) {
	switch t {
	case TransitionLoginSuccess, TransitionLoginLocked, TransitionLoginFailed, TransitionAuthChanged:
		return SystemKindLogin, true
	case TransitionBackupFailed, TransitionBackupRecovered, TransitionBackupDisabled, TransitionBackupSuccess, TransitionBackupRestored:
		return SystemKindBackup, true
	case TransitionTrafficReport:
		return SystemKindTrafficReport, true
	}
	return "", false
}

// SystemKindLogin、SystemKindBackup、SystemKindTrafficReport 是系统事件的种类，投递时作为消息的 Kind。
const (
	SystemKindLogin         = "login"
	SystemKindBackup        = "backup"
	SystemKindTrafficReport = "traffic_report"
)

type AlertEvent struct {
	ID             int64
	RuleID, NodeID int64
	Transition     Transition
	At             time.Time
	Summary        string
	Value          float64
	// Silenced 表示事件生成时处于维护静默覆盖内：事件照常落库但不产生任何投递行。
	// 恢复事件记配对 firing 的值，与恢复时刻是否静默无关。
	Silenced   bool
	Deliveries []Delivery
}

type Delivery struct {
	ID        int64
	EventID   int64
	ChannelID int64
	// BatchID 是发送批次（批次第一行的 id）：同批各行一次发送、共享尝试次数与结果，见 schema.go 的 batch_id 列。
	BatchID    int64
	Attempts   int // 已开始的尝试次数；BeginBatchAttempt 在发送之前提交，发送前崩溃也计入。
	OK         bool
	Done       bool
	Failure    DeliveryFailure // 最近一次失败的类别；成功或尚无结果时为空。
	HTTPStatus int             // 仅 FailureHTTPStatus 非零，且 outbound.ValidHTTPStatus；写路径见 schema.go 的 http_status 列。
	// 最近一次失败的原文：HTTP 失败时是响应体片段，其余是出站错误文本。hub 不把 URL 写进原文，
	// 但响应体由接收方决定：接收方可能回显请求体模板里的密钥，也可能回显请求路径或头值这些只写
	// 不读的配置。所以只经仅会话的 GetAlertDeliveryError 读出（会话本就有权管理这些配置）；
	// 只读的 ListAlertEvents 逐字段映射 Delivery，不带这个字段。
	LastError   string
	DeliveredAt time.Time
	// NotBefore 是下一次尝试的最早墙钟时刻（整秒），零值表示没有限制；见 schema.go 的 not_before 列。
	NotBefore time.Time
}

// DeliveryFailure 在产生失败的地方确定，不从错误文本反推；与 transition、渠道 kind 一样按 TEXT 落库。
type DeliveryFailure string

const (
	FailureNone             DeliveryFailure = ""
	FailureHTTPStatus       DeliveryFailure = "http_status"
	FailureTransport        DeliveryFailure = "transport"
	FailureRequest          DeliveryFailure = "request"
	FailureChannelInvalid   DeliveryFailure = "channel_invalid"
	FailureChannelDeleted   DeliveryFailure = "channel_deleted"
	FailureResultUnrecorded DeliveryFailure = "result_unrecorded"
	FailureUnclassified     DeliveryFailure = "unclassified"
)

// DeliveryFailures 列出全部失败类别（不含 FailureNone）：写侧只接受其中之一；
// api 的协议映射测试据它核对与协议枚举一一对应，新增类别时漏改映射或枚举由该测试发现。
func DeliveryFailures() []DeliveryFailure {
	return []DeliveryFailure{FailureHTTPStatus, FailureTransport, FailureRequest, FailureChannelInvalid,
		FailureChannelDeleted, FailureResultUnrecorded, FailureUnclassified}
}

// DeliveryResult 是一次投递结果的完整写入；UpdateBatch 在写库前校验字段之间的一致性。
type DeliveryResult struct {
	OK, Done    bool
	Failure     DeliveryFailure
	HTTPStatus  int
	Error       string
	DeliveredAt time.Time
	// NotBefore 是下一次尝试的最早墙钟时刻，只随还会重试的失败给出；零值表示没有限制。见 schema.go 的 not_before 列。
	NotBefore time.Time
}

// 投递尝试的唯一上限（按批次计）；队列达到上限时写入 done，重启也不能绕过它。
const MaxDeliveryAttempts = 3

var ErrDeliveryDone = errors.New("delivery is done")
var ErrDeliveryExhausted = errors.New("delivery attempts exhausted")

// DeliveryTarget 是一次转换要投递的一个渠道。Batch 非零时请求加入这个批次（同一次发送），为零或批次已不能加入时
// 新开一批；哪些行该合在一起发是投递层的策略（alert 包），存储层只保证批次的不变式（见 recordAlertEvent）。
type DeliveryTarget struct {
	ChannelID int64
	Batch     int64
}

func (s *Store) ListAlertRules(ctx context.Context) ([]AlertRule, error) {
	return s.listAlertRules(ctx, false)
}

// ListVisibleAlertRules 与变更策略、引用错误共用 RuleInScope；规则自身和引用任务都必须在授权范围内。
func (s *Store) ListVisibleAlertRules(ctx context.Context) ([]AlertRule, error) {
	return s.listAlertRules(ctx, true)
}

// 主表、关联与可见性在同一读事务中裁决，不能把并发保存前后的两份作用域拼在一起。
func (s *Store) listAlertRules(ctx context.Context, visibleOnly bool) ([]AlertRule, error) {
	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	out, err := listAlertRulesTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	if p, ok := Principal(ctx); ok && visibleOnly {
		visible := out[:0]
		for _, r := range out {
			ok, err := RuleInScope(ruleScopeReader{tx}, p.TokenGrant, ChangeAlert, r.ID)
			if err != nil {
				return nil, err
			}
			if ok {
				visible = append(visible, r)
			}
		}
		out = visible
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

func listAlertRulesTx(ctx context.Context, tx *sql.Tx) ([]AlertRule, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, name, kind, enabled, all_nodes, task_id, metric, threshold, for_minutes, days_before, created_at, resource_metric, recovery_threshold,
		rtt_mode, baseline_mode, baseline_window_s, baseline_min_samples, upper_deviation_pct, lower_deviation_pct, cooldown_s, fixed_baseline_ms FROM alert_rule ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AlertRule
	index := map[int64]int{}
	for rows.Next() {
		var r AlertRule
		var task, minutes, daysBefore sql.NullInt64
		var metric sql.NullString
		var threshold sql.NullFloat64
		var resourceMetric sql.NullString
		var recovery sql.NullFloat64
		var created int64
		var rttMode, baselineMode sql.NullString
		var window, minSamples, cooldown sql.NullInt64
		var upper, lower, fixed sql.NullFloat64
		if err := rows.Scan(&r.ID, &r.Name, &r.Kind, &r.Enabled, &r.AllNodes, &task, &metric, &threshold, &minutes, &daysBefore, &created, &resourceMetric, &recovery,
			&rttMode, &baselineMode, &window, &minSamples, &upper, &lower, &cooldown, &fixed); err != nil {
			return nil, err
		}
		r.RttMode, r.BaselineMode = RttMode(rttMode.String), BaselineMode(baselineMode.String)
		r.BaselineWindowS, r.BaselineMinSamples, r.CooldownS = int(window.Int64), int(minSamples.Int64), int(cooldown.Int64)
		r.UpperDeviationPct, r.LowerDeviationPct, r.FixedBaselineMs = upper.Float64, lower.Float64, fixed.Float64
		r.TaskID, r.Metric, r.Threshold, r.ForMinutes = uint64(task.Int64), ProbeMetric(metric.String), threshold.Float64, int(minutes.Int64)
		r.DaysBefore = int(daysBefore.Int64)
		r.ResourceMetric, r.RecoveryThreshold = ResourceMetric(resourceMetric.String), recovery.Float64
		r.CreatedAt = time.Unix(created, 0).UTC()
		index[r.ID] = len(out)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, rel := range []struct {
		table, column string
		nodes         bool
	}{
		{"alert_rule_node", "node_id", true}, {"alert_rule_channel", "channel_id", false},
	} {
		rows, err := tx.QueryContext(ctx, "SELECT rule_id, "+rel.column+" FROM "+rel.table+" ORDER BY rule_id, "+rel.column)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var rule, id int64
			if err := rows.Scan(&rule, &id); err != nil {
				rows.Close()
				return nil, err
			}
			if i, ok := index[rule]; ok {
				if rel.nodes {
					out[i].NodeIDs = append(out[i].NodeIDs, id)
				} else {
					out[i].ChannelIDs = append(out[i].ChannelIDs, id)
				}
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	for i := range out {
		out[i].SelectorTags, err = selectorTags(tx, "alert_rule", "rule_id", out[i].ID)
		if err != nil {
			return nil, err
		}
		if len(out[i].SelectorTags) > 0 {
			out[i].NodeIDs, err = scanIDs(tx.Query("SELECT node_id FROM ("+alertCoverage+") WHERE owner_id = ? ORDER BY node_id", out[i].ID))
			if err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

func sortedAlertIDs(ids []int64) []int64 {
	out := slices.Clone(ids)
	slices.Sort(out)
	return slices.Compact(out)
}

// KindFieldError 表示字段组合违反存储约束。Field 是协议字段名，Constraint 是违反的约束。
type KindFieldError struct {
	Field, Constraint string
}

func (e KindFieldError) Error() string { return e.Field + " " + e.Constraint }

// CheckKindFields 裁决种类与专用字段的组合：任务只属于探测与证书到期，探测指标只属于探测，资源指标与恢复阈值只属于资源；
// 阈值由探测、资源与流量共用，持续分钟只属于探测与资源，days_before 只属于到期与证书到期。SaveAlertRule 对非法组合报错而不改写，
// alert.CheckRule 在保存与载入时调它，协议层经 CheckRule 得到同样的字段与约束（规则见 alert.CheckRule）。阈值用 != 0 判：NaN 与任何数
// 都不等，也被拒绝。rtt 判定方式的八个字段只属于 rtt 规则，矩阵见 checkRttFields。种类本身是否合法不在这里判断。
func CheckKindFields(r AlertRule) error {
	if r.Kind != KindProbe && r.Kind != KindCertExpiry {
		if r.TaskID != 0 {
			return KindFieldError{"task_id", "must be 0 unless kind is probe or cert_expiry"}
		}
	}
	if r.Kind != KindProbe && r.Metric != "" {
		return KindFieldError{"metric", "must be unspecified unless kind is probe"}
	}
	if r.Kind != KindProbe && r.Kind != KindResource && r.Kind != KindTraffic {
		if r.Threshold != 0 {
			return KindFieldError{"threshold", "must be 0 unless kind is probe, resource or traffic"}
		}
	}
	if r.Kind != KindProbe && r.Kind != KindResource {
		if r.ForMinutes != 0 {
			return KindFieldError{"for_minutes", "must be 0 unless kind is probe or resource"}
		}
	}
	if r.Kind != KindResource {
		if r.ResourceMetric != "" {
			return KindFieldError{"resource_metric", "must be unspecified unless kind is resource"}
		}
		if r.RecoveryThreshold != 0 {
			return KindFieldError{"recovery_threshold", "must be 0 unless kind is resource"}
		}
	}
	if r.Kind != KindExpiry && r.Kind != KindCertExpiry && r.DaysBefore != 0 {
		return KindFieldError{"days_before", "must be 0 unless kind is expiry or cert_expiry"}
	}
	return checkRttFields(r)
}

// checkRttFields 裁决 rtt 判定方式的八个字段（零值矩阵，规则见 alert.CheckRule）。字段是否非零按"放宽还是缺省"逐个裁决：
// 非 rtt 规则与固定阈值下它们全部为零值；相对判定下偏差、冷却与基线来源必填，两种基线来源各自只认自己的字段。
// 浮点用 !(lo < x && x <= hi) 的形式判区间，NaN 不满足任何比较，一并被拒。
func checkRttFields(r AlertRule) error {
	if r.Kind != KindProbe || r.Metric != MetricRttMs {
		if r.RttMode != "" {
			return KindFieldError{"rtt_mode", "must be unspecified unless kind is probe and metric is rtt_ms"}
		}
		return checkRelativeFieldsZero(r, "unless kind is probe and metric is rtt_ms")
	}
	switch r.RttMode {
	case RttThreshold:
		return checkRelativeFieldsZero(r, "unless rtt_mode is relative")
	case RttRelative:
	default:
		return KindFieldError{"rtt_mode", fmt.Sprintf("must be %s or %s for rtt_ms probe rules; got %q", RttThreshold, RttRelative, r.RttMode)}
	}
	if r.Threshold != 0 {
		return KindFieldError{"threshold", "must be 0 when rtt_mode is relative"}
	}
	if !(0 < r.UpperDeviationPct && r.UpperDeviationPct <= MaxUpperDeviationPct) {
		return KindFieldError{"upper_deviation_pct", fmt.Sprintf("must be greater than 0 and at most %d", MaxUpperDeviationPct)}
	}
	if !(0 < r.LowerDeviationPct && r.LowerDeviationPct <= MaxLowerDeviationPct) {
		return KindFieldError{"lower_deviation_pct", fmt.Sprintf("must be greater than 0 and at most %d", MaxLowerDeviationPct)}
	}
	if r.CooldownS < MinCooldownS || r.CooldownS > MaxCooldownS {
		return KindFieldError{"cooldown_s", fmt.Sprintf("must be between %d and %d", MinCooldownS, MaxCooldownS)}
	}
	switch r.BaselineMode {
	case BaselineAdaptive:
		// 基线窗口排除判定窗口，比判定窗口还短的基线窗口里一个桶也取不到。
		if r.BaselineWindowS < r.ForMinutes*60 || r.BaselineWindowS > MaxBaselineWindowS {
			return KindFieldError{"baseline_window_s", fmt.Sprintf("must be at least for_minutes×60 and at most %d", MaxBaselineWindowS)}
		}
		if r.BaselineMinSamples < 1 {
			return KindFieldError{"baseline_min_samples", "must be at least 1"}
		}
		if r.FixedBaselineMs != 0 {
			return KindFieldError{"fixed_baseline_ms", "must be 0 unless baseline_mode is fixed"}
		}
	case BaselineFixed:
		if !(0 < r.FixedBaselineMs && r.FixedBaselineMs <= MaxFixedBaselineMs) {
			return KindFieldError{"fixed_baseline_ms", fmt.Sprintf("must be greater than 0 and at most %d", MaxFixedBaselineMs)}
		}
		if r.BaselineWindowS != 0 {
			return KindFieldError{"baseline_window_s", "must be 0 unless baseline_mode is adaptive"}
		}
		if r.BaselineMinSamples != 0 {
			return KindFieldError{"baseline_min_samples", "must be 0 unless baseline_mode is adaptive"}
		}
	default:
		return KindFieldError{"baseline_mode", "must be specified when rtt_mode is relative"}
	}
	return nil
}

// checkRelativeFieldsZero 要求相对判定专用的七个字段全部为零值，when 说明它们在什么条件下才可以非零。
func checkRelativeFieldsZero(r AlertRule, when string) error {
	if r.BaselineMode != "" {
		return KindFieldError{"baseline_mode", "must be unspecified " + when}
	}
	for _, f := range []struct {
		field string
		zero  bool
	}{
		{"baseline_window_s", r.BaselineWindowS == 0},
		{"baseline_min_samples", r.BaselineMinSamples == 0},
		{"upper_deviation_pct", r.UpperDeviationPct == 0},
		{"lower_deviation_pct", r.LowerDeviationPct == 0},
		{"cooldown_s", r.CooldownS == 0},
		{"fixed_baseline_ms", r.FixedBaselineMs == 0},
	} {
		if !f.zero {
			return KindFieldError{f.field, "must be 0 " + when}
		}
	}
	return nil
}

// 引用检查与保存同在单写事务，删除不能插入两者之间造成孤儿引用。
func (s *Store) SaveAlertRule(ctx context.Context, r AlertRule) (AlertRule, error) {
	if err := (NodeSelector{AllNodes: r.AllNodes, NodeIDs: r.NodeIDs, Tags: r.SelectorTags}).Check(); err != nil {
		return AlertRule{}, err
	}
	// 非法组合报错而不是清零：静默清零是放宽方向，调用方发了什么、存下的却是零值，无从察觉。
	if err := CheckKindFields(r); err != nil {
		return AlertRule{}, err
	}
	r.NodeIDs, r.ChannelIDs = sortedAlertIDs(r.NodeIDs), sortedAlertIDs(r.ChannelIDs)
	if r.AllNodes {
		r.NodeIDs = nil
	}
	err := s.writeChange(ctx, ChangeTarget{Action: ActionSaveAlertRule, ResourceID: r.ID}, func(tx *sql.Tx) error {
		for _, id := range r.NodeIDs {
			exists, err := nodeExistsTx(tx, id)
			if err != nil {
				return err
			}
			if !exists {
				return NotFoundError{Kind: ObjectNode, ID: id}
			}
		}
		for _, id := range r.ChannelIDs {
			if err := requireAlertReference(tx, "notify_channel", ObjectNotifyChannel, id); err != nil {
				return err
			}
		}
		// 每种规则只落自己的专用列，其余列写 NULL。CheckKindFields 已保证别的种类的专用字段都是零值，NULL 与回显的
		// 零值一致。
		var task, metric, threshold, minutes, daysBefore, resourceMetric, recovery any
		var rttMode, baselineMode, window, minSamples, upper, lower, cooldown, fixed any
		if r.Kind == KindProbe && r.Metric == MetricRttMs {
			rttMode = r.RttMode
			if r.RttMode == RttRelative {
				baselineMode, upper, lower, cooldown = r.BaselineMode, r.UpperDeviationPct, r.LowerDeviationPct, r.CooldownS
				if r.BaselineMode == BaselineAdaptive {
					window, minSamples = r.BaselineWindowS, r.BaselineMinSamples
				} else {
					fixed = r.FixedBaselineMs
				}
			}
		}
		if r.Kind == KindTraffic {
			threshold = r.Threshold
		}
		if r.Kind == KindProbe {
			if err := requireAlertReference(tx, "probe_task", ObjectProbeTask, int64(r.TaskID)); err != nil {
				return err
			}
			task, metric, threshold, minutes = int64(r.TaskID), r.Metric, r.Threshold, r.ForMinutes
		}
		if r.Kind == KindExpiry {
			daysBefore = r.DaysBefore
		}
		if r.Kind == KindCertExpiry {
			if err := requireHTTPSProbeTask(tx, int64(r.TaskID)); err != nil {
				return err
			}
			task, daysBefore = int64(r.TaskID), r.DaysBefore
		}
		if r.Kind == KindResource {
			resourceMetric, recovery, threshold, minutes = r.ResourceMetric, r.RecoveryThreshold, r.Threshold, r.ForMinutes
		}
		var created int64
		var identityChanged bool
		if r.ID == 0 {
			created = s.clk.Now().Unix()
			err := tx.QueryRow(`INSERT INTO alert_rule (name, kind, enabled, all_nodes, task_id, metric, threshold, for_minutes, days_before, created_at, resource_metric, recovery_threshold,
				rtt_mode, baseline_mode, baseline_window_s, baseline_min_samples, upper_deviation_pct, lower_deviation_pct, cooldown_s, fixed_baseline_ms)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`, r.Name, r.Kind, r.Enabled, r.AllNodes, task, metric, threshold, minutes, daysBefore, created, resourceMetric, recovery,
				rttMode, baselineMode, window, minSamples, upper, lower, cooldown, fixed).Scan(&r.ID)
			if err != nil {
				return err
			}
		} else {
			err := tx.QueryRow(`SELECT kind != ? OR COALESCE(task_id, 0) != ? OR COALESCE(metric, '') != ? OR COALESCE(resource_metric, '') != ? FROM alert_rule WHERE id = ?`, r.Kind, r.TaskID, r.Metric, r.ResourceMetric, r.ID).Scan(&identityChanged)
			if errors.Is(err, sql.ErrNoRows) {
				return NotFoundError{Kind: ObjectAlertRule, ID: r.ID}
			}
			if err != nil {
				return err
			}
			err = tx.QueryRow(`UPDATE alert_rule SET name = ?, kind = ?, enabled = ?, all_nodes = ?, task_id = ?, metric = ?, threshold = ?, for_minutes = ?, days_before = ?, resource_metric = ?, recovery_threshold = ?,
				rtt_mode = ?, baseline_mode = ?, baseline_window_s = ?, baseline_min_samples = ?, upper_deviation_pct = ?, lower_deviation_pct = ?, cooldown_s = ?, fixed_baseline_ms = ?
				WHERE id = ? RETURNING created_at`, r.Name, r.Kind, r.Enabled, r.AllNodes, task, metric, threshold, minutes, daysBefore, resourceMetric, recovery,
				rttMode, baselineMode, window, minSamples, upper, lower, cooldown, fixed, r.ID).Scan(&created)
			if errors.Is(err, sql.ErrNoRows) {
				return NotFoundError{Kind: ObjectAlertRule, ID: r.ID}
			}
			if err != nil {
				return err
			}
		}
		r.CreatedAt = time.Unix(created, 0).UTC()
		for _, rel := range []struct {
			table, column string
			ids           []int64
		}{
			{"alert_rule_node", "node_id", r.NodeIDs}, {"alert_rule_channel", "channel_id", r.ChannelIDs},
		} {
			if _, err := tx.Exec("DELETE FROM "+rel.table+" WHERE rule_id = ?", r.ID); err != nil {
				return err
			}
			for _, id := range rel.ids {
				if _, err := tx.Exec("INSERT INTO "+rel.table+" (rule_id, "+rel.column+") VALUES (?, ?)", r.ID, id); err != nil {
					return err
				}
			}
		}
		if err := setSelectorTags(tx, "alert_rule", "rule_id", r.ID, r.SelectorTags); err != nil {
			return err
		}
		var err error
		r.SelectorTags, err = selectorTags(tx, "alert_rule", "rule_id", r.ID)
		if err != nil {
			return err
		}
		if len(r.SelectorTags) > 0 {
			r.NodeIDs, err = scanIDs(tx.Query("SELECT node_id FROM ("+alertCoverage+") WHERE owner_id = ? ORDER BY node_id", r.ID))
			if err != nil {
				return err
			}
		}
		// 规则与状态由本次写事务一起提交；种类、任务或指标变化后，旧观测不再描述当前规则。
		// threshold、for_minutes 与 days_before 不是身份：同一个量换阈值时保留状态，下一轮按新阈值判断是否恢复。
		// 禁用与作用域收缩也在这里裁剪，重启不能重新载入已不适用的 firing。
		if identityChanged || !r.Enabled || !r.AllNodes {
			query := "DELETE FROM alert_state WHERE rule_id = ?"
			args := []any{r.ID}
			if !identityChanged && r.Enabled && len(r.NodeIDs) > 0 {
				query += " AND node_id NOT IN (" + placeholders(len(r.NodeIDs)) + ")"
				for _, id := range r.NodeIDs {
					args = append(args, id)
				}
			}
			if _, err := tx.Exec(query, args...); err != nil {
				return err
			}
		}
		// 基线行只属于自适应基线的规则，且描述的是这条规则的任务（task_fingerprint 只比任务的种类与目标，换成目标相同的
		// 另一个任务认不出来）：身份变化或不再是自适应基线时随规则一起提交删除。基线窗口与最小样本数不是身份，行沿用，
		// 引擎在保存后的下一轮重算按新值重算（Engine.SaveRule 标记）。作用域与任务分配收缩留下的行由重算轮次删除
		// （它只处理启用的规则：停用期间这些行留着，不被评估，规则删除或重新启用后的第一轮重算时消失）。
		if identityChanged || !r.AdaptiveBaseline() {
			if _, err := tx.Exec("DELETE FROM alert_baseline WHERE rule_id = ?", r.ID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return AlertRule{}, err
	}
	return r, nil
}

func requireAlertReference(tx *sql.Tx, table string, kind ObjectKind, id int64) error {
	var one int
	err := tx.QueryRow("SELECT 1 FROM "+table+" WHERE id = ?", id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return NotFoundError{Kind: kind, ID: id}
	}
	return err
}

// requireHTTPSProbeTask 裁决证书到期规则的任务：必须存在，且是 target 为 https:// 的 HTTP 任务——
// 只有它能带回证书观测；别的任务上这条规则永远没有读数。引用检查与保存同在单写事务，
// 把任务改成非 https:// 或删除它都要先摘掉规则（删除任务的引用检查在 DeleteProbeTask）。
func requireHTTPSProbeTask(tx *sql.Tx, id int64) error {
	var kind int64
	var target string
	err := tx.QueryRow("SELECT kind, target FROM probe_task WHERE id = ?", id).Scan(&kind, &target)
	if errors.Is(err, sql.ErrNoRows) {
		return NotFoundError{Kind: ObjectProbeTask, ID: id}
	}
	if err != nil {
		return err
	}
	if !probelimit.IsHTTPSTarget(heronv1.ProbeKind(kind), target) {
		return KindFieldError{"task_id", "must reference an https:// HTTP probe task for cert_expiry rules"}
	}
	return nil
}

func (s *Store) DeleteAlertRule(ctx context.Context, id int64) error {
	return s.writeChange(ctx, ChangeTarget{Action: ActionDeleteAlertRule, ResourceID: id}, func(tx *sql.Tx) error {
		if err := deleteAlertEntity(tx, "alert_rule", ObjectAlertRule, id); err != nil {
			return err
		}
		for _, table := range []string{"alert_rule_node", "alert_rule_channel", "alert_rule_tag", "alert_state", "alert_baseline"} {
			if _, err := tx.Exec("DELETE FROM "+table+" WHERE rule_id = ?", id); err != nil {
				return err
			}
		}
		return nil
	})
}

func deleteAlertEntity(tx *sql.Tx, table string, kind ObjectKind, id int64) error {
	res, err := tx.Exec("DELETE FROM "+table+" WHERE id = ?", id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return NotFoundError{Kind: kind, ID: id}
	}
	return nil
}

func (s *Store) ListNotifyChannels(ctx context.Context) ([]NotifyChannel, error) {
	rows, err := s.r.QueryContext(ctx, "SELECT id, name, kind, config, created_at, rate_per_minute FROM notify_channel ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NotifyChannel
	for rows.Next() {
		var c NotifyChannel
		var created int64
		if err := rows.Scan(&c.ID, &c.Name, &c.Kind, &c.Config, &created, &c.RatePerMinute); err != nil {
			return nil, err
		}
		c.CreatedAt = time.Unix(created, 0).UTC()
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) SaveNotifyChannel(ctx context.Context, c NotifyChannel) (NotifyChannel, error) {
	err := s.write(ctx, func(tx *sql.Tx) error {
		var created int64
		if c.ID == 0 {
			created = s.clk.Now().Unix()
			if err := tx.QueryRow("INSERT INTO notify_channel (name, kind, config, created_at, rate_per_minute) VALUES (?, ?, ?, ?, ?) RETURNING id", c.Name, c.Kind, c.Config, created, c.RatePerMinute).Scan(&c.ID); err != nil {
				return err
			}
		} else {
			err := tx.QueryRow("UPDATE notify_channel SET name = ?, kind = ?, config = ?, rate_per_minute = ? WHERE id = ? RETURNING created_at", c.Name, c.Kind, c.Config, c.RatePerMinute, c.ID).Scan(&created)
			if errors.Is(err, sql.ErrNoRows) {
				return NotFoundError{Kind: ObjectNotifyChannel, ID: c.ID}
			}
			if err != nil {
				return err
			}
		}
		c.CreatedAt = time.Unix(created, 0).UTC()
		return nil
	})
	if err != nil {
		return NotifyChannel{}, err
	}
	return c, nil
}

// 检查与删除共用写事务；SaveAlertRule 也经单写协程，检查之后不会新添引用。
func checkAlertReferences(ctx context.Context, tx *sql.Tx, query string, kind ObjectKind, id int64) error {
	rows, err := tx.Query(query, id)
	if err != nil {
		return err
	}
	defer rows.Close()
	used := InUseError{Kind: kind, ID: id}
	p, bearer := Principal(ctx)
	for rows.Next() {
		var ref RuleReference
		if err := rows.Scan(&ref.ID, &ref.Name); err != nil {
			return err
		}
		if bearer {
			ok, err := RuleInScope(ruleScopeReader{tx}, p.TokenGrant, ChangeAlert, ref.ID)
			if err != nil {
				return err
			}
			if !ok {
				used.HiddenRules = true
				continue
			}
		}
		used.Rules = append(used.Rules, ref)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(used.Rules) > 0 || used.HiddenRules {
		return used
	}
	return nil
}

func (s *Store) DeleteNotifyChannel(ctx context.Context, id int64) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		if err := checkAlertReferences(ctx, tx, `SELECT r.id, r.name FROM alert_rule r JOIN alert_rule_channel c ON c.rule_id = r.id WHERE c.channel_id = ? ORDER BY r.id`, ObjectNotifyChannel, id); err != nil {
			return err
		}
		if err := deleteAlertEntity(tx, "notify_channel", ObjectNotifyChannel, id); err != nil {
			return err
		}
		for _, l := range NotifyLists {
			if err := removeChannelID(tx, l.List, id); err != nil {
				return err
			}
		}
		_, err := tx.Exec("UPDATE alert_delivery SET done = 1, failure = ?, http_status = NULL, last_error = '' WHERE channel_id = ? AND done = 0", FailureChannelDeleted, id)
		return err
	})
}

func (s *Store) ListAlertStates(ctx context.Context) ([]StateRow, error) {
	rows, err := s.r.QueryContext(ctx, "SELECT rule_id, node_id, state, since_at, fired_expires_on, recovered_at, fired_silenced, fired_at FROM alert_state ORDER BY rule_id, node_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StateRow
	for rows.Next() {
		var r StateRow
		var since int64
		var recovered sql.NullInt64
		var fired int64
		if err := rows.Scan(&r.RuleID, &r.NodeID, &r.State, &since, &r.FiredExpiresOn, &recovered, &r.FiredSilenced, &fired); err != nil {
			return nil, err
		}
		r.SinceAt = time.Unix(since, 0).UTC()
		if fired != 0 {
			r.FiredAt = time.Unix(fired, 0).UTC()
		}
		if recovered.Valid {
			r.RecoveredAt = time.Unix(recovered.Int64, 0).UTC()
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// 两个状态写入口共用事务内准入，单写协程保证删除之后排队的写不能重建孤儿状态。整行替换：每次写都给出
// fired_expires_on、recovered_at 与 fired_silenced，上一个状态记的值不会留到下一个状态；要沿用时由调用方显式带上。
// fired_at 例外，由这里按库里的上一行推出（见 StateRow.FiredAt）：从非 firing 写成 firing 记 since，其余沿用上一行的值，
// 没有上一行时为 0。它不经调用方，所以任何种类、任何写入口进入 firing 都记下它，不会有入口漏带而把冷却的起点抹掉。
func setAlertState(tx *sql.Tx, ruleID, nodeID int64, state AlertState, since time.Time, firedExpiresOn string, recoveredAt time.Time, firedSilenced bool) error {
	if err := requireAlertReference(tx, "alert_rule", ObjectAlertRule, ruleID); err != nil {
		return err
	}
	exists, err := nodeExistsTx(tx, nodeID)
	if err != nil {
		return err
	}
	if !exists {
		return NotFoundError{Kind: ObjectNode, ID: nodeID}
	}
	var recovered any
	if !recoveredAt.IsZero() {
		recovered = recoveredAt.Unix()
	}
	var prevState string
	var prevFired int64
	err = tx.QueryRow("SELECT state, fired_at FROM alert_state WHERE rule_id = ? AND node_id = ?", ruleID, nodeID).Scan(&prevState, &prevFired)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	firedAt := prevFired
	if state == StateFiring && AlertState(prevState) != StateFiring {
		firedAt = since.Unix()
	}
	_, err = tx.Exec("INSERT OR REPLACE INTO alert_state (rule_id, node_id, state, since_at, fired_expires_on, recovered_at, fired_silenced, fired_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		ruleID, nodeID, state, since.Unix(), firedExpiresOn, recovered, firedSilenced, firedAt)
	return err
}

// SetAlertState 写不带事件的状态变化。alert 的状态机只经触发转换进入 firing（那一步走 RecordTransition），所以这里
// 写的状态没有触发时的到期日，也不在 firing 内：fired_silenced 恒写 0。recoveredAt 含义见 StateRow.RecoveredAt，零值写 NULL。
func (s *Store) SetAlertState(ctx context.Context, ruleID, nodeID int64, state AlertState, since, recoveredAt time.Time) error {
	return s.write(ctx, func(tx *sql.Tx) error { return setAlertState(tx, ruleID, nodeID, state, since, "", recoveredAt, false) })
}

// 候选集撤销不是恢复观测，删除状态不产生事件；重复清理同一对规则与节点仍然成功。
func (s *Store) DeleteAlertState(ctx context.Context, ruleID, nodeID int64) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec("DELETE FROM alert_state WHERE rule_id = ? AND node_id = ?", ruleID, nodeID)
		return err
	})
}

// 状态、事件与续投队列同事务提交，崩溃不能留下已转换但没有通知记录的状态。firedExpiresOn 与 recoveredAt 随状态写入，
// 含义见 StateRow.FiredExpiresOn 与 StateRow.RecoveredAt。fired_silenced 由转换方向推出：进入 firing 记这次触发是否被
// 静默（ev.Silenced），恢复转换写回 0——配对的 firing 已随这次恢复闭环，下一轮的恢复判定从新的触发重新计。
func (s *Store) RecordTransition(ctx context.Context, ruleID, nodeID int64, state AlertState, firedExpiresOn string, recoveredAt time.Time, ev AlertEvent, targets []DeliveryTarget) (AlertEvent, error) {
	if _, system := SystemEventKind(ev.Transition); system {
		return AlertEvent{}, fmt.Errorf("rule %d, node %d: transition %q belongs to a system event, not to a rule and node", ruleID, nodeID, ev.Transition)
	}
	ev.ID, ev.RuleID, ev.NodeID, ev.Deliveries = 0, ruleID, nodeID, nil
	ev.At = time.Unix(ev.At.Unix(), 0).UTC()
	err := s.write(ctx, func(tx *sql.Tx) error {
		if err := setAlertState(tx, ruleID, nodeID, state, ev.At, firedExpiresOn, recoveredAt, state == StateFiring && ev.Silenced); err != nil {
			return err
		}
		return recordAlertEvent(tx, &ev, targets)
	})
	if err != nil {
		return AlertEvent{}, err
	}
	return ev, nil
}

// RecordLoginEvent 记一条登录事件（系统事件，rule_id、node_id 为 0），给设置里选定的每个渠道各建一条投递；
// 没选渠道也保留审计；普通认证失败只写事件，不投递通知。
//
// 登录不走规则×节点：它没有节点，也没有恢复，规则×节点的状态机与以 (rule_id, node_id) 为主键的 alert_state
// 都装不下它，所以不建 alert_state。事件与投递的写入（渠道引用检查、入批）与规则事件、备份事件共用
// recordAlertEvent，每个渠道新开一批（systemTargets）。
//
// 读渠道列表与写事件、投递在同一个写事务里；s.write 经单写协程提交，这个事务与改设置（SaveSettings）、
// 删渠道（DeleteNotifyChannel）串行，两种交错各有结果：
//   - 与删渠道：删渠道先提交，这里读到的列表已摘除该渠道（删渠道在同一事务里摘除）；这里先提交，删渠道随后
//     把这条尚未完成的投递置为终态（FailureChannelDeleted）。列表里若残留不存在的 ID，recordAlertEvent 的
//     引用检查让整条事件失败、什么都不写，不会留下指向不存在渠道的投递。
//   - 与关闭通知：关闭先提交，这里读到空列表，只记事件；这里先提交，已记下的这条照常投递，所以关闭之后仍可能
//     收到关闭提交前已记下的通知。
func (s *Store) RecordLoginEvent(ctx context.Context, ev AlertEvent) (AlertEvent, error) {
	if kind, _ := SystemEventKind(ev.Transition); kind != SystemKindLogin {
		return AlertEvent{}, fmt.Errorf("invalid login event transition %q", ev.Transition)
	}
	ev.ID, ev.RuleID, ev.NodeID, ev.Deliveries = 0, 0, 0, nil
	ev.At = time.Unix(ev.At.Unix(), 0).UTC()
	err := s.write(ctx, func(tx *sql.Tx) error {
		var ids []int64
		if ev.Transition != TransitionLoginFailed {
			var err error
			ids, err = storedChannelIDs(tx, LoginNotifyList)
			if err != nil {
				return err
			}
		}
		return recordAlertEvent(tx, &ev, systemTargets(ids))
	})
	if err != nil {
		return AlertEvent{}, err
	}
	return ev, nil
}

// systemTargets 是系统事件（登录、备份、流量报告）的投递目标：每个选定的渠道新开一批。合批是告警转换的投递策略（alert 包的
// Engine.apply 按渠道、评估周期、规则与转换方向决定加入哪一批）；系统事件各自单独成批，与所有批次一样受渠道的节奏
// 上限约束（Queue.rateSlot）。
func systemTargets(channels []int64) []DeliveryTarget {
	targets := make([]DeliveryTarget, len(channels))
	for i, channel := range channels {
		targets[i] = DeliveryTarget{ChannelID: channel}
	}
	return targets
}

// recordAlertEvent 是告警转换与系统事件（登录、备份）共用的事件及投递写入路径，调用方的 Store.write 事务承载其原子性。
// 每个目标一行投递。批次的不变式是同批各行的尝试次数与结果始终相同（schema.go 的 batch_id 列），由两处共同维持：
// 尝试与结果只按批次整批写（BeginBatchAttempt、UpdateBatch），新行只加入还没开始尝试、且发往同一渠道的批次
// （joinableBatch，与这里的插入同在一个写事务，和 BeginBatchAttempt 经 Store.write 串行）。请求加入的批次已开始、
// 已终态或已不存在时新开一批。
//
// 加入与发送之间没有别的同步：worker 可能已经读过这个批次、正按读到的行拼消息（补货路径可以在一次巡检中途读到
// 尚未收齐的批次）。新加入的行不会被记进那次发送，是因为 BeginBatchAttempt 只在批次当前的行集合与调用方拼消息时
// 读到的相同时才开始尝试，否则拒绝、由调用方重读。
func recordAlertEvent(tx *sql.Tx, ev *AlertEvent, targets []DeliveryTarget) error {
	if err := tx.QueryRow("INSERT INTO alert_event (rule_id, node_id, transition, at, summary, value, silenced) VALUES (?, ?, ?, ?, ?, ?, ?) RETURNING id", ev.RuleID, ev.NodeID, ev.Transition, ev.At.Unix(), ev.Summary, ev.Value, ev.Silenced).Scan(&ev.ID); err != nil {
		return err
	}
	// 维护静默的不变式：被静默的事件没有投递行，保留期内它因此在界面上呈现"已静默、未投递"，
	// 队列也不会因它发出任何通知。
	if ev.Silenced {
		return nil
	}
	for _, target := range targets {
		if err := requireAlertReference(tx, "notify_channel", ObjectNotifyChannel, target.ChannelID); err != nil {
			return err
		}
		d := Delivery{EventID: ev.ID, ChannelID: target.ChannelID}
		if target.Batch != 0 {
			open, err := joinableBatch(tx, target.Batch, target.ChannelID)
			if err != nil {
				return err
			}
			if open {
				d.BatchID = target.Batch
			}
		}
		own := d.BatchID == 0
		if own {
			// 新批次的批次号就是这一行将得到的 id：AUTOINCREMENT 表的新 id 是 sqlite_sequence 记下的值加一（还没有
			// 记录时是 1）。插入后用 RETURNING 核对，对不上就报错回滚，而不是写下一个不指向自己的批次号。
			if err := tx.QueryRow("SELECT COALESCE((SELECT seq FROM sqlite_sequence WHERE name = 'alert_delivery'), 0) + 1").Scan(&d.BatchID); err != nil {
				return err
			}
		}
		if err := tx.QueryRow("INSERT INTO alert_delivery (event_id, channel_id, batch_id) VALUES (?, ?, ?) RETURNING id", d.EventID, d.ChannelID, d.BatchID).Scan(&d.ID); err != nil {
			return err
		}
		if own && d.ID != d.BatchID {
			return fmt.Errorf("alert delivery %d was inserted as batch %d: sqlite_sequence does not predict the next id", d.ID, d.BatchID)
		}
		ev.Deliveries = append(ev.Deliveries, d)
	}
	return nil
}

// joinableBatch 判断新行能否加入 batch：批次还有行、都发往 channel、且都没开始尝试也没终态。调用方在写事务里。
func joinableBatch(tx *sql.Tx, batch, channel int64) (bool, error) {
	var rows, closed int
	err := tx.QueryRow("SELECT COUNT(*), COALESCE(MAX(attempts > 0 OR done = 1 OR channel_id <> ?), 0) FROM alert_delivery WHERE batch_id = ?", channel, batch).Scan(&rows, &closed)
	return rows > 0 && closed == 0, err
}

// ErrBatchChanged 表示批次当前未终态的行与调用方拼消息时读到的不同：其间有行加入，或有行随事件清理而消失。
var ErrBatchChanged = errors.New("delivery batch changed since it was read")

// BeginBatchAttempt 为整批提交一次尝试：每个已提交的尝试只授权至多一次发送，不保证恰好一次，提交后发送前崩溃也消耗名额。
// 条件更新由 Store.write 串行化；结果写失败或重启不能重新使用已消耗的名额。
//
// rows 是调用方拼这次消息时读到的行。批次当前的行与它不同时拒绝（ErrBatchChanged，不消耗名额）：尝试与随后的结果
// 只能记在这次发送真正包含的行上，否则后加入的行会被记成已送达而消息里没有它。比对与计数在同一个写事务里，
// recordAlertEvent 的调用方也经 Store.write，所以比对通过之后到计数之间不会再有行加入。
//
// 同批全部行（含已耗尽名额的）的次数、终态、渠道与 not_before 必须相同（见 recordAlertEvent），不同说明批次不变式
// 已被破坏，报错而不是按其中某行发送。返回计数之后的各行（按 id 升序）。
func (s *Store) BeginBatchAttempt(ctx context.Context, batch int64, rows []int64) ([]Delivery, error) {
	var ds []Delivery
	var refused error
	err := s.write(ctx, func(tx *sql.Tx) error {
		all, err := queryDeliveries(tx, selectDeliveries+" WHERE batch_id = ? ORDER BY id", batch)
		switch {
		case err != nil:
			return err
		case len(all) == 0:
			return NotFoundError{Kind: ObjectAlertDelivery, ID: batch}
		}
		if err := uniformBatch(batch, all); err != nil {
			return err
		}
		ids := make([]int64, len(all))
		for i, d := range all {
			ids[i] = d.ID
		}
		switch {
		case all[0].Done:
			refused = ErrDeliveryDone
			return nil
		case all[0].Attempts >= MaxDeliveryAttempts:
			refused = ErrDeliveryExhausted
			return nil
		case !slices.Equal(ids, slices.Sorted(slices.Values(rows))):
			refused = ErrBatchChanged
			return nil
		}
		ds, err = queryDeliveries(tx, "UPDATE alert_delivery SET attempts = attempts + 1 WHERE batch_id = ? RETURNING "+deliveryColumns, batch)
		slices.SortFunc(ds, func(a, b Delivery) int { return cmp.Compare(a.ID, b.ID) })
		return err
	})
	if err != nil {
		return nil, err
	}
	if refused != nil {
		return nil, refused
	}
	return ds, nil
}

func queryDeliveries(tx *sql.Tx, query string, args ...any) ([]Delivery, error) {
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDeliveries(rows)
}

func uniformBatch(batch int64, ds []Delivery) error {
	for _, d := range ds[1:] {
		if d.Attempts != ds[0].Attempts || d.Done != ds[0].Done || d.ChannelID != ds[0].ChannelID || !d.NotBefore.Equal(ds[0].NotBefore) {
			return fmt.Errorf("delivery batch %d is not uniform: row %d (attempts %d, done %v, channel %d, not_before %v) differs from row %d (attempts %d, done %v, channel %d, not_before %v)",
				batch, d.ID, d.Attempts, d.Done, d.ChannelID, d.NotBefore, ds[0].ID, ds[0].Attempts, ds[0].Done, ds[0].ChannelID, ds[0].NotBefore)
		}
	}
	return nil
}

// UpdateBatch 把一次尝试的结果写到整批未终态的行上。结果不能改写已开始的次数；计数只由 BeginBatchAttempt 推进。
func (s *Store) UpdateBatch(ctx context.Context, batch int64, r DeliveryResult) error {
	if err := r.check(); err != nil {
		return err
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		var delivered, status any
		if !r.DeliveredAt.IsZero() {
			delivered = r.DeliveredAt.Unix()
		}
		if r.HTTPStatus != 0 {
			status = r.HTTPStatus
		}
		// 按秒向上取整：写成更早的时刻会让补回的批次提前重试。
		var notBefore int64
		if !r.NotBefore.IsZero() {
			notBefore = r.NotBefore.Add(time.Second - time.Nanosecond).Unix()
		}
		res, err := tx.Exec("UPDATE alert_delivery SET ok = ?, done = ?, failure = ?, http_status = ?, last_error = ?, delivered_at = ?, not_before = ? WHERE batch_id = ? AND done = 0", r.OK, r.Done, r.Failure, status, r.Error, delivered, notBefore, batch)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			// 删除渠道或其他终止已先提交，迟到的 HTTP 结果不能把终态重新打开。
			var exists bool
			if err := tx.QueryRow("SELECT EXISTS (SELECT 1 FROM alert_delivery WHERE batch_id = ?)", batch).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return NotFoundError{Kind: ObjectAlertDelivery, ID: batch}
			}
		}
		return nil
	})
}

// 读侧据类别解释状态码与原文、只读口径据类别给出状态码、PendingBatches 据 done 续投，
// 都依赖这里的一致性；违反即拒绝，不写库，也不改写成某个"最接近"的类别。
func (r DeliveryResult) check() error {
	if r.OK {
		if r.Failure != FailureNone || r.HTTPStatus != 0 || r.Error != "" {
			return fmt.Errorf("delivery result: success carries failure %q, status %d, error %q", r.Failure, r.HTTPStatus, r.Error)
		}
		if !r.NotBefore.IsZero() {
			return errors.New("delivery result: success carries a next-attempt time")
		}
		// 成功未终态会被 PendingBatches 重新入队，接收方收到重复通知。
		if !r.Done {
			return errors.New("delivery result: success must be done")
		}
		if r.DeliveredAt.IsZero() {
			return errors.New("delivery result: success without delivery time")
		}
		return nil
	}
	if !r.DeliveredAt.IsZero() {
		return errors.New("delivery result: failure with delivery time")
	}
	// 终态不再尝试，带着"不早于"的时刻没有意义，也会让读者以为它还在等。
	if r.Done && !r.NotBefore.IsZero() {
		return fmt.Errorf("delivery result: finished failure %q carries a next-attempt time", r.Failure)
	}
	if r.Failure == FailureNone {
		return errors.New("delivery result: failure without category")
	}
	if !slices.Contains(DeliveryFailures(), r.Failure) {
		return fmt.Errorf("delivery result: unknown failure category %q", r.Failure)
	}
	if r.Failure == FailureHTTPStatus && !outbound.ValidHTTPStatus(r.HTTPStatus) {
		return fmt.Errorf("delivery result: failure %q with invalid HTTP status %d", r.Failure, r.HTTPStatus)
	}
	if r.Failure != FailureHTTPStatus && r.HTTPStatus != 0 {
		return fmt.Errorf("delivery result: failure %q carries HTTP status %d", r.Failure, r.HTTPStatus)
	}
	if (r.Failure == FailureChannelDeleted || r.Failure == FailureResultUnrecorded) && r.Error != "" {
		return fmt.Errorf("delivery result: failure %q carries error text", r.Failure)
	}
	// 这五类原样重发只会得到同样结果（或已无从重发），不能留在续投队列里。
	switch r.Failure {
	case FailureChannelDeleted, FailureResultUnrecorded, FailureChannelInvalid, FailureRequest, FailureUnclassified:
		if !r.Done {
			return fmt.Errorf("delivery result: failure %q must be done", r.Failure)
		}
	}
	return nil
}

// GetDeliveryError 读出最近一次失败的原文；只供仅会话的方法使用（见 Delivery.LastError）。
func (s *Store) GetDeliveryError(ctx context.Context, id int64) (string, error) {
	var text string
	err := s.r.QueryRowContext(ctx, "SELECT last_error FROM alert_delivery WHERE id = ?", id).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		return "", NotFoundError{Kind: ObjectAlertDelivery, ID: id}
	}
	return text, err
}

const deliveryColumns = "id, event_id, channel_id, batch_id, attempts, ok, done, failure, http_status, last_error, delivered_at, not_before"
const selectDeliveries = "SELECT " + deliveryColumns + " FROM alert_delivery"

func scanDelivery(row interface{ Scan(...any) error }) (Delivery, error) {
	var d Delivery
	var delivered, status sql.NullInt64
	var notBefore int64
	if err := row.Scan(&d.ID, &d.EventID, &d.ChannelID, &d.BatchID, &d.Attempts, &d.OK, &d.Done, &d.Failure, &status, &d.LastError, &delivered, &notBefore); err != nil {
		return Delivery{}, err
	}
	if notBefore != 0 {
		d.NotBefore = time.Unix(notBefore, 0).UTC()
	}
	d.HTTPStatus = int(status.Int64)
	if delivered.Valid {
		d.DeliveredAt = time.Unix(delivered.Int64, 0).UTC()
	}
	return d, nil
}

func scanDeliveries(rows *sql.Rows) ([]Delivery, error) {
	var out []Delivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

const selectPendingBatches = "SELECT DISTINCT batch_id, channel_id FROM alert_delivery WHERE done = 0 ORDER BY batch_id"

// PendingBatch 是一个还有未终态行的批次与它发往的渠道（同批各行渠道相同，见 recordAlertEvent）。
type PendingBatch struct{ ID, ChannelID int64 }

// PendingBatches 是还有未终态行的批次，按批次号（即批次第一行的 id）升序：重启与补货按原批次重投，不重新合并。
func (s *Store) PendingBatches(ctx context.Context) ([]PendingBatch, error) {
	rows, err := s.r.QueryContext(ctx, selectPendingBatches)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingBatch
	for rows.Next() {
		var b PendingBatch
		if err := rows.Scan(&b.ID, &b.ChannelID); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// DeliveryBatch 是一个批次的全部行与它们的事件：Events[i] 是 Deliveries[i] 的事件（不带 Deliveries）。一个批次只发往
// 一个渠道（joinableBatch 只让同渠道的行加入）；引擎给出的目标来自规则的渠道列表，alert_rule_channel 的主键
// (rule_id, channel_id) 让一个事件在每个渠道至多一行，所以同批各行的事件互不相同。
type DeliveryBatch struct {
	ID         int64
	Deliveries []Delivery
	Events     []AlertEvent
}

// GetDeliveryBatch 在一个读快照里读出批次的行与事件，按行 id 升序；批次已不存在（随事件清理）时返回 NotFoundError。
func (s *Store) GetDeliveryBatch(ctx context.Context, batch int64) (DeliveryBatch, error) {
	out := DeliveryBatch{ID: batch}
	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, selectDeliveries+" WHERE batch_id = ? ORDER BY id", batch)
	if err != nil {
		return out, err
	}
	out.Deliveries, err = scanDeliveries(rows)
	rows.Close()
	if err != nil {
		return out, err
	}
	if len(out.Deliveries) == 0 {
		return out, NotFoundError{Kind: ObjectAlertDelivery, ID: batch}
	}
	for _, d := range out.Deliveries {
		var ev AlertEvent
		var at int64
		err := tx.QueryRowContext(ctx, selectAlertEvents+"WHERE id = ?", d.EventID).Scan(&ev.ID, &ev.RuleID, &ev.NodeID, &ev.Transition, &at, &ev.Summary, &ev.Value, &ev.Silenced)
		if errors.Is(err, sql.ErrNoRows) {
			// 事件与投递由 PruneAlertEvents 在同一事务删除，同一快照里有行就有事件。
			return out, fmt.Errorf("alert delivery %d refers to missing alert event %d", d.ID, d.EventID)
		}
		if err != nil {
			return out, err
		}
		ev.At = time.Unix(at, 0).UTC()
		out.Events = append(out.Events, ev)
	}
	return out, tx.Commit()
}

func placeholders(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

// 事件与投递行共用一个读快照，返回值不混合通知更新前后的状态。
func (s *Store) readAlertEvents(ctx context.Context, predicate string, args ...any) ([]AlertEvent, error) {
	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, selectAlertEvents+predicate, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AlertEvent
	var ids []any
	index := map[int64]int{}
	for rows.Next() {
		var ev AlertEvent
		var at int64
		if err := rows.Scan(&ev.ID, &ev.RuleID, &ev.NodeID, &ev.Transition, &at, &ev.Summary, &ev.Value, &ev.Silenced); err != nil {
			return nil, err
		}
		ev.At = time.Unix(at, 0).UTC()
		index[ev.ID] = len(out)
		out = append(out, ev)
		ids = append(ids, ev.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) > 0 {
		rows, err := tx.QueryContext(ctx, selectDeliveries+" WHERE event_id IN ("+placeholders(len(ids))+") ORDER BY id", ids...)
		if err != nil {
			return nil, err
		}
		deliveries, err := scanDeliveries(rows)
		rows.Close()
		if err != nil {
			return nil, err
		}
		for _, d := range deliveries {
			i := index[d.EventID]
			out[i].Deliveries = append(out[i].Deliveries, d)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) ListAlertEvents(ctx context.Context, nodeID int64, beforeID int64, limit int) ([]AlertEvent, error) {
	where, args := alertEventWindow(nodeID, beforeID, limit)
	if p, ok := Principal(ctx); ok && !p.AllNodes {
		prefix, order, _ := strings.Cut(where, " ORDER BY")
		if prefix == "" {
			prefix = "WHERE "
		} else {
			prefix += " AND "
		}
		where = prefix + nodeScopeSQL(ctx, "alert_event.node_id") + " ORDER BY" + order
	}
	return s.readAlertEvents(ctx, where, args...)
}

const selectAlertEvents = "SELECT id, rule_id, node_id, transition, at, summary, value, silenced FROM alert_event "

// 只拼接实际过滤条件，node_id 的等值条件才能走 alert_event_by_node 的前缀。
func alertEventWindow(nodeID, beforeID int64, limit int) (string, []any) {
	var conditions []string
	var args []any
	if nodeID != 0 {
		conditions = append(conditions, "node_id = ?")
		args = append(args, nodeID)
	}
	if beforeID != 0 {
		conditions = append(conditions, "id < ?")
		args = append(args, beforeID)
	}
	where := ""
	if len(conditions) > 0 {
		where = "WHERE " + strings.Join(conditions, " AND ")
	}
	return where + " ORDER BY id DESC LIMIT ?", append(args, limit)
}

const countExpiredPendingDeliveries = "SELECT COUNT(*) FROM alert_delivery WHERE done = 0 AND event_id IN (SELECT id FROM alert_event WHERE at < ?)"
const deleteExpiredDeliveries = "DELETE FROM alert_delivery WHERE event_id IN (SELECT id FROM alert_event WHERE at < ?)"
const deleteExpiredAlertEvents = "DELETE FROM alert_event WHERE at < ?"

// 一次性静默到期后保留供审计，随告警事件的保留期一起清理：until_at 早于同一截止点即删，关联行同事务删除。
// 每日重复的静默没有到期，不在清理之列。
const deleteExpiredSilenceNodes = "DELETE FROM silence_node WHERE silence_id IN (SELECT id FROM silence WHERE kind = ? AND until_at < ?)"
const deleteExpiredSilenceTags = "DELETE FROM silence_tag WHERE silence_id IN (SELECT id FROM silence WHERE kind = ? AND until_at < ?)"
const deleteExpiredSilences = "DELETE FROM silence WHERE kind = ? AND until_at < ?"

// 事件与投递共享事件时间的保留期；同一写事务内先删投递，不能留下孤行。
// 未完成投递也随过期事件删除，提交后再告警计数，避免把回滚误报成投递丢失。
func (s *Store) PruneAlertEvents(ctx context.Context, before time.Time) (int64, error) {
	var deleted, unfinished int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRow(countExpiredPendingDeliveries, before.Unix()).Scan(&unfinished); err != nil {
			return err
		}
		if _, err := tx.Exec(deleteExpiredDeliveries, before.Unix()); err != nil {
			return err
		}
		result, err := tx.Exec(deleteExpiredAlertEvents, before.Unix())
		if err != nil {
			return err
		}
		for _, query := range []string{deleteExpiredSilenceNodes, deleteExpiredSilenceTags, deleteExpiredSilences} {
			if _, err := tx.Exec(query, SilenceOnce, before.Unix()); err != nil {
				return err
			}
		}
		deleted, err = result.RowsAffected()
		return err
	})
	if err != nil {
		return 0, err
	}
	if unfinished > 0 {
		s.log.Warn("expired alert events discarded unfinished deliveries", "unfinished_deliveries", unfinished)
	}
	return deleted, nil
}

func (s *Store) GetAlertEvent(ctx context.Context, id int64) (AlertEvent, error) {
	events, err := s.readAlertEvents(ctx, "WHERE id = ?", id)
	if err != nil {
		return AlertEvent{}, err
	}
	if len(events) == 0 {
		return AlertEvent{}, NotFoundError{Kind: ObjectAlertEvent, ID: id}
	}
	return events[0], nil
}
