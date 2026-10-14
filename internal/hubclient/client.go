// Package hubclient 构造连 hub 的 AgentService 客户端。agent（register、run）与节点上的 root 更新器
// （hub 来源）共用这一个构造，下面几条约束因此对两者同时成立：
//   - 响应正文在 HTTP 层限读 maxBody 字节，成功与错误响应都经过这一层。connect-go 的 ReadMaxBytes 只管成功
//     响应的消息，错误正文与解压后的大小都不受它约束。
//   - 不声明压缩：hub 没法用一个小的压缩正文换出大块内存；hub 不顾声明仍回 gzip 时 connect 按不认识的编码报错。
//   - 不跟随重定向。http.Client 跟随同主机重定向时保留 Authorization 而不看协议，https 到同主机 http 的重定向
//     会把节点 token 明文发出；AgentService 没有需要重定向的场景。
//   - 响应头上限 32 KiB。
//
// 其余传输设置克隆 http.DefaultTransport（代理取自进程环境）。
package hubclient

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"connectrpc.com/connect"

	"github.com/xjetry/heron/gen/heron/v1/heronv1connect"
)

// New 返回连 hub 的 AgentService 客户端。maxBody 是响应正文在 HTTP 层的读取上限，由调用方按自己要收的
// 最大消息给出：agent 传 agentwire.MaxResponseBytes，hub 来源的更新器传它那档产物大小。timeout 是含读完正文的
// 总时限；0 表示不设，此时期限只来自调用方的 ctx，没有期限时卡住的应答会一直等下去——hub 来源的更新器传 0，
// 由 update.HubSource 拒绝没有期限的 ctx。
func New(hub string, timeout time.Duration, maxBody int64) heronv1connect.AgentServiceClient {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DisableCompression = true
	tr.MaxResponseHeaderBytes = maxResponseHeaderBytes
	hc := &http.Client{
		Timeout:       timeout,
		Transport:     limitedTransport{base: tr, max: maxBody},
		CheckRedirect: refuseRedirect,
	}
	return heronv1connect.NewAgentServiceClient(hc, hub, connect.WithAcceptCompression("gzip", nil, nil))
}

// maxResponseHeaderBytes 是响应头的上限。Go 的默认值是 10 MiB，远大于正文的常见上限；AgentService 的响应头
// 只有 Content-Type 与反代加的几行，32 KiB 容得下常见 CDN 附加的头。
const maxResponseHeaderBytes = 32 << 10

var errRedirect = errors.New("hub answered with a redirect; the client does not follow redirects, so configure the final hub URL")

func refuseRedirect(*http.Request, []*http.Request) error { return errRedirect }

type limitedTransport struct {
	base http.RoundTripper
	max  int64
}

func (t limitedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	resp.Body = &limitedBody{rc: resp.Body, left: t.max, max: t.max}
	return resp, nil
}

// limitedBody 读满 left 字节后再读到任何数据即报错，而不是静默截断：截断的正文可能恰好仍能解码成一条更短的合法消息。
type limitedBody struct {
	rc   io.ReadCloser
	left int64
	// max 只用于超限报错：上限是每个客户端自己的参数，错误消息里带的必须是它的值。
	max int64
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if b.left <= 0 {
		var one [1]byte
		if n, err := b.rc.Read(one[:]); n > 0 {
			return 0, fmt.Errorf("hub response body exceeds %d bytes", b.max)
		} else {
			return 0, err
		}
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.rc.Read(p)
	b.left -= int64(n)
	return n, err
}

func (b *limitedBody) Close() error { return b.rc.Close() }
