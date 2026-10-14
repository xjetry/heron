// Package s3 实现备份所需的 S3 SigV4 操作，不依赖云厂商 SDK。
package s3

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/outbound"
)

// responseLimit 是非 2xx 应答体（摘要取前 outbound.SummaryChars 个字符）与 PutObject、DeleteObject 成功应答体排空时的
// 读取上限，与通知渠道相同：S3 的错误体与这两个操作的成功体都只有几百字节的 XML，超出部分不读（net/http 因此可能
// 不复用这条连接）。
const responseLimit = 64 << 10

// 列举一页的条数与应答上限。一条 <Contents> 的上界：S3 的对象键至多 maxKeyBytes 字节（UTF-8），encoding-type=url 时
// 每字节最坏编成 3 字节的 %XX；其余子元素（LastModified、ETag、Size、StorageClass，以及各家追加的校验和字段）连同
// 标签按 contentsOverhead 计，约为实际大小（三四百字节）的三倍。页级元素（Name、回显的 Prefix、两个 continuation
// token、KeyCount 等）按 pageOverhead 计，其中回显的 Prefix 同样至多 3×maxKeyBytes。按份数上限 1000 列一层，
// 256 条一页要 4 个来回；应答上限 listPageKeys×(3×maxKeyBytes+contentsOverhead)+pageOverhead = 1064960 字节，
// 超过即当作不合法的应答，不截断解析。
const (
	maxKeyBytes       = 1024
	contentsOverhead  = 1 << 10
	pageOverhead      = 16 << 10
	listPageKeys      = 256
	listResponseLimit = listPageKeys*(3*maxKeyBytes+contentsOverhead) + pageOverhead
)

type Config struct {
	Endpoint  string
	Bucket    string
	Region    string
	AccessKey string
	Secret    string
	// VirtualHost 为 false 时用 path-style，适用于 R2 与自定义 endpoint。
	VirtualHost bool
}

var bucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

// ValidateTarget 不回显配置值，避免 URL 中误填的凭据进入错误文本。
func ValidateTarget(endpoint, bucket string) error {
	if err := ValidateEndpoint(endpoint); err != nil {
		return err
	}
	return ValidateBucket(bucket)
}

// ValidateEndpoint 同样不回显配置值。主机只收签名与线上一致的写法：签名覆盖 URL 里的 host，而 net/http 在线上
// 把非 ASCII 主机转成 punycode、把 IPv6 zone 从 Host 头里去掉（Go 1.27.1 实测），两者不一致时每个请求都会被
// 服务端以签名不符拒绝。
func ValidateEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return errors.New("backup.endpoint must be an http(s) URL without userinfo, query or fragment")
	}
	if strings.ContainsFunc(u.Host, func(r rune) bool { return r >= utf8.RuneSelf || r == '%' }) {
		return errors.New("backup.endpoint host must be ASCII without an IPv6 zone; write internationalized names in punycode (xn--...)")
	}
	return nil
}

func ValidateBucket(bucket string) error {
	if !bucketName.MatchString(bucket) || strings.Contains(bucket, "..") {
		return errors.New("backup.bucket must be a non-empty S3 bucket name (3-63 lowercase letters, digits, dots or hyphens)")
	}
	return nil
}

func ValidateSigningIdentifier(field, value string) error {
	if strings.Contains(value, "/") || strings.ContainsFunc(value, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return fmt.Errorf("backup.%s must not contain slash, whitespace or control characters", field)
	}
	return nil
}

func (c Config) Enabled() bool {
	return c.Endpoint != "" && c.Bucket != "" && c.AccessKey != "" && c.Secret != ""
}

// 出站客户端的两个时限（outbound.NewTransferClient）。传输本身不限时：对象从几百 KB 到 GB 级，总时长由调用方按
// 对象大小给 context 截止时间，四个操作在入口拒绝没有截止时间的 context。
const (
	// connectTimeout 分别限 DNS 加 TCP 建连与 TLS 握手：这两步只交换几个往返的小包（DNS 一个、TCP 一个、TLS 1.2 两个），
	// 与对象大小无关。按 1 秒的高延迟往返计约 4 秒，取 10 秒留出一倍多的余量。
	connectTimeout = 10 * time.Second
	// firstByteTimeout 限请求连同正文写完之后到收到响应头。PutObject 的应答在服务端收完最后一个字节之后才发出，
	// 服务端在应答前可能还要把整个对象转交出去：缓冲请求体的反代或网关型实现按指标层约 2 GB、100 Mbit/s 的
	// 内侧链路计约 160 秒，取 5 分钟。它只在服务端收下请求却迟迟不应答时起作用，不限上传与下载本身。
	firstByteTimeout = 5 * time.Minute
)

type Client struct {
	cfg      Config
	endpoint *url.URL
	http     *http.Client
	clk      clock.Clock
}

// New 的时钟只用于签名时刻（x-amz-date）。
func New(cfg Config, clk clock.Clock) (*Client, error) {
	if !cfg.Enabled() {
		return nil, errors.New("backup endpoint, bucket, access key and secret are required")
	}
	if err := ValidateTarget(cfg.Endpoint, cfg.Bucket); err != nil {
		return nil, err
	}
	if cfg.Region == "" {
		return nil, errors.New("backup.region must not be empty")
	}
	for _, f := range []struct{ name, value string }{{"region", cfg.Region}, {"access_key", cfg.AccessKey}} {
		if err := ValidateSigningIdentifier(f.name, f.value); err != nil {
			return nil, err
		}
	}
	u, _ := url.Parse(cfg.Endpoint)
	return &Client{cfg: cfg, endpoint: u, http: outbound.NewTransferClient(connectTimeout, firstByteTimeout), clk: clk}, nil
}

// Error 的类别在产生失败的地方确定：http_status（服务端以非 2xx 应答）、transport（没有收到合法应答）、
// request（请求没能构造或调用方的参数不合用）、response（2xx 的应答体不合法或超出上限）。
type Error struct {
	Kind       string
	StatusCode int
	// HTTP 失败时是应答体的前 outbound.SummaryChars 个字符（合法 UTF-8），其余是不含 URL 的"操作: 底层原因"。
	// 应答体由服务端决定，可能回显请求内容，不应公开下发。
	Detail string
	err    error
}

func (e *Error) Error() string {
	if e.StatusCode != 0 {
		return fmt.Sprintf("s3 %s HTTP %d: %s", e.Kind, e.StatusCode, e.Detail)
	}
	return "s3 " + e.Kind + ": " + e.Detail
}
func (e *Error) Unwrap() error { return e.err }

// Retryable 与通知投递同一规则：没收到合法应答可重试，HTTP 失败按 outbound.RetryableStatus。
func (e *Error) Retryable() bool {
	return e.Kind == "transport" || e.Kind == "http_status" && outbound.RetryableStatus(e.StatusCode)
}

// fail 的 Detail 是 what 加上不含 URL 的底层原因；what 为空时只有底层原因（传输失败的"操作: 原因"）。
// 保存的底层错误也是去掉 URL 的那个：Unwrap 出去的错误同样不含 URL。
func fail(kind, what string, err error) error {
	if err == nil {
		return &Error{Kind: kind, Detail: what}
	}
	err = outbound.WithoutURL(err)
	detail := err.Error()
	if what != "" {
		detail = what + ": " + detail
	}
	return &Error{Kind: kind, Detail: detail, err: err}
}

// checkDeadline 在各操作读取输入之前调用：传输首字节之后没有客户端总时限，且上传散列可能遍历 GB 级文件，
// 缺少截止时间必须在这些工作开始前拒绝，不能等到发请求才发现预算缺失。
func checkDeadline(ctx context.Context) error {
	if _, ok := ctx.Deadline(); !ok {
		return fail("request", "context has no deadline", nil)
	}
	return nil
}

func (c *Client) request(ctx context.Context, method, key string, query url.Values, body io.Reader, size int64, hash string) (*http.Response, error) {
	if err := checkDeadline(ctx); err != nil {
		return nil, err
	}
	u := *c.endpoint
	base := strings.TrimRight(u.Path, "/")
	if c.cfg.VirtualHost {
		u.Host = c.cfg.Bucket + "." + u.Host
	} else {
		base += "/" + c.cfg.Bucket
	}
	u.Path = base + "/" + key
	u.RawPath = escape(u.Path, true)
	u.RawQuery = canonicalQuery(query)
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, fail("request", "", err)
	}
	req.ContentLength = size
	req.Header.Set("X-Amz-Date", c.clk.Now().UTC().Format("20060102T150405Z"))
	req.Header.Set("X-Amz-Content-Sha256", hash)
	canonical, signed := canonicalRequest(req, hash)
	_, auth := authorization(req, canonical, signed, c.cfg.Region, "s3", c.cfg.AccessKey, c.cfg.Secret)
	req.Header.Set("Authorization", auth)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fail("transport", "", err)
	}
	if !outbound.ValidHTTPStatus(resp.StatusCode) {
		resp.Body.Close()
		return nil, fail("transport", fmt.Sprintf("malformed HTTP status %d", resp.StatusCode), nil)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, responseLimit))
		return nil, &Error{Kind: "http_status", StatusCode: resp.StatusCode, Detail: outbound.ResponseSummary(data)}
	}
	return resp, nil
}

// PutObject 从 body 的当前位置上传到末尾，不关闭 body。先散列再回到原位上传同一段内容：不把整个数据库读进内存，
// 也不用 UNSIGNED-PAYLOAD。前提是两次读取之间内容不变，由调用方保证——持有快照临时文件的消费方在 PutObject 返回
// 之前不改写它。PutObject 返回之后不再读 body（见 uploadBody），调用方可以立即 Seek 回原位重试，或自行关闭它。
func (c *Client) PutObject(ctx context.Context, key string, body io.ReadSeeker) error {
	if err := checkDeadline(ctx); err != nil {
		return err
	}
	if body == nil {
		return fail("request", "object body is required", nil)
	}
	start, err := body.Seek(0, io.SeekCurrent)
	if err != nil {
		return fail("request", "cannot seek object", err)
	}
	h := sha256.New()
	n, err := io.Copy(h, &contextReader{ctx, body})
	if err != nil {
		return fail("request", "cannot hash object", err)
	}
	if _, err := body.Seek(start, io.SeekStart); err != nil {
		return fail("request", "cannot rewind object", err)
	}
	// 空对象交 http.NoBody：长度为 0 而正文非 nil 时 net/http 当作长度未知，改用 chunked 编码，S3 不收。
	var payload io.Reader = http.NoBody
	if n > 0 {
		upload := &uploadBody{r: body}
		defer upload.detach()
		payload = upload
	}
	resp, err := c.request(ctx, http.MethodPut, key, nil, payload, n, hex.EncodeToString(h.Sum(nil)))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, responseLimit))
	return nil
}

var errBodyDetached = errors.New("s3: upload body read after PutObject returned")

// uploadBody 是交给 transport 的请求正文，它把调用方的 reader 与 RoundTripper 的两条约定隔开：
//   - RoundTripper 会关闭请求正文。调用方传的多半是快照临时文件（*os.File），关掉它，重试时就 Seek 不回原位；
//     所以 Close 不往下传。
//   - RoundTripper 可以在 RoundTrip 返回之后，在另一个协程里继续读正文（服务端没收完正文就应答时）。PutObject 返回
//     前 detach：它等正在进行的那次 Read 结束，此后的 Read 一律失败，底层 reader 在 PutObject 返回之后不会再被读，
//     与调用方随后的 Seek、重读不交错。
type uploadBody struct {
	mu       sync.Mutex
	r        io.Reader
	detached bool
}

func (b *uploadBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.detached {
		return 0, errBodyDetached
	}
	return b.r.Read(p)
}

func (b *uploadBody) Close() error { return nil }

func (b *uploadBody) detach() {
	b.mu.Lock()
	b.detached = true
	b.mu.Unlock()
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func (c *Client) DeleteObject(ctx context.Context, key string) error {
	if err := checkDeadline(ctx); err != nil {
		return err
	}
	resp, err := c.request(ctx, http.MethodDelete, key, nil, nil, 0, hashHex(nil))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, responseLimit))
	return nil
}

// GetObject 的上限由消费方按产物类型指定；超过上限、读中断或写失败均返回错误，
// 调用方必须丢弃失败时的部分产物，不得把截断文件当作成功备份。
func (c *Client) GetObject(ctx context.Context, key string, dst io.Writer, maxBytes int64) error {
	if err := checkDeadline(ctx); err != nil {
		return err
	}
	if dst == nil || maxBytes <= 0 || maxBytes == math.MaxInt64 {
		return fail("request", "destination and positive bounded maxBytes are required", nil)
	}
	resp, err := c.request(ctx, http.MethodGet, key, nil, nil, 0, hashHex(nil))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// 声明的长度已超上限就不读正文：dst 里不留任何部分产物。没有长度头的超限由下面多读一个字节判出。
	if resp.ContentLength > maxBytes {
		return fail("response", "object exceeds maxBytes", nil)
	}
	n, err := io.Copy(dst, io.LimitReader(resp.Body, maxBytes))
	if err != nil {
		return fail("response", "cannot copy object", err)
	}
	if n == maxBytes {
		var extra [1]byte
		if _, err := io.ReadFull(resp.Body, extra[:]); err != io.EOF {
			return fail("response", "object exceeds maxBytes or cannot finish reading", err)
		}
	}
	return nil
}

type Object struct {
	Key          string    `xml:"Key"`
	Size         int64     `xml:"Size"`
	LastModified time.Time `xml:"LastModified"`
}

// ListObjectsV2 列出 prefix 下的全部对象。maxObjects 由消费方按自己的保留份数给出上界，列举超过它即返回错误，
// 不无界地翻页：前缀下混进大量别的对象、或删除长期失败而对象越积越多时，报错让它可见。
func (c *Client) ListObjectsV2(ctx context.Context, prefix string, maxObjects int) ([]Object, error) {
	if err := checkDeadline(ctx); err != nil {
		return nil, err
	}
	if maxObjects <= 0 {
		return nil, fail("request", "positive maxObjects is required", nil)
	}
	var objects []Object
	seen := map[string]bool{}
	token := ""
	for {
		query := url.Values{"list-type": {"2"}, "prefix": {prefix}, "max-keys": {strconv.Itoa(listPageKeys)}, "encoding-type": {"url"}}
		if token != "" {
			query.Set("continuation-token", token)
		}
		resp, err := c.request(ctx, http.MethodGet, "", query, nil, 0, hashHex(nil))
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, listResponseLimit+1))
		resp.Body.Close()
		if err != nil {
			return nil, fail("response", "cannot read list response", err)
		}
		if len(data) > listResponseLimit {
			return nil, fail("response", fmt.Sprintf("list response exceeds %d bytes", listResponseLimit), nil)
		}
		var page struct {
			XMLName   xml.Name `xml:"ListBucketResult"`
			Truncated bool     `xml:"IsTruncated"`
			Next      string   `xml:"NextContinuationToken"`
			Encoding  string   `xml:"EncodingType"`
			Objects   []Object `xml:"Contents"`
		}
		if err := xml.Unmarshal(data, &page); err != nil {
			return nil, fail("response", "invalid list XML", err)
		}
		for _, obj := range page.Objects {
			// encoding-type=url 的键是表单编码：空格写成 +，字面的 + 写成 %2B。PathUnescape 不把 + 还原成空格，
			// 含空格的键会被解成另一个键，按它删除得到的是对不存在的键的 204，保留删除静默失效。
			if page.Encoding == "url" {
				obj.Key, err = url.QueryUnescape(obj.Key)
				if err != nil {
					return nil, fail("response", "invalid encoded object key", err)
				}
			}
			objects = append(objects, obj)
		}
		if len(objects) > maxObjects {
			return nil, fail("response", fmt.Sprintf("listing exceeds %d objects", maxObjects), nil)
		}
		if !page.Truncated {
			return objects, nil
		}
		if page.Next == "" || seen[page.Next] {
			return nil, fail("response", "missing or repeated continuation token", nil)
		}
		seen[page.Next] = true
		token = page.Next
	}
}
