package collect

import (
	"strings"
	"testing"
)

func TestParseStatCPUTimes(t *testing.T) {
	in := "cpu  100 0 50 800 20 0 10 0 0 0\ncpu0 1 2 3 4 5 6 7 8 9 10\n"
	c, err := parseStat(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	// idle = idle + iowait = 820；total = 100+0+50+800+20+0+10+0 = 980
	if c.idle != 820 || c.total != 980 {
		t.Fatalf("idle/total = %d/%d, want 820/980", c.idle, c.total)
	}
}

// 7 计数器的 CPU 行也能解析；不足 5 个就没有 iowait，应返回格式错误而不是越界。
func TestParseStatAcceptsOlderKernelsWithFewerCounters(t *testing.T) {
	c, err := parseStat(strings.NewReader("cpu 1 2 3 4 5 6 7\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.total != 28 || c.idle != 9 {
		t.Fatalf("total/idle = %d/%d, want 28/9", c.total, c.idle)
	}
	if _, err := parseStat(strings.NewReader("cpu 1 2 3 4\n")); err == nil {
		t.Fatal("four counters cannot carry iowait; must be a parse error, not a panic")
	}
}

func TestCPUPercentFromTwoSamples(t *testing.T) {
	a := cpuTimes{idle: 800, total: 1000}
	b := cpuTimes{idle: 850, total: 1100} // 100 个 tick 里 50 个空闲 → 50%
	got, ok := cpuPercent(a, b)
	if !ok || got != 50 {
		t.Fatalf("cpu%% = %v,%v", got, ok)
	}
	if _, ok := cpuPercent(b, b); ok {
		t.Fatal("no elapsed ticks must yield no reading, not 0%")
	}
}

// /proc/stat 首行里的 iowait（第 5 个）与 steal（第 8 个）各取各的；steal 字段缺失时是「没有」而不是 0。
func TestParseStatExtractsStealAndIowait(t *testing.T) {
	c, err := parseStat(strings.NewReader("cpu  100 0 50 800 20 0 10 5 0 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.iowait != 20 || c.steal != 5 || !c.hasIowait || !c.hasSteal {
		t.Fatalf("iowait/steal = %d/%d flags %v/%v, want 20/5", c.iowait, c.steal, c.hasIowait, c.hasSteal)
	}
	c, err = parseStat(strings.NewReader("cpu 1 2 3 4 5 6 7\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !c.hasIowait || c.hasSteal {
		t.Fatalf("hasIowait/hasSteal = %v/%v, want true/false", c.hasIowait, c.hasSteal)
	}
}

// 三个占比共用同一个分母与有效性判定；cpu_pct 的口径不变（idle 含 iowait），steal 与 iowait 是独立系列。
func TestCPURatiosShareOneDifferential(t *testing.T) {
	// Δtotal=100、Δidle=50 → busy 50%；Δiowait=20、Δsteal=20 → 各 20%。
	prev := cpuTimes{idle: 800, total: 1000, iowait: 20, steal: 5, hasIowait: true, hasSteal: true}
	cur := cpuTimes{idle: 850, total: 1100, iowait: 40, steal: 25, hasIowait: true, hasSteal: true}
	busy, steal, iowait, ok := cpuRatios(prev, cur)
	if !ok || busy != 50 || steal != 20 || iowait != 20 {
		t.Fatalf("busy/steal/iowait = %v/%v/%v ok=%v, want 50/20/20", busy, steal, iowait, ok)
	}
	if got, ok := cpuPercent(prev, cur); !ok || got != busy {
		t.Fatalf("cpu_pct = %v ok=%v, want the same busy as cpuRatios", got, ok)
	}
}

// Δtotal=0 或任一计数回退时三项一起缺失：不能只丢回退的那一项，留下另外两项。
func TestCPURatiosDropAllOnNoElapsedOrRegression(t *testing.T) {
	prev := cpuTimes{idle: 800, total: 1000, iowait: 20, steal: 5, hasIowait: true, hasSteal: true}
	if _, _, _, ok := cpuRatios(prev, prev); ok {
		t.Fatal("no elapsed ticks must yield no readings")
	}
	regressed := cpuTimes{idle: 850, total: 1100, iowait: 30, steal: 4, hasIowait: true, hasSteal: true}
	if _, _, _, ok := cpuRatios(prev, regressed); ok {
		t.Fatal("a steal regression must drop busy and iowait as well")
	}
}

func TestParseMeminfoKBToBytes(t *testing.T) {
	in := "MemTotal:       2048 kB\nMemFree:        100 kB\nMemAvailable:   1024 kB\nSwapTotal:      512 kB\nSwapFree:       256 kB\n"
	m, err := parseMeminfo(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if m.total != 2048*1024 || m.available != 1024*1024 || m.swapTotal != 512*1024 || m.swapFree != 256*1024 {
		t.Fatalf("%+v", m)
	}
}

func TestParseLoadavg(t *testing.T) {
	l, err := parseLoadavg(strings.NewReader("0.52 0.31 0.20 3/721 12345\n"))
	if err != nil {
		t.Fatal(err)
	}
	if l.l1 != 0.52 || l.l5 != 0.31 || l.l15 != 0.20 {
		t.Fatalf("%+v", l)
	}
}

func TestParseSockstatBothFamilies(t *testing.T) {
	tcp, udp, err := parseSockstat(strings.NewReader("sockets: used 5\nTCP: inuse 3 orphan 0 tw 0 alloc 1 mem 0\nUDP: inuse 2 mem 1\n"))
	if err != nil || tcp != 3 || udp != 2 {
		t.Fatalf("v4: %d %d %v", tcp, udp, err)
	}
	tcp, udp, err = parseSockstat(strings.NewReader("TCP6: inuse 4\nUDP6: inuse 1\nUDPLITE6: inuse 0\n"))
	if err != nil || tcp != 4 || udp != 1 {
		t.Fatalf("v6: %d %d %v", tcp, udp, err)
	}
}

func TestParseCPUInfo(t *testing.T) {
	in := "processor\t: 0\nmodel name\t: Fancy CPU @ 3.0GHz\n\nprocessor\t: 1\nmodel name\t: Fancy CPU @ 3.0GHz\n"
	c := parseCPUInfo(strings.NewReader(in))
	if c.model != "Fancy CPU @ 3.0GHz" || c.cores != 2 {
		t.Fatalf("%+v", c)
	}
}

func TestParseOSReleasePrettyName(t *testing.T) {
	in := "NAME=\"Debian GNU/Linux\"\nPRETTY_NAME=\"Debian GNU/Linux 12 (bookworm)\"\nID=debian\n"
	if got := parseOSRelease(strings.NewReader(in)); got != "Debian GNU/Linux 12 (bookworm)" {
		t.Fatalf("%q", got)
	}
}

func TestInterfaceFilterDefaults(t *testing.T) {
	c := &Collector{Host: &ProcFS{}}
	for _, n := range []string{"lo", "docker0", "veth1234", "br-abc", "virbr0", "tap100i0", "cali1234", "lxc1234", "cilium_host", "zt1234", "ifb0"} {
		if c.includeIface(n) {
			t.Fatalf("%s must be excluded by default", n)
		}
	}
	for _, n := range []string{"eth0", "ens3", "wlan0"} {
		if !c.includeIface(n) {
			t.Fatalf("%s must be included by default", n)
		}
	}
	c = &Collector{Host: &ProcFS{}, NetInclude: []string{"eth*"}}
	if c.includeIface("ens3") || !c.includeIface("eth1") {
		t.Fatal("explicit include list must be exclusive")
	}
}
