import { Code, ConnectError } from "@connectrpc/connect";

// 查询失败后只对可能自行恢复的错误重试，面板与公开页两个入口共用。写成白名单：以后新出现的码默认不重试，重试不会把
// 确定性的失败变成成功，只会让页面晚几秒才显示同一个结果。
//
// connect-es 2.2.0 的传输层映射：fetch 本身失败（网络断开、连接被拒）得到 Unknown；没有 Connect 错误正文的 HTTP 502、
// 503、504 与 429（反代或网关的瞬时错误页）得到 Unavailable，HTTP 500 得到 Unknown。Unavailable、DeadlineExceeded、Aborted
// 本身就是"稍后再试"的语义。connect-go 对没有码的错误同样回 Unknown，与网络失败共用一个码、分不开，一并重试。
export const retryableCodes: ReadonlySet<Code> = new Set([Code.Unavailable, Code.DeadlineExceeded, Code.Aborted, Code.Unknown]);

// 不重试的码：配置态（FailedPrecondition，例如 public-dir 与启用主题冲突）、调用方的错与权限（InvalidArgument、NotFound、
// AlreadyExists、PermissionDenied、Unauthenticated——会话不会因重试复活）、服务端的确定性失败（Internal、Unimplemented、
// DataLoss、OutOfRange）、取消（Canceled：页面自己放弃了这次请求），以及 ResourceExhausted。AdminService 用它报主题、
// token、节点等的数量上限，重试腾不出名额。PublicService 用它报按来源计的限流：桶容量 60、每秒补 10 个
// （internal/hub/api/public.go），一个公开页稳态每秒约 0.5 个请求（快照每 2 秒轮询一次，历史图每 60 秒刷新），自己耗不空
// 它。页面收到 429 时，桶是被同一个桶上的其他请求耗掉的：其他标签页、反代后没配 --trusted-proxies 时的全部访客、同一
// IPv4 或 IPv6 /64 后面的其他人。超额持续时，每秒放行的请求数由补充速率决定，重试不增加放行数，只多一个请求去争同一批
// 令牌；一次性的突发一秒内就补回令牌，那时重试能成功，不重试的代价是这一次查询显示错误：轮询的查询下一轮（2 秒后）就恢复，只在加载时取一次的站点设置会带着错误横幅停到刷新页面。
// 只供枚举用例核对每个码都被有意归了类，谓词只看 retryableCodes。
export const nonRetryableCodes: ReadonlySet<Code> = new Set([
  Code.Canceled,
  Code.InvalidArgument,
  Code.NotFound,
  Code.AlreadyExists,
  Code.PermissionDenied,
  Code.ResourceExhausted,
  Code.FailedPrecondition,
  Code.OutOfRange,
  Code.Unimplemented,
  Code.Internal,
  Code.DataLoss,
  Code.Unauthenticated,
]);

// retryQuery 是面板与公开页查询的重试谓词（TanStack Query 的 retry）：可重试的错误至多再试两次，其余不重试。不是
// ConnectError 的异常来自页面自己的代码，同样不重试。
export function retryQuery(failureCount: number, err: unknown): boolean {
  return failureCount < 2 && err instanceof ConnectError && retryableCodes.has(err.code);
}
