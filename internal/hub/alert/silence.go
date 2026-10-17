package alert

import (
	"context"
	"errors"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/xjetry/heron/internal/hub/store"
)

// SilenceActive 只判定一条静默在 now 是否处于窗口内，API 展示与运行时抑制（engine.apply）共用它。
// enabled 与规则、节点覆盖由调用方判定。DAILY 按 loc（hub 的 --timezone）的墙钟计
// 午夜起的分钟数，start 含、end 不含，允许跨午夜（22:00–06:00）；ONCE 按 Unix 秒闭开区间。
func SilenceActive(s store.Silence, now time.Time, loc *time.Location) bool {
	switch s.Kind {
	case store.SilenceOnce:
		t := now.Unix()
		return t >= s.FromAt && t < s.UntilAt
	case store.SilenceDaily:
		start, err := store.ParseHHMM(s.StartHHMM)
		if err != nil {
			return false
		}
		end, err := store.ParseHHMM(s.EndHHMM)
		if err != nil {
			return false
		}
		wall := now.In(loc)
		m := wall.Hour()*60 + wall.Minute()
		if start < end {
			return m >= start && m < end
		}
		return m >= start || m < end
	}
	return false
}

// CheckSilence 只校验静默的展示字段；种类与专用字段组合由 store.CheckSilenceFields 在保存时裁决。
// 运行时的窗口判定由 SilenceActive 承载，展示字段不能偷偷扩大作用域或时间窗口。
func CheckSilence(s store.Silence) error {
	if err := checkName(s.Name); err != nil {
		return err
	}
	if utf8.RuneCountInString(s.Reason) > 256 {
		return invalid("reason", "must be at most 256 characters")
	}
	return nil
}

// silenced 在 Engine.mu 下读取已持久化后发布的快照，只裁决投递，不改变状态转移。
// 节点维护开关覆盖全部规则；静默则必须同时覆盖规则与节点。空规则集不表示全部，规则删光后不能扩大覆盖。
func (e *Engine) silenced(ruleID, nodeID int64, maintenance bool, now time.Time) bool {
	if maintenance {
		return true
	}
	for _, s := range e.silences {
		if !s.Enabled || (!s.AllRules && !slices.Contains(s.RuleIDs, ruleID)) {
			continue
		}
		if store.ScopeContains(s.AllNodes, s.NodeIDs, nodeID) && SilenceActive(s, now, e.cfg.Location) {
			return true
		}
	}
	return false
}

func cloneSilence(s store.Silence) store.Silence {
	s.NodeIDs = slices.Clone(s.NodeIDs)
	s.RuleIDs = slices.Clone(s.RuleIDs)
	s.SelectorTags = slices.Clone(s.SelectorTags)
	return s
}

// 快照整表替换：与 publishRule 的逐条合并不同，静默之间没有需要沿用的部分状态（状态在 alert_state 上），
// 整表替换与库的一致性最容易论证。调用方须已持久化成功。
//
// 快照允许落后于库：维护任务（store.PruneAlertEvents）不经引擎删掉到期的一次性静默，被删的条目留在快照里
// 直到下一次 publish 或 Load——它们 until_at 已过、SilenceActive 恒为 false，对抑制没有任何作用，展示侧
// （ListSilences）读库，不看这里。这条惰性成立的前提是 prune 只动 until_at 已过的 ONCE 行；前提变了
// （比如将来 prune 也清 DAILY 或未到期行）快照就必须同步失效。
func (e *Engine) publishSilences(silences []store.Silence) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.silences = map[int64]store.Silence{}
	for _, s := range silences {
		e.silences[s.ID] = cloneSilence(s)
	}
}

// SaveSilence 校验后持久化并发布快照。保存请求必须明确指定非空作用域（与 SaveRule 同一约束）；
// 持久化失败不改变缓存。
func (e *Engine) SaveSilence(ctx context.Context, s store.Silence) (store.Silence, error) {
	if err := (store.NodeSelector{AllNodes: s.AllNodes, NodeIDs: s.NodeIDs, Tags: s.SelectorTags}).Check(); err != nil {
		var field store.KindFieldError
		if errors.As(err, &field) {
			return store.Silence{}, FieldError{Path: field.Field, Constraint: field.Constraint}
		}
		return store.Silence{}, err
	}
	if !s.AllNodes && len(s.NodeIDs) == 0 && len(s.SelectorTags) == 0 {
		return store.Silence{}, invalid("node_ids", "must not be empty unless all_nodes or selector_tags is set")
	}
	if err := CheckSilence(s); err != nil {
		return store.Silence{}, err
	}
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	saved, err := e.st.SaveSilence(ctx, s)
	if err != nil {
		return store.Silence{}, err
	}
	e.mu.Lock()
	e.silences[saved.ID] = cloneSilence(saved)
	e.mu.Unlock()
	return saved, nil
}

func (e *Engine) DeleteSilence(ctx context.Context, id int64) error {
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	if err := e.st.DeleteSilence(ctx, id); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.silences, id)
	return nil
}
