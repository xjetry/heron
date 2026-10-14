//go:build darwin

package collect

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/clock"
)

// 本文件把系统调用层的读数与系统自带命令行工具的输出对照：命令行工具走的是另一条代码路径，
// 字节偏移、单位或来源选错时两边对不上。gauge 类读数在两次读取之间会变，容差按变化幅度给，
// 远小于偏移或单位错误造成的偏差。

func run(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		t.Fatalf("%s %v: %v", name, args, err)
	}
	return strings.TrimSpace(string(out))
}

func cliUint(t *testing.T, name string, args ...string) uint64 {
	t.Helper()
	s := run(t, name, args...)
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		t.Fatalf("%s %v printed %q: %v", name, args, s, err)
	}
	return v
}

func within(a, b, tol uint64) bool {
	if a > b {
		return a-b <= tol
	}
	return b-a <= tol
}

func realHost(t *testing.T) *darwinHost {
	t.Helper()
	s := openDarwinSyscalls()
	for _, sym := range libSystemSyms {
		if err := s.need(sym.name); err != nil {
			t.Fatalf("libSystem: %v", err)
		}
	}
	return &darwinHost{src: s}
}

func TestDarwinEveryMetricPresent(t *testing.T) {
	c, err := NewPlatform("test", clock.Real(), nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Metrics(c.Identify()); err != nil {
		t.Fatalf("first sample: %v", err)
	}
	time.Sleep(1100 * time.Millisecond)
	m, err := c.Metrics(c.Identify())
	if err != nil {
		t.Fatalf("second sample: %v", err)
	}
	if m.GetBootId() == "" || m.CpuPct == nil || m.MemTotal == nil || m.MemUsed == nil || m.SwapTotal == nil || m.SwapUsed == nil ||
		m.DiskTotal == nil || m.DiskUsed == nil || m.Load1 == nil || m.Procs == nil || m.UptimeS == nil ||
		m.TcpConns == nil || m.UdpConns == nil || m.NetRxTotal == nil || m.NetTxTotal == nil || m.NetRxBps == nil {
		t.Fatalf("missing readings: %+v", m)
	}
}

func TestDarwinBootIDAndUptimeMatchSysctl(t *testing.T) {
	h := realHost(t)
	id, err := h.bootID()
	if err != nil || id != run(t, "sysctl", "-n", "kern.bootsessionuuid") {
		t.Fatalf("boot id %q, %v", id, err)
	}
	// kern.boottime 是 "{ sec = N, usec = M } ..."；now − boottime 含睡眠时间，与 CLOCK_MONOTONIC 同口径。
	bt := run(t, "sysctl", "-n", "kern.boottime")
	sec, _, _ := strings.Cut(strings.TrimPrefix(bt, "{ sec = "), ",")
	boot, err := strconv.ParseInt(sec, 10, 64)
	if err != nil {
		t.Fatalf("kern.boottime %q: %v", bt, err)
	}
	up, err := h.uptime()
	if err != nil || !within(up, uint64(time.Now().Unix()-boot), 2) {
		t.Fatalf("uptime %d, %v; now − kern.boottime = %d", up, err, time.Now().Unix()-boot)
	}
}

// vmStat 解析 vm_stat 的页大小与各行计数。
func vmStat(t *testing.T) (page uint64, pages map[string]uint64) {
	t.Helper()
	pages = map[string]uint64{}
	sc := bufio.NewScanner(strings.NewReader(run(t, "vm_stat")))
	for sc.Scan() {
		line := sc.Text()
		if i := strings.Index(line, "page size of "); i >= 0 {
			page, _ = strconv.ParseUint(strings.Fields(line[i+len("page size of "):])[0], 10, 64)
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if n, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(v), "."), 10, 64); err == nil {
			pages[strings.Trim(k, `"`)] = n
		}
	}
	return page, pages
}

// 内存读数的口径对照。host_statistics64(HOST_VM_INFO64) 对第三方调用者限流（macOS 26.3.1 / kernel 25.3.0
// 实测）：内核按 flavor 维护一份全局缓存，返回值约每秒才变一次；变的时候连续几次调用各拿到调用时刻的值，
// 随后的调用拿最后那次的副本，直到下一个窗口。所以单次读数可能是近一秒内任一时刻的值，而负载下 free 在
// 一秒里非单调地摆动、幅度与它自身同量级：在读数前后采一段 sysctl 取包络再加容差，要求采样间隔里的极值
// 不越出容差，负载下 fork 一次 sysctl 命令的间隔做不到这一点，按读数比例给的容差在 free 很小时也很小。
// 另一方面，拿到调用时刻值的那次结果与紧邻它的 sysctl 读数逐页相等。
// 于是这里连续调用 vmStatistics64，每两次调用之间在进程内读一次 sysctl（与 sysctl 命令同一个内核接口，
// 相邻读数只隔微秒）。缓存只在有调用取实时值时改写，所以返回值与上一次不同时，这个值是在上一次调用到
// 本次调用返回之间取的，被上一次调用之前、两次调用之间、本次调用之后这三次读数夹住。三次读数相同时，
// 实时值与它们逐页相等；读数之间计数动过时（负载下 free 成批增减，跨度只有微秒也会动），计数可能在两次
// 读数之间越出再折回，实时值落在三次读数围成的范围之外，实测偏离从未超过这个范围的宽度。所以每个计数的
// 容差取它三次读数的极差：没动就逐页相等，动了就按观测到的移动量放宽，通常远小于把 speculative 算错一遍
// 的偏差。每个限流窗口通常有多次改写可选，取三个计数极差之和最小的那次（并列取跨度最短）。
// 循环时长须超过一个限流窗口，才保证至少跨过一次窗口起点、看到一次改写。
// 这样对照的是 free 含 speculative 的语义（头文件注明）与 free/speculative/external 三个计数的偏移：
// 快照的 external_page_count 与 vm.page_pageable_external_count 逐页相等，vm.vm_page_external_count 则
// 恒大一截，不能拿来逐页对照。页大小与内存总量是静态量，与 vm_stat、hw.memsize 对照；队列分类合计
// （free+active+inactive+wired+compressor+throttled）与总页数的差是 REV3 的 tag storage 页，偏移读错会把
// 零填充等 lifetime 计数（十亿级）混进页计数让合计爆掉。
func TestDarwinMemoryMatchesVMStat(t *testing.T) {
	h := realHost(t)
	s := h.src.(*darwinSyscalls)
	type counts struct{ free, spec, external uint64 }
	read := func() counts {
		t.Helper()
		var c counts
		for _, f := range []struct {
			name string
			dst  *uint64
		}{{"vm.page_free_count", &c.free}, {"vm.page_speculative_count", &c.spec}, {"vm.page_pageable_external_count", &c.external}} {
			v, err := unix.SysctlUint32(f.name)
			if err != nil {
				t.Fatalf("sysctl %s: %v", f.name, err)
			}
			*f.dst = uint64(v)
		}
		return c
	}
	// ring 是最近三次 sysctl 读数，ringAt 是各自开始读的时刻；ring[2] 紧跟最近一次调用之后。
	var ring [3]counts
	var ringAt [3]time.Time
	push := func() {
		copy(ring[:], ring[1:])
		copy(ringAt[:], ringAt[1:])
		ringAt[2] = time.Now()
		ring[2] = read()
	}
	push()
	var prev, liveRaw []byte
	var bracket [3]counts
	span := time.Duration(-1)
	moved := uint64(0)
	changes := 0
	movement := func(r [3]counts) uint64 {
		var sum uint64
		for _, pick := range []func(counts) uint64{
			func(w counts) uint64 { return w.free }, func(w counts) uint64 { return w.spec }, func(w counts) uint64 { return w.external },
		} {
			a, b, d := pick(r[0]), pick(r[1]), pick(r[2])
			sum += max(a, b, d) - min(a, b, d)
		}
		return sum
	}
	for t0 := time.Now(); time.Since(t0) < 1500*time.Millisecond; {
		raw, err := s.vmStatistics64()
		if err != nil {
			t.Fatal(err)
		}
		push()
		// prev 非空时已有三次读数：调用前两次、调用后一次，夹住上一次调用开始到本次调用返回。
		if prev != nil && !bytes.Equal(raw, prev) {
			changes++
			d, m := time.Since(ringAt[0]), movement(ring)
			if span < 0 || m < moved || (m == moved && d < span) {
				span, moved, liveRaw, bracket = d, m, raw, ring
			}
		}
		prev = raw
	}
	if changes == 0 {
		t.Fatalf("host_statistics64 returned the same value for 1.5s: no live result to compare against")
	}
	c, err := parseVMStatistics64(liveRaw)
	if err != nil {
		t.Fatal(err)
	}
	cliPage, _ := vmStat(t)
	page, err := s.pageSize()
	if err != nil || page != cliPage {
		t.Fatalf("page size %d, %v; vm_stat says %d", page, err, cliPage)
	}
	mem, err := h.memory()
	if err != nil || mem.total != cliUint(t, "sysctl", "-n", "hw.memsize") {
		t.Fatalf("memory %+v, %v; total must equal hw.memsize", mem, err)
	}
	total := mem.total / page
	// 队列分类合计：free+active+inactive+wired+compressor+throttled 与总页数的差是 tag storage 页，
	// 必须非负且远小于总量（实测约 0.7%，上限取 1/25）；任一页计数读成 lifetime 计数都会让合计溢出总量。
	queues := c.free + c.active + c.inactive + c.wire + c.compressor + c.throttle
	if residual := int64(total) - int64(queues); residual < 0 || residual > int64(total/25) {
		t.Errorf("queue pages free %d + active %d + inactive %d + wired %d + compressor %d + throttled %d = %d, total %d: residual %d out of [0, %d]",
			c.free, c.active, c.inactive, c.wire, c.compressor, c.throttle, queues, total, residual, total/25)
	}
	bracketed := func(name string, got uint64, pick func(counts) uint64) {
		t.Helper()
		a, b, d := pick(bracket[0]), pick(bracket[1]), pick(bracket[2])
		lo, hi := min(a, b, d), max(a, b, d)
		if tol := hi - lo; got+tol < lo || got > hi+tol {
			t.Errorf("%s %d pages, sysctl read %d, %d, %d around it (tolerance %d, span %v, %d changes seen)", name, got, a, b, d, tol, span, changes)
		}
	}
	// free_count 含 speculative（头文件注明），sysctl 把两者分列。
	if c.free < c.speculative {
		t.Errorf("free %d below speculative %d: header says speculative is counted in free_count", c.free, c.speculative)
	} else {
		bracketed("free − speculative =", c.free-c.speculative, func(w counts) uint64 { return w.free })
	}
	bracketed("speculative", c.speculative, func(w counts) uint64 { return w.spec })
	bracketed("external", c.external, func(w counts) uint64 { return w.external })
}

// netstat -ib 的 <Link#n> 行给出每块网卡的完整 64 位字节数。先比网卡集合：实现漏读一块网卡，
// 合计就少了它的全部字节（netstat 给 down 的网卡名加 *，先去掉）；只放过两次读取之间新建或销毁的网卡。
// 再比计数：计数只增不减，前后各读一次本实现，netstat 的值必须夹在两次之间，没有容差。
func TestDarwinInterfaceCountersSandwichNetstat(t *testing.T) {
	h := realHost(t)
	before, err := h.ifaces()
	if err != nil {
		t.Fatal(err)
	}
	out := run(t, "netstat", "-ib", "-n")
	after, err := h.ifaces()
	if err != nil {
		t.Fatal(err)
	}
	index := func(ifs []ifaceCounters) map[string]ifaceCounters {
		m := map[string]ifaceCounters{}
		for _, i := range ifs {
			m[i.name] = i
		}
		return m
	}
	b, a := index(before), index(after)
	cli := map[string]bool{}
	checked := 0
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 7 || !strings.HasPrefix(f[2], "<Link#") {
			continue
		}
		name := strings.TrimSuffix(f[0], "*")
		cli[name] = true
		// 有链路地址的行多一列 Address；字节列按从行尾倒数取：Ibytes 在倒数第五，Obytes 在倒数第二。
		rx, err1 := strconv.ParseUint(f[len(f)-5], 10, 64)
		tx, err2 := strconv.ParseUint(f[len(f)-2], 10, 64)
		if err1 != nil || err2 != nil {
			t.Fatalf("cannot parse netstat line %q", line)
		}
		x, ok1 := b[name]
		y, ok2 := a[name]
		if ok1 != ok2 {
			continue // 两次读取之间新建或销毁的网卡
		}
		if !ok1 {
			t.Errorf("%s: listed by netstat, missing from both reads", name)
			continue
		}
		if x.rx > rx || rx > y.rx || x.tx > tx || tx > y.tx {
			t.Errorf("%s: netstat rx=%d tx=%d not within [%d,%d]/[%d,%d]", name, rx, tx, x.rx, y.rx, x.tx, y.tx)
		}
		checked++
	}
	for name := range b {
		if _, again := a[name]; again && !cli[name] {
			t.Errorf("%s: read twice by the implementation, not listed by netstat", name)
		}
	}
	if checked == 0 || !cli["lo0"] {
		t.Fatalf("no interface compared (lo0 listed: %v)", cli["lo0"])
	}
}

func TestDarwinSwapLoadProcsDiskMatchCLI(t *testing.T) {
	h := realHost(t)
	// swap 的对照方与本实现读同一个 sysctl（vm.swapusage 的 struct xsw_usage），钉的是解析与单位，不是独立来源。
	// 已用量随内存压力随时在变，fork 一次 sysctl 命令的几毫秒里可以变出任何固定容差，所以只与紧邻的进程内读数
	// 对照：本实现读取前后各读一次结构体，值须落在两次之间，容差取两次之差。结构体按头文件自行解码（total、
	// avail、used 在字节 0、8、16）；同一次读数里 avail + used = total（实测），钉住 total 的位置与 8 字节字宽，
	// avail 与 used 谁前谁后它看不出，靠头文件。总量随 swap 文件的增删成步变化，sysctl 命令打印的总量
	// （"total = 0.00M  used = …"，MiB，两位小数）须落在命令前后两次解码的总量之间（外扩 0.005 MiB 容下舍入），
	// 钉住字节单位。没有 swap 的机器上各量全为零，这几条只剩格式检查。
	xsw := func() (total, used uint64) {
		t.Helper()
		b, err := unix.SysctlRaw("vm.swapusage")
		if err != nil {
			t.Fatal(err)
		}
		if len(b) < 24 {
			t.Fatalf("vm.swapusage: %d bytes", len(b))
		}
		le := binary.LittleEndian
		total, avail, used := le.Uint64(b[0:]), le.Uint64(b[8:]), le.Uint64(b[16:])
		if avail+used != total {
			t.Fatalf("xsw_usage avail %d + used %d != total %d", avail, used, total)
		}
		return total, used
	}
	cliTotal0, _ := xsw()
	f := strings.Fields(run(t, "sysctl", "-n", "vm.swapusage"))
	cliTotal1, _ := xsw()
	cli, err := strconv.ParseFloat(strings.TrimSuffix(f[2], "M"), 64)
	if err != nil {
		t.Fatalf("vm.swapusage total %q: %v", f[2], err)
	}
	if lo, hi := float64(min(cliTotal0, cliTotal1))/(1<<20), float64(max(cliTotal0, cliTotal1))/(1<<20); cli < lo-(hi-lo)-0.005 || cli > hi+(hi-lo)+0.005 {
		t.Errorf("vm.swapusage prints total %v MiB, struct read %d and %d bytes around it", cli, cliTotal0, cliTotal1)
	}
	total0, used0 := xsw()
	sw, err := h.swap()
	if err != nil {
		t.Fatal(err)
	}
	total1, used1 := xsw()
	for _, c := range []struct {
		name               string
		got, before, after uint64
	}{{"total", sw.total, total0, total1}, {"used", sw.used, used0, used1}} {
		lo, hi := min(c.before, c.after), max(c.before, c.after)
		if tol := hi - lo; c.got+tol < lo || c.got > hi+tol {
			t.Errorf("swap %s: implementation %d, vm.swapusage %d before and %d after (tolerance %d)", c.name, c.got, c.before, c.after, tol)
		}
	}
	// "{ 1.23 4.56 7.89 }"，两位小数。负载每 5 秒更新一次，两次读取之间可能恰好更新：
	// 前后各读一次本实现，CLI 值须落在两次读数围成的区间内（外扩 0.005 容下舍入）。
	l0, err := h.load()
	if err != nil {
		t.Fatal(err)
	}
	lf := strings.Fields(strings.Trim(run(t, "sysctl", "-n", "vm.loadavg"), "{ }"))
	l1, err := h.load()
	if err != nil {
		t.Fatal(err)
	}
	for i, pair := range [][2]float64{{l0.l1, l1.l1}, {l0.l5, l1.l5}, {l0.l15, l1.l15}} {
		cli, err := strconv.ParseFloat(lf[i], 64)
		if err != nil {
			t.Fatalf("vm.loadavg %v: %v", lf, err)
		}
		if lo, hi := min(pair[0], pair[1])-0.005, max(pair[0], pair[1])+0.005; cli < lo || cli > hi {
			t.Fatalf("load[%d]: vm.loadavg %v not within [%v, %v]", i, cli, lo, hi)
		}
	}
	// 进程数的对照方是 kern.proc.all 的进程表（ps 的来源），与本实现的 proc_listallpids 是两个内核接口，同一时刻
	// 给出同一个数（僵尸进程两边都算，实测）。本实现每次读到的都是某一时刻的真实计数，所以它落在紧邻的前后两次
	// 进程表读数之间（容差取两次之差）就是充分凭据。但读一次进程表要拷整张表，负载下要毫秒级，这段空窗里新建又
	// 退出的进程前后两次都看不见，单独一轮会越界；空窗里的生灭与本实现无关，各轮彼此独立。于是计划内跑 procRounds
	// 轮，至少一轮被夹住即通过：误报要每一轮都恰有进程在空窗里生灭，而系统性偏差（单位错、取缓冲区容量、取不带
	// 缓冲区时返回的估计值）每一轮都越界。
	const procRounds = 5
	procTable := func() uint64 {
		t.Helper()
		ps, err := unix.SysctlKinfoProcSlice("kern.proc.all")
		if err != nil {
			t.Fatal(err)
		}
		return uint64(len(ps))
	}
	var procMisses []string
	for range procRounds {
		before := procTable()
		n, err := h.procs()
		if err != nil {
			t.Fatal(err)
		}
		after := procTable()
		lo, hi := min(before, after), max(before, after)
		if tol := hi - lo; uint64(n)+tol >= lo && uint64(n) <= hi+tol {
			procMisses = nil
			break
		}
		procMisses = append(procMisses, fmt.Sprintf("%d (kern.proc.all %d before, %d after)", n, before, after))
	}
	if len(procMisses) == procRounds {
		t.Errorf("procs outside the process-table reads around it in all %d rounds: %s", procRounds, strings.Join(procMisses, "; "))
	}
	d, err := h.disk()
	if err != nil {
		t.Fatal(err)
	}
	df := strings.Fields(strings.Split(run(t, "df", "-k", "/"), "\n")[1])
	if kb, _ := strconv.ParseUint(df[1], 10, 64); d.total != kb*1024 {
		t.Fatalf("disk total %d vs df %s KiB", d.total, df[1])
	}
}

// 连接数是全机的 TCP/UDP pcb 计数，别的进程随时成批开关 socket：fork 一次 sysctl 命令的几毫秒里计数就可能
// 变出任何固定容差，本进程自己开 socket 再看增量也一样——开 socket 的那几毫秒里别的进程开关的数量可以
// 超过自己开的数量。能逐值对照的只有紧邻的读数：在本实现读取前后各在进程内读一次 sysctl（与 sysctl 命令
// 同一个内核接口，相隔微秒），本实现的值须落在两次读数之间，容差取两次读数之差——没动就逐值相等，动了
// 就按观测到的移动量放宽。这守住的是计数与协议的对应：TCP 与 UDP 的计数通常相差很远，读反或读成别的协议族
// （如 net.local 的 AF_UNIX）都越出容差。net.inet.tcp/udp.pcbcount 本身同时计入 IPv4 与 IPv6 socket（实测各开
// 一批，计数都增同样的数目）；本进程持有的两种地址族 socket 数又是计数的下界，与读数时刻无关，读成只计
// 一小部分 socket 的计数会落到下界以下。
func TestDarwinConnsCountOpenSockets(t *testing.T) {
	const perFamily = 16
	var socks []io.Closer
	defer func() {
		for _, s := range socks {
			s.Close()
		}
	}()
	for _, addr := range []string{"127.0.0.1:0", "[::1]:0"} {
		for range perFamily {
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				t.Fatal(err)
			}
			pc, err := net.ListenPacket("udp", addr)
			if err != nil {
				ln.Close()
				t.Fatal(err)
			}
			socks = append(socks, ln, pc)
		}
	}
	read := func() (uint64, uint64) {
		t.Helper()
		tcp, err := unix.SysctlUint32("net.inet.tcp.pcbcount")
		if err != nil {
			t.Fatal(err)
		}
		udp, err := unix.SysctlUint32("net.inet.udp.pcbcount")
		if err != nil {
			t.Fatal(err)
		}
		return uint64(tcp), uint64(udp)
	}
	tcp0, udp0 := read()
	tcp, udp, err := realHost(t).conns()
	if err != nil {
		t.Fatal(err)
	}
	tcp1, udp1 := read()
	for _, c := range []struct {
		name               string
		got, before, after uint64
	}{{"net.inet.tcp.pcbcount", uint64(tcp), tcp0, tcp1}, {"net.inet.udp.pcbcount", uint64(udp), udp0, udp1}} {
		lo, hi := min(c.before, c.after), max(c.before, c.after)
		if tol := hi - lo; c.got+tol < lo || c.got > hi+tol {
			t.Errorf("%s: implementation %d, sysctl %d before and %d after (tolerance %d)", c.name, c.got, c.before, c.after, tol)
		}
		if c.got < 2*perFamily {
			t.Errorf("%s: implementation %d with %d such sockets of this process open over IPv4 and IPv6", c.name, c.got, 2*perFamily)
		}
	}
}

// 每个 CPU 每秒走 hz 个 tick（kern.clockrate）；逐 CPU 之和除以 CPU 数与经过的秒数应接近 hz。
// 单位或字段错位会差出数量级。
func TestDarwinProcessorTicksAdvanceAtClockRate(t *testing.T) {
	s := openDarwinSyscalls()
	a, err := s.processorTicks()
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	time.Sleep(time.Second)
	b, err := s.processorTicks()
	if err != nil {
		t.Fatal(err)
	}
	dt := time.Since(t0).Seconds()
	var sum uint64
	for i := range a {
		sum += uint64(b[i] - a[i])
	}
	cr := run(t, "sysctl", "-n", "kern.clockrate")
	hzs, _, _ := strings.Cut(strings.TrimPrefix(cr, "{ hz = "), ",")
	hz, err := strconv.ParseFloat(hzs, 64)
	if err != nil {
		t.Fatalf("kern.clockrate %q", cr)
	}
	cpus := float64(len(a) / cpuStateMax)
	if rate := float64(sum) / cpus / dt; rate < hz*0.75 || rate > hz*1.25 {
		t.Fatalf("%.1f ticks per cpu per second, hz = %v", rate, hz)
	}
	if uint64(cpus) != cliUint(t, "sysctl", "-n", "hw.logicalcpu") {
		t.Fatalf("%v cpus vs hw.logicalcpu", cpus)
	}
}

func TestDarwinFactsMatchCLI(t *testing.T) {
	h := realHost(t)
	f := h.facts()
	if f.os != "macOS "+run(t, "sw_vers", "-productVersion") || f.kernel != run(t, "uname", "-r") ||
		f.cpuModel != run(t, "sysctl", "-n", "machdep.cpu.brand_string") || f.hostname != run(t, "sysctl", "-n", "kern.hostname") {
		t.Fatalf("%+v", f)
	}
	// Facts.cpu_cores 来自识别快照：darwin 的有效核数是 hw.logicalcpu。
	if cores := h.identify().exec.GetCpuEffectiveCores(); uint64(cores) != cliUint(t, "sysctl", "-n", "hw.logicalcpu") {
		t.Fatalf("identify cores = %v, want hw.logicalcpu", cores)
	}
}

// 两次采样之后的全部读数；第二次才有 cpu_pct。
func twoSamples(t *testing.T, c *Collector) (*heronv1.Metrics, error) {
	t.Helper()
	c.Metrics(c.Identify())
	time.Sleep(1100 * time.Millisecond)
	return c.Metrics(c.Identify())
}

// 缺一个 libSystem 函数只让依赖它的读数缺失：NewPlatform 与这里共用 newDarwinCollector，
// 构造不失败、不 panic，否则 launchd 的 KeepAlive 会每 5 秒拉起一个立即退出的进程。
func TestDarwinMissingLibSystemFunctionOnlyDropsItsReading(t *testing.T) {
	syms := slices.Clone(libSystemSyms)
	for i := range syms {
		if syms[i].name == "proc_listallpids" {
			syms[i].name = "proc_listallpids_nosuch"
		}
	}
	c, err := newDarwinCollector(openLibSystem(libSystemPath, syms), "t", clock.Real(), nil, nil)
	if err != nil {
		t.Fatalf("a missing function must not fail construction: %v", err)
	}
	m, err := twoSamples(t, c)
	if m.Procs != nil || err == nil || !strings.Contains(err.Error(), "proc_listallpids") {
		t.Fatalf("procs set %v, err = %v; want procs missing and the symbol named", m.Procs != nil, err)
	}
	if m.CpuPct == nil || m.MemUsed == nil {
		t.Fatalf("cpu_pct set %v, mem_used set %v: readings that do not need proc_listallpids must survive", m.CpuPct != nil, m.MemUsed != nil)
	}
}

// libSystem 整个打不开：依赖它的 CPU、已用内存与进程数缺失，sysctl 读数照常，构造不失败。
func TestDarwinWithoutLibSystemKeepsSysctlReadings(t *testing.T) {
	c, err := newDarwinCollector(openLibSystem("/nonexistent/libSystem.B.dylib", libSystemSyms), "t", clock.Real(), nil, nil)
	if err != nil {
		t.Fatalf("an unloadable libSystem must not fail construction: %v", err)
	}
	m, err := twoSamples(t, c)
	if err == nil || !strings.Contains(err.Error(), "dlopen /nonexistent/libSystem.B.dylib") {
		t.Fatalf("err = %v, want the libSystem failure reported", err)
	}
	if m.CpuPct != nil || m.MemUsed != nil || m.Procs != nil {
		t.Fatalf("readings that need libSystem must be missing: cpu_pct %v mem_used %v procs %v", m.CpuPct != nil, m.MemUsed != nil, m.Procs != nil)
	}
	if m.GetBootId() == "" || m.Load1 == nil || m.SwapTotal == nil || m.DiskTotal == nil || m.TcpConns == nil || m.NetRxTotal == nil || m.UptimeS == nil {
		t.Fatalf("sysctl readings must survive: %+v", m)
	}
}
