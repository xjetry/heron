package collect

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"path"
	"runtime"
	"slices"
	"strings"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/agentwire"
	"github.com/xjetry/heron/internal/clock"
	"google.golang.org/protobuf/proto"
)

// Collector 把 Host 的原始读数变成一次上报。
type Collector struct {
	Host  Host
	Clock clock.Clock
	// Log 记采集口径的状态切换（v1 ↔ v2 等，noteScopeKind）；nil 时不记。切换不是采集失败，
	// 失败走 Metrics 返回的 error 与 diagnostics。
	Log *slog.Logger
	// NetInclude 非空时只统计匹配的网卡；否则统计除 NetExclude 外的全部。
	NetInclude []string
	// NetExclude 为 nil 时用 Host.defaultNetExclude。
	NetExclude []string
	Version    string
	// IcmpAvailable 默认为 false；须在 Runner.Run 前赋值，运行期间只读。
	IcmpAvailable bool

	// 两次采样的差分基线。这组 prev* 字段都在休眠重置范围内：跨越休眠的区间会把停机时段
	// 混进分母，休眠信号到达时全部作废，下一个周期只重建基线、不出速率与占比读数。
	prevCPU       *cpuTimes
	prevCPUKey    string
	prevCgroup    *uint64
	prevCgroupT   time.Duration
	prevNet       []ifaceCounters // 上一次采样计入合计的网卡，按名称字节序；nil 表示没有基线
	prevNetT      time.Duration
	prevDisk      *diskCounters
	prevDiskT     time.Duration
	prevDiskEpoch string
	prevTCP       *tcpCounters
	prevTCPT      time.Duration
	scopeKind     heronv1.ScopeKind
	execErrPrev   string // 上一次 Facts 自检的错误文本；空 = 合法。只在变化时记日志
	diagnostics   *heronv1.AgentDiagnostics
	// netList 是最近一次 Metrics 计入合计的逐网卡计数，交给 ReportRequest.net_interfaces；计入网卡多于
	// agentwire.MaxNetInterfaces 或网络采集失败时为 nil。netListOver 记上一次是否因超限省掉列表，只在切换时记日志。
	netList     []*heronv1.NetInterfaceCounter
	netListOver bool
	netFallback bool // 上一次采样是否退回了名字规则，只在切换时记日志
}

// Close 释放 Host 持有的资源。cmd/agent 退出时调用；之后 Collector 不能再用。
func (c *Collector) Close() error { return c.Host.close() }

// structureRules：结构规则只属于平台默认规则——显式给出包含或排除规则即整个替换默认规则；
// 平台不提供结构属性时不启用。
func (c *Collector) structureRules() bool {
	return len(c.NetInclude) == 0 && c.NetExclude == nil && c.Host.hasNetStructure()
}

// noteNetFallback 在进入与离开"结构规则排空、退回名字规则"时各记一行日志；按周期记会把日志刷满，只记切换。
func (c *Collector) noteNetFallback(fallback bool, counted int) {
	if fallback == c.netFallback {
		return
	}
	c.netFallback = fallback
	if c.Log == nil {
		return
	}
	if fallback {
		c.Log.Warn("structure rules excluded every interface the name rules allow; counting by name rules only", "interfaces", counted)
	} else {
		c.Log.Info("structure rules leave interfaces to count again; fallback to name rules ended", "interfaces", counted)
	}
}

func (c *Collector) netFilters() (include, exclude []string) {
	if len(c.NetInclude) > 0 {
		return c.NetInclude, nil
	}
	if c.NetExclude != nil {
		return nil, c.NetExclude
	}
	return nil, c.Host.defaultNetExclude()
}

// Validate 在上报循环启动前约束参数；错误配置不进入不断上报、不断被拒绝的循环。
func (c *Collector) Validate() error {
	return agentwire.ValidateNetFilters(c.NetInclude, c.NetExclude)
}

func (c *Collector) includeIface(name string) bool {
	include, exclude := c.netFilters()
	if len(include) > 0 {
		return matchAny(include, name)
	}
	return !matchAny(exclude, name)
}

func matchAny(patterns []string, name string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, name); ok {
			return true
		}
	}
	return false
}

// checkUsage 拒收超过总量的读数：按读不到处理并记日志，不截到总量——截断会把错误的计数伪装成满载。
// 单次无符号减法回绕的结果必然大于被减数，所以 Linux 的 total − available、total − free 回绕由这里兜住。
// 先减后加的组合（darwin 的 internal − purgeable + wire + compressor）回绕后可能落回总量以内，
// 这里看不出；这类组合的每个中间差由 Host 自己拒收（Host.memory 的约定，darwin 由 vmCounts.usedPages 承担）。
func checkUsage(u usage, err error) (usage, error) {
	if err != nil {
		return usage{}, err
	}
	if u.used > u.total {
		return usage{}, fmt.Errorf("used %d exceeds total %d", u.used, u.total)
	}
	return u, nil
}

// Identify 由 runner 每个上报周期调一次：识别一次执行环境得到快照，交给 Metrics 与 Facts。
// 识别文件（mountinfo、cgroup.type、cpu.max……）每周期只读一遍；Metrics 与 Facts 不再
// 各自判断口径，同一周期内两者的范围与容量一致。
func (c *Collector) Identify() *execSnapshot { return c.Host.identify() }

// Metrics 生成一次上报。任何一个来源读不到只让对应读数缺失，其余照常；
// 返回的 error 汇总了这些失败，供调用方记日志，不阻止上报。
// snap 必须来自同周期的 Identify；nil 按识别失败处理（依赖它的读数缺失，不硬编一个口径）。
func (c *Collector) Metrics(snap *execSnapshot) (*heronv1.Metrics, error) {
	m := &heronv1.Metrics{}
	include, exclude := c.netFilters()
	c.diagnostics = &heronv1.AgentDiagnostics{NetInclude: slices.Clone(include), NetExclude: slices.Clone(exclude)}
	c.diagnostics.NetStructureRules = c.structureRules()
	var errs []error
	fail := func(part heronv1.CollectionComponent, err error) {
		what := strings.ToLower(strings.TrimPrefix(part.String(), "COLLECTION_COMPONENT_"))
		errs = append(errs, fmt.Errorf("%s: %w", what, err))
		c.diagnostics.FailedCollectors = append(c.diagnostics.FailedCollectors, part)
	}
	h := c.Host
	if snap == nil {
		snap = identifyFailedSnapshot()
	}
	c.noteScopeKind(snap.exec.GetKind())

	if id, err := h.bootID(); err == nil {
		m.BootId = id
	} else {
		fail(heronv1.CollectionComponent_COLLECTION_COMPONENT_BOOT_ID, err)
	}

	// CPU：识别已定口径。环境 cgroup 走 cpu.stat 差分，真根或 v1 退回 /proc/stat 差分；
	// 范围未知时不读——按别的口径报出的是伪装成真值的错值。基线键（来源身份 + 有效核数）
	// 变了就丢弃旧基线：容器重建、限额或 cpuset 变化后，旧差分的分母不再是本次口径。
	switch {
	case snap.exec.GetCpu() == heronv1.ResourceScope_RESOURCE_SCOPE_ENVIRONMENT && snap.readCPUUsage != nil:
		c.changeCPUBaseline(snap.cpuBaseline)
		// 环境内 /proc/stat 是宿主全机口径：cpu_pct 只由本环境用量差分得出，
		// steal/iowait 是全机计数的一部分、不归属于本环境的算力，不设置。
		c.prevCPU = nil
		usage, err := snap.readCPUUsage()
		if err != nil {
			fail(heronv1.CollectionComponent_COLLECTION_COMPONENT_CPU, err)
			break
		}
		now := c.Clock.Mono()
		if c.prevCgroup != nil {
			if pct, ok := cgroupCPUPercent(*c.prevCgroup, usage, snap.cpuCores, now-c.prevCgroupT); ok {
				// cpu.max 允许 burst 时用量可超过 cores × Δt（内核先记账后扣），算出 >100；
				// ingest 拒绝一切 >100 的百分比，钳到 100。
				m.CpuPct = proto.Float64(min(pct, 100))
			}
		}
		c.prevCgroup, c.prevCgroupT = &usage, now
	case snap.exec.GetCpu() == heronv1.ResourceScope_RESOURCE_SCOPE_HOST ||
		snap.exec.GetCpu() == heronv1.ResourceScope_RESOURCE_SCOPE_LEGACY:
		c.changeCPUBaseline(snap.cpuBaseline)
		c.prevCgroup = nil
		if cur, err := h.cpuTimes(); err == nil {
			if c.prevCPU != nil {
				if busy, steal, iowait, ok := cpuRatios(*c.prevCPU, cur); ok {
					m.CpuPct = proto.Float64(busy)
					if cur.hasSteal {
						m.CpuStealPct = proto.Float64(steal)
					}
					if cur.hasIowait {
						m.CpuIowaitPct = proto.Float64(iowait)
					}
				}
			}
			c.prevCPU = &cur
		} else {
			fail(heronv1.CollectionComponent_COLLECTION_COMPONENT_CPU, err)
		}
	default:
		// 范围未知：缺读数，也不记采集失败——原因在 Facts.execution 的范围与说明里。
		c.changeCPUBaseline("")
	}

	// 内存与 swap：环境 cgroup 走 controller 文件（分母是识别给出的可见上限），
	// 真根或 v1 走 meminfo；范围未知时缺读数，不记采集失败。
	if u, err := c.readUsage(snap.exec.GetMemory(), snap.readMemEnv, h.memory); err == nil {
		m.MemTotal, m.MemUsed = proto.Uint64(u.total), proto.Uint64(u.used)
	} else if !errors.Is(err, errScopeUnknown) {
		fail(heronv1.CollectionComponent_COLLECTION_COMPONENT_MEMORY, err)
	}
	if u, err := c.readUsage(snap.exec.GetSwap(), snap.readSwapEnv, h.swap); err == nil {
		m.SwapTotal, m.SwapUsed = proto.Uint64(u.total), proto.Uint64(u.used)
	} else if !errors.Is(err, errScopeUnknown) {
		fail(heronv1.CollectionComponent_COLLECTION_COMPONENT_SWAP, err)
	}
	if u, err := checkUsage(h.disk()); err == nil {
		m.DiskTotal, m.DiskUsed = proto.Uint64(u.total), proto.Uint64(u.used)
	} else {
		fail(heronv1.CollectionComponent_COLLECTION_COMPONENT_DISK, err)
	}

	if snap.exec.GetLoad() == heronv1.ResourceScope_RESOURCE_SCOPE_HOST ||
		snap.exec.GetLoad() == heronv1.ResourceScope_RESOURCE_SCOPE_LEGACY {
		if l, err := h.load(); err == nil {
			m.Load1, m.Load5, m.Load15 = proto.Float64(l.l1), proto.Float64(l.l5), proto.Float64(l.l15)
			// 按核负载：分母是识别给出的负载范围核数；缺分母时不设置（不猜）。
			if cores := snap.exec.GetLoadCores(); cores > 0 {
				m.Load1PerCore = proto.Float64(l.l1 / float64(cores))
			}
		} else {
			fail(heronv1.CollectionComponent_COLLECTION_COMPONENT_LOAD, err)
		}
	}
	if n, err := h.procs(); err == nil {
		m.Procs = proto.Uint32(n)
	} else {
		fail(heronv1.CollectionComponent_COLLECTION_COMPONENT_PROCS, err)
	}
	if up, err := h.uptime(); err == nil {
		m.UptimeS = proto.Uint64(up)
	} else {
		fail(heronv1.CollectionComponent_COLLECTION_COMPONENT_UPTIME, err)
	}
	if tcp, udp, err := h.conns(); err == nil {
		m.TcpConns, m.UdpConns = proto.Uint32(tcp), proto.Uint32(udp)
	} else {
		fail(heronv1.CollectionComponent_COLLECTION_COMPONENT_CONNS, err)
	}

	// TCP 计数：四项来自同一次读取，一起出现或一起缺失（hub 按此准入）。任一计数倒退（网络命名空间重建、
	// 32 位内核回绕）只换基线；读不出时保留旧基线，下一次成功采样的差分覆盖更长的间隔，速率仍正确。
	if cur, err := h.tcpStats(); err == nil {
		now := c.Clock.Mono()
		if p := c.prevTCP; p != nil && now > c.prevTCPT && cur.activeOpens >= p.activeOpens && cur.passiveOpens >= p.passiveOpens &&
			cur.outSegs >= p.outSegs && cur.retransSegs >= p.retransSegs {
			secs := float64(now-c.prevTCPT) / float64(time.Second)
			m.TcpActiveOpensPs = proto.Float64(float64(cur.activeOpens-p.activeOpens) / secs)
			m.TcpPassiveOpensPs = proto.Float64(float64(cur.passiveOpens-p.passiveOpens) / secs)
			m.TcpOutSegsPs = proto.Float64(float64(cur.outSegs-p.outSegs) / secs)
			m.TcpRetransSegsPs = proto.Float64(float64(cur.retransSegs-p.retransSegs) / secs)
		}
		c.prevTCP, c.prevTCPT = &cur, now
	} else if !errors.Is(err, errNoTCPStats) {
		fail(heronv1.CollectionComponent_COLLECTION_COMPONENT_TCP_STATS, err)
	}

	c.netList = nil
	if ifs, sel, err := c.netCounters(); err == nil {
		list := make([]*heronv1.NetInterfaceCounter, len(ifs))
		names := make([]string, len(ifs))
		for i, it := range ifs {
			list[i] = &heronv1.NetInterfaceCounter{Name: it.name, RxBytes: it.rx, TxBytes: it.tx}
			names[i] = it.name
		}
		// 合计与摘要取自全集，与列表是否随请求发出无关。列表非空时 Runner.omitDerivedNetTotals
		// 省掉这三项；hub 准入只对仍出现的摘要做格式检查（agentwire.ValidateCounterEpoch），通过之后
		// normalizeNetTotals 用列表覆盖合计与摘要，不对照原报。列表因超过上限而缺席时，
		// hub 保留这里的合计，账本按合计差分。
		sum := agentwire.NetInterfaceSummary(list)
		m.NetRxTotal, m.NetTxTotal, m.NetCounterEpoch = proto.Uint64(sum.Rx), proto.Uint64(sum.Tx), sum.Epoch
		c.diagnostics.NetInterfacesTotal = uint32(len(names))
		c.diagnostics.NetInterfaces = names[:min(len(names), agentwire.MaxDiagnosticInterfaces)]
		c.diagnostics.NetStructureExcludedTotal = uint32(len(sel.excluded))
		c.diagnostics.NetStructureExcluded = sel.excluded[:min(len(sel.excluded), agentwire.MaxDiagnosticInterfaces)]
		c.diagnostics.NetStructureFallback = sel.fallback
		c.noteNetFallback(sel.fallback, len(ifs))
		c.noteNetListOver(len(list))
		if len(list) <= agentwire.MaxNetInterfaces {
			c.netList = list
		}
		now := c.Clock.Mono()
		if c.prevNet != nil && now > c.prevNetT {
			if rx, tx, ok := netDelta(c.prevNet, ifs); ok {
				secs := float64(now-c.prevNetT) / float64(time.Second)
				m.NetRxBps = proto.Uint64(uint64(rx / secs))
				m.NetTxBps = proto.Uint64(uint64(tx / secs))
			}
		}
		c.prevNet, c.prevNetT = ifs, now
	} else {
		fail(heronv1.CollectionComponent_COLLECTION_COMPONENT_NET, err)
	}

	if sum, epoch, err := c.diskTotals(); err == nil {
		now := c.Clock.Mono()
		// 整盘设备集合不变、计数不回退，两次采样才可相减；首样本与集合变化都只换基线，不伪造速率。
		if c.prevDisk != nil && c.prevDiskEpoch == epoch && now > c.prevDiskT && sum.read >= c.prevDisk.read && sum.write >= c.prevDisk.write {
			secs := float64(now-c.prevDiskT) / float64(time.Second)
			m.DiskReadBps = proto.Uint64(uint64(float64(sum.read-c.prevDisk.read) / secs))
			m.DiskWriteBps = proto.Uint64(uint64(float64(sum.write-c.prevDisk.write) / secs))
		}
		c.prevDisk, c.prevDiskT, c.prevDiskEpoch = &sum, now, epoch
	} else if !errors.Is(err, errNoDiskCounters) {
		fail(heronv1.CollectionComponent_COLLECTION_COMPONENT_DISK_IO, err)
	}

	return m, errors.Join(errs...)
}

// ResetRates 作废全部速率与差分基线，下一次 Metrics 回到首样本语义（无 cpu_pct、无网卡/磁盘速率）。
// 跨越休眠的那一次差分，分子含休眠前后的计数变化、分母（单调钟增量）不含休眠时长，
// 速率会偏大数十倍；"计数回退不置速率"的既有防线照不到这种情形，只能在休眠信号（规则见 client.Runner）
// 触发时把基线整体置空，让本轮采样成为新基线的首样本。
// 属于重置范围的基线清单——新增差分基线必须加进来，否则跨休眠的那一次差分会漏过：
//   - prevCPU、prevCPUKey（cpu_pct / steal / iowait 的两次采样差分与它的基线键）
//   - prevCgroup、prevCgroupT（cgroup 限额下 cpu_pct 的 usage_usec 差分）
//   - prevNet、prevNetT（网卡速率）
//   - prevDisk、prevDiskT、prevDiskEpoch（磁盘速率）
//   - prevTCP、prevTCPT（TCP 建连与段速率）
func (c *Collector) ResetRates() {
	c.prevCPU = nil
	c.prevCPUKey = ""
	c.prevCgroup, c.prevCgroupT = nil, 0
	c.prevNet, c.prevNetT = nil, 0
	c.prevDisk, c.prevDiskT, c.prevDiskEpoch = nil, 0, ""
	c.prevTCP, c.prevTCPT = nil, 0
}

// errScopeUnknown 表示资源的范围未知：读数缺失，但不记采集失败——
// 原因上报在 Facts.execution 的范围与说明里。
var errScopeUnknown = errors.New("scope unknown")

// readUsage 按识别给出的范围读一次内存或 swap：环境 cgroup 用快照挂上的读数入口
// （used 来自 controller 文件，total 是识别给出的可见上限），真根或 v1 用 meminfo 口径。
func (c *Collector) readUsage(scope heronv1.ResourceScope, readEnv func() (usage, error), readHost func() (usage, error)) (usage, error) {
	switch scope {
	case heronv1.ResourceScope_RESOURCE_SCOPE_ENVIRONMENT:
		if readEnv == nil {
			return usage{}, errScopeUnknown
		}
		return checkUsage(readEnv())
	case heronv1.ResourceScope_RESOURCE_SCOPE_HOST, heronv1.ResourceScope_RESOURCE_SCOPE_LEGACY:
		return checkUsage(readHost())
	default:
		return usage{}, errScopeUnknown
	}
}

// changeCPUBaseline 在基线键变化时丢弃两条 CPU 基线：键是来源身份（/proc/stat 或
// 挂载根 cgroup 目录的 dev:inode）加有效核数，任一变化都意味着旧差分的分母不再是本次口径。
func (c *Collector) changeCPUBaseline(key string) {
	if c.prevCPUKey == key {
		return
	}
	c.prevCPUKey = key
	c.prevCPU, c.prevCgroup = nil, nil
}

// noteScopeKind 在执行环境形态切换时记一行日志：识别失败、v1/无 cgroup2（口径退回
// /proc/stat、范围标 legacy），以及 v1 与 cgroup2 各形态之间的切换。每个周期都识别，
// 但只有切换才记——切换本身少见（容器重建、宿主机改挂载），按周期记会把日志刷满。
func (c *Collector) noteScopeKind(kind heronv1.ScopeKind) {
	prev := c.scopeKind
	if prev == kind {
		return
	}
	c.scopeKind = kind
	if c.Log == nil {
		return
	}
	switch {
	case kind == heronv1.ScopeKind_SCOPE_KIND_IDENTIFY_FAILED:
		c.Log.Info("execution scope identify failed: readings depending on it are missing")
	case kind == heronv1.ScopeKind_SCOPE_KIND_CGROUP_V1_LEGACY:
		// v1 的 cpu.cfs_quota_us 在宿主 root 的 cpu 控制器上也存在（与 v2 的 cpu.max 不同，
		// 它没有"只在非 root"的属性），裸机 v1 宿主同样走到这里：这不是故障，也不预设容器。
		c.Log.Info("cgroup v1: cpu limits are not readable, cpu_pct follows host-wide /proc/stat")
	case kind == heronv1.ScopeKind_SCOPE_KIND_CGROUP_NAMESPACE && prev != heronv1.ScopeKind_SCOPE_KIND_CGROUP_NAMESPACE:
		c.Log.Info("cgroup v2: mount root is the environment cgroup, cpu_pct follows cpu.stat")
	case kind == heronv1.ScopeKind_SCOPE_KIND_HOST && prev == heronv1.ScopeKind_SCOPE_KIND_CGROUP_V1_LEGACY:
		c.Log.Info("cgroup v2: mount root is the host root, readings are host-wide")
	}
}

// diskTotals 把整盘设备的计数合计，并给出设备集合标识：集合变化时标识随之改变，Collector 据此只换基线。
func (c *Collector) diskTotals() (diskCounters, string, error) {
	devs, err := c.Host.diskCounters()
	if err != nil {
		return diskCounters{}, "", err
	}
	var sum diskCounters
	var names []string
	for _, d := range devs {
		sum.read, sum.write = sum.read+d.read, sum.write+d.write
		names = append(names, d.name)
	}
	if len(names) == 0 {
		return diskCounters{}, "", errors.New("no whole-disk device")
	}
	slices.Sort(names)
	return sum, strings.Join(names, "\x00"), nil
}

// netSelection 是一次采样里结构规则的结果，供诊断。
type netSelection struct {
	excluded []*heronv1.NetStructureExclusion // 按名称字节序
	fallback bool
}

// netCounters 返回计入合计的网卡，按名称字节序。先按名字规则，再（启用时）按结构规则；结构规则把名字规则放行的
// 网卡全部排除时退回名字规则的结果。名称经 ValidateInterfaceName 准入且唯一：同名的两项无法分别求差，
// 也让合计里同一个名字算两次，按读不出处理（整个读数缺失），与 Host 对合计型读数的约定一致。
func (c *Collector) netCounters() ([]ifaceCounters, netSelection, error) {
	var sel netSelection
	ifs, err := c.Host.ifaces()
	if err != nil {
		return nil, sel, err
	}
	var out []ifaceCounters
	for _, i := range ifs {
		if !c.includeIface(i.name) {
			continue
		}
		if err := agentwire.ValidateInterfaceName(i.name); err != nil {
			return nil, sel, fmt.Errorf("interface %q: %w", i.name, err)
		}
		out = append(out, i)
	}
	if c.structureRules() {
		var kept, present []ifaceCounters
		for _, i := range out {
			s, err := c.Host.netStructure(i.name)
			if errors.Is(err, errIfaceGone) {
				continue
			}
			if err != nil {
				return nil, sel, fmt.Errorf("interface %q: %w", i.name, err)
			}
			present = append(present, i)
			if r := structureReason(s); r != heronv1.NetStructureReason_NET_STRUCTURE_REASON_UNSPECIFIED {
				sel.excluded = append(sel.excluded, &heronv1.NetStructureExclusion{Name: i.name, Reason: r})
				continue
			}
			kept = append(kept, i)
		}
		out = kept
		if len(kept) == 0 && len(present) > 0 {
			out, sel.excluded, sel.fallback = present, nil, true
		}
	}
	if len(out) == 0 {
		return nil, sel, errors.New("no included interface")
	}
	slices.SortFunc(out, func(a, b ifaceCounters) int { return strings.Compare(a.name, b.name) })
	for i := 1; i < len(out); i++ {
		if out[i-1].name == out[i].name {
			return nil, sel, fmt.Errorf("interface %q: listed twice", out[i].name)
		}
	}
	slices.SortFunc(sel.excluded, func(a, b *heronv1.NetStructureExclusion) int { return strings.Compare(a.GetName(), b.GetName()) })
	return out, sel, nil
}

// netDelta 逐网卡求两次采样之间的字节数：只计两次都在、且收发计数都不倒退的网卡。新出现的网卡已有的计数不是这段
// 区间的流量，消失的网卡没有终值，倒退的网卡（重建、计数清零）只能换基线；它们都不进分子，其余网卡照常计。
// 没有任何网卡满足时 ok 为假，速率缺失而不是 0。prev 与 cur 都按名称字节序排列（netCounters 的输出）。
func netDelta(prev, cur []ifaceCounters) (rx, tx float64, ok bool) {
	i := 0
	for _, c := range cur {
		for i < len(prev) && prev[i].name < c.name {
			i++
		}
		if i == len(prev) || prev[i].name != c.name || c.rx < prev[i].rx || c.tx < prev[i].tx {
			continue
		}
		rx += float64(c.rx - prev[i].rx)
		tx += float64(c.tx - prev[i].tx)
		ok = true
	}
	return rx, tx, ok
}

// noteNetListOver 在计入网卡数越过 agentwire.MaxNetInterfaces 与回落时各记一行日志：越过时请求不带逐网卡列表，
// hub 退回对合计做差分，集合每变一次就丢一段区间（规则见 traffic.Book）。按周期记会把日志刷满，只记切换。
func (c *Collector) noteNetListOver(n int) {
	over := n > agentwire.MaxNetInterfaces
	if over == c.netListOver {
		return
	}
	c.netListOver = over
	if c.Log == nil {
		return
	}
	if over {
		c.Log.Warn("too many counted interfaces for per-interface traffic accounting; reporting totals only", "interfaces", n, "limit", agentwire.MaxNetInterfaces)
	} else {
		c.Log.Info("counted interfaces back within the per-interface limit; reporting per-interface counters", "interfaces", n, "limit", agentwire.MaxNetInterfaces)
	}
}

// NetInterfaces 返回最近一次 Metrics 计入合计的逐网卡计数，供 ReportRequest.net_interfaces；超出上限或网络采集
// 失败时为 nil。与 Metrics 同一次采样，必须在同一周期的 Metrics 之后取。
func (c *Collector) NetInterfaces() []*heronv1.NetInterfaceCounter { return c.netList }

// Facts 收集静态信息；读不到的字段留空，由 hub 侧展示为未知。
// arch 取本二进制的 GOARCH：与发布资产名、安装脚本的架构名同一套词汇，两个平台一致。
func (c *Collector) Facts(snap *execSnapshot) *heronv1.Facts {
	hf := c.Host.facts()
	f := &heronv1.Facts{
		Hostname: hf.hostname, Os: hf.os, Kernel: hf.kernel, Arch: runtime.GOARCH,
		Virtualization: hf.virtualization, CpuModel: hf.cpuModel,
		AgentVersion: c.Version, IcmpAvailable: c.IcmpAvailable,
	}
	// 核数只来自本周期识别的有效核数：环境是 min(配额, cpuset) 的上取整
	// （1.5 核报 2），真根是主机处理器数；识别不出是 0——不再退回 runtime.NumCPU()，
	// 那会把"识别失败"伪装成一个像样的值。
	if snap != nil {
		// 上报前自检：块由本方构造，非法即构造 bug；不发非法块（hub 的同形校验会拒整份
		// Facts）。只在错误变化时记日志——按周期记会把同一个 bug 刷满日志（noteScopeKind
		// 的做法）；块被丢弃时同一快照算出的核数也不发（报 0），不让静态信息与被丢弃的
		// 范围块互相矛盾。
		if err := agentwire.ValidateExecutionScope(&snap.exec); err != nil {
			if c.Log != nil && err.Error() != c.execErrPrev {
				c.Log.Error("dropping invalid execution scope", "err", err)
			}
			c.execErrPrev = err.Error()
		} else {
			c.execErrPrev = ""
			if cores := snap.exec.GetCpuEffectiveCores(); cores > 0 {
				f.CpuCores = uint32(math.Ceil(cores))
			}
			f.Execution = proto.Clone(&snap.exec).(*heronv1.ExecutionScope)
		}
	}
	if c.diagnostics != nil {
		f.Diagnostics = proto.Clone(c.diagnostics).(*heronv1.AgentDiagnostics)
	}
	return f
}
