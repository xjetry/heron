package collect

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/clock"
)

// fakeDarwin 按头文件布局构造字节；键是 sysctl 名，ifdata 以 "net.link.generic.ifdata/<idx>" 为键。
type fakeDarwin struct {
	strs  map[string]string
	u32   map[string]uint32
	u64   map[string]uint64
	raw   map[string][]byte
	errs  map[string]error
	ticks [][]uint32 // 每次 processorTicks 取下一份
	vm    []byte
	page  uint64
	pids  uint32
	disk  usage
	up    uint64
}

func (f *fakeDarwin) err(name string) error {
	if e, ok := f.errs[name]; ok {
		return e
	}
	return nil
}

func (f *fakeDarwin) sysctlString(n string) (string, error) {
	if e := f.err(n); e != nil {
		return "", e
	}
	v, ok := f.strs[n]
	if !ok {
		return "", syscall.ENOENT
	}
	return v, nil
}

func (f *fakeDarwin) sysctlUint32(n string) (uint32, error) {
	if e := f.err(n); e != nil {
		return 0, e
	}
	v, ok := f.u32[n]
	if !ok {
		return 0, syscall.ENOENT
	}
	return v, nil
}

func (f *fakeDarwin) sysctlUint64(n string) (uint64, error) {
	if e := f.err(n); e != nil {
		return 0, e
	}
	v, ok := f.u64[n]
	if !ok {
		return 0, syscall.ENOENT
	}
	return v, nil
}

func (f *fakeDarwin) sysctlRaw(n string, args ...int) ([]byte, error) {
	key := n
	if n == "net.link.generic.ifdata" {
		key = fmt.Sprintf("%s/%d", n, args[0])
	}
	if e := f.err(key); e != nil {
		return nil, e
	}
	v, ok := f.raw[key]
	if !ok {
		return nil, syscall.ENOENT
	}
	return v, nil
}

func (f *fakeDarwin) statfs(string) (uint64, uint64, error) { return f.disk.total, f.disk.used, nil }
func (f *fakeDarwin) monotonicSeconds() (uint64, error)     { return f.up, nil }
func (f *fakeDarwin) processorTicks() ([]uint32, error) {
	if len(f.ticks) == 0 {
		return nil, errors.New("no ticks")
	}
	t := f.ticks[0]
	f.ticks = f.ticks[1:]
	return t, nil
}
func (f *fakeDarwin) vmStatistics64() ([]byte, error) { return f.vm, nil }
func (f *fakeDarwin) pageSize() (uint64, error)       { return f.page, nil }
func (f *fakeDarwin) pidCount() (uint32, error)       { return f.pids, nil }

var le = binary.LittleEndian

func loadavgBytes(l1, l5, l15 uint32, fscale int64) []byte {
	b := make([]byte, sizeofLoadavg)
	le.PutUint32(b[0:], l1)
	le.PutUint32(b[4:], l5)
	le.PutUint32(b[8:], l15)
	le.PutUint64(b[16:], uint64(fscale))
	return b
}

func swapBytes(total, avail, used uint64) []byte {
	b := make([]byte, sizeofXswUsage)
	le.PutUint64(b[0:], total)
	le.PutUint64(b[8:], avail)
	le.PutUint64(b[16:], used)
	le.PutUint32(b[24:], 16384)
	le.PutUint32(b[28:], 1)
	return b
}

// vmBytes 只填 parseVMStatistics64 读的字段，各字段值互不相同，其余字段写入按偏移编号的哨兵值：
// 偏移错一位就读到哨兵。
func vmBytes(c vmCounts) []byte {
	b := make([]byte, 152)
	for off := 0; off+4 <= len(b); off += 4 {
		le.PutUint32(b[off:], 0x5A5A0000|uint32(off))
	}
	le.PutUint32(b[0:], uint32(c.free))
	le.PutUint32(b[4:], uint32(c.active))
	le.PutUint32(b[8:], uint32(c.inactive))
	le.PutUint32(b[12:], uint32(c.wire))
	le.PutUint32(b[88:], uint32(c.purgeable))
	le.PutUint32(b[92:], uint32(c.speculative))
	le.PutUint32(b[128:], uint32(c.compressor))
	le.PutUint32(b[132:], uint32(c.throttle))
	le.PutUint32(b[136:], uint32(c.external))
	le.PutUint32(b[140:], uint32(c.internal))
	return b
}

// ifmibBytes 把包计数 ifi_ipackets(24)、ifi_opackets(40) 与紧随 ifi_obytes 的 ifi_imcasts(80) 写成与字节数
// 不同的值，紧邻 ifi_ibytes 的 ifi_collisions(56) 留 0：把包计数当字节数、或错读相邻字段，都得不出期望值。
func ifmibBytes(name string, rx, tx uint64) []byte {
	b := make([]byte, 180)
	copy(b, name)
	d := b[ifmibDataOffset:]
	d[0] = 6 // IFT_ETHER
	le.PutUint64(d[24:], 11)
	le.PutUint64(d[40:], 22)
	le.PutUint64(d[64:], rx)
	le.PutUint64(d[72:], tx)
	le.PutUint64(d[80:], 33)
	return b
}

func newFakeDarwin() *fakeDarwin {
	return &fakeDarwin{
		strs: map[string]string{
			"kern.bootsessionuuid": "DEF2AAD6-739B-4A47-AC3D-8D8AB38FE277", "kern.hostname": "mac.local",
			"kern.osproductversion": "26.3.1", "kern.osrelease": "25.3.0", "machdep.cpu.brand_string": "Apple M4 Max",
		},
		u32: map[string]uint32{
			"net.inet.tcp.pcbcount": 614, "net.inet.udp.pcbcount": 94, "hw.logicalcpu": 13, "kern.hv_vmm_present": 0,
			"net.link.generic.system.ifcount": 4,
		},
		u64: map[string]uint64{"hw.memsize": 1 << 37},
		raw: map[string][]byte{
			"vm.loadavg": loadavgBytes(3072, 2048, 1024, 2048),
			// total、avail、used 互不相等（3、1、2 GiB）：错读成 xsu_avail 得不出 used。
			"vm.swapusage":              swapBytes(3<<30, 1<<30, 2<<30),
			"net.link.generic.ifdata/1": ifmibBytes("lo0", 9<<32, 9<<32),
			// 大于 2^32 且不是 1024 的倍数：32 位截断或 KiB 取整的来源都得不出这两个值。
			"net.link.generic.ifdata/2": ifmibBytes("en0", 1<<32+1025, 40_156_680_979),
			"net.link.generic.ifdata/4": ifmibBytes("utun0", 5, 5),
		},
		// 每个 CPU：user +30、system +15、idle +50、nice +5，四个状态的增量两两不同，
		// idle 下标错成任何别的状态都得不出 50%。
		ticks: [][]uint32{
			{100, 50, 800, 0, 100, 50, 800, 0},
			{130, 65, 850, 5, 130, 65, 850, 5},
		},
		vm:   vmBytes(vmCounts{wire: 400, purgeable: 30, compressor: 70, internal: 5000}),
		page: 16384,
		pids: 1566,
		disk: usage{total: 994662584320, used: 781134274560},
		up:   245955,
	}
}

func TestDarwinMetricsFromSyscallLayout(t *testing.T) {
	c := &Collector{Host: &darwinHost{src: newFakeDarwin()}, Clock: clock.NewFake(time.Unix(0, 0))}
	m, err := c.Metrics(c.Identify())
	if err != nil {
		t.Fatalf("unexpected read failures: %v", err)
	}
	if m.GetBootId() != "DEF2AAD6-739B-4A47-AC3D-8D8AB38FE277" || m.CpuPct != nil {
		t.Fatalf("boot_id %q, cpu_pct set %v (%v) on the first sample", m.GetBootId(), m.CpuPct != nil, m.GetCpuPct())
	}
	if m.GetMemTotal() != 1<<37 || m.GetMemUsed() != (5000-30+400+70)*16384 {
		t.Fatalf("mem = %d/%d, want total 2^37 and (internal-purgeable+wire+compressor)*page", m.GetMemUsed(), m.GetMemTotal())
	}
	if m.GetSwapTotal() != 3<<30 || m.GetSwapUsed() != 2<<30 {
		t.Fatalf("swap = %d/%d", m.GetSwapUsed(), m.GetSwapTotal())
	}
	if m.GetLoad1() != 1.5 || m.GetLoad5() != 1 || m.GetLoad15() != 0.5 {
		t.Fatalf("load = %v %v %v, want ldavg/fscale", m.GetLoad1(), m.GetLoad5(), m.GetLoad15())
	}
	if m.GetDiskTotal() != 994662584320 || m.GetDiskUsed() != 781134274560 || m.GetProcs() != 1566 || m.GetUptimeS() != 245955 {
		t.Fatalf("disk/procs/uptime: %+v", m)
	}
	if m.GetTcpConns() != 614 || m.GetUdpConns() != 94 {
		t.Fatalf("conns = %d/%d", m.GetTcpConns(), m.GetUdpConns())
	}
	// lo0 与 utun0 被默认排除，索引 3 缺号被跳过，只剩 en0。
	if m.GetNetRxTotal() != 1<<32+1025 || m.GetNetTxTotal() != 40_156_680_979 {
		t.Fatalf("net = %d/%d, want en0's 64-bit counters only", m.GetNetRxTotal(), m.GetNetTxTotal())
	}
	m, _ = c.Metrics(c.Identify())
	// 两个 CPU 各 +100 tick，其中各 50 空闲。
	if m.CpuPct == nil || m.GetCpuPct() != 50 {
		t.Fatalf("second cpu_pct set %v = %v, want 50", m.CpuPct != nil, m.GetCpuPct())
	}
}

func TestDarwinDefaultExcludeKeepsOnlyPhysicalInterfaces(t *testing.T) {
	c := &Collector{Host: &darwinHost{}}
	for _, n := range []string{"lo0", "gif0", "stf0", "utun0", "utun1024", "ipsec0", "bridge0", "bridge100", "vmenet0", "awdl0", "llw0", "anpi0", "ap1"} {
		if c.includeIface(n) {
			t.Errorf("%s must be excluded by default", n)
		}
	}
	for _, n := range []string{"en0", "en1", "en10"} {
		if !c.includeIface(n) {
			t.Errorf("%s must be included by default", n)
		}
	}
}

// 缺号之外的错误说明内核拒绝了一个存在的索引：合计会少一块网卡，读数整体缺失而不是少算。
func TestDarwinInterfaceReadErrorDropsTheWholeReading(t *testing.T) {
	f := newFakeDarwin()
	f.errs = map[string]error{"net.link.generic.ifdata/3": syscall.EPERM}
	c := &Collector{Host: &darwinHost{src: f}, Clock: clock.NewFake(time.Unix(0, 0))}
	m, err := c.Metrics(c.Identify())
	if m.NetRxTotal != nil || m.NetTxTotal != nil {
		t.Fatalf("partial interface set must not be reported: rx %d tx %d", m.GetNetRxTotal(), m.GetNetTxTotal())
	}
	if err == nil || !strings.Contains(err.Error(), "ifdata 3") {
		t.Fatalf("err = %v, want the failing index named", err)
	}
}

// 读完 ifcount 之后 en0 被删除，它的索引读出 ENOENT：只略过它，不让整个读数缺失。
func TestDarwinVanishedInterfaceIsSkipped(t *testing.T) {
	f := newFakeDarwin()
	f.errs = map[string]error{"net.link.generic.ifdata/2": syscall.ENOENT}
	ifs, err := (&darwinHost{src: f}).ifaces()
	var names []string
	for _, i := range ifs {
		names = append(names, i.name)
	}
	if err != nil || strings.Join(names, " ") != "lo0 utun0" {
		t.Fatalf("interfaces %v, %v; want [lo0 utun0] with the vanished en0 skipped", names, err)
	}
}

func TestTickAccumulatorAcrossWrapAndCPUCountChange(t *testing.T) {
	var a tickAccumulator
	first, err := a.add([]uint32{10, 5, math.MaxUint32 - 19, 1})
	if err != nil {
		t.Fatal(err)
	}
	// idle 从 2^32−20 回绕到 30：增量 50；user +30、system +15、nice +5，四个增量两两不同。
	cur, _ := a.add([]uint32{40, 20, 30, 6})
	if pct, ok := cpuPercent(first, cur); !ok || pct != 50 {
		t.Fatalf("pct across wrap = %v,%v, want 50 (50 busy of 100)", pct, ok)
	}
	// CPU 个数变化：累计值不动，没有读数。
	next, _ := a.add([]uint32{30, 0, 40, 0, 1, 1, 1, 1})
	if _, ok := cpuPercent(cur, next); ok {
		t.Fatal("cpu count change must yield no reading")
	}
	// 换基线之后，同样个数的下一份照常给读数：每 CPU user +10、idle +30，共 80 tick、60 空闲。
	after, _ := a.add([]uint32{40, 0, 70, 0, 11, 1, 31, 1})
	if pct, ok := cpuPercent(next, after); !ok || pct != 25 {
		t.Fatalf("pct after rebaselining = %v,%v, want 25 (20 busy of 80)", pct, ok)
	}
	if _, err := a.add([]uint32{1, 2, 3}); err == nil {
		t.Fatal("ticks not a multiple of cpuStateMax must be an error")
	}
}

// 直接对照头文件偏移，不经 Collector：偏移错一位就读到 vmBytes 的哨兵，失败信息里带 0x5A5A…。
func TestVMStatisticsReadsHeaderOffsets(t *testing.T) {
	want := vmCounts{free: 9000, active: 800, inactive: 700, wire: 400, purgeable: 30, speculative: 600, compressor: 70, throttle: 4, external: 500, internal: 5000}
	got, err := parseVMStatistics64(vmBytes(want))
	if err != nil || got != want {
		t.Fatalf("pages %+v, %v; want %+v (0x5a5a.... means a sentinel word was read)", got, err, want)
	}
}

func TestVMUsedRejectsInconsistentCounts(t *testing.T) {
	if used, err := (vmCounts{wire: 400, purgeable: 900, compressor: 70, internal: 500}).usedPages(); err == nil {
		t.Fatalf("purgeable above internal must be an error, got %d", used)
	}
	if _, err := parseVMStatistics64(make([]byte, vmStatsMinLen-1)); err == nil {
		t.Fatal("short vm_statistics64 must be an error")
	}
}

// ifcount 是最大索引而不是个数：最大索引上的网卡（开机后插上的 USB、雷雳网卡常拿到它）也要读到。
func TestDarwinEnumeratesEveryIndexUpToIfcount(t *testing.T) {
	ifs, err := (&darwinHost{src: newFakeDarwin()}).ifaces()
	var names []string
	for _, i := range ifs {
		names = append(names, i.name)
	}
	if err != nil || strings.Join(names, " ") != "lo0 en0 utun0" {
		t.Fatalf("interfaces %v, %v; want [lo0 en0 utun0]", names, err)
	}
}

// internal − purgeable 回绕后再加上 wire，结果落在总量以内：checkUsage 放行，只有 usedPages 的检查拒收。
func TestVMUsedWrapIsCaughtBeforeCheckUsage(t *testing.T) {
	internal, purgeable, wire, page := uint64(100), uint64(110), uint64(50), uint64(16384)
	if wrapped := (internal - purgeable + wire) * page; wrapped > 1<<37 {
		t.Fatalf("fixture no longer demonstrates a wrap within total: %d", wrapped)
	}
	f := newFakeDarwin()
	f.vm = vmBytes(vmCounts{wire: wire, purgeable: purgeable, internal: internal})
	dc := &Collector{Host: &darwinHost{src: f}, Clock: clock.NewFake(time.Unix(0, 0))}
	m, err := dc.Metrics(dc.Identify())
	if m.MemUsed != nil || m.MemTotal != nil {
		t.Fatalf("wrapped memory reading reported: used %d total %d", m.GetMemUsed(), m.GetMemTotal())
	}
	if err == nil || !strings.Contains(err.Error(), "purgeable 110 exceeds internal 100") {
		t.Fatalf("err = %v, want usedPages's rejection", err)
	}
}

func TestDarwinLayoutsRejectWrongSizes(t *testing.T) {
	if _, err := parseLoadavgSysctl(loadavgBytes(1, 1, 1, 0)); err == nil {
		t.Fatal("fscale 0 must be an error, not +Inf")
	}
	if l, err := parseLoadavgSysctl(loadavgBytes(1, 1, 1, -65536)); err == nil {
		t.Fatalf("negative fscale must be an error, not a negative load %v", l.l1)
	}
	if _, err := parseLoadavgSysctl(make([]byte, 20)); err == nil {
		t.Fatal("short loadavg must be an error")
	}
	if _, err := parseSwapUsage(make([]byte, 24)); err == nil {
		t.Fatal("short xsw_usage must be an error")
	}
	if _, err := parseIfmibData(make([]byte, sizeofIfmibdata)); err == nil {
		t.Fatal("empty interface name must be an error")
	}
	// ifmibdata 定长：长一点短一点都说明布局变了，按旧偏移读出的计数不可信。
	for _, n := range []int{sizeofIfmibdata - 1, sizeofIfmibdata + 1} {
		b := make([]byte, n)
		copy(b, "en0")
		if _, err := parseIfmibData(b); err == nil {
			t.Fatalf("%d-byte ifmibdata must be an error", n)
		}
	}
}

func TestDarwinFacts(t *testing.T) {
	f := newFakeDarwin()
	f.u32["kern.hv_vmm_present"] = 1
	dc := &Collector{Host: &darwinHost{src: f}, Version: "v"}
	got := dc.Facts(dc.Identify())
	if got.GetOs() != "macOS 26.3.1" || got.GetKernel() != "25.3.0" || got.GetCpuModel() != "Apple M4 Max" ||
		got.GetCpuCores() != 13 || got.GetHostname() != "mac.local" || got.GetVirtualization() != "vm" {
		t.Fatalf("%+v", got)
	}
	delete(f.u32, "kern.hv_vmm_present")
	if v := (&darwinHost{src: f}).facts().virtualization; v != "" {
		t.Fatalf("missing kern.hv_vmm_present must not claim a VM: %q", v)
	}
}
