package alert

import (
	"context"
	"fmt"
	"time"

	"github.com/xjetry/heron/internal/hub/store"
)

// calendarRule 是日历类规则：到期与证书到期都按日历日评估（sweepExpiry 的节奏），
// 也都与节点到期一样不受维护静默约束——日历提醒的触发条件每天都在走近，静默窗口压不住它。
func calendarRule(k store.AlertKind) bool { return k == store.KindExpiry || k == store.KindCertExpiry }

// sweepCertExpiry 评估全部启用的证书到期规则，与到期规则同一次 sweepExpiry、同一个 today。
// 观测来自 probe_cert 的最新一份：(节点, 任务) 没有行即无读数——不评估、不恢复，已有状态原样保留；
// 证书更换后 days_left 变大，下一轮（或由 ingest 的变化触发的那一轮）按同一状态机恢复。
// 候选集撤销（任务删除先被规则引用检查拦住、节点删除经 RemoveNode 清理）由 pruneCandidates 承载。
func (e *Engine) sweepCertExpiry(ctx context.Context, cy *cycle, nodes []store.Node, today time.Time, errs []error) []error {
	for _, r := range e.Rules() {
		if !r.Enabled || r.Kind != store.KindCertExpiry {
			continue
		}
		certs, err := e.st.ProbeCertsByTask(ctx, r.TaskID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		candidates := map[int64]bool{}
		for _, n := range nodes {
			if !inScope(r, n.ID) {
				continue
			}
			candidates[n.ID] = true
			notAfter, ok := certs[n.ID]
			if !ok {
				continue
			}
			// 到期时刻换成 hub 时区的日历日再与 today 相减，与到期规则同一口径。
			expires := Today(time.Unix(notAfter, 0), e.cfg.Location)
			expiresOn := expires.Format(time.DateOnly)
			o := ExpiryObservation{HasExpiry: true, DaysLeft: daysBetween(today, expires), DaysBefore: r.DaysBefore}
			cur := e.entry(stateKey{r.ID, n.ID})
			next, tr := NextExpiry(cur.state, o)
			summary, value := certSummary(n, r, expiresOn, o, tr, cur.firedExpiresOn)
			// 与到期规则同一机制：进入 firing 时把当时的证书到期日记进状态，恢复文案据此区分
			// "已不在提醒窗口内"与"证书已更换"。
			fired := ""
			if next == store.StateFiring {
				fired = expiresOn
			}
			if err := e.apply(ctx, cy, r, n.ID, next, false, false, fired, tr, summary, value); err != nil {
				errs = append(errs, err)
			}
		}
		if err := e.pruneCandidates(ctx, r.ID, candidates); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// certSummary 是证书到期的文案，与 expirySummary 同一形状：触发按进入窗口时的剩余天数写"将于"或"已于"；
// 恢复按离开窗口的原因——firedOn 是触发时记下的证书到期日，相同写"已不在提醒窗口内"，
// 不同说明证书已更换、到期日更新。无读数（没有 probe_cert 行）不产生任何转换，所以没有"清除到期日"分支。
func certSummary(n store.Node, r store.AlertRule, expiresOn string, o ExpiryObservation, tr *store.Transition, firedOn string) (string, float64) {
	recovered := tr != nil && *tr == store.TransitionRecovered
	switch {
	case recovered && expiresOn == firedOn:
		return fmt.Sprintf("节点 %s 的证书已不在提醒窗口内（规则 %s）", n.Name, r.Name), float64(o.DaysLeft)
	case recovered:
		return fmt.Sprintf("节点 %s 的证书到期日已更新为 %s（规则 %s）", n.Name, expiresOn, r.Name), float64(o.DaysLeft)
	case o.DaysLeft < 0:
		return fmt.Sprintf("节点 %s 的证书已于 %s 到期（已过期 %d 天，规则 %s）", n.Name, expiresOn, -o.DaysLeft, r.Name), float64(o.DaysLeft)
	}
	return fmt.Sprintf("节点 %s 的证书将于 %s 到期（剩 %d 天，规则 %s）", n.Name, expiresOn, o.DaysLeft, r.Name), float64(o.DaysLeft)
}
