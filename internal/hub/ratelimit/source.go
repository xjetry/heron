package ratelimit

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"

	"connectrpc.com/connect"

	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/auth"
)

type sourceCtxKey struct{}

// BySource 是匿名入口的限流中间件：按来源从 b 取令牌，取不到时按请求的协议写出 ResourceExhausted，不进 next。
//
// 它包在 connect 处理器外面：connect 先读取并解码请求，再进拦截器，放在拦截器里的限流数不到解码失败的请求。
// 包在外面，到达挂载点的每个请求都计数，超限的请求也不再消耗解码。
// 来源地址只在对端属于 trusted 时才取 X-Forwarded-For（auth.ClientIP），否则客户端改一个头就能换桶；
// 传给它的是该头的全部字段行，可信代理另起一行追加的地址才看得到。
// 桶按 auth.SourceKey 归一化后的键计：IPv4 一个地址一桶，IPv6 一个 /64 一桶。
// 放行的请求带着这个键进入 next（SourceOf 取出），下游按同一个键做后续裁决，不再各算一遍。
func BySource(b *Buckets[netip.Addr], trusted []netip.Prefix, clk clock.Clock, next http.Handler) http.Handler {
	errs := connect.NewErrorWriter()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := auth.SourceKey(auth.ClientIP(r.RemoteAddr, r.Header.Values("X-Forwarded-For"), trusted))
		if !b.Allow(key, clk.Mono()) {
			errs.Write(w, r, connect.NewError(connect.CodeResourceExhausted, fmt.Errorf(
				"rate limit exceeded for %s: each source (one IPv4 address, or one IPv6 /64) may make %d requests at once and then one every %v",
				auth.DescribeSource(key), int(b.capacity), b.refillPer)))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sourceCtxKey{}, key)))
	})
}

// SourceOf 取出 BySource 放行时记下的来源键（auth.SourceKey 归一化之后：IPv4 是地址本身，IPv6 是所在 /64 的网络地址）；
// 请求没有经过 BySource 时 ok 为 false。
func SourceOf(ctx context.Context) (netip.Addr, bool) {
	a, ok := ctx.Value(sourceCtxKey{}).(netip.Addr)
	return a, ok
}
