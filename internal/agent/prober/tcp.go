package prober

import (
	"context"
	"net"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/clock"
)

// TCP 只测量连接建立耗时；解析与连接共用预算，解析失败、地址非法与本地策略拒绝是 error。
// 连接或发送失败经 classify 区分可达性与本地故障。
type TCP struct {
	Clock       clock.Clock
	Targets     Targets
	DialContext func(context.Context, string, string) (net.Conn, error)
}

func (p TCP) Probe(ctx context.Context, t *heronv1.ProbeTask) Outcome {
	timeout := time.Duration(t.GetTimeoutMs()) * time.Millisecond
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	host, port, err := net.SplitHostPort(t.GetTarget())
	if err != nil {
		return Outcome{Err: err.Error()}
	}
	ip, err := p.Targets.Resolve(ctx, host, true, true)
	if err != nil {
		return Outcome{Err: err.Error()}
	}
	start := p.Clock.Mono()
	dial := p.DialContext
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	conn, err := dial(ctx, "tcp", net.JoinHostPort(ip.String(), port))
	if err != nil {
		return classify(err)
	}
	elapsed := p.Clock.Mono() - start
	conn.Close()
	// CheckTask 把任务超时限制在 hub 接受的 RTT 上限内；测得耗时超过任务预算时只回报超时。
	if elapsed > timeout {
		return Outcome{Timeout: true}
	}
	return Outcome{RttUs: uint32(elapsed / time.Microsecond)}
}
