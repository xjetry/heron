package collect

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/agentwire"
)

// darwinNetExclude 与 Linux 的默认列表同一原则：回环，以及流量同时计在物理网口上或不出本机的接口。
// 隧道 gif、stf、utun、ipsec（VPN 的字节也计在承载它的网口上），桥与虚拟机网卡 bridge、vmenet，
// Wi-Fi 上的点对点接口 awdl、llw，以及 anpi、ap。以太网、Wi-Fi 与雷雳网口都叫 en*，不在此列。
var darwinNetExclude = []string{"lo*", "gif*", "stf*", "utun*", "ipsec*", "bridge*", "vmenet*", "awdl*", "llw*", "anpi*", "ap*"}

// 下列布局取自 macOS SDK：<sys/sysctl.h> 的 loadavg 与 xsw_usage、<mach/vm_statistics.h> 的
// vm_statistics64、<net/if_mib.h> 的 ifmibdata 与 <net/if_var.h> 的 if_data64（#pragma pack(4)）。
// 每个尺寸与偏移都在 SDK 26.5 上用 clang 的 sizeof/offsetof 核对过，arm64 与 x86_64 相同。
// darwin 的两个目标 amd64 与 arm64 都是小端，字段按小端解码。
const (
	// struct loadavg { fixpt_t ldavg[3]; long fscale; }：ldavg 占 0–11，long 按 8 字节对齐，
	// 其后留 4 字节填充，fscale 在 16。
	sizeofLoadavg = 24
	// struct xsw_usage { u_int64_t xsu_total, xsu_avail, xsu_used; u_int32_t xsu_pagesize; boolean_t xsu_encrypted; }
	sizeofXswUsage = 32
	// parseVMStatistics64 读到 internal_page_count（偏移 140）为止。host_statistics64 按请求的字数回填整个 revision
	// （REV0 24 字、REV1 38 字……），新字段追加在末尾，所以只要求不短于此、不要求等长；
	// 请求多少字由 platform_darwin.go 的 hostVMInfo64Rev1Count 决定，两处一起改。
	vmStatsMinLen = 144
	// struct ifmibdata：ifmd_name[IFNAMSIZ] 16 字节 + 9 个 unsigned int（pcount、flags、snd_len、snd_maxlen、
	// snd_drops、filler[4]）36 字节，if_data64 从 52 开始、长 128 字节，共 180。内核按 sizeof 整个复制出来，
	// 长度不等说明布局变了，按旧偏移读出的计数不可信。
	sizeofIfmibdata = 180
	ifmibDataOffset = 52
	ifNameSize      = 16 // IFNAMSIZ

	// PROCESSOR_CPU_LOAD_INFO 每个 CPU 一组 natural_t：<mach/processor_info.h> 的
	// processor_cpu_load_info.cpu_ticks[CPU_STATE_MAX]，下标是 <mach/machine.h> 的
	// CPU_STATE_USER 0、SYSTEM 1、IDLE 2、NICE 3。
	cpuStateMax  = 4
	cpuStateIdle = 2
)

// darwinSource 是 darwin 取数的系统调用层，由 platform_darwin.go 实现；
// 测试以按头文件布局构造的字节替换它。
type darwinSource interface {
	sysctlString(name string) (string, error)
	sysctlUint32(name string) (uint32, error)
	sysctlUint64(name string) (uint64, error)
	sysctlRaw(name string, args ...int) ([]byte, error)
	statfs(path string) (total, used uint64, err error)
	// mounts 是 getfsstat(MNT_NOWAIT) 的挂载表：不等任何文件系统应答，远端挂载失联也不阻塞。
	mounts() ([]fsstatRecord, error)
	// monotonicSeconds 是自启动起的秒数，含睡眠（CLOCK_MONOTONIC），与 Linux /proc/uptime 同口径。
	monotonicSeconds() (uint64, error)
	// processorTicks 是 host_processor_info 的逐 CPU 计数，每 CPU cpuStateMax 个。
	processorTicks() ([]uint32, error)
	// vmStatistics64 是 host_statistics64(HOST_VM_INFO64) 的原始字节。
	vmStatistics64() ([]byte, error)
	// pageSize 是 vmStatistics64 页计数的单位（host_page_size）。
	pageSize() (uint64, error)
	pidCount() (uint32, error)
}

type darwinHost struct {
	src   darwinSource
	ticks tickAccumulator
}

// close：darwin 的读数都是 sysctl 与 Mach 调用，没有要释放的资源。
func (d *darwinHost) close() error { return nil }

// bootID 用 kern.bootsessionuuid：开机时生成（统一日志的 "system boot" 行记的就是它），睡眠唤醒不变。
// hub 靠它区分"同一次开机内 agent 重启"（接着差分）与"机器重启"（只换基线）（规则见 traffic.Book）。
// kern.bootuuid 与 kern.apfsprebootuuid 相同，是 preboot 卷的标识而不是这次开机的，不能用。
func (d *darwinHost) bootID() (string, error) {
	id, err := d.src.sysctlString("kern.bootsessionuuid")
	if err != nil {
		return "", err
	}
	if id == "" {
		return "", errors.New("kern.bootsessionuuid is empty")
	}
	return id, nil
}

func (d *darwinHost) cpuTimes() (cpuTimes, error) {
	raw, err := d.src.processorTicks()
	if err != nil {
		return cpuTimes{}, err
	}
	return d.ticks.add(raw)
}

// memory 的已用量 = 匿名页 − 可清除页 + 联动页 + 压缩器占用页。文件缓存与可清除内存随时可回收，
// 不算占用，与 Linux 的 total − MemAvailable 同义。
func (d *darwinHost) memory() (usage, error) {
	total, err := d.src.sysctlUint64("hw.memsize")
	if err != nil {
		return usage{}, err
	}
	raw, err := d.src.vmStatistics64()
	if err != nil {
		return usage{}, err
	}
	page, err := d.src.pageSize()
	if err != nil {
		return usage{}, err
	}
	if page == 0 {
		return usage{}, errors.New("page size is 0")
	}
	vm, err := parseVMStatistics64(raw)
	if err != nil {
		return usage{}, err
	}
	pages, err := vm.usedPages()
	if err != nil {
		return usage{}, err
	}
	return usage{total: total, used: pages * page}, nil
}

func (d *darwinHost) swap() (usage, error) {
	b, err := d.src.sysctlRaw("vm.swapusage")
	if err != nil {
		return usage{}, err
	}
	return parseSwapUsage(b)
}

func (d *darwinHost) disk() (usage, error) {
	total, used, err := d.src.statfs("/")
	return usage{total: total, used: used}, err
}

func (d *darwinHost) diskMounts(*execSnapshot) ([]mountEntry, error) {
	recs, err := d.src.mounts()
	if err != nil {
		return nil, err
	}
	return darwinMountEntries(recs), nil
}

// mountUsage 只对 darwinMountEntries 判为合格的本地卷调用；darwin 的挂载点必是目录（mount(2) 对非目录报 ENOTDIR），
// 不另查。
func (d *darwinHost) mountUsage(p string) (usage, error) {
	total, used, err := d.src.statfs(p)
	if errors.Is(err, fs.ErrNotExist) {
		return usage{}, errMountGone
	}
	return usage{total: total, used: used}, err
}

func (d *darwinHost) load() (loadAvg, error) {
	b, err := d.src.sysctlRaw("vm.loadavg")
	if err != nil {
		return loadAvg{}, err
	}
	return parseLoadavgSysctl(b)
}

func (d *darwinHost) procs() (uint32, error) { return d.src.pidCount() }

func (d *darwinHost) uptime() (uint64, error) { return d.src.monotonicSeconds() }

// conns 是内核里 TCP、UDP 协议控制块的个数，与 Linux sockstat 的 inuse 同为内核计数口径。
func (d *darwinHost) conns() (uint32, uint32, error) {
	tcp, err := d.src.sysctlUint32("net.inet.tcp.pcbcount")
	if err != nil {
		return 0, 0, err
	}
	udp, err := d.src.sysctlUint32("net.inet.udp.pcbcount")
	if err != nil {
		return 0, 0, err
	}
	return tcp, udp, nil
}

// tcpStats：darwin 的 TCP 统计在 net.inet.tcp.stats 的结构体里，布局随系统版本变化，本平台不提供。
func (d *darwinHost) tcpStats() (tcpCounters, error) { return tcpCounters{}, errNoTCPStats }

// ifaces 读 net.link.generic.ifdata.<index>.IFDATA_GENERAL：它给出 64 位、不取整的字节计数。
// 同一台机器上，普通用户进程经 NET_RT_IFLIST2 读到的 if_data64 字节数截到了 32 位并按 1 KiB 取整；
// 累计值过 4 GiB 就回绕，hub 把变小的计数当成重置、只换基线不入账（规则见 traffic.Book），流量就丢了。
// 索引空间有空洞（实测 ifcount 为 35 时 23、34、35 号不存在），缺号返回 ENOENT，跳过。读完 ifcount 之后
// 被删除的网卡读出 ENOENT 时同样按不存在略过；删网卡要 root，它实际报哪个错误码本机未复现，
// 若是别的错误码，按下一句整个读数缺失——缺失而不是错。
// 其余错误让整个读数缺失，不能把计数未知的网卡伪装成不存在；Collector.Metrics 据此报告网络采集失败，
// 不发布不完整的合计或集合标识，hub 的 Account 对缺读数保持基线不动。
func (d *darwinHost) ifaces() ([]ifaceCounters, error) {
	n, err := d.src.sysctlUint32("net.link.generic.system.ifcount")
	if err != nil {
		return nil, err
	}
	var out []ifaceCounters
	for idx := 1; idx <= int(n); idx++ {
		b, err := d.src.sysctlRaw("net.link.generic.ifdata", idx, 1) // IFDATA_GENERAL
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("ifdata %d: %w", idx, err)
		}
		ic, err := parseIfmibData(b)
		if err != nil {
			return nil, fmt.Errorf("ifdata %d: %w", idx, err)
		}
		out = append(out, ic)
	}
	return out, nil
}

func (d *darwinHost) defaultNetExclude() []string { return darwinNetExclude }

// darwin 没有与 sysfs 对应的结构属性；默认名字表本身已排除桥接与隧道接口。
func (d *darwinHost) hasNetStructure() bool { return false }
func (d *darwinHost) netStructure(string) (netStructure, error) {
	return netStructure{}, errors.New("interface structure attributes are not provided on darwin")
}

// diskCounters：darwin 没有与 /proc/diskstats 对应的整盘累计读写口径，磁盘速率两项不设置。
func (d *darwinHost) diskCounters() ([]diskCounters, error) { return nil, errNoDiskCounters }

// identify：cgroup 与 /proc 来源判断是 Linux 的设施，darwin 恒为主机，
// 读数与按核负载的分母都来自本机 sysctl。
func (d *darwinHost) identify() *execSnapshot {
	var cores float64
	if n, err := d.src.sysctlUint32("hw.logicalcpu"); err == nil {
		cores = float64(n)
	}
	s := &execSnapshot{cpuBaseline: procStatBaseline("host", cores), cpuCores: cores}
	e := &s.exec
	e.Kind = heronv1.ScopeKind_SCOPE_KIND_HOST
	e.Cpu = heronv1.ResourceScope_RESOURCE_SCOPE_HOST
	e.Memory = heronv1.ResourceScope_RESOURCE_SCOPE_HOST
	e.Swap = heronv1.ResourceScope_RESOURCE_SCOPE_HOST
	e.Load = heronv1.ResourceScope_RESOURCE_SCOPE_HOST
	if cores > 0 {
		e.CpuEffectiveCores = &cores
		n := uint32(cores)
		e.LoadCores = &n
	}
	return s
}

func (d *darwinHost) facts() hostFacts {
	var f hostFacts
	f.hostname, _ = d.src.sysctlString("kern.hostname")
	if v, err := d.src.sysctlString("kern.osproductversion"); err == nil && v != "" {
		f.os = "macOS " + v
	}
	f.kernel, _ = d.src.sysctlString("kern.osrelease")
	f.cpuModel, _ = d.src.sysctlString("machdep.cpu.brand_string")
	if v, err := d.src.sysctlUint32("kern.hv_vmm_present"); err == nil && v == 1 {
		f.virtualization = "vm"
	}
	return f
}

// tickAccumulator 把逐 CPU 的 natural_t 计数折成单调不减的 64 位累计值。
// 单个计数是 uint32，每 CPU 每秒 hz（100）个 tick，约 497 天回绕一次；
// 按 uint32 相减得到的增量在一次回绕之内总是对的，再累加进 64 位和。
// CPU 个数变化时没有可对齐的上一份：只换基线、累计值不动，
// Collector 见 total 未增长就不给 cpu_pct。
type tickAccumulator struct {
	last []uint32
	sum  cpuTimes
}

func (a *tickAccumulator) add(raw []uint32) (cpuTimes, error) {
	if len(raw) == 0 || len(raw)%cpuStateMax != 0 {
		return cpuTimes{}, fmt.Errorf("processor ticks: %d values, want a positive multiple of %d", len(raw), cpuStateMax)
	}
	if len(a.last) == len(raw) {
		for i, v := range raw {
			d := uint64(v - a.last[i])
			a.sum.total += d
			if i%cpuStateMax == cpuStateIdle {
				a.sum.idle += d
			}
		}
	}
	a.last = append(a.last[:0], raw...)
	return a.sum, nil
}

func parseLoadavgSysctl(b []byte) (loadAvg, error) {
	if len(b) != sizeofLoadavg {
		return loadAvg{}, fmt.Errorf("vm.loadavg: %d bytes, want %d", len(b), sizeofLoadavg)
	}
	le := binary.LittleEndian
	scale := float64(int64(le.Uint64(b[16:])))
	// 守卫的两半各防一种读数：fscale 为 0 时比值是 Inf 或 NaN，为负时是负数（有限）。hub 的
	// validateMetrics（internal/hub/ingest/validate.go）对非有限或负的负载都拒收整条上报，CPU、内存、
	// 流量随之一起丢失；所以在这里按读不到处理，只让负载缺失。
	if scale <= 0 {
		return loadAvg{}, fmt.Errorf("vm.loadavg: fscale %v", scale)
	}
	return loadAvg{
		l1:  float64(le.Uint32(b[0:])) / scale,
		l5:  float64(le.Uint32(b[4:])) / scale,
		l15: float64(le.Uint32(b[8:])) / scale,
	}, nil
}

func parseSwapUsage(b []byte) (usage, error) {
	if len(b) != sizeofXswUsage {
		return usage{}, fmt.Errorf("vm.swapusage: %d bytes, want %d", len(b), sizeofXswUsage)
	}
	return usage{total: binary.LittleEndian.Uint64(b[0:]), used: binary.LittleEndian.Uint64(b[16:])}, nil
}

// vmCounts 是 vm_statistics64 的页计数：wire、purgeable、compressor、internal 是已用内存公式要的四个；
// 其余六个已用内存公式不用，解析出来给真机一致性测试对照（sysctl 的 free/speculative/external 与
// 队列分类的合计），布局漂移时它们比四个公式字段先显形。
type vmCounts struct {
	wire, purgeable, compressor, internal                   uint64
	free, active, inactive, speculative, external, throttle uint64
}

// parseVMStatistics64 取 free_count(0)、active_count(4)、inactive_count(8)、wire_count(12)、
// purgeable_count(88)、speculative_count(92)、compressor_page_count(128)、throttled_count(132)、
// external_page_count(136)、internal_page_count(140)。
func parseVMStatistics64(b []byte) (vmCounts, error) {
	if len(b) < vmStatsMinLen {
		return vmCounts{}, fmt.Errorf("vm_statistics64: %d bytes, want at least %d", len(b), vmStatsMinLen)
	}
	le := binary.LittleEndian
	return vmCounts{
		free:        uint64(le.Uint32(b[0:])),
		active:      uint64(le.Uint32(b[4:])),
		inactive:    uint64(le.Uint32(b[8:])),
		wire:        uint64(le.Uint32(b[12:])),
		purgeable:   uint64(le.Uint32(b[88:])),
		speculative: uint64(le.Uint32(b[92:])),
		compressor:  uint64(le.Uint32(b[128:])),
		throttle:    uint64(le.Uint32(b[132:])),
		external:    uint64(le.Uint32(b[136:])),
		internal:    uint64(le.Uint32(b[140:])),
	}, nil
}

// usedPages = 匿名页 − 可清除页 + 联动页 + 压缩器占用页。可清除页是匿名页的子集；读到前者大于后者
// 说明这份计数不自洽，按读不到处理。这条检查是回绕的唯一防线：internal − purgeable 回绕后再加上
// wire 与 compressor，结果可能落回 hw.memsize 以内，Collector 的 checkUsage 看不出（Host.memory 的约定）。
func (c vmCounts) usedPages() (uint64, error) {
	if c.purgeable > c.internal {
		return 0, fmt.Errorf("vm_statistics64: purgeable %d exceeds internal %d", c.purgeable, c.internal)
	}
	return c.internal - c.purgeable + c.wire + c.compressor, nil
}

// parseIfmibData 取 struct ifmibdata 的 ifmd_name 与 if_data64 的 ifi_ibytes(64)、ifi_obytes(72)。
func parseIfmibData(b []byte) (ifaceCounters, error) {
	if len(b) != sizeofIfmibdata {
		return ifaceCounters{}, fmt.Errorf("ifmibdata: %d bytes, want %d", len(b), sizeofIfmibdata)
	}
	name, _, _ := strings.Cut(string(b[:ifNameSize]), "\x00")
	if name == "" {
		return ifaceCounters{}, errors.New("ifmibdata: empty interface name")
	}
	d := b[ifmibDataOffset:]
	return ifaceCounters{name: name, rx: binary.LittleEndian.Uint64(d[64:]), tx: binary.LittleEndian.Uint64(d[72:])}, nil
}

// fsstatRecord 是 getfsstat 一项里选盘要用的字段，取自 <sys/mount.h> 的 struct statfs。
type fsstatRecord struct {
	mntOn, mntFrom, fsType string
	flags                  uint32
	fsid                   [2]int32
}

// <sys/mount.h> 的挂载标志（MacOSX SDK 26.5）。
const (
	darwinMntRdonly     = 0x00000001 // MNT_RDONLY
	darwinMntLocal      = 0x00001000 // MNT_LOCAL
	darwinMntDontBrowse = 0x00100000 // MNT_DONTBROWSE
)

// darwinLocalFS 是 darwin 上本地持久化文件系统的白名单；darwinRemoteFS 是路径穿过它就可能等远端应答的类型
// （另加 "fuse" 前缀，见 remoteFS）。
var (
	darwinLocalFS  = map[string]bool{"apfs": true, "hfs": true}
	darwinRemoteFS = map[string]bool{"nfs": true, "smbfs": true, "afpfs": true, "webdav": true, "ftp": true, "autofs": true, "macfuse": true, "osxfuse": true}
)

// darwinMountEntries 把 getfsstat 的挂载表变成候选挂载点。除根之外，要求白名单类型、MNT_LOCAL、可写、
// 不带 MNT_DONTBROWSE、路径可上报，且不在远端挂载之下。
//
// MNT_DONTBROWSE 是系统给不该出现在 Finder 里的卷打的标记：/System/Volumes 下的 VM、Preboot、Update、xarts、
// Hardware 等系统卷都带它。数据卷 /System/Volumes/Data 也带，但它与根在同一个 APFS 容器里，容器的用量已经由根报出。
//
// APFS 的卷共享所在容器的空间：statfs 对容器内每个卷给出同一组块数与空闲块（statfsUsage），所以按容器归并，
// 容器取自来源设备名（/dev/disk3s5、快照 /dev/disk3s1s1 都属于容器 disk3）。
func darwinMountEntries(recs []fsstatRecord) []mountEntry {
	var out []mountEntry
	var remote []string
	for _, r := range recs {
		e := mountEntry{path: r.mntOn, devKey: fmt.Sprintf("fsid:%d:%d", r.fsid[0], r.fsid[1])}
		switch {
		case r.fsType == "apfs":
			if c := apfsContainer(r.mntFrom); c != "" {
				e.poolKey = "apfs:" + c
			}
		case strings.HasPrefix(r.mntFrom, "/dev/"):
			e.poolKey = "src:" + r.mntFrom
		}
		e.eligible = r.mntOn == "/" || darwinLocalFS[r.fsType] && r.flags&darwinMntLocal != 0 &&
			r.flags&darwinMntRdonly == 0 && r.flags&darwinMntDontBrowse == 0
		if remoteFS(darwinRemoteFS, r.fsType) {
			remote = append(remote, r.mntOn)
		}
		out = append(out, e)
	}
	for i := range out {
		if out[i].path != "/" && (underAny(out[i].path, remote) || agentwire.ValidateDiskMountPath(out[i].path) != nil) {
			out[i].eligible = false
		}
	}
	return out
}

// apfsContainer 从 APFS 卷的来源设备名取容器的整盘名："/dev/disk3s1s1" 得 "/dev/disk3"；认不出时返回空串。
func apfsContainer(from string) string {
	rest, ok := strings.CutPrefix(from, "/dev/disk")
	if !ok {
		return ""
	}
	n := 0
	for n < len(rest) && rest[n] >= '0' && rest[n] <= '9' {
		n++
	}
	if n == 0 || n == len(rest) || rest[n] != 's' {
		return ""
	}
	return "/dev/disk" + rest[:n]
}
