package collect

import (
	"bufio"
	"bytes"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/clock"
)

// ---------- mountinfo 解析 ----------

func TestParseMountinfo(t *testing.T) {
	// 行来自真实 mountinfo：内核 7.0.14-orbstack 上叠加 proc 与 Incus 6.0.4 guest 的
	// lxcfs 6.0.4 逐文件绑定（fuse.lxcfs 设备与 proc 设备不同）。
	// 真实行：OrbStack/Incus 环境的 proc 挂载（0:307）、lxcfs 逐文件绑定（0:319）、
	// runner 的 btrfs 根（0:38），设备号互不相同。
	mounts, err := parseMountinfo(strings.NewReader(strings.Join([]string{
		"8349 7681 0:307 / /proc rw,nosuid,nodev,noexec,relatime - proc proc rw",
		"8668 8349 0:319 /proc/cpuinfo /proc/cpuinfo rw,relatime - fuse.lxcfs lxcfs rw,user_id=0,group_id=0,allow_other",
		"42 41 0:38 /scon/containers/x/rootfs / rw,noatime - btrfs /dev/vdb1 rw,nodatasum",
	}, "\n") + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	types := mountTypes(mounts)
	for dev, want := range map[string]string{"0:307": "proc", "0:319": "fuse.lxcfs", "0:38": "btrfs"} {
		if types[dev] != want {
			t.Fatalf("fstype[%s] = %q, want %q", dev, types[dev], want)
		}
	}
	// 同一设备两种 fstype 是冲突：从表里剔除（来源按未知），且与行序无关。
	for _, order := range []string{
		"1 2 0:44 / /a rw - proc proc rw\n2 3 0:44 / /b rw - tmpfs none rw",
		"2 3 0:44 / /b rw - tmpfs none rw\n1 2 0:44 / /a rw - proc proc rw",
	} {
		m, err := parseMountinfo(strings.NewReader(order))
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := mountTypes(m)["0:44"]; ok {
			t.Fatalf("conflicting fstypes for one device must be dropped, input order: %q", order)
		}
	}
	// 同一设备同一种 fstype（叠加挂载的常态）：保留。
	same, err := parseMountinfo(strings.NewReader("1 2 0:44 / /a rw - proc proc rw\n2 3 0:44 / /b rw - proc none rw"))
	if err != nil || mountTypes(same)["0:44"] != "proc" {
		t.Fatalf("stacked mounts of one device share fstype: %v %v", mountTypes(same), err)
	}
	for _, bad := range []string{"", "no separator here", "42 41 0:294 / /proc rw - proc\n42 41 0:294 / /proc rw", "1 2"} {
		if _, err := parseMountinfo(strings.NewReader(bad)); err == nil {
			t.Fatalf("parseMountinfo(%q) must fail", bad)
		}
	}
}

// ---------- 决策表（buildExecSnapshot，纯函数） ----------

func dev(dev string) fileDev { return fileDev{dev: dev} }

func procMounts() []mountEntry {
	return []mountEntry{
		{dev: fixtureProcDev, fstype: "proc"},
		{dev: fixtureLxcfsDev, fstype: "fuse.lxcfs"},
	}
}

// baseEvidence 给出全 procfs 来源的宿主证据；各用例改自己需要的字段。
func baseEvidence() execEvidence {
	return execEvidence{
		cgroup2:       true,
		mounts:        procMounts(),
		statDev:       dev(fixtureProcDev),
		meminfoDev:    dev(fixtureProcDev),
		loadavgDev:    dev(fixtureProcDev),
		cpuinfoDev:    dev(fixtureProcDev),
		cpuProcessors: 16,
		meminfo:       memInfo{total: 16 << 30, swapTotal: 2 << 30},
		env:           baseEnv(),
	}
}

func baseEnv() *envEvidence {
	return &envEvidence{
		cpuMax:        limitValue[float64]{value: 2, numeric: true},
		cpusetCount:   8,
		cpuset:        fileOK,
		cpuStatOK:     true,
		memMax:        limitValue[uint64]{value: 512 << 20, numeric: true},
		memCurrentOK:  true,
		memStatOK:     true,
		swapMax:       limitValue[uint64]{value: 256 << 20, numeric: true},
		swapCurrentOK: true,
		dirID:         fixtureCgroupDev + ":101",
	}
}

func hostEvidence() execEvidence {
	ev := baseEvidence()
	ev.hostRoot = true
	ev.env = nil
	return ev
}

func TestBuildExecSnapshotDecisionTable(t *testing.T) {
	const (
		host = heronv1.ResourceScope_RESOURCE_SCOPE_HOST
		envr = heronv1.ResourceScope_RESOURCE_SCOPE_ENVIRONMENT
		leg  = heronv1.ResourceScope_RESOURCE_SCOPE_LEGACY
		unkn = heronv1.ResourceScope_RESOURCE_SCOPE_UNKNOWN
	)
	const mem512 = uint64(512) << 20
	tests := []struct {
		name string
		ev   func() execEvidence
		want *heronv1.ExecutionScope
		// cpuBaseline 空串断言"无 CPU 读数"。环境读数入口由 ProcFS.identify 挂上
		//（TestProcIdentifyEnvAndHost 覆盖），纯决策函数不碰文件。
		cpuBaseline string
	}{
		{
			name:        "真根：全 procfs，读数是整机",
			ev:          hostEvidence,
			want:        &heronv1.ExecutionScope{Kind: heronv1.ScopeKind_SCOPE_KIND_HOST, Cpu: host, Memory: host, Swap: host, Load: host, CpuEffectiveCores: f16(16), LoadCores: u16(16), MemoryLimitBytes: u64p(16 << 30), SwapLimitBytes: u64p(2 << 30)},
			cpuBaseline: "host:/proc/stat|cores=16",
		},
		{
			name: "真根 + 容器标识（CI runner 里跑 slice 服务）：读数仍是整机，记说明",
			ev: func() execEvidence {
				ev := hostEvidence()
				ev.containerSignal = true
				return ev
			},
			want: &heronv1.ExecutionScope{Kind: heronv1.ScopeKind_SCOPE_KIND_HOST, Cpu: host, Memory: host, Swap: host, Load: host, CpuEffectiveCores: f16(16), LoadCores: u16(16), MemoryLimitBytes: u64p(16 << 30), SwapLimitBytes: u64p(2 << 30),
				Notes: []heronv1.ScopeNote{heronv1.ScopeNote_SCOPE_NOTE_CONTAINER_SIGNAL_ON_HOST_ROOT}},
			cpuBaseline: "host:/proc/stat|cores=16",
		},
		{
			name: "真根，宿主没有 swap：swap 上限照报已知的 0",
			ev: func() execEvidence {
				ev := hostEvidence()
				ev.meminfo.swapTotal = 0
				return ev
			},
			want:        &heronv1.ExecutionScope{Kind: heronv1.ScopeKind_SCOPE_KIND_HOST, Cpu: host, Memory: host, Swap: host, Load: host, CpuEffectiveCores: f16(16), LoadCores: u16(16), MemoryLimitBytes: u64p(16 << 30), SwapLimitBytes: u64p(0)},
			cpuBaseline: "host:/proc/stat|cores=16",
		},
		{
			name: "真根，/proc/stat 被 tmpfs 盖住：CPU 未知并记说明，其余照常",
			ev: func() execEvidence {
				ev := hostEvidence()
				ev.statDev = dev("9:9")
				return ev
			},
			want: &heronv1.ExecutionScope{Kind: heronv1.ScopeKind_SCOPE_KIND_HOST, Cpu: unkn, Memory: host, Swap: host, Load: host, LoadCores: u16(16), MemoryLimitBytes: u64p(16 << 30), SwapLimitBytes: u64p(2 << 30),
				Notes: []heronv1.ScopeNote{heronv1.ScopeNote_SCOPE_NOTE_PROC_STAT_NOT_PROCFS}},
		},
		{
			name: "真根，cpuinfo 来自 lxcfs：CPU 照读、核数与按核负载缺失",
			ev: func() execEvidence {
				ev := hostEvidence()
				ev.cpuinfoDev = dev(fixtureLxcfsDev)
				return ev
			},
			want: &heronv1.ExecutionScope{Kind: heronv1.ScopeKind_SCOPE_KIND_HOST, Cpu: host, Memory: host, Swap: host, Load: host, MemoryLimitBytes: u64p(16 << 30), SwapLimitBytes: u64p(2 << 30),
				Notes: []heronv1.ScopeNote{heronv1.ScopeNote_SCOPE_NOTE_CPUINFO_NOT_PROCFS}},
			cpuBaseline: "host:/proc/stat|cores=0",
		},
		{
			name: "真根，loadavg 来自 lxcfs：负载未知并记说明",
			ev: func() execEvidence {
				ev := hostEvidence()
				ev.loadavgDev = dev(fixtureLxcfsDev)
				return ev
			},
			want: &heronv1.ExecutionScope{Kind: heronv1.ScopeKind_SCOPE_KIND_HOST, Cpu: host, Memory: host, Swap: host, Load: unkn, CpuEffectiveCores: f16(16), MemoryLimitBytes: u64p(16 << 30), SwapLimitBytes: u64p(2 << 30),
				Notes: []heronv1.ScopeNote{heronv1.ScopeNote_SCOPE_NOTE_LOADAVG_NOT_PROCFS}},
			cpuBaseline: "host:/proc/stat|cores=16",
		},
		{
			name: "真根，meminfo 来源不明：内存与 swap 未知并记说明",
			ev: func() execEvidence {
				ev := hostEvidence()
				ev.meminfoDev = dev("9:9")
				return ev
			},
			want: &heronv1.ExecutionScope{Kind: heronv1.ScopeKind_SCOPE_KIND_HOST, Cpu: host, Memory: unkn, Swap: unkn, Load: host, CpuEffectiveCores: f16(16), LoadCores: u16(16),
				Notes: []heronv1.ScopeNote{heronv1.ScopeNote_SCOPE_NOTE_MEMINFO_UNUSABLE}},
			cpuBaseline: "host:/proc/stat|cores=16",
		},
		{
			name: "环境 cgroup 全套：配额 2 核、cpuset 8、memory.max 512MiB、swap.max 256MiB",
			ev:   baseEvidence,
			want: &heronv1.ExecutionScope{Kind: heronv1.ScopeKind_SCOPE_KIND_CGROUP_NAMESPACE, Cpu: envr, Memory: envr, Swap: envr, Load: host, CpuEffectiveCores: f16(2), LoadCores: u16(16), MemoryLimitBytes: u64p(mem512), SwapLimitBytes: u64p(256 << 20)},
			// 上限 = min(memory.max 512MiB, MemTotal 16GiB) 与 min(swap.max, SwapTotal)。
			cpuBaseline: "cgroup:" + fixtureCgroupDev + ":101|cores=2",
		},
		{
			name: "环境配额不限、cpuset 文件缺失：回退 cpuinfo 处理器数（主机核数）",
			ev: func() execEvidence {
				ev := baseEvidence()
				ev.env.cpuMax = limitValue[float64]{}
				ev.env.cpuset = fileMissing
				return ev
			},
			want:        &heronv1.ExecutionScope{Kind: heronv1.ScopeKind_SCOPE_KIND_CGROUP_NAMESPACE, Cpu: envr, Memory: envr, Swap: envr, Load: host, CpuEffectiveCores: f16(16), LoadCores: u16(16), MemoryLimitBytes: u64p(mem512), SwapLimitBytes: u64p(256 << 20)},
			cpuBaseline: "cgroup:" + fixtureCgroupDev + ":101|cores=16",
		},
		{
			name: "环境 cpuset 内容认不出：CPU 未知并记说明",
			ev: func() execEvidence {
				ev := baseEvidence()
				ev.env.cpuset = fileBad
				return ev
			},
			want: &heronv1.ExecutionScope{Kind: heronv1.ScopeKind_SCOPE_KIND_CGROUP_NAMESPACE, Cpu: unkn, Memory: envr, Swap: envr, Load: host, LoadCores: u16(16), MemoryLimitBytes: u64p(mem512), SwapLimitBytes: u64p(256 << 20),
				Notes: []heronv1.ScopeNote{heronv1.ScopeNote_SCOPE_NOTE_CPU_CONTROLLER_UNREADABLE}},
		},
		{
			name: "环境 cpuset 读错误（权限）：CPU 未知并记说明",
			ev: func() execEvidence {
				ev := baseEvidence()
				ev.env.cpuset = fileBad
				ev.env.cpuMax = limitValue[float64]{missing: true}
				return ev
			},
			want: &heronv1.ExecutionScope{Kind: heronv1.ScopeKind_SCOPE_KIND_CGROUP_NAMESPACE, Cpu: unkn, Memory: envr, Swap: envr, Load: host, LoadCores: u16(16), MemoryLimitBytes: u64p(mem512), SwapLimitBytes: u64p(256 << 20),
				Notes: []heronv1.ScopeNote{heronv1.ScopeNote_SCOPE_NOTE_CPU_CONTROLLER_UNREADABLE}},
		},
		{
			name: "环境 cpu.stat 不存在（部分控制器未下放的 slice）：CPU 未知并记说明",
			ev: func() execEvidence {
				ev := baseEvidence()
				ev.env.cpuStatOK = false
				return ev
			},
			want: &heronv1.ExecutionScope{Kind: heronv1.ScopeKind_SCOPE_KIND_CGROUP_NAMESPACE, Cpu: unkn, Memory: envr, Swap: envr, Load: host, LoadCores: u16(16), MemoryLimitBytes: u64p(mem512), SwapLimitBytes: u64p(256 << 20),
				Notes: []heronv1.ScopeNote{heronv1.ScopeNote_SCOPE_NOTE_CPU_CONTROLLER_UNREADABLE}},
		},
		{
			name: "cpu.max 认不出：有效核数未知，CPU 未知并记说明",
			ev: func() execEvidence {
				ev := baseEvidence()
				ev.env.cpuMax = limitValue[float64]{err: fmt.Errorf("bogus")}
				return ev
			},
			want: &heronv1.ExecutionScope{Kind: heronv1.ScopeKind_SCOPE_KIND_CGROUP_NAMESPACE, Cpu: unkn, Memory: envr, Swap: envr, Load: host, LoadCores: u16(16), MemoryLimitBytes: u64p(mem512), SwapLimitBytes: u64p(256 << 20),
				Notes: []heronv1.ScopeNote{heronv1.ScopeNote_SCOPE_NOTE_CPU_CONTROLLER_UNREADABLE}},
		},
		{
			name: "环境 loadavg 来自 lxcfs：负载未知并记说明（lxcfs 的队列是 guest 的）",
			ev: func() execEvidence {
				ev := baseEvidence()
				ev.loadavgDev = dev(fixtureLxcfsDev)
				return ev
			},
			want: &heronv1.ExecutionScope{Kind: heronv1.ScopeKind_SCOPE_KIND_CGROUP_NAMESPACE, Cpu: envr, Memory: envr, Swap: envr, Load: unkn, CpuEffectiveCores: f16(2), MemoryLimitBytes: u64p(mem512), SwapLimitBytes: u64p(256 << 20),
				Notes: []heronv1.ScopeNote{heronv1.ScopeNote_SCOPE_NOTE_LOADAVG_NOT_PROCFS}},
			cpuBaseline: "cgroup:" + fixtureCgroupDev + ":101|cores=2",
		},
		{
			name: "环境 memory.max 不限、meminfo 是 lxcfs（Incus 6.0.4 guest，lxcfs 6.0.4，Debian 12）：上限取 guest 视图，swap 上限折到已知的 0",
			ev: func() execEvidence {
				ev := baseEvidence()
				ev.env.memMax = limitValue[uint64]{}
				ev.env.swapMax = limitValue[uint64]{}
				ev.meminfoDev = dev(fixtureLxcfsDev)
				ev.meminfo = memInfo{total: 512 << 20, swapTotal: 0}
				return ev
			},
			want:        &heronv1.ExecutionScope{Kind: heronv1.ScopeKind_SCOPE_KIND_CGROUP_NAMESPACE, Cpu: envr, Memory: envr, Swap: envr, Load: host, CpuEffectiveCores: f16(2), LoadCores: u16(16), MemoryLimitBytes: u64p(mem512), SwapLimitBytes: u64p(0)},
			cpuBaseline: "cgroup:" + fixtureCgroupDev + ":101|cores=2",
		},
		{
			name: "环境 memory.max 不限、meminfo 来源不明：上限未知，内存未知并记 meminfo 说明",
			ev: func() execEvidence {
				ev := baseEvidence()
				ev.env.memMax = limitValue[uint64]{}
				ev.env.swapMax = limitValue[uint64]{}
				ev.meminfoDev = dev("9:9")
				return ev
			},
			want: &heronv1.ExecutionScope{Kind: heronv1.ScopeKind_SCOPE_KIND_CGROUP_NAMESPACE, Cpu: envr, Memory: unkn, Swap: unkn, Load: host, CpuEffectiveCores: f16(2), LoadCores: u16(16),
				Notes: []heronv1.ScopeNote{heronv1.ScopeNote_SCOPE_NOTE_MEMINFO_UNUSABLE}},
			cpuBaseline: "cgroup:" + fixtureCgroupDev + ":101|cores=2",
		},
		{
			name: "环境 meminfo 来源不明但 memory.max 是数值：上限就是它，仍记 meminfo 说明",
			ev: func() execEvidence {
				ev := baseEvidence()
				ev.meminfoDev = dev("9:9")
				return ev
			},
			want: &heronv1.ExecutionScope{Kind: heronv1.ScopeKind_SCOPE_KIND_CGROUP_NAMESPACE, Cpu: envr, Memory: envr, Swap: envr, Load: host, CpuEffectiveCores: f16(2), LoadCores: u16(16), MemoryLimitBytes: u64p(mem512), SwapLimitBytes: u64p(256 << 20),
				Notes: []heronv1.ScopeNote{heronv1.ScopeNote_SCOPE_NOTE_MEMINFO_UNUSABLE}},
			cpuBaseline: "cgroup:" + fixtureCgroupDev + ":101|cores=2",
		},
		{
			name: "memory.swap.max 写 0（容器禁 swap）且宿主也无 swap：已知 0 照报，读数 0/0",
			ev: func() execEvidence {
				ev := baseEvidence()
				ev.env.swapMax = limitValue[uint64]{value: 0, numeric: true}
				ev.meminfo.swapTotal = 0
				return ev
			},
			want:        &heronv1.ExecutionScope{Kind: heronv1.ScopeKind_SCOPE_KIND_CGROUP_NAMESPACE, Cpu: envr, Memory: envr, Swap: envr, Load: host, CpuEffectiveCores: f16(2), LoadCores: u16(16), MemoryLimitBytes: u64p(mem512), SwapLimitBytes: u64p(0)},
			cpuBaseline: "cgroup:" + fixtureCgroupDev + ":101|cores=2",
		},
		{
			name: "swap 记账文件缺失（宿主没开 swap cgroup 记账）：swap 未知并记说明，内存照常",
			ev: func() execEvidence {
				ev := baseEvidence()
				ev.env.swapMax = limitValue[uint64]{missing: true}
				ev.env.swapCurrentOK = false
				return ev
			},
			want: &heronv1.ExecutionScope{Kind: heronv1.ScopeKind_SCOPE_KIND_CGROUP_NAMESPACE, Cpu: envr, Memory: envr, Swap: unkn, Load: host, CpuEffectiveCores: f16(2), LoadCores: u16(16), MemoryLimitBytes: u64p(mem512),
				Notes: []heronv1.ScopeNote{heronv1.ScopeNote_SCOPE_NOTE_SWAP_ACCOUNTING_MISSING}},
			cpuBaseline: "cgroup:" + fixtureCgroupDev + ":101|cores=2",
		},
		{
			name: "v1 / 无 cgroup2：沿用现状读法，范围标 legacy",
			ev: func() execEvidence {
				ev := baseEvidence()
				ev.cgroup2 = false
				ev.env = nil
				return ev
			},
			want:        &heronv1.ExecutionScope{Kind: heronv1.ScopeKind_SCOPE_KIND_CGROUP_V1_LEGACY, Cpu: leg, Memory: leg, Swap: leg, Load: leg, CpuEffectiveCores: f16(16), LoadCores: u16(16), MemoryLimitBytes: u64p(16 << 30), SwapLimitBytes: u64p(2 << 30)},
			cpuBaseline: "legacy:/proc/stat|cores=16",
		},
		{
			name: "v1 且 loadavg 不是 procfs：负载照读（现状口径），按核负载不设置",
			ev: func() execEvidence {
				ev := baseEvidence()
				ev.cgroup2 = false
				ev.env = nil
				ev.loadavgDev = dev(fixtureLxcfsDev)
				return ev
			},
			want:        &heronv1.ExecutionScope{Kind: heronv1.ScopeKind_SCOPE_KIND_CGROUP_V1_LEGACY, Cpu: leg, Memory: leg, Swap: leg, Load: leg, CpuEffectiveCores: f16(16), MemoryLimitBytes: u64p(16 << 30), SwapLimitBytes: u64p(2 << 30)},
			cpuBaseline: "legacy:/proc/stat|cores=16",
		},
		{
			name: "挂载根 cgroup.type stat 出错：识别失败，全部未知",
			ev: func() execEvidence {
				ev := baseEvidence()
				ev.rootStatErr = fmt.Errorf("permission denied")
				return ev
			},
			want: &heronv1.ExecutionScope{Kind: heronv1.ScopeKind_SCOPE_KIND_IDENTIFY_FAILED, Cpu: unkn, Memory: unkn, Swap: unkn, Load: unkn},
		},
		{
			name: "mountinfo 读不了：来源全部未知，真根上资源全未知并记各说明",
			ev: func() execEvidence {
				ev := hostEvidence()
				ev.mounts = nil
				ev.mountErr = fmt.Errorf("unreadable")
				return ev
			},
			want: &heronv1.ExecutionScope{Kind: heronv1.ScopeKind_SCOPE_KIND_HOST, Cpu: unkn, Memory: unkn, Swap: unkn, Load: unkn,
				Notes: []heronv1.ScopeNote{heronv1.ScopeNote_SCOPE_NOTE_MOUNTINFO_UNREADABLE, heronv1.ScopeNote_SCOPE_NOTE_LOADAVG_NOT_PROCFS, heronv1.ScopeNote_SCOPE_NOTE_CPUINFO_NOT_PROCFS, heronv1.ScopeNote_SCOPE_NOTE_PROC_STAT_NOT_PROCFS, heronv1.ScopeNote_SCOPE_NOTE_MEMINFO_UNUSABLE}},
		},
		{
			name: "mountinfo 读不了（环境 cgroup）：cpuset 回退失效，CPU/内存/swap/负载未知并记说明",
			ev: func() execEvidence {
				ev := baseEvidence()
				ev.mounts = nil
				ev.mountErr = fmt.Errorf("unreadable")
				ev.env.cpuset = fileMissing
				return ev
			},
			want: &heronv1.ExecutionScope{Kind: heronv1.ScopeKind_SCOPE_KIND_CGROUP_NAMESPACE, Cpu: unkn, Memory: envr, Swap: envr, Load: unkn, MemoryLimitBytes: u64p(mem512), SwapLimitBytes: u64p(256 << 20),
				Notes: []heronv1.ScopeNote{heronv1.ScopeNote_SCOPE_NOTE_MOUNTINFO_UNREADABLE, heronv1.ScopeNote_SCOPE_NOTE_LOADAVG_NOT_PROCFS, heronv1.ScopeNote_SCOPE_NOTE_CPUINFO_NOT_PROCFS, heronv1.ScopeNote_SCOPE_NOTE_MEMINFO_UNUSABLE}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap := buildExecSnapshot(tt.ev())
			if d := protoDiff(&snap.exec, tt.want); d != nil {
				t.Fatalf("execution 差异:\n%s", strings.Join(d, "\n"))
			}
			// 不变式（proto：ExecutionScope.notes）：kind 不是识别失败而任一资源未知时，
			// 说明必须非空——每个"未知"都要有读者可见的原因。
			if snap.exec.GetKind() != heronv1.ScopeKind_SCOPE_KIND_IDENTIFY_FAILED {
				anyUnknown := snap.exec.GetCpu() == unknown || snap.exec.GetMemory() == unknown ||
					snap.exec.GetSwap() == unknown || snap.exec.GetLoad() == unknown
				if anyUnknown && len(snap.exec.GetNotes()) == 0 {
					t.Fatal("有资源未知却没有任何说明")
				}
			}
			if snap.cpuBaseline != tt.cpuBaseline {
				t.Fatalf("cpuBaseline = %q, want %q", snap.cpuBaseline, tt.cpuBaseline)
			}
			if snap.readCPUUsage != nil || snap.readMemEnv != nil || snap.readSwapEnv != nil {
				t.Fatal("纯决策函数不得挂读数入口（identify 的职责）")
			}
		})
	}
}

func f16(v float64) *float64 { return &v }
func u16(v uint32) *uint32   { return &v }
func u64p(v uint64) *uint64  { return &v }

// 环境路径的 cpu_cores 上取整（1.5 核报 2），识别不出是 0，不退回 runtime.NumCPU()。
func TestCpuCoresRoundingAndNoFallback(t *testing.T) {
	ev := baseEvidence()
	ev.env.cpuMax = limitValue[float64]{value: 1.5, numeric: true}
	ev.env.cpusetCount = 16
	ev.env.cpuset = fileOK
	snap := buildExecSnapshot(ev)
	c := &Collector{Host: envProc(fstest.MapFS{}, noDisk), Clock: clock.NewFake(time.Unix(0, 0))}
	f := c.Facts(snap)
	e := f.GetExecution()
	if f.GetCpuCores() != 2 || e.CpuEffectiveCores == nil || e.GetCpuEffectiveCores() != 1.5 {
		t.Fatalf("cpu_cores = %d, want ceil(1.5) = 2（execution 里是精确值）", f.GetCpuCores())
	}

	host := buildExecSnapshot(hostEvidence())
	host.exec.CpuEffectiveCores = nil // 识别不出
	f = c.Facts(host)
	if f.GetCpuCores() != 0 {
		t.Fatalf("cpu_cores = %d, want 0（不退回 runtime.NumCPU()）", f.GetCpuCores())
	}
}

// ---------- ProcFS.identify（证据收集 + 决策 + 读数入口） ----------

func TestProcIdentifyEnvAndHost(t *testing.T) {
	// 环境 cgroup：识别结果带上基线身份与三个环境读数入口。
	fsys := limitedFS()
	fsys["sys/fs/cgroup/memory.max"] = &fstest.MapFile{Data: []byte("536870912\n")}
	fsys["sys/fs/cgroup/memory.current"] = &fstest.MapFile{Data: []byte("1000\n")}
	fsys["sys/fs/cgroup/memory.stat"] = &fstest.MapFile{Data: []byte("file 100\nshmem 0\nslab_reclaimable 0\n")}
	fsys["sys/fs/cgroup/memory.swap.max"] = &fstest.MapFile{Data: []byte("268435456\n")}
	fsys["sys/fs/cgroup/memory.swap.current"] = &fstest.MapFile{Data: []byte("10\n")}
	snap := envProc(fsys, noDisk).identify()
	if snap.exec.GetKind() != heronv1.ScopeKind_SCOPE_KIND_CGROUP_NAMESPACE {
		t.Fatalf("kind = %v", snap.exec.GetKind())
	}
	if snap.cpuBaseline != "cgroup:"+fixtureCgroupDev+":101|cores=2" {
		t.Fatalf("cpuBaseline = %q", snap.cpuBaseline)
	}
	if snap.readCPUUsage == nil || snap.readMemEnv == nil || snap.readSwapEnv == nil {
		t.Fatal("env readers must be attached")
	}
	if used, err := snap.readMemEnv(); err != nil || used.used != 900 || used.total != 536870912 {
		t.Fatalf("mem = %+v, %v; want used 900 / total 536870912", used, err)
	}
	if used, err := snap.readSwapEnv(); err != nil || used.used != 10 || used.total != 268435456 {
		t.Fatalf("swap = %+v, %v", used, err)
	}

	// 真根：没有环境读数入口，基线是 /proc/stat。
	hostSnap := hostProc(limitedFS(), noDisk).identify()
	if hostSnap.exec.GetKind() != heronv1.ScopeKind_SCOPE_KIND_HOST || hostSnap.readMemEnv != nil {
		t.Fatalf("host snapshot = %+v", &hostSnap.exec)
	}
	if hostSnap.cpuBaseline != "host:/proc/stat|cores=16" {
		t.Fatalf("cpuBaseline = %q", hostSnap.cpuBaseline)
	}

}

// 识别失败或范围未知：读数缺失，也不记采集失败——原因在 execution，不在组件故障。
func TestMetricsUnknownScopesLeaveReadingsMissing(t *testing.T) {
	fsys := fstest.MapFS{
		"proc/sys/kernel/random/boot_id": {Data: []byte("b7c3a1e-5d2f-4e6a-9c8b-1a2b3c4d5e6f\n")},
		"proc/stat":                      {Data: []byte("cpu  100 0 50 800 20 0 10 5 0 0\n")},
		"proc/cpuinfo":                   {Data: cpuinfoProcessors(4)},
	}
	c := &Collector{Host: withSource(hostProc(fsys, noDisk), "9:9", "proc/stat", "proc/meminfo", "proc/loadavg"), Clock: clock.NewFake(time.Unix(0, 0))}
	m, err := c.Metrics(c.Identify())
	if m.CpuPct != nil || m.MemTotal != nil || m.SwapTotal != nil || m.Load1 != nil {
		t.Fatalf("unknown scopes must leave readings unset, got %+v", m)
	}
	// 夹具缺别的文件，那些组件照常报失败；范围未知只要求不冒充组件故障。
	for _, part := range []string{"cpu:", "memory:", "swap:", "load:"} {
		if err != nil && strings.Contains(err.Error(), part) {
			t.Fatalf("scope unknown must not be a collection failure: %v", err)
		}
	}
	if c.Facts(c.Identify()).GetExecution().GetCpu() != heronv1.ResourceScope_RESOURCE_SCOPE_UNKNOWN {
		t.Fatal("cpu scope must be unknown")
	}

	// 识别整体失败（nil 快照按失败处理）同样缺读数。
	m, _ = c.Metrics(nil)
	if m.CpuPct != nil || m.MemTotal != nil || m.Load1 != nil {
		t.Fatalf("identify failure must leave readings unset, got %+v", m)
	}
}

// ---------- 内存算式：t19 的真实快照 ----------

// memSnap 是 t19 实验快照文件（== memory.current == / == memory.stat == / == /proc/meminfo ==
// 分节）的解析结果，字段取用到的部分。
type memSnap struct {
	current  uint64
	file     uint64
	shmem    uint64
	slab     uint64
	memTot   uint64
	memAvail uint64
}

func parseMemSnap(t *testing.T, path string) memSnap {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var s memSnap
	section := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "== ") && strings.HasSuffix(line, " ==") {
			section = strings.Trim(line, "= ")
			continue
		}
		fields := strings.Fields(line)
		if section == "memory.current" && len(fields) >= 1 {
			s.current, _ = strconv.ParseUint(fields[0], 10, 64)
			continue
		}
		if len(fields) < 2 {
			continue
		}
		switch {
		case section == "memory.current":
		case section == "memory.stat":
			v, _ := strconv.ParseUint(fields[1], 10, 64)
			switch fields[0] {
			case "file":
				s.file = v
			case "shmem":
				s.shmem = v
			case "slab_reclaimable":
				s.slab = v
			}
		case section == "/proc/meminfo":
			v, _ := strconv.ParseUint(fields[1], 10, 64)
			v *= 1024
			switch fields[0] {
			case "MemTotal:":
				s.memTot = v
			case "MemAvailable:":
				s.memAvail = v
			}
		}
	}
	return s
}

// metricsFromMemSnap 把一份 t19 快照装进环境 cgroup 夹具并采一次，返回 (used, total)。
func metricsFromMemSnap(t *testing.T, snap memSnap, memMax uint64) (used, total uint64) {
	t.Helper()
	fsys := fstest.MapFS{
		"proc/meminfo":                        {Data: []byte(fmt.Sprintf("MemTotal: %d kB\nMemAvailable: %d kB\n", snap.memTot/1024, snap.memAvail/1024))},
		"proc/cpuinfo":                        {Data: cpuinfoProcessors(16)},
		"sys/fs/cgroup/cpu.stat":              {Data: []byte("usage_usec 1\n")},
		"sys/fs/cgroup/cpu.max":               {Data: []byte("max 100000\n")},
		"sys/fs/cgroup/cpuset.cpus.effective": {Data: []byte("0-15\n")},
		"sys/fs/cgroup/memory.max":            {Data: []byte(fmt.Sprintf("%d\n", memMax))},
		"sys/fs/cgroup/memory.current":        {Data: []byte(fmt.Sprintf("%d\n", snap.current))},
		"sys/fs/cgroup/memory.stat":           {Data: []byte(fmt.Sprintf("file %d\nshmem %d\nslab_reclaimable %d\nanon 1\n", snap.file, snap.shmem, snap.slab))},
	}
	c := &Collector{Host: envProc(fsys, noDisk), Clock: clock.NewFake(time.Unix(0, 0))}
	m, err := c.Metrics(c.Identify())
	if m.MemUsed == nil || m.MemTotal == nil || err != nil && strings.Contains(err.Error(), "memory:") {
		t.Fatalf("memory reading missing: m=%+v err=%v", m, err)
	}
	return m.GetMemUsed(), m.GetMemTotal()
}

// memStep 是内存算式验证的一步：注入名、前后快照文件、期望增量（MiB）、
// 是否对照宿主同向。
type memStep struct {
	name         string
	before, held string
	wantDelta    int64
	hostDirSame  bool
}

// 内存算式用真实快照验证。三组条件：Docker 容器 --memory 512m、Incus 6.0.4 guest
// （Debian 12，lxcfs 6.0.4，limits.memory=512MiB，内核 7.0.14-orbstack）、GitHub Actions
// ubuntu-24.04（内核 6.17.0-1022-azure）的 systemd-run scope（MemoryMax=512M）。各自做匿名、
// 页缓存、tmpfs 注入，CI 再加可回收 slab。used = current − (file − shmem) − slab_reclaimable，
// total = min(memory.max, MemTotal)，每步增量对应该组自己的注入量，不把一组的数字套到另一组。
func TestEnvMemoryArithmeticWithRealSnapshots(t *testing.T) {
	const mem512 = 536870912
	// 每步的期望增量（MiB）：与注入量 200/0/100/0 对应；CI 的 slab 步注入 30 万空文件
	// （dentry 约 385MiB）增量应 ≈0。宿主侧 MemTotal − MemAvailable 只在专用小机上可对照，
	// GitHub Actions ubuntu-24.04（内核 6.17.0-1022-azure）上宿主 MemTotal−MemAvailable 被 runner 背景淹没，不作断言。
	envs := []struct {
		dir   string
		steps []memStep
	}{
		{dir: "docker-512m", steps: []memStep{
			{"匿名 +200M", "snap-2-anon-before.txt", "snap-2-anon-held.txt", 211, true},
			{"页缓存 ≈0", "snap-3-pc-before.txt", "snap-3-pc-held.txt", 0, true},
			{"tmpfs +100M", "snap-4-shm-before.txt", "snap-4-shm-held.txt", 107, true},
		}},
		{dir: "incus-512m", steps: []memStep{
			{"匿名 +200M", "snap-2-anon-before.txt", "snap-2-anon-held.txt", 216, false},
			{"页缓存 ≈0", "snap-3-pc-before.txt", "snap-3-pc-held.txt", 0, false},
			{"tmpfs +100M", "snap-4-shm-before.txt", "snap-4-shm-held.txt", 101, false},
		}},
		{dir: "runner-scope-512m", steps: []memStep{
			{"匿名 +200M", "snap-1-anon-before.txt", "snap-1-anon-held.txt", 204, false},
			{"页缓存 ≈0", "snap-2-pc-before.txt", "snap-2-pc-held.txt", 0, false},
			{"tmpfs +100M", "snap-3-shm-before.txt", "snap-3-shm-held.txt", 99, false},
			{"可回收 slab ≈0", "snap-4-slab-before.txt", "snap-4-slab-held.txt", 0, false},
		}},
	}
	for _, env := range envs {
		t.Run(env.dir, func(t *testing.T) {
			for _, st := range env.steps {
				before := parseMemSnap(t, filepath.Join("testdata/memsnap", env.dir, st.before))
				held := parseMemSnap(t, filepath.Join("testdata/memsnap", env.dir, st.held))
				uB, tB := metricsFromMemSnap(t, before, mem512)
				uH, tH := metricsFromMemSnap(t, held, mem512)
				if tB != mem512 || tH != mem512 {
					t.Fatalf("%s %s: total = %d/%d, want min(memory.max, MemTotal) = %d", env.dir, st.name, tB, tH, mem512)
				}
				// 公式本身：used = current − (file − shmem) − slab。
				for _, x := range []struct {
					label string
					snap  memSnap
					used  uint64
				}{{"before", before, uB}, {"held", held, uH}} {
					want := x.snap.current - (x.snap.file - x.snap.shmem) - x.snap.slab
					if x.used != want {
						t.Fatalf("%s %s %s: used = %d, want %d", env.dir, st.name, x.label, x.used, want)
					}
				}
				// 增量与注入量对应（±10% 或 ±2MiB）。
				dMB := int64(uH-uB) / (1 << 20)
				lo, hi := st.wantDelta-2, st.wantDelta+2
				if st.wantDelta > 0 {
					lo, hi = st.wantDelta*9/10, st.wantDelta*11/10
				}
				if dMB < lo || dMB > hi {
					t.Fatalf("%s %s: Δused = %d MB, want ≈%d MB", env.dir, st.name, dMB, st.wantDelta)
				}
				// 专用小机（docker/Incus，宿主侧信号不被背景淹没）对照同向。
				if st.hostDirSame {
					hostB := int64(before.memTot) - int64(before.memAvail)
					hostH := int64(held.memTot) - int64(held.memAvail)
					if dMB > 0 && hostH < hostB || dMB < 0 && hostH > hostB {
						t.Fatalf("%s %s: Δused 与宿主不同向（cgroup %+d MB, 宿主 %+d MB）", env.dir, st.name, dMB, (hostH-hostB)/(1<<20))
					}
				}
			}
		})
	}
}

// 两文件两次读之间的竞争可能给出回绕的减法：按缺读数处理（checkUsage 拦 used>total，
// 中间值为负在这里拦），不报出伪装值。
func TestCgroupMemoryWrapGuards(t *testing.T) {
	base := fstest.MapFS{
		"proc/cpuinfo":                        {Data: cpuinfoProcessors(4)},
		"sys/fs/cgroup/cpu.max":               {Data: []byte("max 100000\n")},
		"sys/fs/cgroup/cpuset.cpus.effective": {Data: []byte("0-3\n")},
		"sys/fs/cgroup/cpu.stat":              {Data: []byte("usage_usec 1\n")},
		"sys/fs/cgroup/memory.max":            {Data: []byte("1000000\n")},
	}
	cases := []struct {
		name    string
		current string
		file    uint64
		shmem   uint64
		slab    uint64
	}{
		{"file 小于 shmem（memory.stat 自相矛盾）", "5000", 100, 200, 0},
		{"current 小于页缓存（两文件竞争）", "100", 5000, 0, 0},
		{"slab_reclaimable 大于匿名部分", "5000", 1000, 0, 9999},
	}
	for _, tc := range cases {
		fsys := base
		fsys["sys/fs/cgroup/memory.current"] = &fstest.MapFile{Data: []byte(tc.current + "\n")}
		fsys["sys/fs/cgroup/memory.stat"] = &fstest.MapFile{Data: []byte(fmt.Sprintf("file %d\nshmem %d\nslab_reclaimable %d\n", tc.file, tc.shmem, tc.slab))}
		p := envProc(fsys, noDisk)
		snap := p.identify()
		if snap.readMemEnv == nil {
			t.Fatalf("%s: memory scope must be environment", tc.name)
		}
		if _, err := snap.readMemEnv(); err == nil {
			t.Fatalf("%s: wrap-around subtraction must fail the reading", tc.name)
		}
	}
}

// ---------- 与本分支基点口径的等价（主机范围读数不变） ----------

// 同一主机快照驱动新旧两版采集：主机范围的 CPU、内存、swap、负载读数逐项相同。
// 旧版逻辑即本分支基点的实现（/proc/stat 差分、MemTotal−MemAvailable、SwapTotal−SwapFree、
// loadavg 原样），用同一批解析函数在测试里重算作为参照。
func TestHostReadingsEquivalentToBaseSemantics(t *testing.T) {
	fsys := fstest.MapFS{
		"proc/stat":    {Data: statLines("cpu  100 0 50 800 20 0 10 5 0 0\n", 4)},
		"proc/cpuinfo": {Data: cpuinfoProcessors(4)},
		"proc/meminfo": {Data: []byte("MemTotal: 8000 kB\nMemFree: 1000 kB\nMemAvailable: 3000 kB\nSwapTotal: 4000 kB\nSwapFree: 1500 kB\n")},
		"proc/loadavg": {Data: []byte("1.25 2.5 3.75 4/555 99\n")},
	}
	clk := clock.NewFake(time.Unix(0, 0))
	c := &Collector{Host: hostProc(fsys, noDisk), Clock: clk}
	c.Metrics(c.Identify())
	fsys["proc/stat"] = &fstest.MapFile{Data: statLines("cpu  180 30 90 850 40 10 10 5 0 0\n", 4)} // Δtotal 200，空闲 60
	clk.Advance(2 * time.Second)
	m, err := c.Metrics(c.Identify())
	if err != nil && strings.Contains(err.Error(), "cpu:") {
		t.Fatalf("主机 CPU 读数不应失败: %v", err)
	}

	// 旧口径参照：cpuRatios 是基点版本的差分公式，meminfo/swap/load 同为基点读法。
	cur, _ := parseStat(strings.NewReader(string(statLines("cpu  180 30 90 850 40 10 10 5 0 0\n", 4))))
	prev, _ := parseStat(strings.NewReader(string(statLines("cpu  100 0 50 800 20 0 10 5 0 0\n", 4))))
	busy, steal, iowait, ok := cpuRatios(prev, cur)
	if !ok || m.GetCpuPct() != busy || m.GetCpuStealPct() != steal || m.GetCpuIowaitPct() != iowait {
		t.Fatalf("cpu 读数与基点口径不同: %+v vs %v/%v/%v", m, busy, steal, iowait)
	}
	if m.GetMemTotal() != 8000*1024 || m.GetMemUsed() != (8000-3000)*1024 {
		t.Fatalf("mem 读数与基点口径不同: %d/%d", m.GetMemTotal(), m.GetMemUsed())
	}
	if m.GetSwapTotal() != 4000*1024 || m.GetSwapUsed() != (4000-1500)*1024 {
		t.Fatalf("swap 读数与基点口径不同: %d/%d", m.GetSwapTotal(), m.GetSwapUsed())
	}
	if m.GetLoad1() != 1.25 || m.GetLoad5() != 2.5 || m.GetLoad15() != 3.75 {
		t.Fatalf("load 读数与基点口径不同: %+v", m)
	}
	if f := c.Facts(c.Identify()); f.GetCpuCores() != 4 {
		t.Fatalf("cpu_cores = %d, want 主机处理器数 4（基点口径）", f.GetCpuCores())
	}

	// v1（legacy）同样与基点一致：识别只改标注，不改读法（同样两个周期拿到差分）。
	clk2 := clock.NewFake(time.Unix(0, 0))
	v1 := &Collector{Host: legacyProc(fsys, noDisk), Clock: clk2}
	v1.Metrics(v1.Identify())
	// 第三个样本 = 第二个样本 + （第二个 − 第一个）：legacy 两周期后的差分与主机口径相同。
	fsys["proc/stat"] = &fstest.MapFile{Data: statLines("cpu  260 60 130 900 60 20 10 5 0 0\n", 4)}
	clk2.Advance(2 * time.Second)
	if m1, _ := v1.Metrics(v1.Identify()); m1.GetCpuPct() != busy || m1.GetMemUsed() != m.GetMemUsed() || m1.GetLoad1() != m.GetLoad1() {
		t.Fatalf("v1 读数 %v 与主机口径 %v 不一致", m1, m)
	}
}

// 按核负载：分母是识别给出的负载范围核数；缺分母（cpuinfo 不是 procfs）时不设置。
func TestLoadPerCore(t *testing.T) {
	fsys := fstest.MapFS{
		"proc/stat":    {Data: []byte("cpu  1 1 1 1 1 1 1 1 1 1\n")},
		"proc/cpuinfo": {Data: cpuinfoProcessors(4)},
		"proc/loadavg": {Data: []byte("2.00 2.00 2.00 1/1 1\n")},
	}
	c := &Collector{Host: hostProc(fsys, noDisk), Clock: clock.NewFake(time.Unix(0, 0))}
	m, _ := c.Metrics(c.Identify())
	if m.GetLoad1PerCore() != 0.5 {
		t.Fatalf("load1_per_core = %v, want 2.00/4", m.GetLoad1PerCore())
	}

	// cpuinfo 换到 lxcfs：主机核数无从得知，按核负载缺失，load1 照报。
	p := withSource(hostProc(fsys, noDisk), fixtureLxcfsDev, "proc/cpuinfo")
	c2 := &Collector{Host: p, Clock: clock.NewFake(time.Unix(0, 0))}
	m2, _ := c2.Metrics(c2.Identify())
	if m2.Load1PerCore != nil || m2.GetLoad1() != 2.00 {
		t.Fatalf("load1_per_core = %v（应缺失）, load1 = %v", m2.Load1PerCore, m2.GetLoad1())
	}

	// v1：按核负载用 cpu_cores（cpuinfo 处理器数），同样要求两个文件都是 procfs。
	c3 := &Collector{Host: legacyProc(fsys, noDisk), Clock: clock.NewFake(time.Unix(0, 0))}
	m3, _ := c3.Metrics(c3.Identify())
	if m3.GetLoad1PerCore() != 0.5 {
		t.Fatalf("legacy load1_per_core = %v, want 0.5", m3.GetLoad1PerCore())
	}
}

// 部分 lxcfs（lxcfs 6.0.4，内核 7.0.14-orbstack，只 bind cpuinfo）：只把 cpuinfo 换源、loadavg 仍 proc——
// 来源判断按文件，而不是按 /proc 挂载点一概而论。
func TestPartialLxcfsIdentifiedPerFile(t *testing.T) {
	fsys := fstest.MapFS{
		"proc/stat":    {Data: []byte("cpu  1 1 1 1 1 1 1 1 1 1\n")},
		"proc/cpuinfo": {Data: cpuinfoProcessors(2)}, // lxcfs 视图
		"proc/loadavg": {Data: []byte("0.1 0.1 0.1 1/1 1\n")},
	}
	snap := withSource(hostProc(fsys, noDisk), fixtureLxcfsDev, "proc/cpuinfo").identify()
	e := &snap.exec
	if e.GetKind() != heronv1.ScopeKind_SCOPE_KIND_HOST || e.GetLoad() != heronv1.ResourceScope_RESOURCE_SCOPE_HOST {
		t.Fatalf("kind/load = %v/%v", e.GetKind(), e.GetLoad())
	}
	if e.CpuEffectiveCores != nil || e.LoadCores != nil {
		t.Fatalf("主机核数无从得知（cpuinfo 是 lxcfs）: cores=%v load_cores=%v", e.GetCpuEffectiveCores(), e.GetLoadCores())
	}
	for _, n := range e.GetNotes() {
		if n == heronv1.ScopeNote_SCOPE_NOTE_CPUINFO_NOT_PROCFS {
			return // 命中说明即通过
		}
	}
	t.Fatalf("必须记 CPUINFO_NOT_PROCFS 说明: %v", e.GetNotes())
}

// Facts 挂 execution 前自检：块由本方构造，非法即构造 bug——不把非法块发出去
// （hub 的同形校验会拒整份 Facts），日志留线索。合法块原样挂上。
func TestFactsDropsInvalidExecution(t *testing.T) {
	fsys := fstest.MapFS{"proc/cpuinfo": {Data: cpuinfoProcessors(4)}}
	c := &Collector{Host: hostProc(fsys, noDisk), Clock: clock.NewFake(time.Unix(0, 0))}

	valid := hostProc(fsys, noDisk).identify()
	if f := c.Facts(valid); f.GetExecution() == nil || f.GetExecution().GetKind() != heronv1.ScopeKind_SCOPE_KIND_HOST {
		t.Fatalf("合法块必须挂上: %+v", f.GetExecution())
	}

	bad := hostProc(fsys, noDisk).identify()
	bad.exec.Kind = heronv1.ScopeKind_SCOPE_KIND_UNSPECIFIED
	var logs bytes.Buffer
	c2 := &Collector{Host: hostProc(fsys, noDisk), Clock: c.Clock, Log: slog.New(slog.NewTextHandler(&logs, nil))}
	if f := c2.Facts(bad); f.GetExecution() != nil {
		t.Fatal("非法块必须被丢弃")
	}
	if !strings.Contains(logs.String(), "dropping invalid execution scope") {
		t.Fatalf("丢弃必须记日志: %q", logs.String())
	}
}

// ---------- identify 层（证据收集与决策的集成） ----------

// errFS 把指定文件的 Open/Stat 换成给定错误：制造"文件在、读不出"（EACCES）的识别场景。
type errFS struct {
	fstest.MapFS
	errs map[string]error // 相对路径 → 错误
}

// ReadFile 也覆写：fs.ReadFile 会先走 ReadFileFS 快路径（嵌入的 MapFS 带着这个方法），
// 不覆写就绕过 Open 的拦截。
func (f errFS) ReadFile(name string) ([]byte, error) {
	if err, ok := f.errs[name]; ok {
		return nil, err
	}
	return f.MapFS.ReadFile(name)
}

func (f errFS) Open(name string) (fs.File, error) {
	if err, ok := f.errs[name]; ok {
		return nil, err
	}
	return f.MapFS.Open(name)
}

func (f errFS) Stat(name string) (fs.FileInfo, error) {
	if err, ok := f.errs[name]; ok {
		return nil, err
	}
	return f.MapFS.Stat(name)
}

// 控制器未下放（内核 7.0.14-orbstack 的特权容器子 cgroup，未在 subtree_control 打开 cpu 与 memory）：cgroup.type 与 cpu.stat 在、cpu.max 与 memory.current 不在。
// 识别必须仍判成环境 cgroup（判据是 cgroup.type，不是 cpu.max 的存在性）；CPU 用量可读、
// 核数取 cpuinfo 估计；内存与 swap 未知并各记说明。这具夹具同时钉住"不能用 cpu.max
// 判环境"的注入基线。
func TestProcIdentifyControllerNotDelegated(t *testing.T) {
	fsys := fstest.MapFS{
		"proc/stat":    {Data: []byte("cpu  1 1 1 1 1 1 1 1 1 1\n")},
		"proc/cpuinfo": {Data: cpuinfoProcessors(16)},
		// 没有 cpu.max、没有 cpuset.cpus.effective、没有 memory.current/memory.stat/memory.max。
		"sys/fs/cgroup/cgroup.type": {Data: []byte("domain\n")},
		"sys/fs/cgroup/cpu.stat":    {Data: []byte("usage_usec 1000000\n")},
	}
	snap := envProc(fsys, noDisk).identify()
	e := &snap.exec
	if e.GetKind() != heronv1.ScopeKind_SCOPE_KIND_CGROUP_NAMESPACE {
		t.Fatalf("kind = %v, want cgroup_namespace（cgroup.type 存在即环境，与 cpu.max 无关）", e.GetKind())
	}
	if e.GetCpu() != heronv1.ResourceScope_RESOURCE_SCOPE_ENVIRONMENT || e.GetCpuEffectiveCores() != 16 {
		t.Fatalf("cpu = %v/%v, want environment 16 核（cpu.max 缺失按不限，cpuset 缺失回退 cpuinfo）", e.GetCpu(), e.GetCpuEffectiveCores())
	}
	if snap.readCPUUsage == nil {
		t.Fatal("cpu.stat 在，环境用量读数入口必须挂上")
	}
	if e.GetMemory() != unknown || e.GetSwap() != unknown {
		t.Fatalf("memory/swap = %v/%v, want unknown（控制器未下放）", e.GetMemory(), e.GetSwap())
	}
	for _, n := range e.GetNotes() {
		if n == heronv1.ScopeNote_SCOPE_NOTE_MEMORY_CONTROLLER_MISSING {
			return
		}
	}
	t.Fatalf("必须记 MEMORY_CONTROLLER_MISSING 说明: %v", e.GetNotes())
}

// 环境 cpu.stat 不存在（内核 7.0.14-orbstack 上部分控制器下放的 slice）：CPU 未知并记说明，
// 不回退 /proc/stat（那是别的口径）。
func TestProcIdentifyCpuStatMissing(t *testing.T) {
	fsys := limitedFS()
	delete(fsys, "sys/fs/cgroup/cpu.stat")
	snap := envProc(fsys, noDisk).identify()
	if snap.exec.GetCpu() != unknown || snap.readCPUUsage != nil {
		t.Fatalf("cpu = %v, want unknown without a reader", snap.exec.GetCpu())
	}
	for _, n := range snap.exec.GetNotes() {
		if n == heronv1.ScopeNote_SCOPE_NOTE_CPU_CONTROLLER_UNREADABLE {
			return
		}
	}
	t.Fatalf("必须记 CPU_CONTROLLER_UNREADABLE: %v", snap.exec.GetNotes())
}

// cpuset 读错误（权限）与内容认不出：CPU 未知并记说明，不回退也不把坏值当不限。
func TestProcIdentifyCpusetUnreadable(t *testing.T) {
	for name, mk := range map[string]func(fstest.MapFS) *ProcFS{
		"读错误": func(f fstest.MapFS) *ProcFS {
			p := envProc(f, noDisk)
			fixtureOf(p).FS = errFS{MapFS: f, errs: map[string]error{"sys/fs/cgroup/cpuset.cpus.effective": fs.ErrPermission}}
			return p
		},
		"内容认不出": func(f fstest.MapFS) *ProcFS {
			f["sys/fs/cgroup/cpuset.cpus.effective"] = &fstest.MapFile{Data: []byte("not-a-cpuset\n")}
			return envProc(f, noDisk)
		},
	} {
		fsys := limitedFS()
		snap := mk(fsys).identify()
		if snap.exec.GetCpu() != unknown {
			t.Fatalf("%s: cpu = %v, want unknown", name, snap.exec.GetCpu())
		}
		hit := false
		for _, n := range snap.exec.GetNotes() {
			if n == heronv1.ScopeNote_SCOPE_NOTE_CPU_CONTROLLER_UNREADABLE {
				hit = true
			}
		}
		if !hit {
			t.Fatalf("%s: 必须记 CPU_CONTROLLER_UNREADABLE: %v", name, snap.exec.GetNotes())
		}
	}
}

// cgroup.type 的 stat 出权限错误（文件在、看不了）：识别失败，不冒充真根。
func TestProcIdentifyCgroupTypePermissionError(t *testing.T) {
	fsys := limitedFS()
	p := envProc(fsys, noDisk)
	fixtureOf(p).FS = errFS{MapFS: fsys, errs: map[string]error{"sys/fs/cgroup/cgroup.type": fs.ErrPermission}}
	snap := p.identify()
	if snap.exec.GetKind() != heronv1.ScopeKind_SCOPE_KIND_IDENTIFY_FAILED {
		t.Fatalf("kind = %v, want identify failed（EACCES 不是 ENOENT）", snap.exec.GetKind())
	}
	if snap.readCPUUsage != nil || snap.readMemEnv != nil {
		t.Fatal("识别失败不得挂任何读数入口")
	}
}

// 叠加挂载与单文件换源（内核 7.0.14-orbstack 上实测）：mountinfo 同时留着被盖住的下层 proc
// 挂载（0:294）与新盖上的 proc（0:319），loadavg 被换成 tmpfs 文件（0:320）——结论跟着
// 每个文件自己的设备号走，与行序无关。
func TestProcIdentifyOvermountSources(t *testing.T) {
	fsys := fstest.MapFS{
		"proc/stat":    {Data: []byte("cpu  1 1 1 1 1 1 1 1 1 1\n")},
		"proc/meminfo": {Data: []byte("MemTotal: 8000 kB\nMemAvailable: 4000 kB\nSwapTotal: 4000 kB\nSwapFree: 1000 kB\n")},
		"proc/cpuinfo": {Data: cpuinfoProcessors(4)},
		"proc/loadavg": {Data: []byte("9.99 9.99 9.99 1/1 1\n")},
		"proc/self/mountinfo": {Data: []byte(strings.Join([]string{
			"7547 7516 0:294 / /proc rw,nosuid,nodev,noexec,relatime shared:4137 - proc proc rw",
			"8278 7547 0:319 / /proc rw,relatime - proc none rw",
			"8280 8278 0:320 /loadavg /proc/loadavg rw,relatime - tmpfs none rw",
		}, "\n") + "\n")},
	}
	p := hostProc(fsys, noDisk)
	inner := fixtureOf(p).StatIDFunc
	fixtureOf(p).StatIDFunc = func(name string) (string, uint64, error) {
		if name == "proc/loadavg" {
			return "0:320", 1, nil // 单文件换源后的实际设备
		}
		if strings.HasPrefix(name, "proc/") {
			return "0:319", 1, nil // 被盖住的下层 0:294 不可见
		}
		return inner(name)
	}
	snap := p.identify()
	e := &snap.exec
	if e.GetKind() != heronv1.ScopeKind_SCOPE_KIND_HOST || e.GetCpu() != heronv1.ResourceScope_RESOURCE_SCOPE_HOST {
		t.Fatalf("kind/cpu = %v/%v，stat 在新 proc（0:319）上是 procfs", e.GetKind(), e.GetCpu())
	}
	if e.GetMemory() != heronv1.ResourceScope_RESOURCE_SCOPE_HOST || e.MemoryLimitBytes == nil {
		t.Fatal("meminfo 在新 proc 上，内存照常")
	}
	if e.GetLoad() != unknown {
		t.Fatalf("load = %v, want unknown（loadavg 在 tmpfs 上）", e.GetLoad())
	}
	for _, n := range e.GetNotes() {
		if n == heronv1.ScopeNote_SCOPE_NOTE_LOADAVG_NOT_PROCFS {
			return
		}
	}
	t.Fatalf("必须记 LOADAVG_NOT_PROCFS: %v", e.GetNotes())
}

// 宿主没有 swap：真根与环境（controller 上限折到 0）都把已知的 0 报出来，读数 0/0。
func TestSwapKnownZeroReported(t *testing.T) {
	fsys := fstest.MapFS{
		"proc/stat":    {Data: []byte("cpu  1 1 1 1 1 1 1 1 1 1\n")},
		"proc/cpuinfo": {Data: cpuinfoProcessors(4)},
		"proc/meminfo": {Data: []byte("MemTotal: 8000 kB\nMemAvailable: 4000 kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\n")},
		"proc/loadavg": {Data: []byte("0.1 0.1 0.1 1/1 1\n")},
	}
	c := &Collector{Host: hostProc(fsys, noDisk), Clock: clock.NewFake(time.Unix(0, 0))}
	m, _ := c.Metrics(c.Identify())
	if m.GetSwapTotal() != 0 || m.SwapTotal == nil {
		t.Fatalf("swap_total = %v（nil=%v）, want 已知的 0", m.GetSwapTotal(), m.SwapTotal == nil)
	}
	if e := c.Facts(c.Identify()).GetExecution(); e.GetSwap() != heronv1.ResourceScope_RESOURCE_SCOPE_HOST || e.SwapLimitBytes == nil || e.GetSwapLimitBytes() != 0 {
		t.Fatalf("swap 上限必须照报 0: %+v", e)
	}

	// 环境：memory.swap.max 数值小于 0 也不会出现，这里验证 min(swap.max=512MiB, SwapTotal=0) = 0。
	envFsys := fstest.MapFS{
		"proc/stat":                           {Data: []byte("cpu  1 1 1 1 1 1 1 1 1 1\n")},
		"proc/cpuinfo":                        {Data: cpuinfoProcessors(4)},
		"proc/meminfo":                        {Data: []byte("MemTotal: 8000 kB\nMemAvailable: 4000 kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\n")},
		"proc/loadavg":                        {Data: []byte("0.1 0.1 0.1 1/1 1\n")},
		"sys/fs/cgroup/cpu.max":               {Data: []byte("max 100000\n")},
		"sys/fs/cgroup/cpuset.cpus.effective": {Data: []byte("0-3\n")},
		"sys/fs/cgroup/cpu.stat":              {Data: []byte("usage_usec 1\n")},
		"sys/fs/cgroup/memory.max":            {Data: []byte("104857600\n")},
		"sys/fs/cgroup/memory.current":        {Data: []byte("1000\n")},
		"sys/fs/cgroup/memory.stat":           {Data: []byte("file 100\nshmem 0\nslab_reclaimable 0\n")},
		"sys/fs/cgroup/memory.swap.max":       {Data: []byte("536870912\n")},
		"sys/fs/cgroup/memory.swap.current":   {Data: []byte("0\n")},
	}
	ec := &Collector{Host: envProc(envFsys, noDisk), Clock: clock.NewFake(time.Unix(0, 0))}
	em, err := ec.Metrics(ec.Identify())
	if err != nil && strings.Contains(err.Error(), "swap") {
		t.Fatalf("swap 读数不应失败: %v", err)
	}
	if em.SwapTotal == nil || em.GetSwapTotal() != 0 || em.GetSwapUsed() != 0 {
		t.Fatalf("swap 读数 = %v/%v, want 已知的 0/0", em.SwapTotal, em.SwapUsed)
	}
	if e := ec.Facts(ec.Identify()).GetExecution(); e.GetSwap() != environment || e.SwapLimitBytes == nil || e.GetSwapLimitBytes() != 0 {
		t.Fatalf("环境 swap 上限必须照报 0: %+v", e)
	}
}
