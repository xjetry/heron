package agentwire

import (
	"strings"
	"testing"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
)

func disk(path string, total, used uint64) *heronv1.DiskMountUsage {
	return &heronv1.DiskMountUsage{Path: path, Total: total, Used: used}
}

// 使用率按 used/total 的精确比较排序：10/20 与 1/2 相等，按路径决序；大数相乘不溢出。
func TestCompareDiskUsage(t *testing.T) {
	const tib = uint64(1) << 40
	for _, tc := range []struct {
		name string
		a, b *heronv1.DiskMountUsage
		want int
	}{
		{"fuller first", disk("/b", 100, 90), disk("/a", 100, 10), -1},
		{"emptier later", disk("/a", 100, 10), disk("/b", 100, 90), 1},
		{"equal ratio by path", disk("/a", 20, 10), disk("/b", 2, 1), -1},
		{"equal ratio reversed path", disk("/b", 20, 10), disk("/a", 2, 1), 1},
		{"same", disk("/a", 2, 1), disk("/a", 2, 1), 0},
		// 两个约 1 EiB 的盘：used×total 超出 64 位。差一个字节也要分得出。
		{"huge", disk("/a", 1<<60, 1<<59+1), disk("/b", 1<<60, 1<<59), -1},
		{"small full beats big half", disk("/", 20<<30, 20<<30), disk("/data", 2*tib, tib), -1},
	} {
		if got := CompareDiskUsage(tc.a, tc.b); got != tc.want {
			t.Errorf("%s: CompareDiskUsage = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestValidateDiskMountPath(t *testing.T) {
	for _, p := range []string{"/", "/data", "/mnt/disk 1", "/srv/数据", "/" + strings.Repeat("x", MaxMountPathBytes-1)} {
		if err := ValidateDiskMountPath(p); err != nil {
			t.Errorf("%q rejected: %v", p, err)
		}
	}
	for _, p := range []string{"", "data", "/data/", "/a//b", "/a/./b", "/a/../b", "/a\nb", "/a\x00", "/\xff", "/" + strings.Repeat("x", MaxMountPathBytes)} {
		if err := ValidateDiskMountPath(p); err == nil {
			t.Errorf("%q accepted", p)
		}
	}
}

func TestValidateDiskMounts(t *testing.T) {
	full := func() []*heronv1.DiskMountUsage {
		out := make([]*heronv1.DiskMountUsage, MaxDiskMounts)
		for i := range out {
			// 使用率逐项递减：第 i 项用 100-i。
			out[i] = disk("/d"+string(rune('a'+i)), 100, uint64(100-i))
		}
		return out
	}
	ok := []struct {
		name  string
		list  []*heronv1.DiskMountUsage
		total uint32
	}{
		{"absent", nil, 0},
		{"root only", []*heronv1.DiskMountUsage{disk("/", 100, 50)}, 1},
		{"tie by path", []*heronv1.DiskMountUsage{disk("/", 100, 50), disk("/data", 2, 1)}, 2},
		{"empty disk", []*heronv1.DiskMountUsage{disk("/", 100, 0)}, 1},
		{"truncated", full(), MaxDiskMounts + 5},
		{"exactly max", full(), MaxDiskMounts},
	}
	for _, tc := range ok {
		if err := ValidateDiskMounts(tc.list, tc.total); err != nil {
			t.Errorf("%s rejected: %v", tc.name, err)
		}
	}
	bad := []struct {
		name  string
		list  []*heronv1.DiskMountUsage
		total uint32
		want  string
	}{
		{"list without total", []*heronv1.DiskMountUsage{disk("/", 100, 50)}, 0, "disk_mounts: must contain"},
		{"total without list", nil, 1, "disk_mounts: must contain"},
		{"short of total", []*heronv1.DiskMountUsage{disk("/", 100, 50)}, 2, "disk_mounts: must contain"},
		{"over max", append(full(), disk("/z", 100, 1)), MaxDiskMounts + 1, "disk_mounts: must contain"},
		{"zero total", []*heronv1.DiskMountUsage{disk("/", 0, 0)}, 1, "total must be positive"},
		{"used over total", []*heronv1.DiskMountUsage{disk("/", 100, 101)}, 1, "used must not exceed total"},
		{"bad path", []*heronv1.DiskMountUsage{disk("data", 100, 1)}, 1, "disk_mounts: path"},
		{"ascending usage", []*heronv1.DiskMountUsage{disk("/a", 100, 10), disk("/b", 100, 90)}, 2, "sorted"},
		{"tie out of path order", []*heronv1.DiskMountUsage{disk("/b", 100, 50), disk("/a", 100, 50)}, 2, "sorted"},
		{"duplicate path", []*heronv1.DiskMountUsage{disk("/a", 100, 90), disk("/a", 100, 10)}, 2, "unique"},
		{"unknown field", func() []*heronv1.DiskMountUsage {
			d := disk("/", 100, 1)
			d.ProtoReflect().SetUnknown([]byte{0x3a, 0x01, 'x'})
			return []*heronv1.DiskMountUsage{d}
		}(), 1, "unknown fields"},
	}
	for _, tc := range bad {
		err := ValidateDiskMounts(tc.list, tc.total)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestValidateDiskExclude(t *testing.T) {
	if err := ValidateDiskExclude([]string{"/mnt/*", "/srv/backup", "/media/[ab]*"}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateDiskExclude(nil); err != nil {
		t.Fatal(err)
	}
	many := make([]string, MaxNetPatterns+1)
	for i := range many {
		many[i] = "/mnt"
	}
	for _, tc := range []struct {
		name     string
		patterns []string
		want     string
	}{
		{"relative", []string{"mnt/*"}, "must start with /"},
		{"invalid glob", []string{"/mnt/["}, "invalid glob"},
		{"too long", []string{"/" + strings.Repeat("x", MaxNetPatternBytes)}, "at most"},
		{"control", []string{"/mnt\n"}, "control"},
		{"too many", many, "at most 64 patterns"},
	} {
		err := ValidateDiskExclude(tc.patterns)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
}

// 诊断里的逐盘部分：排除规则同一准入；逐盘采集失败时不得声称纳入了挂载点。
func TestDiagnosticsDiskMounts(t *testing.T) {
	good := &heronv1.AgentDiagnostics{DiskExclude: []string{"/mnt/*"}, DiskMountsTotal: 3}
	if err := ValidateDiagnostics(good); err != nil {
		t.Fatal(err)
	}
	failed := &heronv1.AgentDiagnostics{FailedCollectors: []heronv1.CollectionComponent{heronv1.CollectionComponent_COLLECTION_COMPONENT_DISK_MOUNTS}}
	if err := ValidateDiagnostics(failed); err != nil {
		t.Fatalf("disk mounts component must be accepted: %v", err)
	}
	failed.DiskMountsTotal = 1
	if err := ValidateDiagnostics(failed); err == nil || !strings.Contains(err.Error(), "disk_mounts_total") {
		t.Errorf("failed collection claiming a set: err = %v", err)
	}
	if err := ValidateDiagnostics(&heronv1.AgentDiagnostics{DiskExclude: []string{"mnt"}}); err == nil || !strings.Contains(err.Error(), "disk_exclude") {
		t.Errorf("relative exclude pattern: err = %v", err)
	}
}
