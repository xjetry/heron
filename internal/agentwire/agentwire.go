// Package agentwire 定义 AgentService 下行方向上 hub 与 agent 共同依赖的量。
//
// hub 按这里的边界准入 TTL 并编码下发的间隔，agent 按同一组边界限定它收到的间隔与响应体；
// hub 失守时 agent 仍按这组常量行事，所以两侧不能各自持有一份。
package agentwire

import "time"

const (
	// MinTTL 与 MaxTTL 是离线判定时长的准入边界，命令行、直接构造与 agent 的限定共用。
	MinTTL = 10 * time.Second
	MaxTTL = 180 * time.Second
	// ReportsPerTTL 是 TTL 内的上报机会数：容得下 ReportsPerTTL-1 次连续失败。agent 的退避规则见 client.Runner，退避上限按它从间隔推回 TTL。
	ReportsPerTTL = 3
	// MaxResponseBytes 是 agent 读取 AgentService 响应正文的上限，成功与错误响应都计（agent 不接受压缩，
	// 读到的字节就是解码前的全部大小，见 hubclient.New）。hub 侧由测试钉住满载 ReportResponse 的编码不超过它。
	MaxResponseBytes = 64 << 10
)

// ReportInterval 是上报间隔的唯一算法。hub 的下发与上报限速都从它推出。
func ReportInterval(ttl time.Duration) time.Duration { return ttl / ReportsPerTTL }

// ReportIntervalMs 是 ReportResponse.report_interval_ms 的唯一编码：按毫秒截断。
// agent 的边界也取编码后的值，否则 TTL 取边界时截断掉的零头会让合法下发落在边界之外。
func ReportIntervalMs(ttl time.Duration) uint32 {
	return uint32(ReportInterval(ttl) / time.Millisecond)
}

// ClampReportInterval 把 hub 下发的间隔限定在 TTL 取 [MinTTL, MaxTTL] 时的编码值之间，越界（含 0）取最近的边界。
// ok 为假表示下发值越界：合法的 hub 只会下发 ReportIntervalMs(ttl)，而它随 ttl 单调，边界内的 ttl 编码后必在边界内。
func ClampReportInterval(ms uint32) (d time.Duration, ok bool) {
	lo, hi := ReportIntervalMs(MinTTL), ReportIntervalMs(MaxTTL)
	switch {
	case ms < lo:
		return time.Duration(lo) * time.Millisecond, false
	case ms > hi:
		return time.Duration(hi) * time.Millisecond, false
	}
	return time.Duration(ms) * time.Millisecond, true
}
