package collect

import (
	"bytes"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/agent/collect/collecttest"
	"github.com/xjetry/heron/internal/clock"
)

func cpuinfoProcessors(n int) []byte {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "processor: %d\nmodel name: Synth CPU\n", i)
	}
	return []byte(b.String())
}

// statLines 给聚合 cpu 行补上 n 条逐核行：真实 /proc/stat 的逐核行与宿主核数一致，
// 夹具带上它们才能区分"有效核数"与"宿主核数"两种口径。
func statLines(agg string, n int) []byte {
	var b strings.Builder
	b.WriteString(agg)
	for i := range n {
		fmt.Fprintf(&b, "cpu%d 1 1 1 1 1 1 1 1 0 0\n", i)
	}
	return []byte(b.String())
}

func noDisk() (uint64, uint64, error) { return 0, 0, nil }

// 识别注入的设备号约定：四个 /proc 文件都在 procfs 设备 0:22 上（mountinfo 夹具同值），
// /sys/fs/cgroup 在设备 0:33、inode 101；个别测试用 withSource 把单个文件改指 0:24
// （mountinfo 里的 fuse.lxcfs）或别的设备。
const (
	fixtureProcDev   = "0:22"
	fixtureLxcfsDev  = "0:24"
	fixtureCgroupDev = "0:33"
)

// newFixtureProc 构造带识别注入的 ProcFS：StatID 按 device 约定返回，FSType 按 cgroup2
// 开关返回 cgroup2fs 或 tmpfs 类型。MapFS 夹具缺 mountinfo 时补上两条挂载（proc 与
// fuse.lxcfs，后者供 withSource 的来源测试用）。读者根与宿主根是同一棵树（默认根 "/" 的形状）。
func newFixtureProc(fsys fs.FS, cgroup2 bool, diskUsage func() (uint64, uint64, error)) *ProcFS {
	if m, ok := fsys.(fstest.MapFS); ok {
		collecttest.EthernetDefaults(m)
		if _, has := m["proc/self/mountinfo"]; !has {
			m["proc/self/mountinfo"] = &fstest.MapFile{Data: []byte(strings.Join([]string{
				"42 41 " + fixtureProcDev + " / /proc rw,nosuid,nodev,noexec,relatime - proc proc rw",
				"44 41 " + fixtureLxcfsDev + " / /var/lib/lxcfs/proc rw,relatime - fuse.lxcfs lxcfs rw",
			}, "\n") + "\n")}
		}
	}
	kind := uint64(0x01001994) // tmpfs：任意非 cgroup2 的 statfs 类型
	if cgroup2 {
		kind = fsTypeCgroup2
	}
	r := collecttest.New(fsys)
	r.UsageFunc = diskUsage
	r.StatIDFunc = func(name string) (string, uint64, error) {
		if name == "sys/fs/cgroup" || strings.HasPrefix(name, "sys/fs/cgroup/") {
			return fixtureCgroupDev, 101, nil
		}
		return fixtureProcDev, 1, nil
	}
	r.FSTypeFunc = func(string) (uint64, error) { return kind, nil }
	return &ProcFS{Root: r, Reader: r}
}

// hostProc：cgroup2、挂载根是真根（夹具不放 cgroup.type）。
func hostProc(fsys fs.FS, diskUsage func() (uint64, uint64, error)) *ProcFS {
	return newFixtureProc(fsys, true, diskUsage)
}

// envProc：cgroup2、挂载根是环境 cgroup（写入 cgroup.type）。
func envProc(fsys fstest.MapFS, diskUsage func() (uint64, uint64, error)) *ProcFS {
	fsys["sys/fs/cgroup/cgroup.type"] = &fstest.MapFile{Data: []byte("domain\n")}
	return newFixtureProc(fsys, true, diskUsage)
}

// legacyProc：statfs 说 /sys/fs/cgroup 不是 cgroup2（v1 / 无 cgroup2）。
func legacyProc(fsys fs.FS, diskUsage func() (uint64, uint64, error)) *ProcFS {
	return newFixtureProc(fsys, false, diskUsage)
}

// withSource 把个别 /proc 文件改指到指定设备（mountinfo 里 0:24 是 fuse.lxcfs，
// 其余设备在来源判断里按 other/unknown 处理）。
func withSource(p *ProcFS, dev string, names ...string) *ProcFS {
	r := fixtureOf(p)
	inner := r.StatIDFunc
	set := make(map[string]bool, len(names))
	for _, s := range names {
		set[s] = true
	}
	r.StatIDFunc = func(name string) (string, uint64, error) {
		if set[name] {
			return dev, 1, nil
		}
		return inner(name)
	}
	return p
}

// 限额夹具：quota 2 核、cpuset 8 核、宿主 16 核，有效核数取较小者 2。
// /proc/stat 带 steal 列（8 个计数器）：真根路径就会把 steal/iowait 报出来。
func limitedFS() fstest.MapFS {
	return fstest.MapFS{
		"proc/stat":                           {Data: statLines("cpu  100 0 50 800 20 0 10 5 0 0\n", 16)},
		"proc/cpuinfo":                        {Data: cpuinfoProcessors(16)},
		"sys/fs/cgroup/cpu.max":               {Data: []byte("200000 100000\n")},
		"sys/fs/cgroup/cpuset.cpus.effective": {Data: []byte("0-7\n")},
		"sys/fs/cgroup/cpu.stat":              {Data: []byte("usage_usec 1000000\nuser_usec 600000\nsystem_usec 400000\n")},
	}
}

// 有限额时 cpu_pct 是 Δusage_usec / (有效核数 × Δt)，steal/iowait 不设置，Facts 报有效核数。
func TestCgroupLimitedCPUFollowsUsageNotProcStat(t *testing.T) {
	fsys := limitedFS()
	clk := clock.NewFake(time.Unix(0, 0))
	c := &Collector{Host: envProc(fsys, noDisk), Clock: clk}
	if m, _ := c.Metrics(c.Identify()); m.CpuPct != nil {
		t.Fatal("first sample must not carry cpu_pct")
	}
	// Δusage = 2000000 µs（两秒跑满一核）；/proc/stat 也动了，若被采用会得出别的值。
	fsys["sys/fs/cgroup/cpu.stat"] = &fstest.MapFile{Data: []byte("usage_usec 3000000\nuser_usec 1600000\nsystem_usec 1400000\n")}
	fsys["proc/stat"] = &fstest.MapFile{Data: statLines("cpu  200 0 100 1600 40 0 20 10 0 0\n", 16)}
	clk.Advance(2 * time.Second)
	m, _ := c.Metrics(c.Identify())
	if m.CpuPct == nil || m.GetCpuPct() != 50 {
		t.Fatalf("cpu_pct set %v = %v, want Δusage 2000000 / (2 cores × 2000000 µs) = 50", m.CpuPct != nil, m.GetCpuPct())
	}
	if m.CpuStealPct != nil || m.CpuIowaitPct != nil {
		t.Fatalf("steal/iowait are host-wide counters, must be unset under a limit: %v/%v", m.GetCpuStealPct(), m.GetCpuIowaitPct())
	}
	if f := c.Facts(c.Identify()); f.GetCpuCores() != 2 {
		t.Fatalf("Facts.cpu_cores = %d, want the effective 2 of min(quota 2, cpuset 8)", f.GetCpuCores())
	}

	// burst：Δusage 超过 有效核数 × Δt，算出 >100 钳到 100（ingest 拒绝 >100）。
	fsys["sys/fs/cgroup/cpu.stat"] = &fstest.MapFile{Data: []byte("usage_usec 13000000\n")}
	clk.Advance(2 * time.Second)
	m, _ = c.Metrics(c.Identify())
	if m.CpuPct == nil || m.GetCpuPct() != 100 {
		t.Fatalf("burst cpu_pct = %v, want clamped 100 (raw 100×10000000/(2×2000000)=250)", m.CpuPct)
	}

	// 用量回退（cgroup 复用）：不设置，只换基线。
	fsys["sys/fs/cgroup/cpu.stat"] = &fstest.MapFile{Data: []byte("usage_usec 500000\n")}
	clk.Advance(2 * time.Second)
	m, _ = c.Metrics(c.Identify())
	if m.CpuPct != nil {
		t.Fatalf("usage regression must leave cpu_pct unset, got %v", m.GetCpuPct())
	}
}

// cpuset 比 quota 更严格时取 cpuset：quota 4 核、cpuset 2 核，有效核数 2。
func TestCgroupEffectiveCoresTakeTheStricterOfQuotaAndCpuset(t *testing.T) {
	fsys := limitedFS()
	fsys["sys/fs/cgroup/cpu.max"] = &fstest.MapFile{Data: []byte("400000 100000\n")}
	fsys["sys/fs/cgroup/cpuset.cpus.effective"] = &fstest.MapFile{Data: []byte("0-1\n")}
	clk := clock.NewFake(time.Unix(0, 0))
	c := &Collector{Host: envProc(fsys, noDisk), Clock: clk}
	c.Metrics(c.Identify())
	fsys["sys/fs/cgroup/cpu.stat"] = &fstest.MapFile{Data: []byte("usage_usec 3000000\n")}
	clk.Advance(2 * time.Second)
	m, _ := c.Metrics(c.Identify())
	if m.CpuPct == nil || m.GetCpuPct() != 50 {
		t.Fatalf("cpu_pct set %v = %v, want Δusage 2000000 / (cpuset 2 × 2000000 µs) = 50", m.CpuPct != nil, m.GetCpuPct())
	}
	if f := c.Facts(c.Identify()); f.GetCpuCores() != 2 {
		t.Fatalf("Facts.cpu_cores = %d, want the cpuset 2 stricter than quota 4", f.GetCpuCores())
	}
}

// 环境配额不限（cpu.max 为 max，cpuset 覆盖宿主全部核或文件缺失）仍是环境口径：
// cpu_pct 走 cpu.stat、按宿主核数归一，steal/iowait 不设置；Facts 报 cpuinfo 的核数。
func TestEnvUnlimitedStillUsesCpuStat(t *testing.T) {
	for name, cpuset := range map[string]*fstest.MapFile{
		"cpuset covers all": {Data: []byte("0-15\n")},
		"no cpuset file":    nil,
	} {
		fsys := limitedFS()
		fsys["sys/fs/cgroup/cpu.max"] = &fstest.MapFile{Data: []byte("max 100000\n")}
		if cpuset == nil {
			delete(fsys, "sys/fs/cgroup/cpuset.cpus.effective")
		} else {
			fsys["sys/fs/cgroup/cpuset.cpus.effective"] = cpuset
		}
		clk := clock.NewFake(time.Unix(0, 0))
		c := &Collector{Host: envProc(fsys, noDisk), Clock: clk}
		c.Metrics(c.Identify())
		// Δusage 8000000 µs，16 核 × 2s：cpu_pct = 25；/proc/stat 的差分是 50，被采用会露馅。
		fsys["sys/fs/cgroup/cpu.stat"] = &fstest.MapFile{Data: []byte("usage_usec 9000000\n")}
		fsys["proc/stat"] = &fstest.MapFile{Data: statLines("cpu  135 0 60 850 20 0 10 10 0 0\n", 16)}
		clk.Advance(2 * time.Second)
		m, _ := c.Metrics(c.Identify())
		if m.CpuPct == nil || m.GetCpuPct() != 25 {
			t.Fatalf("%s: cpu_pct = %v, want cpu.stat diff 25 over 16 cores", name, m.GetCpuPct())
		}
		if m.CpuStealPct != nil || m.CpuIowaitPct != nil {
			t.Fatalf("%s: steal/iowait must stay unset in the environment scope", name)
		}
		if f := c.Facts(c.Identify()); f.GetCpuCores() != 16 {
			t.Fatalf("%s: Facts.cpu_cores = %d, want 16 from cpuinfo/cpuset", name, f.GetCpuCores())
		}
	}
}

// 真根（挂载根没有 cgroup.type）：读数是整台主机，cpu_pct 走 /proc/stat 差分，
// steal/iowait 照报，核数取 cpuinfo 处理器数。
func TestHostRootKeepsProcStatPath(t *testing.T) {
	fsys := limitedFS()
	delete(fsys, "sys/fs/cgroup/cpu.max")
	delete(fsys, "sys/fs/cgroup/cpuset.cpus.effective")
	delete(fsys, "sys/fs/cgroup/cpu.stat")
	clk := clock.NewFake(time.Unix(0, 0))
	c := &Collector{Host: hostProc(fsys, noDisk), Clock: clk}
	c.Metrics(c.Identify())
	fsys["proc/stat"] = &fstest.MapFile{Data: statLines("cpu  135 0 60 850 20 0 10 10 0 0\n", 16)} // Δtotal 100：steal 5、iowait 10、空闲 50
	clk.Advance(2 * time.Second)
	m, _ := c.Metrics(c.Identify())
	if m.CpuPct == nil || m.GetCpuPct() != 50 {
		t.Fatalf("cpu_pct = %v, want /proc/stat diff 50", m.GetCpuPct())
	}
	if m.CpuStealPct == nil || m.GetCpuStealPct() != 5 {
		t.Fatalf("steal = %v, want 5 from the same /proc/stat diff", m.GetCpuStealPct())
	}
	f := c.Facts(c.Identify())
	if f.GetCpuCores() != 16 {
		t.Fatalf("Facts.cpu_cores = %d, want 16 from cpuinfo", f.GetCpuCores())
	}
	if f.GetExecution().GetKind() != heronv1.ScopeKind_SCOPE_KIND_HOST {
		t.Fatalf("kind = %v, want HOST", f.GetExecution().GetKind())
	}
}

// cgroup v1 / 无 cgroup2（cpu/cpu.cfs_quota_us 存在也罢）：沿用现状读法，cpu_pct 走
// /proc/stat；回退只在状态变化时记一行日志，第二个周期不再记，换回 v2 时再记一行。
func TestCgroupV1FallsBackToProcStatAndLogsOnce(t *testing.T) {
	fsys := fstest.MapFS{
		"proc/stat":                          {Data: []byte("cpu  100 0 50 800 20 0 10 5 0 0\n")},
		"proc/cpuinfo":                       {Data: cpuinfoProcessors(16)},
		"sys/fs/cgroup/cpu/cpu.cfs_quota_us": {Data: []byte("200000\n")},
	}
	clk := clock.NewFake(time.Unix(0, 0))
	var logBuf bytes.Buffer
	// statfs 结果随宿主挂载变化：切换时把 /sys/fs/cgroup 换成 cgroup2。
	cgroup2 := false
	p := legacyProc(fsys, noDisk)
	fixtureOf(p).FSTypeFunc = func(string) (uint64, error) {
		if cgroup2 {
			return fsTypeCgroup2, nil
		}
		return 0x01001994, nil
	}
	c := &Collector{Host: p, Clock: clk, Log: slog.New(slog.NewTextHandler(&logBuf, nil))}
	if f := c.Facts(c.Identify()); f.GetExecution().GetKind() != heronv1.ScopeKind_SCOPE_KIND_CGROUP_V1_LEGACY {
		t.Fatalf("kind = %v, want legacy", f.GetExecution().GetKind())
	}
	c.Metrics(c.Identify())
	fsys["proc/stat"] = &fstest.MapFile{Data: []byte("cpu  135 0 60 850 20 0 10 10 0 0\n")}
	clk.Advance(2 * time.Second)
	m, _ := c.Metrics(c.Identify())
	if m.CpuPct == nil || m.GetCpuPct() != 50 {
		t.Fatalf("v1 must fall back to /proc/stat: cpu_pct = %v, want 50", m.GetCpuPct())
	}
	lines := strings.Count(strings.TrimRight(logBuf.String(), "\n"), "\n") + 1
	if lines != 1 || !strings.Contains(logBuf.String(), "cgroup v1: cpu limits are not readable") {
		t.Fatalf("v1 must be logged exactly once across two periods, got %d lines: %q", lines, logBuf.String())
	}

	// v1 → v2：挂载根成了环境 cgroup，再记一行；cpu_pct 换到 cpu.stat 口径。
	delete(fsys, "sys/fs/cgroup/cpu/cpu.cfs_quota_us")
	cgroup2 = true
	fsys["sys/fs/cgroup/cgroup.type"] = &fstest.MapFile{Data: []byte("domain\n")}
	fsys["sys/fs/cgroup/cpu.stat"] = &fstest.MapFile{Data: []byte("usage_usec 1000000\n")}
	clk.Advance(2 * time.Second)
	c.Metrics(c.Identify())
	if got := strings.Count(logBuf.String(), "cgroup v2"); got != 1 {
		t.Fatalf("v1→v2 switch must add one log line, cgroup v2 appears %d times: %q", got, logBuf.String())
	}
	fsys["sys/fs/cgroup/cpu.stat"] = &fstest.MapFile{Data: []byte("usage_usec 5000000\n")} // 16 核 × 2s：12.5
	clk.Advance(2 * time.Second)
	if m, _ := c.Metrics(c.Identify()); m.CpuPct == nil || m.GetCpuPct() != 12.5 {
		t.Fatalf("after the switch cpu_pct = %v, want cpu.stat based 12.5", m.GetCpuPct())
	}
}

// 休眠重置对环境 cgroup 路径同样回到首样本语义：prevCgroup 只能靠 ResetRates 清空；
// 不重置时，恢复后的第一次采样拿休眠前的 usage_usec 做差分，漏出跨重置边界的读数。
func TestResetRatesClearsCgroupUsageBaseline(t *testing.T) {
	fsys := limitedFS()
	clk := clock.NewFake(time.Unix(0, 0))
	c := &Collector{Host: envProc(fsys, noDisk), Clock: clk}
	usage := uint64(1000000)
	bump := func() {
		usage += 2000000 // 有效核数 2 × 2s 区间的一半：cpu_pct 恒为 50
		fsys["sys/fs/cgroup/cpu.stat"] = &fstest.MapFile{Data: []byte(fmt.Sprintf("usage_usec %d\n", usage))}
		clk.Advance(2 * time.Second)
	}
	c.Metrics(c.Identify())
	bump()
	if m, _ := c.Metrics(c.Identify()); m.GetCpuPct() != 50 {
		t.Fatalf("second sample must carry cpu_pct 50, got %+v", m)
	}
	c.ResetRates()
	bump()
	if m, _ := c.Metrics(c.Identify()); m.CpuPct != nil {
		t.Fatalf("cgroup usage baseline must be reset: cpu_pct = %v across the reset boundary", m.GetCpuPct())
	}
	bump()
	if m, _ := c.Metrics(c.Identify()); m.GetCpuPct() != 50 {
		t.Fatalf("cpu_pct must resume one sample after reset, got %+v", m)
	}
}

// 无 cgroup2 的环境（连 v1 的标记文件都没有）：走 /proc/stat，不记日志。
func TestNoCgroupKeepsProcStatPath(t *testing.T) {
	fsys := fstest.MapFS{
		"proc/stat":    {Data: []byte("cpu  100 0 50 800 20 0 10 5 0 0\n")},
		"proc/cpuinfo": {Data: cpuinfoProcessors(4)},
	}
	clk := clock.NewFake(time.Unix(0, 0))
	var logBuf bytes.Buffer
	// statfs 结果随宿主挂载变化：切换时把 /sys/fs/cgroup 换成 cgroup2。
	cgroup2 := false
	p := legacyProc(fsys, noDisk)
	fixtureOf(p).FSTypeFunc = func(string) (uint64, error) {
		if cgroup2 {
			return fsTypeCgroup2, nil
		}
		return 0x01001994, nil
	}
	c := &Collector{Host: p, Clock: clk, Log: slog.New(slog.NewTextHandler(&logBuf, nil))}
	c.Metrics(c.Identify())
	fsys["proc/stat"] = &fstest.MapFile{Data: []byte("cpu  135 0 60 850 20 0 10 10 0 0\n")}
	clk.Advance(2 * time.Second)
	m, _ := c.Metrics(c.Identify())
	if m.CpuPct == nil || m.GetCpuPct() != 50 {
		t.Fatalf("cpu_pct = %v, want /proc/stat diff 50", m.CpuPct)
	}
	// 没有挂载根信息与 v1 同一分支：口径标 legacy，切换记一行。
	if !strings.Contains(logBuf.String(), "cgroup v1") {
		t.Fatalf("legacy scope must be logged once, got %q", logBuf.String())
	}
}

// cpu.max 存在却认不出：有效核数未知，CPU 范围未知、读数缺失（不记采集失败——原因在
// execution 里），也绝不回退到全机口径。
func TestGarbledCpuMaxLeavesScopeUnknown(t *testing.T) {
	fsys := limitedFS()
	fsys["sys/fs/cgroup/cpu.max"] = &fstest.MapFile{Data: []byte("bogus\n")}
	c := &Collector{Host: envProc(fsys, noDisk), Clock: clock.NewFake(time.Unix(0, 0))}
	m, err := c.Metrics(c.Identify())
	if m.CpuPct != nil {
		t.Fatalf("garbled cpu.max must not produce cpu_pct, got %v", m.GetCpuPct())
	}
	// 范围未知经由 execution 上报，不冒充 CPU 组件故障（夹具缺 loadavg 等文件，那些照常报）。
	if err != nil && strings.Contains(err.Error(), "cpu:") {
		t.Fatalf("scope unknown must not be a cpu collection failure: %v", err)
	}
	if sc := c.Facts(c.Identify()).GetExecution(); sc.GetCpu() != heronv1.ResourceScope_RESOURCE_SCOPE_UNKNOWN {
		t.Fatalf("cpu scope = %v, want unknown", sc.GetCpu())
	}
}

// 挂载根换了一个 cgroup 目录（容器重建）：基线身份变化，差分清零，首样本不出 cpu_pct。
func TestCgroupDirChangeDropsBaseline(t *testing.T) {
	fsys := limitedFS()
	clk := clock.NewFake(time.Unix(0, 0))
	p := envProc(fsys, noDisk)
	c := &Collector{Host: p, Clock: clk}
	c.Metrics(c.Identify())
	fsys["sys/fs/cgroup/cpu.stat"] = &fstest.MapFile{Data: []byte("usage_usec 3000000\n")}
	clk.Advance(2 * time.Second)
	if m, _ := c.Metrics(c.Identify()); m.GetCpuPct() != 50 {
		t.Fatalf("cpu_pct = %v, want 50 before the container is recreated", m.GetCpuPct())
	}
	// 同一挂载点、新的 cgroup 目录（inode 变化）：旧基线不可比，首样本不出读数。
	inner := fixtureOf(p).StatIDFunc
	fixtureOf(p).StatIDFunc = func(name string) (string, uint64, error) {
		if name == "sys/fs/cgroup" || strings.HasPrefix(name, "sys/fs/cgroup/") {
			return fixtureCgroupDev, 202, nil
		}
		return inner(name)
	}
	fsys["sys/fs/cgroup/cpu.stat"] = &fstest.MapFile{Data: []byte("usage_usec 5000000\n")}
	clk.Advance(2 * time.Second)
	if m, _ := c.Metrics(c.Identify()); m.CpuPct != nil {
		t.Fatalf("baseline must be dropped after the cgroup dir changed, got %v", m.GetCpuPct())
	}
	fsys["sys/fs/cgroup/cpu.stat"] = &fstest.MapFile{Data: []byte("usage_usec 7000000\n")}
	clk.Advance(2 * time.Second)
	if m, _ := c.Metrics(c.Identify()); m.GetCpuPct() != 50 {
		t.Fatalf("cpu_pct must resume on the new baseline, got %v", m.GetCpuPct())
	}
}

// 解析层：cpu.max 的 quota 折核、cpuset 列表计数、cpu.stat 取 usage_usec。
func TestParseCgroupFiles(t *testing.T) {
	if cores, ok, err := parseCPUMax("200000 100000"); err != nil || !ok || cores != 2 {
		t.Fatalf("quota 200000/100000 = %v/%v, %v; want 2 cores", cores, ok, err)
	}
	if cores, ok, err := parseCPUMax("150000 100000"); err != nil || !ok || cores != 1.5 {
		t.Fatalf("quota 150000/100000 = %v/%v, %v; want 1.5 cores", cores, ok, err)
	}
	if _, ok, err := parseCPUMax("max 100000"); err != nil || ok {
		t.Fatalf("max = unlimited %v, %v; want not ok", ok, err)
	}
	for _, s := range []string{"", "200000", "max 0", "0 100000", "x 100000"} {
		if _, _, err := parseCPUMax(s); err == nil {
			t.Fatalf("parseCPUMax(%q) must fail", s)
		}
	}
	for s, want := range map[string]int{"0-15": 16, "0-1": 2, "0": 1, "0-3,8,10-11": 7} {
		if n, err := parseCPUSetCount(s); err != nil || n != want {
			t.Fatalf("parseCPUSetCount(%q) = %d, %v; want %d", s, n, err, want)
		}
	}
	for _, s := range []string{"", "a", "0-", "3-1", "0-1,"} {
		if _, err := parseCPUSetCount(s); err == nil {
			t.Fatalf("parseCPUSetCount(%q) must fail", s)
		}
	}
	if v, err := parseCPUStatUsage("usage_usec 14507514\nuser_usec 6709141\n"); err != nil || v != 14507514 {
		t.Fatalf("usage_usec = %d, %v; want 14507514", v, err)
	}
	if _, err := parseCPUStatUsage("user_usec 1\n"); err == nil {
		t.Fatal("cpu.stat without usage_usec must fail")
	}
}
