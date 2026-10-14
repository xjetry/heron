package outbound

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// SummaryChars 是 HTTP 失败原文取应答体的字符数：原文是响应体的前 200 个字符。
const SummaryChars = 200

// ResponseSummary 取应答体的前 SummaryChars 个字符。range 只用来定位字符边界，不改写非法字节；截断之后再把非法字节
// 换成 U+FFFD：原文要落库、进 proto 的 string 字段，proto 序列化拒绝非法 UTF-8。按字节截断会在多字节字符中间切开，
// 应答体是中文错误页时整条读侧应答就序列化失败。替换只会把连续的非法字节并成一个字符，结果不超过 SummaryChars 个字符。
func ResponseSummary(data []byte) string {
	text := string(data)
	n := 0
	for i := range text {
		if n == SummaryChars {
			text = text[:i]
			break
		}
		n++
	}
	return strings.ToValidUTF8(text, "�")
}

// WithoutURL 把 net/http 的 *url.Error 换成"操作: 底层原因"：URL 里可能有凭据（Telegram 的 bot token 在路径里）或
// 对象键，hub 生成的出站错误文本不含 URL。底层原因以 %w 保留，errors.Is 仍能判出 context.Canceled 等；
// 不是 *url.Error 的错误原样返回。
func WithoutURL(err error) error {
	var target *url.Error
	if errors.As(err, &target) {
		return fmt.Errorf("%s: %w", target.Op, target.Err)
	}
	return err
}

// ValidHTTPStatus 是状态码合法性的唯一判定。Go 的 HTTP/1.1 客户端接受任意三位数字的状态行（000–099 也照样交回），
// 越界的不是合法应答，与连接中断同类（按 transport 失败记）。产生侧（alert 的 sendHTTP、s3 的 request）据它决定一次
// 应答算不算 HTTP 失败，写侧（store 的 DeliveryResult.check）据它守住 http_status 列；两处同一口径，否则产生侧
// 定下的类别会被写侧拒绝，结果写不进库。
func ValidHTTPStatus(code int) bool { return code >= 100 && code <= 999 }

// RetryableStatus 是 HTTP 失败是否值得原样重发：5xx、408、429 可重试；其余非 2xx（3xx 与另外的 4xx）原样
// 重发只会得到同样的结果。通知投递与 S3 备份共用这一条。
func RetryableStatus(code int) bool { return code >= 500 || code == 408 || code == 429 }
