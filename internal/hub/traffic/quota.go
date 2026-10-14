package traffic

// Quota 是展示与告警共用的计费口径。Book 保证单向用量为非负 int64，先转 uint64
// 再相加可容纳两个 MaxInt64；配额为零只停用百分比，不丢弃有效的已用字节。
func Quota(e Entry, quotaBytes uint64, mode string) (usedBytes uint64, pct float64, hasPct bool) {
	rx, tx := uint64(e.PeriodRx), uint64(e.PeriodTx)
	switch mode {
	case "rx":
		usedBytes = rx
	case "tx":
		usedBytes = tx
	case "max":
		usedBytes = max(rx, tx)
	default:
		usedBytes = rx + tx
	}
	if quotaBytes == 0 {
		return usedBytes, 0, false
	}
	return usedBytes, float64(usedBytes) / float64(quotaBytes) * 100, true
}
