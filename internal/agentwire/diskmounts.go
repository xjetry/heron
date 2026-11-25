package agentwire

import (
	"fmt"
	"math/bits"
	"path"
	"strings"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
)

const (
	// MaxDiskMounts 是 ReportRequest.disk_mounts 的条数上限：agent 按 CompareDiskUsage 排序后只发最满的这么多项，
	// 另以 disk_mounts_total 报截断前的总数。
	MaxDiskMounts = 16
	// MaxMountPathBytes 是一条挂载路径的字节上限，与 Facts 主机字符串同一量级；超长的挂载点 agent 不纳入监测。
	MaxMountPathBytes = 256
)

// ValidateDiskMountPath 是挂载路径的唯一准入谓词：agent 选盘时据此丢掉无法上报的挂载点，hub 准入上报时据此拒收。
// 要求规范绝对路径（path.Clean 不变）：同一挂载点只有一种写法，代表路径的去重与排序才有确定含义。
func ValidateDiskMountPath(p string) error {
	if !safeDiagnosticText(p, MaxMountPathBytes) {
		return fmt.Errorf("paths must be non-empty UTF-8 without control characters, at most %d bytes", MaxMountPathBytes)
	}
	if !strings.HasPrefix(p, "/") || path.Clean(p) != p {
		return fmt.Errorf("paths must be clean absolute paths")
	}
	return nil
}

// CompareDiskUsage 是逐盘列表的唯一排序：使用率（used ÷ total）高的在前，相等时按路径字节序升序。
// 使用率按 a.used × b.total 与 b.used × a.total 的 128 位乘积精确比较，不经浮点：两块盘的比值只差一个字节时
// 浮点会判成相等，agent 与 hub 对同一份列表就可能得出不同的顺序。调用方保证 total > 0。
func CompareDiskUsage(a, b *heronv1.DiskMountUsage) int {
	ah, al := bits.Mul64(a.GetUsed(), b.GetTotal())
	bh, bl := bits.Mul64(b.GetUsed(), a.GetTotal())
	switch {
	case ah > bh || (ah == bh && al > bl):
		return -1
	case ah < bh || (ah == bh && al < bl):
		return 1
	}
	return strings.Compare(a.GetPath(), b.GetPath())
}

// ValidateDiskMounts 约束 ReportRequest 的逐盘容量：total 为 0 即没有读数、列表为空；否则列表恰是按 CompareDiskUsage
// 严格递增的前 min(total, MaxDiskMounts) 项，路径唯一且经 ValidateDiskMountPath 准入，每项 0 < total、used ≤ total。
// 排序由 hub 复核而不是重排：最满盘的判定取列表首项，乱序的列表会让它指向错的挂载点。
func ValidateDiskMounts(list []*heronv1.DiskMountUsage, total uint32) error {
	if uint32(len(list)) != min(total, MaxDiskMounts) {
		return fmt.Errorf("disk_mounts: must contain the first %d mounts or the complete smaller set of disk_mounts_total (%d); got %d", MaxDiskMounts, total, len(list))
	}
	seen := make(map[string]bool, len(list))
	for i, d := range list {
		if len(d.ProtoReflect().GetUnknown()) != 0 {
			return fmt.Errorf("disk_mounts: unknown fields are not accepted")
		}
		if err := ValidateDiskMountPath(d.GetPath()); err != nil {
			return fmt.Errorf("disk_mounts: path: %w", err)
		}
		if d.GetTotal() == 0 {
			return fmt.Errorf("disk_mounts: total must be positive")
		}
		if d.GetUsed() > d.GetTotal() {
			return fmt.Errorf("disk_mounts: used must not exceed total")
		}
		if seen[d.GetPath()] {
			return fmt.Errorf("disk_mounts: paths must be unique")
		}
		seen[d.GetPath()] = true
		if i > 0 && CompareDiskUsage(list[i-1], d) >= 0 {
			return fmt.Errorf("disk_mounts: must be sorted by usage descending, then by path")
		}
	}
	return nil
}

// ValidateDiskExclude 约束本地数据盘的排除规则：本机启动参数、配置文件与上报的诊断共用，与网卡规则同样的条数与长度
// 预算。规则按挂载路径做 path.Match，挂载路径都是绝对路径，不以 "/" 开头的规则什么都匹配不到，按配置错误拒绝。
func ValidateDiskExclude(patterns []string) error {
	if err := validatePatterns("disk_exclude", patterns); err != nil {
		return err
	}
	for _, p := range patterns {
		if !strings.HasPrefix(p, "/") {
			return fmt.Errorf("disk_exclude: patterns must start with / (they match absolute mount paths); got %q", p)
		}
	}
	return nil
}
