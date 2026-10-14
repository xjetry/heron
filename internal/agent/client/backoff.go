package client

import (
	"time"

	"github.com/xjetry/heron/internal/agentwire"
)

// Backoff 给出第 attempt 次失败后的等待：指数增长，上限 ReportsPerTTL × interval。
//
// 上限取 ReportsPerTTL × interval 而非独立取值：interval = TTL / ReportsPerTTL（agentwire.ReportInterval），
// 所以上限就是 TTL——恢复后重新可见的时长与掉线被发现的时长共用同一预算。抖动取 [base/2, base]，
// 把 hub 恢复后的重试摊开又不至于让等待坍缩到零。
func Backoff(attempt int, interval time.Duration, rnd func() float64) time.Duration {
	ceiling := agentwire.ReportsPerTTL * interval
	base := interval
	for i := 1; i < attempt && base < ceiling; i++ {
		base *= 2
	}
	if base > ceiling {
		base = ceiling
	}
	half := base / 2
	return half + time.Duration(rnd()*float64(half))
}
