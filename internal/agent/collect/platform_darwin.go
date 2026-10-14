//go:build darwin

package collect

import (
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"unsafe"

	"github.com/ebitengine/purego"
	"golang.org/x/sys/unix"

	"github.com/xjetry/heron/internal/clock"
)

// NewPlatform 构造本平台的 Collector。hostRoot 只在 Linux 上有意义（容器里采集宿主），这里给了就拒绝。
func NewPlatform(version string, clk clock.Clock, netInclude, netExclude []string, hostRoot string) (*Collector, error) {
	if hostRoot != "" {
		return nil, errHostRootUnsupported(hostRoot)
	}
	return newDarwinCollector(openDarwinSyscalls(), version, clk, netInclude, netExclude)
}

// newDarwinCollector 不因 libSystem 的函数缺失而失败：缺的函数只让依赖它的读数缺失（各方法开头的 need），
// client.Runner.Run 遇 Metrics 的错误记日志后照常上报。构造若失败，agent 退出，launchd 的 KeepAlive
// 每 5 秒拉起一个立即退出的进程，连 sysctl 读数也没有了。NewPlatform 与测试共用这一个构造函数。
func newDarwinCollector(src *darwinSyscalls, version string, clk clock.Clock, netInclude, netExclude []string) (*Collector, error) {
	return &Collector{Host: &darwinHost{src: src}, Clock: clk, NetInclude: netInclude, NetExclude: netExclude, Version: version}, nil
}

// DefaultNetExclude 是本平台未给 --net-exclude 时不计入流量的网卡。
func DefaultNetExclude() []string { return darwinNetExclude }

// Mach 常量取自 <mach/processor_info.h>、<mach/host_info.h>、<mach/kern_return.h>。
const (
	processorCPULoadInfo = 2 // PROCESSOR_CPU_LOAD_INFO
	hostVMInfo64         = 4 // HOST_VM_INFO64
	// hostVMInfo64Rev1Count 是向 host_statistics64 请求的字数：HOST_VM_INFO64_REV1_COUNT，38 个 integer_t，
	// 覆盖到偏移 152（swapped_count 之前）。当前 SDK（26.5）的 HOST_VM_INFO64_COUNT 是 62，但这里只读到
	// internal_page_count（140）。内核按整个 revision 回填并把实际字数写回 count（本机实测：请求 24–37
	// 回 24，38–39 回 38，40 及以上回 40），vmStatistics64 按写回的字数切字节；请求偏小时
	// parseVMStatistics64 的长度检查（darwinraw.go 的 vmStatsMinLen）让内存读数整体缺失，不会把靠后的
	// 字段读成 0。两个常量一起改，下面的常量表达式在请求覆盖不到时编译失败。
	hostVMInfo64Rev1Count = 38
	kernSuccess           = 0
)

const _ = uint(hostVMInfo64Rev1Count*4 - vmStatsMinLen)

const libSystemPath = "/usr/lib/libSystem.B.dylib"

// libSym 是 libSystem 里的一个函数：符号名，与把符号地址注册到 darwinSyscalls 对应字段的方式。
type libSym struct {
	name string
	bind func(s *darwinSyscalls, addr uintptr)
}

var libSystemSyms = []libSym{
	{"mach_host_self", func(s *darwinSyscalls, a uintptr) { purego.RegisterFunc(&s.machHostSelf, a) }},
	{"mach_task_self", func(s *darwinSyscalls, a uintptr) { purego.RegisterFunc(&s.machTaskSelf, a) }},
	{"host_processor_info", func(s *darwinSyscalls, a uintptr) { purego.RegisterFunc(&s.hostProcessorInfo, a) }},
	{"host_statistics64", func(s *darwinSyscalls, a uintptr) { purego.RegisterFunc(&s.hostStatistics64, a) }},
	{"host_page_size", func(s *darwinSyscalls, a uintptr) { purego.RegisterFunc(&s.hostPageSize, a) }},
	{"vm_deallocate", func(s *darwinSyscalls, a uintptr) { purego.RegisterFunc(&s.vmDeallocate, a) }},
	{"proc_listallpids", func(s *darwinSyscalls, a uintptr) { purego.RegisterFunc(&s.procListallpids, a) }},
}

// darwinSyscalls 是 darwinSource 的真实实现：sysctl 走 x/sys/unix；逐 CPU 计数与 VM 统计只有 Mach 接口给得出，
// 进程数用 libproc 的 proc_listallpids（sysctl kern.proc.all 也能数，但每次要复制约 1 MB 的 kinfo_proc），
// 这几个经 purego 调 libSystem。
type darwinSyscalls struct {
	// bound 记已绑定的符号，symErr 记没绑上的原因。各方法开头用 need 检查自己要的符号，
	// 缺哪个只让依赖它的读数缺失。
	bound  map[string]bool
	symErr map[string]error
	// mach_host_self、mach_task_self 每次调用都给同一个端口名多加一个发送权引用，只在打开时各取一次。
	host, task uint32

	machHostSelf      func() uint32
	machTaskSelf      func() uint32
	hostProcessorInfo func(host uint32, flavor int32, n *uint32, info **uint32, cnt *uint32) int32
	hostStatistics64  func(host uint32, flavor int32, info *uint32, cnt *uint32) int32
	hostPageSize      func(host uint32, size *uintptr) int32
	vmDeallocate      func(task uint32, addr, size uintptr) int32
	procListallpids   func(buf *int32, size int32) int32
}

func openDarwinSyscalls() *darwinSyscalls { return openLibSystem(libSystemPath, libSystemSyms) }

// openLibSystem 逐个符号绑定，缺哪个记哪个。用 Dlsym 取地址再 RegisterFunc，而不是 RegisterLibFunc：
// 后者找不到符号时 panic，panic 出在 NewPlatform 里就是进程退出（见 newDarwinCollector）。
func openLibSystem(path string, syms []libSym) *darwinSyscalls {
	s := &darwinSyscalls{bound: map[string]bool{}, symErr: map[string]error{}}
	lib, err := purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	for _, sym := range syms {
		if err != nil {
			s.symErr[sym.name] = fmt.Errorf("dlopen %s: %w", path, err)
			continue
		}
		addr, serr := purego.Dlsym(lib, sym.name)
		if serr != nil {
			s.symErr[sym.name] = fmt.Errorf("dlsym %s: %w", sym.name, serr)
			continue
		}
		sym.bind(s, addr)
		s.bound[sym.name] = true
	}
	if s.need("mach_host_self") == nil {
		s.host = s.machHostSelf()
	}
	if s.need("mach_task_self") == nil {
		s.task = s.machTaskSelf()
	}
	return s
}

// need 在任一符号未绑定时返回错误，点名缺的符号与原因。
func (s *darwinSyscalls) need(names ...string) error {
	var errs []error
	for _, n := range names {
		if s.bound[n] {
			continue
		}
		if err := s.symErr[n]; err != nil {
			errs = append(errs, err)
		} else {
			errs = append(errs, fmt.Errorf("libSystem: %s not bound", n))
		}
	}
	return errors.Join(errs...)
}

func (s *darwinSyscalls) sysctlString(name string) (string, error) { return unix.Sysctl(name) }
func (s *darwinSyscalls) sysctlUint32(name string) (uint32, error) { return unix.SysctlUint32(name) }
func (s *darwinSyscalls) sysctlUint64(name string) (uint64, error) { return unix.SysctlUint64(name) }
func (s *darwinSyscalls) sysctlRaw(name string, args ...int) ([]byte, error) {
	return unix.SysctlRaw(name, args...)
}
func (s *darwinSyscalls) statfs(path string) (uint64, uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	total, used := statfsUsage(&st)
	return total, used, nil
}

// monotonicSeconds 用 CLOCK_MONOTONIC：clock_gettime(3) 写明它在系统睡眠时继续计数，与 Linux 的
// /proc/uptime 同口径（含挂起）；本机实测它只有微秒粒度（纳秒部分 2000 次全是 1000 的倍数），
// 与 gettimeofday − kern.boottime 相差几微秒。CLOCK_UPTIME_RAW 睡眠时不计数（实测一台睡过的机器上
// 少 14 小时），不能当运行时长。这个值只作展示：判定机器是否重启的是 kern.bootsessionuuid（bootID）。
func (s *darwinSyscalls) monotonicSeconds() (uint64, error) {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		return 0, err
	}
	return uint64(ts.Sec), nil
}

// processorTicks 用逐 CPU 的 host_processor_info 而不是聚合的 host_statistics(HOST_CPU_LOAD_INFO)：
// 实测一台 16 核 Apple Silicon：前者之和每秒稳定在约 1590（CPU 数 × kern.clockrate 的 hz 100），
// 后者按秒取差在 1057 到 2202 之间摆动，算出的忙碌比例偏差到 12 个百分点。
func (s *darwinSyscalls) processorTicks() ([]uint32, error) {
	if err := s.need("mach_host_self", "mach_task_self", "host_processor_info", "vm_deallocate"); err != nil {
		return nil, err
	}
	var n, cnt uint32
	var info *uint32
	if kr := s.hostProcessorInfo(s.host, processorCPULoadInfo, &n, &info, &cnt); kr != kernSuccess {
		return nil, fmt.Errorf("host_processor_info: kern_return_t %d", kr)
	}
	// 数组由内核在本进程地址空间里 vm_allocate，复制出来后必须归还，否则每次采样都泄漏。
	out := slices.Clone(unsafe.Slice(info, cnt))
	if kr := s.vmDeallocate(s.task, uintptr(unsafe.Pointer(info)), uintptr(cnt)*4); kr != kernSuccess {
		return nil, fmt.Errorf("vm_deallocate: kern_return_t %d", kr)
	}
	if uint64(cnt) != uint64(n)*cpuStateMax {
		return nil, fmt.Errorf("host_processor_info: %d values for %d cpus", cnt, n)
	}
	return out, nil
}

// vmStatistics64 返回的是内核按 flavor 缓存的值，不保证是调用时刻的值：macOS 26（kernel 25.3）上第三方
// 调用者被限流，缓存约每秒才改写一次，改写时连续几次调用各拿到调用时刻的值，之后的调用拿最后那次的副本直到
// 下一个窗口（相邻两次调用常拿到完全相同的冻结值，与请求字数无关）；Apple 平台二进制每次调用都拿到新值。
// 按间隔上报的 gauge 可以容忍这至多约一秒的滞后。需要"调用时刻的值"的场合不能拿单次读数当实时值：只有
// 返回值与上一次不同的那次才取自两次调用之间（platform_darwin_test.go 的内存对照靠这一点逐页比）。
func (s *darwinSyscalls) vmStatistics64() ([]byte, error) {
	if err := s.need("mach_host_self", "host_statistics64"); err != nil {
		return nil, err
	}
	var words [hostVMInfo64Rev1Count]uint32
	cnt := uint32(hostVMInfo64Rev1Count)
	if kr := s.hostStatistics64(s.host, hostVMInfo64, &words[0], &cnt); kr != kernSuccess {
		return nil, fmt.Errorf("host_statistics64: kern_return_t %d", kr)
	}
	// 按字重新编码成小端字节，交给与头文件布局对照的解析；darwin 的两个目标都是小端，逐字编码与内存布局相同。
	b := make([]byte, 4*int(cnt))
	for i := range int(cnt) {
		binary.LittleEndian.PutUint32(b[4*i:], words[i])
	}
	return b, nil
}

// pageSize 取 host_page_size：它是 host_statistics64 页计数的单位。hw.pagesize 对 Rosetta 转译的
// x86_64 进程报 4096，而 Apple Silicon 内核的页是 16384，用它会把已用内存少算四倍。
func (s *darwinSyscalls) pageSize() (uint64, error) {
	if err := s.need("mach_host_self", "host_page_size"); err != nil {
		return 0, err
	}
	var size uintptr
	if kr := s.hostPageSize(s.host, &size); kr != kernSuccess {
		return 0, fmt.Errorf("host_page_size: kern_return_t %d", kr)
	}
	return uint64(size), nil
}

// pidCount 用 proc_listallpids 数进程（不是线程），与 Linux 数 /proc 进程目录同口径。
// 不带缓冲区时它返回带余量的估计值，带缓冲区时返回实际写入的个数；写满缓冲区说明进程表在两次调用之间
// 长过了余量，放大再取。
func (s *darwinSyscalls) pidCount() (uint32, error) {
	if err := s.need("proc_listallpids"); err != nil {
		return 0, err
	}
	est := s.procListallpids(nil, 0)
	if est <= 0 {
		return 0, errors.New("proc_listallpids: no estimate")
	}
	size := int(est) + 64
	for range 3 {
		buf := make([]int32, size)
		got := s.procListallpids(&buf[0], int32(4*len(buf)))
		if got <= 0 {
			return 0, fmt.Errorf("proc_listallpids: returned %d", got)
		}
		if int(got) < len(buf) {
			return uint32(got), nil
		}
		size *= 2
	}
	return 0, errors.New("proc_listallpids: process table kept outgrowing the buffer")
}
