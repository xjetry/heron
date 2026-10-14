// Package prober 在 agent 侧执行 hub 下发的延迟探测任务并暂存结果。
//
// 结果的时间基准是单调钟：入队时记 At，上报时折算成 age_ms，hub 用它反推测量时刻。
package prober

import (
	"context"
	"fmt"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
)

type Outcome struct {
	RttUs   uint32
	Timeout bool
	Err     string
	// CertNotAfter 只在 RttUs 成功结果上有意义：HTTPS 探测顺带观测到的服务端证书到期时刻
	// （Unix 秒），0 表示未携带（非 HTTPS 目标，或距上次携带不足 CertReportInterval）。
	CertNotAfter int64
	// Presented 只在证书相关的丢包（Timeout）上有意义：握手时对方出示的叶证书观测，
	// 供人工确认信任，从不自动生效。
	Presented *PresentedCert
}

// PresentedCert 是证书相关丢包带回的对方叶证书观测（ProbeResult.presented 的引擎侧形状）。
type PresentedCert struct {
	// SPKI 是叶证书 SubjectPublicKeyInfo 的 SHA-256。
	SPKI [32]byte
	// NotAfterS 是叶证书 NotAfter，Unix 秒。
	NotAfterS int64
	// Reason 是探测当时的判定；面板按它显示，不用收到时刻重新推断。
	Reason heronv1.PresentedReason
}

// taskPruner 由 Scheduler.Apply 在任务集更新后调用：引擎清掉不再需要的每任务状态
// （如 HTTP 的证书携带记录）。alive 把每个存活任务映射到它当前的配置身份
// （config_id 原样字节；空 = 无身份）：换了身份的旧状态同样删掉。不实现的引擎没有每任务状态，无需清理。
type taskPruner interface {
	pruneTasks(alive map[uint64]string)
}

// Engine 的 ctx 被调用方取消后返回的 Outcome 无意义，调用方必须丢弃。
type Engine interface {
	Probe(ctx context.Context, t *heronv1.ProbeTask) Outcome
}

// Multi 按任务种类分派；未知种类是 hub 与 agent 版本偏斜的信号，按 error 回报而不是静默跳过。
type Multi struct{ ICMP, TCP, HTTP, DNS Engine }

func (m Multi) Probe(ctx context.Context, t *heronv1.ProbeTask) Outcome {
	switch t.GetKind() {
	case heronv1.ProbeKind_PROBE_KIND_ICMP:
		return m.ICMP.Probe(ctx, t)
	case heronv1.ProbeKind_PROBE_KIND_TCP:
		return m.TCP.Probe(ctx, t)
	case heronv1.ProbeKind_PROBE_KIND_HTTP:
		return m.HTTP.Probe(ctx, t)
	case heronv1.ProbeKind_PROBE_KIND_DNS:
		return m.DNS.Probe(ctx, t)
	}
	return Outcome{Err: fmt.Sprintf("unsupported probe kind %s", t.GetKind())}
}

// pruneTasks 把任务集清理转发给实现了 taskPruner 的引擎。
func (m Multi) pruneTasks(alive map[uint64]string) {
	for _, e := range []Engine{m.ICMP, m.TCP, m.HTTP, m.DNS} {
		if p, ok := e.(taskPruner); ok {
			p.pruneTasks(alive)
		}
	}
}
