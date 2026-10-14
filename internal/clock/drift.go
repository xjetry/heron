package clock

import (
	"time"
)

// MaxClockJump 是一轮上报循环内墙钟与单调钟增量的正常偏差上限，超出即"休眠信号"。
// 推导：
//   - 一轮的长度上限由消费方（agent 的上报循环）的有效间隔保证：hub 下发的间隔经
//     agentwire.ClampReportInterval 限定为 agentwire.MaxTTL/agentwire.ReportsPerTTL=60s，
//     失败退避封顶 agentwire.ReportsPerTTL 倍间隔即 180s。
//   - 一轮内两钟的正常偏差来自 NTP slew，上限 500 ppm：180s 内不超过 90ms，是毫秒量级。
//   - 真实休眠至少数十秒：Linux 的 CLOCK_MONOTONIC 与 darwin 的 CLOCK_UPTIME_RAW 都不计休眠
//     时间，唤醒后 Δwall − Δmono ≈ 休眠时长。
//
// 20s 远大于毫秒级的正常偏差、小于真实休眠。NTP 步进也会越过阈值：与休眠同处理，
// 不为区分两者再加判据。阈值必须小于探测结果的迟到预算（否则存在判不出却已让全部
// 积压结果超龄的休眠区间），这层关系由消费方的编译期断言钉住（agent 上报循环处）。
const MaxClockJump = 20 * time.Second

// Drift 逐轮比较墙钟与单调钟的增量，识别单调钟没有经历的墙钟跳跃（休眠、NTP 步进）。
// 首次 Observe 只建立基线，不报跳变。
type Drift struct {
	wall  time.Time
	mono  time.Duration
	began bool
}

// Observe 记录一对读数，返回与上一轮的增量；jumped 表示 |Δwall − Δmono| 越过 MaxClockJump。
// 取绝对值是因为墙钟被向后拨同样是单调钟未经历的跳变。
// now 与 mono 必须是同一时刻紧邻读取的一对值（Runner 在循环开头连续取），
// 否则两次读取之间的流逝只计入 Δmono，被误算成跳变的一部分。
func (d *Drift) Observe(now time.Time, mono time.Duration) (wallDelta, monoDelta time.Duration, jumped bool) {
	if !d.began {
		d.began, d.wall, d.mono = true, now, mono
		return 0, 0, false
	}
	wallDelta, monoDelta = now.Sub(d.wall), mono-d.mono
	d.wall, d.mono = now, mono
	gap := wallDelta - monoDelta
	if gap < 0 {
		gap = -gap
	}
	return wallDelta, monoDelta, gap > MaxClockJump
}
