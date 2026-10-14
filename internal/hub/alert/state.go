package alert

import (
	"time"

	"github.com/xjetry/heron/internal/hub/store"
)

// 离线告警的抖动抑制：一对规则与节点从 firing 恢复后，flapWindow 之内再次开始的离线要满
// max(节点宽限, flapGrace) 才进入 firing。
//
// 两个数是常量，不按节点或规则配：flapGrace 的作用是压抖动的噪声，与节点自己的宽限是两个量；若随节点配置，
// 调小节点宽限的同时也会把压噪声的下限一起调没。
//
// flapWindow 严格大于 flapGrace 是这两个常量的取值约束，下面的常量表达式在编译期钉住它（差为负时常量转换 uint64 溢出；差以纳秒计，32 位的 uint 放不下成立的差值，不能用 uint）。
// 状态机不比较两者：grace() 按离线开始时刻一次定下宽限，之后不再看窗口。判定不读两者的大小关系，改动其中一个数
// 带来的变化只来自被改的那个数本身（窗口改小，晚开始的离线落到窗口外；宽限改小，pending 更早结束）；改反只会让
// "窗口内再掉"与"抖动宽限"两个量失去各自的含义。
// 只有节点没有任何上报落过库、离线开始退回启动时刻的那条路径（offlineStart）上，重启时刻本身参与窗口判定，
// "恢复后立刻再掉的抖动宽限不被重启打断"才靠这条关系保证：窗口改成 20 分钟、恢复后立刻再掉并在 R+25m 重启，
// 退回路径在 R+26m1s 按节点宽限触发，真实上报路径仍是 pending。
const (
	flapWindow = time.Hour
	flapGrace  = 30 * time.Minute
)

const _ = uint64(flapWindow - flapGrace - 1)

type Observation struct {
	Reported   bool
	Unseen     time.Duration
	Grace, TTL time.Duration
	// Recovered 为真表示这对规则与节点从 firing 恢复过；SinceRecovery 是上次恢复到这次离线开始的时长。离线开始即最后
	// 一次上报的墙钟：本次启动后上报过的取 live 里的，没有的取落库的最后上报时刻，库里也没有才取启动时刻（见
	// Engine.offlineStart）。按离线开始的时刻而不是评估时刻判定：同一次离线在整个 pending 期间用同一个宽限，不会因为
	// 评估时刻走出窗口而中途改回节点宽限、提前触发；这次离线之前的上报已刷出落库时，hub 重启也不换宽限。恢复是在上报
	// 之后的巡检里记下的，离线开始（那次上报）可以比它早一个巡检间隔，所以 SinceRecovery 可以为负，仍算在窗口内。
	Recovered     bool
	SinceRecovery time.Duration
}

// grace 是这次离线进入 firing 所需的时长：落在恢复后的窗口里取 max(节点宽限, flapGrace)，否则取节点宽限。
// 窗口不含右端点：恰好一个 flapWindow 之后开始的离线按节点宽限。
func (o Observation) grace() time.Duration {
	if o.Recovered && o.SinceRecovery < flapWindow {
		return max(o.Grace, flapGrace)
	}
	return o.Grace
}

func recovered(cur store.AlertState) (store.AlertState, *store.Transition) {
	if cur == store.StateFiring {
		tr := store.TransitionRecovered
		return store.StateOK, &tr
	}
	return store.StateOK, nil
}

// 第一条 return 保证离线告警不早于面板按 TTL 显示离线，也为零值宽限提供 TTL 下限。
// 即使保存入口校验宽限下限，也不能约束之后调大的 TTL；实际下限仍在这里承载。
// grace() 用 Engine.offlineStart 给出的同一离线开始时刻挑选本段宽限，评估走出抖动窗口时不改本段判定。
// 没有本次启动后的上报时，短暂的启动时长不代表恢复，Reported 由 live 条目是否存在提供。
func NextOffline(cur store.AlertState, o Observation) (store.AlertState, *store.Transition) {
	if o.Unseen < o.TTL {
		if o.Reported {
			return recovered(cur)
		}
		return cur, nil
	}
	if cur == store.StateFiring {
		return cur, nil
	}
	if o.Unseen >= o.grace() {
		tr := store.TransitionFiring
		return store.StateFiring, &tr
	}
	return store.StatePending, nil
}

// FlapDeferred 报告这一轮是否只因抖动抑制而停在 pending，面板据此标出"抖动中"。字面意思就是定义：NextOffline 给出
// pending，而拿掉恢复历史之后 NextOffline 本会给出 firing。抖动抑制的输入只有恢复历史（grace() 在 Recovered 为假时
// 不看 SinceRecovery），Recovered 置假的那次调用就是不做抖动抑制的状态机。这里只比较两次调用的结果、不复述其中任何
// 一道门（TTL、未上报时保持现状、宽限），NextOffline 以后加门或改门，这里自动跟上。
func FlapDeferred(cur store.AlertState, o Observation) bool {
	with, _ := NextOffline(cur, o)
	o.Recovered = false
	without, _ := NextOffline(cur, o)
	return with == store.StatePending && without == store.StateFiring
}

// EvaluateProbes 按时间升序铺满窗口；Present 为假不能当成一次恢复观测。
type MinuteSample struct {
	Present bool
	Exceeds bool
	Value   float64
}

// forMinutes 的 1–60 范围由保存入口及 Load 的 CheckRule 校验共同保证。
func NextProbe(cur store.AlertState, samples []MinuteSample, forMinutes int) (store.AlertState, *store.Transition) {
	if len(samples) == 0 || !samples[len(samples)-1].Present {
		return cur, nil
	}
	if !samples[len(samples)-1].Exceeds {
		return recovered(cur)
	}
	if cur == store.StateFiring {
		return cur, nil
	}
	if len(samples) < forMinutes {
		return store.StatePending, nil
	}
	for _, s := range samples[len(samples)-forMinutes:] {
		if !s.Present || !s.Exceeds {
			return store.StatePending, nil
		}
	}
	tr := store.TransitionFiring
	return store.StateFiring, &tr
}

// ExpiryObservation 是一个节点在一次到期扫描里的观测。HasExpiry 为假时 DaysLeft 无意义。
type ExpiryObservation struct {
	HasExpiry  bool
	DaysLeft   int // 到期日减今天（hub 时区的日历日），负数是已过期天数
	DaysBefore int // 规则的提前天数，1–365
}

// NextExpiry 没有 pending：到期是日历事件，进入窗口即触发，不存在"持续多久才算"。没有到期日按恢复处理——
// 清除到期日是结束提醒的正当方式，不能让已触发的告警卡在 firing。
func NextExpiry(cur store.AlertState, o ExpiryObservation) (store.AlertState, *store.Transition) {
	if !o.HasExpiry || o.DaysLeft > o.DaysBefore {
		return recovered(cur)
	}
	if cur == store.StateFiring {
		return cur, nil
	}
	tr := store.TransitionFiring
	return store.StateFiring, &tr
}
