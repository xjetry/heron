package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"
	"text/template"

	"github.com/xjetry/heron/internal/hub/outbound"
	"github.com/xjetry/heron/internal/hub/store"
)

type Channel interface {
	Send(context.Context, Message) error
}

// sendFailure 是渠道每条可达失败路径的唯一错误形状：类别在产生失败的地方确定，Classify 据此给出
// 落库结果与是否重试，不从错误文本反推。detail 是落库的原文：hub 不把 URL 写进去（outboundError
// 经 outbound.WithoutURL 剥离），但 HTTP 失败的 detail 是响应体片段，内容由接收方决定。
type sendFailure struct {
	failure store.DeliveryFailure
	status  int // 仅 FailureHTTPStatus 非零。
	detail  string
	err     error // 底层原因，保留 errors.Is(err, ErrInvalid) 等判定；HTTP 应答失败没有。
	// retryAfter 是 429 应答的 Retry-After 原值，其余失败为空；换算要用队列的时钟（HTTP 日期写法相对当前时刻），
	// 所以这里不解析，由 Queue.retryWait 纳入退避。
	retryAfter string
}

func failure(kind store.DeliveryFailure, err error) error {
	return &sendFailure{failure: kind, detail: err.Error(), err: err}
}

// Error 是 TestNotifyChannel 原样给面板的文本，形状沿用 "HTTP 401 Unauthorized: <片段>"、"Post: <原因>"。
func (e *sendFailure) Error() string {
	if e.failure == store.FailureHTTPStatus {
		return fmt.Sprintf("HTTP %d %s: %s", e.status, http.StatusText(e.status), e.detail)
	}
	return e.detail
}
func (e *sendFailure) Unwrap() error { return e.err }

// 可重试只由类别与状态码决定：没收到合法应答可重试，HTTP 失败按 outbound.RetryableStatus；
// 其余失败（请求无法构造、渠道配置无效）原样重发只会得到同样结果。
func (e *sendFailure) retryable() bool {
	switch e.failure {
	case store.FailureTransport:
		return true
	case store.FailureHTTPStatus:
		return outbound.RetryableStatus(e.status)
	}
	return false
}

// URL 可含渠道凭据；构造请求与传输失败共用此出口，只保留操作及底层原因。
func outboundError(kind store.DeliveryFailure, err error) error {
	return failure(kind, outbound.WithoutURL(err))
}

// 两种渠道共用出站边界：不信任响应体长度，错误诊断也只保留有限前缀。
func sendHTTP(ctx context.Context, client *http.Client, method, endpoint string, headers map[string]string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return outboundError(store.FailureRequest, err)
	}
	req.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := client.Do(req)
	if err != nil {
		return outboundError(store.FailureTransport, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	// 越界的状态码不能进 http_status 类别——写侧按同一个 outbound.ValidHTTPStatus 会拒绝它，
	// 结果就写不进库。原文只记状态码：不合法应答的响应体没有可依赖的含义。
	if !outbound.ValidHTTPStatus(resp.StatusCode) {
		return &sendFailure{failure: store.FailureTransport, detail: fmt.Sprintf("malformed HTTP status %d", resp.StatusCode)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		f := &sendFailure{failure: store.FailureHTTPStatus, status: resp.StatusCode, detail: outbound.ResponseSummary(data)}
		if resp.StatusCode == http.StatusTooManyRequests {
			f.retryAfter = resp.Header.Get("Retry-After")
		}
		return f
	}
	// 接收端已用 2xx 确认送达；读体中断不否定确认，重试只会重复通知。
	return nil
}

// mergesBatches 决定一种渠道是否把同一评估周期里同一规则、同一转换方向的多个事件合成一次发送。
// Telegram 合并：它是给人看的 IM，四十个节点一起掉线就是四十条消息，超过群聊每分钟 20 条即 429。
// Webhook 不合并：接收方多是机器，按事件处理，一次请求一个事件的请求体模板（Message 的逐事件字段）是它的契约；
// 429 的问题也主要在 IM 渠道。
func mergesBatches(kind store.ChannelKind) bool { return kind == store.ChannelTelegram }

type telegram struct {
	cfg    TelegramConfig
	client *http.Client
	base   string
}

func NewTelegram(cfg TelegramConfig, client *http.Client, base string) Channel {
	if base == "" {
		base = "https://api.telegram.org"
	}
	return &telegram{cfg, client, strings.TrimRight(base, "/")}
}
func (t *telegram) Send(ctx context.Context, m Message) error {
	body, err := json.Marshal(map[string]string{"chat_id": t.cfg.ChatID, "text": m.Summary})
	if err != nil {
		return failure(store.FailureRequest, err)
	}
	return sendHTTP(ctx, t.client, "POST", t.base+"/bot"+t.cfg.BotToken+"/sendMessage", nil, body)
}

type webhook struct {
	cfg      WebhookConfig
	client   *http.Client
	template *template.Template
}

func NewWebhook(cfg WebhookConfig, client *http.Client) (Channel, error) {
	tmpl, err := checkWebhook(cfg)
	if err != nil {
		return nil, failure(store.FailureChannelInvalid, err)
	}
	return webhookFrom(cfg, client, tmpl)
}
func webhookFrom(cfg WebhookConfig, client *http.Client, tmpl *template.Template) (Channel, error) {
	// SaveChannel 在写侧规范 method；不能让未规范的配置被 net/http 默认为 GET。
	if cfg.Method == "" {
		return nil, failure(store.FailureChannelInvalid, invalid("webhook.method", "must be normalized to POST, PUT or PATCH before delivery"))
	}
	cfg.Headers = maps.Clone(cfg.Headers)
	return &webhook{cfg, client, tmpl}, nil
}
func (w *webhook) Send(ctx context.Context, m Message) error {
	var body bytes.Buffer
	if err := w.template.Execute(&body, m); err != nil {
		return failure(store.FailureRequest, err)
	}
	return sendHTTP(ctx, w.client, w.cfg.Method, w.cfg.URL, w.cfg.Headers, body.Bytes())
}

func ParseChannel(c store.NotifyChannel, client *http.Client, telegramBase string) (Channel, error) {
	parsed, err := parseChannelConfig(c)
	if err != nil {
		return nil, failure(store.FailureChannelInvalid, err)
	}
	if c.Kind == store.ChannelTelegram {
		return NewTelegram(parsed.telegram, client, telegramBase), nil
	}
	return webhookFrom(parsed.webhook, client, parsed.template)
}
