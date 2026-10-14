// Package alert 在存储提交成功后发布规则与状态，避免失败写入造成通知与状态分离。
package alert

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"text/template"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/xjetry/heron/internal/hub/store"
)

var ErrInvalid = errors.New("invalid")

// 字段路径与允许值由校验方提供；协议层只替换枚举词汇，不从错误全文猜测字段。
type FieldError struct {
	Path       string
	Allowed    []string
	Got        string
	Constraint string
}

func (e FieldError) Is(target error) bool { return target == ErrInvalid }
func (e FieldError) Detail() string {
	if len(e.Allowed) > 0 {
		return fmt.Sprintf("must be one of %s; got %q", strings.Join(e.Allowed, ", "), e.Got)
	}
	return e.Constraint
}
func (e FieldError) Error() string { return fmt.Sprintf("%s: %s %s", ErrInvalid, e.Path, e.Detail()) }
func invalid(path, constraint string, args ...any) error {
	return FieldError{Path: path, Constraint: fmt.Sprintf(constraint, args...)}
}
func oneOf(path, got string, allowed ...string) error {
	return FieldError{Path: path, Got: got, Allowed: allowed}
}
func checkName(name string) error {
	if n := utf8.RuneCountInString(name); n < 1 || n > 64 {
		return invalid("name", "must contain between 1 and 64 characters")
	}
	return nil
}

// 资源指标的阈值上限按指标裁决：百分比 100、每核负载 64、字节速率 2^40。
// 上限文案随表给出，2^40 的浮点打印（科学记数）读不出量级。
var resourceThresholdMax = []struct {
	metric  store.ResourceMetric
	max     float64
	maxText string
}{
	{store.MetricMemoryUsedPct, 100, "100"},
	{store.MetricDiskUsedPct, 100, "100"},
	{store.MetricCpuPct, 100, "100"},
	{store.MetricLoad1PerCore, 64, "64"},
	{store.MetricNetRxBps, 1 << 40, "1099511627776"},
	{store.MetricNetTxBps, 1 << 40, "1099511627776"},
}

// CheckRule 让告警作用域保持显式；空作用域表示没有目标，不能放宽成全部节点。
// CheckRule 只校验持久化结构；DeleteNode 可把显式作用域删空，空集仍是不覆盖节点的合法规则。
// store.CheckKindFields 按规则种类和 RTT 判定方式拒绝不属于该形态的非零字段；这一步由保存与加载共用，
// 空值究竟是必填缺失还是不适用按字段逐个裁决，不能把空作用域或无关字段当作放宽条件。
// Load 若丢弃这种规则，列表会不可见，而存储的渠道引用仍阻止删除，库与内存就会不一致。
//
// 种类专用的字段只属于自己的种类，由 store.CheckKindFields 一处裁决；这里在已知种类上调它，把它的结果转成协议层
// 认得的字段错误。保存（Engine.SaveRule）与载入（Engine.Load）都经这里，所以"离线或到期规则带着探测字段"既存不
// 进去，也不会从手改过的库里载入生效；store.SaveAlertRule 自己也用同一个谓词拒绝。
func CheckRule(r store.AlertRule) error {
	if err := checkName(r.Name); err != nil {
		return err
	}
	switch r.Kind {
	case store.KindTraffic:
		if math.IsNaN(r.Threshold) || math.IsInf(r.Threshold, 0) || r.Threshold <= 0 || r.Threshold > 100 {
			return invalid("threshold", "must be greater than 0 and at most 100")
		}
		return checkKindFields(r)
	case store.KindOffline:
		return checkKindFields(r)
	case store.KindExpiry:
		if r.DaysBefore < 1 || r.DaysBefore > 365 {
			return invalid("days_before", "must be between 1 and 365")
		}
		return checkKindFields(r)
	case store.KindCertExpiry:
		// task_id 必须指向 https:// 的 HTTP 任务，由 store.SaveAlertRule 在事务内裁决（Load 的库也经同一入口写入）。
		if r.DaysBefore < 1 || r.DaysBefore > 365 {
			return invalid("days_before", "must be between 1 and 365")
		}
		if err := checkKindFields(r); err != nil {
			return err
		}
		if r.TaskID == 0 {
			return invalid("task_id", "must not be 0")
		}
		return nil
	case store.KindProbe:
		if err := checkKindFields(r); err != nil {
			return err
		}
		if r.TaskID == 0 {
			return invalid("task_id", "must not be 0")
		}
		if math.IsNaN(r.Threshold) || math.IsInf(r.Threshold, 0) {
			return invalid("threshold", "must be finite")
		}
		switch r.Metric {
		case store.MetricLossPct:
			if r.Threshold < 0 || r.Threshold > 100 {
				return invalid("threshold", "must be between 0 and 100")
			}
		case store.MetricRttMs:
			// 相对判定的 threshold 必须为 0，已由 checkKindFields 裁决；它的偏差、冷却与基线字段的范围也在那里。
			if r.RttMode == store.RttThreshold && r.Threshold <= 0 {
				return invalid("threshold", "must be greater than 0")
			}
		default:
			return oneOf("metric", string(r.Metric), string(store.MetricLossPct), string(store.MetricRttMs))
		}
		if r.ForMinutes < 1 || r.ForMinutes > 60 {
			return invalid("for_minutes", "must be between 1 and 60")
		}
		return nil
	case store.KindResource:
		if err := checkKindFields(r); err != nil {
			return err
		}
		max, maxText := -1.0, ""
		allowed := make([]string, 0, len(resourceThresholdMax))
		for _, entry := range resourceThresholdMax {
			allowed = append(allowed, string(entry.metric))
			if r.ResourceMetric == entry.metric {
				max, maxText = entry.max, entry.maxText
			}
		}
		if max < 0 {
			return oneOf("resource_metric", string(r.ResourceMetric), allowed...)
		}
		if math.IsNaN(r.Threshold) || math.IsInf(r.Threshold, 0) || r.Threshold <= 0 || r.Threshold > max {
			return invalid("threshold", "must be greater than 0 and at most %s", maxText)
		}
		if math.IsNaN(r.RecoveryThreshold) || math.IsInf(r.RecoveryThreshold, 0) || r.RecoveryThreshold < 0 || r.RecoveryThreshold >= r.Threshold {
			return invalid("recovery_threshold", "must be nonnegative and less than threshold")
		}
		if r.ForMinutes < 1 || r.ForMinutes > 60 {
			return invalid("for_minutes", "must be between 1 and 60")
		}
		return nil
	default:
		return oneOf("kind", string(r.Kind), string(store.KindOffline), string(store.KindProbe), string(store.KindExpiry), string(store.KindResource), string(store.KindCertExpiry), string(store.KindTraffic))
	}
}

// checkKindFields 把 store.CheckKindFields 的结果转成 FieldError，字段与约束原样沿用。
func checkKindFields(r store.AlertRule) error {
	var kf store.KindFieldError
	if err := store.CheckKindFields(r); errors.As(err, &kf) {
		return FieldError{Path: kf.Field, Constraint: kf.Constraint}
	} else if err != nil {
		return err
	}
	return nil
}

type TelegramConfig struct {
	BotToken string `json:"bot_token"`
	ChatID   string `json:"chat_id"`
}
type WebhookConfig struct {
	URL           string            `json:"url"`
	Method        string            `json:"method"`
	Headers       map[string]string `json:"headers"`
	BodyTemplate  string            `json:"body_template"`
	RemoveHeaders []string          `json:"remove_headers,omitempty"`
}

type Message struct {
	Rule, Node, Kind, Transition, Summary string
	Value                                 float64
	At                                    time.Time
}

const DefaultWebhookTemplate = `{"rule":{{json .Rule}},"node":{{json .Node}},"kind":{{json .Kind}},"transition":{{json .Transition}},"value":{{.Value}},"at":{{.At.Unix}},"summary":{{json .Summary}}}`

func parseBodyTemplate(body string) (*template.Template, error) {
	if body == "" {
		body = DefaultWebhookTemplate
	}
	tmpl, err := template.New("body").Funcs(template.FuncMap{"json": func(s string) string { b, _ := json.Marshal(s); return string(b) }}).Parse(body)
	if err != nil {
		return nil, err
	}
	// 样例与投递共用 Message 的字段类型，能在保存时发现执行到的分支中的字段或类型错误。
	err = tmpl.Execute(io.Discard, Message{Rule: "rule", Node: "node", Kind: "probe", Transition: "firing", Summary: "summary", Value: 1, At: time.Unix(0, 0).UTC()})
	if err != nil {
		return nil, err
	}
	return tmpl, nil
}

func httpToken(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", c)) {
			return false
		}
	}
	return true
}

func decodeTelegram(config string) (TelegramConfig, error) {
	var cfg TelegramConfig
	if err := json.Unmarshal([]byte(config), &cfg); err != nil {
		return cfg, invalid("telegram", "config must be a JSON object: %v", err)
	}
	return cfg, nil
}

func decodeWebhook(config string) (WebhookConfig, error) {
	var cfg WebhookConfig
	if err := json.Unmarshal([]byte(config), &cfg); err != nil {
		return cfg, invalid("webhook", "config must be a JSON object: %v", err)
	}
	// HTTP 头名不区分大小写；解码时拒绝歧义，避免合并或发送时由 map 遍历顺序决定凭据。
	keys := make([]string, 0, len(cfg.Headers))
	for key := range cfg.Headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	headers, original := map[string]string{}, map[string]string{}
	for _, key := range keys {
		canonical := http.CanonicalHeaderKey(key)
		if old, exists := original[canonical]; exists {
			return cfg, invalid("webhook.headers", "keys %q and %q name the same HTTP header", old, key)
		}
		original[canonical], headers[canonical] = key, cfg.Headers[key]
	}
	cfg.Headers = headers
	return cfg, nil
}

func checkWebhook(cfg WebhookConfig) (*template.Template, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, invalid("webhook.url", "must be an absolute http or https URL")
	}
	if cfg.Method != "" && cfg.Method != "POST" && cfg.Method != "PUT" && cfg.Method != "PATCH" {
		return nil, oneOf("webhook.method", cfg.Method, "POST", "PUT", "PATCH")
	}
	if len(cfg.Headers) > 16 {
		return nil, invalid("webhook.headers", "must contain at most 16 entries")
	}
	for key, value := range cfg.Headers {
		if !httpToken(key) {
			return nil, invalid("webhook.headers", "key %q must be an HTTP token", key)
		}
		if strings.ContainsFunc(value, unicode.IsControl) {
			return nil, invalid("webhook.headers", "value for %q must not contain control characters", key)
		}
	}
	for _, key := range cfg.RemoveHeaders {
		if !httpToken(key) {
			return nil, invalid("webhook.remove_headers", "key %q must be an HTTP token", key)
		}
	}
	tmpl, err := parseBodyTemplate(cfg.BodyTemplate)
	if err != nil {
		return nil, invalid("webhook.body_template", "must be a valid Go text/template: %v", err)
	}
	return tmpl, nil
}

type channelConfig struct {
	telegram TelegramConfig
	webhook  WebhookConfig
	template *template.Template
}

// 保存准入与投递构造共用解码、字段检查和模板执行；写侧允许的空 method
// 还须由 SaveChannel 规范后才能投递，避免读侧重复补缺省。
func parseChannelConfig(c store.NotifyChannel) (channelConfig, error) {
	var parsed channelConfig
	if err := checkName(c.Name); err != nil {
		return parsed, err
	}
	if c.RatePerMinute < 0 {
		return parsed, invalid("rate_per_minute", "must not be negative (0 means unlimited)")
	}
	switch c.Kind {
	case store.ChannelTelegram:
		cfg, err := decodeTelegram(c.Config)
		if err != nil {
			return parsed, err
		}
		if cfg.ChatID == "" {
			return parsed, invalid("telegram.chat_id", "must not be empty")
		}
		if cfg.BotToken == "" {
			return parsed, invalid("telegram.bot_token", "must not be empty")
		}
		parsed.telegram = cfg
	case store.ChannelWebhook:
		cfg, err := decodeWebhook(c.Config)
		if err != nil {
			return parsed, err
		}
		tmpl, err := checkWebhook(cfg)
		if err != nil {
			return parsed, err
		}
		parsed.webhook, parsed.template = cfg, tmpl
	default:
		return parsed, oneOf("kind", string(c.Kind), string(store.ChannelTelegram), string(store.ChannelWebhook))
	}
	return parsed, nil
}

// DefaultRatePerMinute 是保存渠道时未给出节奏上限所取的值：Telegram 20，群聊的文档值；Webhook 0（不限），
// 接收方多是机器，没有公认的上限。
func DefaultRatePerMinute(kind store.ChannelKind) int {
	if kind == store.ChannelTelegram {
		return 20
	}
	return 0
}

func CheckChannel(c store.NotifyChannel) error {
	_, err := parseChannelConfig(c)
	return err
}
