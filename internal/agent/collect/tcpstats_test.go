package collect

import (
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/clock"
)

const snmpHeader = "Tcp: RtoAlgorithm RtoMin RtoMax MaxConn ActiveOpens PassiveOpens AttemptFails EstabResets CurrEstab InSegs OutSegs RetransSegs InErrs OutRsts InCsumErrors\n"

func snmpTCP(active, passive, out, retrans string) string {
	return "Ip: Forwarding DefaultTTL\nIp: 1 64\n" + snmpHeader + "Tcp: 1 200 120000 -1 " + active + " " + passive + " 0 0 0 490 " + out + " " + retrans + " 0 2 0\n"
}

func TestParseSNMPTCPFromRealSnapshot(t *testing.T) {
	f, err := os.Open("testdata/docker-debian/proc/net/snmp")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := parseSNMPTCP(f); err != nil {
		t.Fatalf("real snapshot: %v", err)
	}
}

// 按表头名取列：四列各取到自己的值，MaxConn 的 -1 不在四列之内、不影响解析。reordered 保留内核的全部 15 列，
// 只把 ActiveOpens 与 RetransSegs、PassiveOpens 与 OutSegs 的位置对调：按固定位置（第 5–6、11–12 列）取值的
// 实现即使保留了列数与缺列检查，也会读到互换后的合法数值而不是越界，所以这一例是"按名取列"的凭据。
func TestParseSNMPTCPByHeaderName(t *testing.T) {
	want := tcpCounters{activeOpens: 11, passiveOpens: 22, outSegs: 33, retransSegs: 44}
	for name, text := range map[string]string{
		"kernel order": snmpTCP("11", "22", "33", "44"),
		"reordered": "Tcp: RtoAlgorithm RtoMin RtoMax MaxConn RetransSegs OutSegs AttemptFails EstabResets CurrEstab InSegs PassiveOpens ActiveOpens InErrs OutRsts InCsumErrors\n" +
			"Tcp: 1 200 120000 -1 44 33 0 0 0 490 22 11 0 2 0\n",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := parseSNMPTCP(strings.NewReader(text))
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("got %+v, want %+v", got, want)
			}
		})
	}
}

func TestParseSNMPTCPRejectsIncompleteInput(t *testing.T) {
	for name, text := range map[string]string{
		"no tcp lines":    "Ip: Forwarding\nIp: 1\n",
		"header only":     snmpHeader,
		"length mismatch": snmpHeader + "Tcp: 1 200\n",
		"missing column":  "Tcp: ActiveOpens PassiveOpens OutSegs\nTcp: 1 2 3\n",
		"not a number":    strings.Replace(snmpTCP("11", "22", "33", "44"), " 33 ", " x ", 1),
		"negative":        strings.Replace(snmpTCP("11", "22", "33", "44"), " 11 ", " -11 ", 1),
	} {
		if _, err := parseSNMPTCP(strings.NewReader(text)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func tcpProc(text string) fstest.MapFS {
	return fstest.MapFS{"proc/net/snmp": {Data: []byte(text)}}
}

// 首样本没有读数；第二次按单调钟间隔出四项速率；任一计数倒退时四项一起缺失并换基线，再下一次照常出读数。
func TestTCPRatesNeedTwoSamplesAndRebaselineOnDecrease(t *testing.T) {
	fsys := tcpProc(snmpTCP("100", "200", "1000", "10"))
	clk := clock.NewFake(time.Unix(0, 0))
	c := &Collector{Host: hostProc(fsys, func() (uint64, uint64, error) { return 0, 0, nil }), Clock: clk}
	if m, _ := c.Metrics(c.Identify()); m.TcpActiveOpensPs != nil || m.TcpOutSegsPs != nil {
		t.Fatal("first sample must not carry tcp rates")
	}
	fsys["proc/net/snmp"] = &fstest.MapFile{Data: []byte(snmpTCP("120", "260", "1400", "30"))}
	clk.Advance(4 * time.Second)
	m, _ := c.Metrics(c.Identify())
	if m.GetTcpActiveOpensPs() != 5 || m.GetTcpPassiveOpensPs() != 15 || m.GetTcpOutSegsPs() != 100 || m.GetTcpRetransSegsPs() != 5 {
		t.Fatalf("rates = %v %v %v %v, want 5 15 100 5", m.GetTcpActiveOpensPs(), m.GetTcpPassiveOpensPs(), m.GetTcpOutSegsPs(), m.GetTcpRetransSegsPs())
	}
	fsys["proc/net/snmp"] = &fstest.MapFile{Data: []byte(snmpTCP("1", "260", "1500", "30"))}
	clk.Advance(4 * time.Second)
	if m, _ := c.Metrics(c.Identify()); m.TcpActiveOpensPs != nil || m.TcpPassiveOpensPs != nil || m.TcpOutSegsPs != nil || m.TcpRetransSegsPs != nil {
		t.Fatal("a decreasing counter must leave all four rates unset")
	}
	fsys["proc/net/snmp"] = &fstest.MapFile{Data: []byte(snmpTCP("9", "260", "1500", "30"))}
	clk.Advance(4 * time.Second)
	if m, _ := c.Metrics(c.Identify()); m.GetTcpActiveOpensPs() != 2 || m.TcpOutSegsPs == nil || m.GetTcpOutSegsPs() != 0 {
		t.Fatalf("after rebaseline: active=%v out=%v, want 2 and a measured 0", m.GetTcpActiveOpensPs(), m.GetTcpOutSegsPs())
	}
	// 只有重传段倒退：四项同样一起缺失，不单独保留其余三项。
	fsys["proc/net/snmp"] = &fstest.MapFile{Data: []byte(snmpTCP("13", "270", "1600", "5"))}
	clk.Advance(4 * time.Second)
	if m, _ := c.Metrics(c.Identify()); m.TcpActiveOpensPs != nil || m.TcpRetransSegsPs != nil {
		t.Fatal("a decreasing retrans counter must leave all four rates unset")
	}
}

// 休眠信号作废 TCP 基线：跨休眠的差分分母不含休眠时长。
func TestResetRatesDropsTCPBaseline(t *testing.T) {
	fsys := tcpProc(snmpTCP("100", "200", "1000", "10"))
	clk := clock.NewFake(time.Unix(0, 0))
	c := &Collector{Host: hostProc(fsys, func() (uint64, uint64, error) { return 0, 0, nil }), Clock: clk}
	c.Metrics(c.Identify())
	fsys["proc/net/snmp"] = &fstest.MapFile{Data: []byte(snmpTCP("200", "300", "2000", "20"))}
	clk.Advance(4 * time.Second)
	c.ResetRates()
	if m, _ := c.Metrics(c.Identify()); m.TcpActiveOpensPs != nil {
		t.Fatal("ResetRates must drop the tcp baseline")
	}
}

// 四个计数中任何一个单独倒退，四项都缺失并换基线；下一次以新基线出读数。
func TestTCPRatesRebaselineWhenAnySingleCounterDecreases(t *testing.T) {
	base := [4]string{"100", "200", "1000", "10"}
	for i, name := range []string{"active", "passive", "out", "retrans"} {
		t.Run(name, func(t *testing.T) {
			fsys := tcpProc(snmpTCP(base[0], base[1], base[2], base[3]))
			clk := clock.NewFake(time.Unix(0, 0))
			c := &Collector{Host: hostProc(fsys, func() (uint64, uint64, error) { return 0, 0, nil }), Clock: clk}
			c.Metrics(c.Identify())
			dec := base
			dec[i] = "5"
			fsys["proc/net/snmp"] = &fstest.MapFile{Data: []byte(snmpTCP(dec[0], dec[1], dec[2], dec[3]))}
			clk.Advance(4 * time.Second)
			if m, _ := c.Metrics(c.Identify()); m == nil || m.TcpActiveOpensPs != nil || m.TcpPassiveOpensPs != nil || m.TcpOutSegsPs != nil || m.TcpRetransSegsPs != nil {
				t.Fatal("a decreasing counter must leave all four rates unset")
			}
			next := dec
			next[i] = "25"
			fsys["proc/net/snmp"] = &fstest.MapFile{Data: []byte(snmpTCP(next[0], next[1], next[2], next[3]))}
			clk.Advance(4 * time.Second)
			m, _ := c.Metrics(c.Identify())
			got := []*float64{m.TcpActiveOpensPs, m.TcpPassiveOpensPs, m.TcpOutSegsPs, m.TcpRetransSegsPs}
			if got[i] == nil || *got[i] != 5 {
				t.Fatalf("after rebaseline %s = %v, want (25-5)/4 = 5", name, got[i])
			}
		})
	}
}

// 单调钟没有前进（同一时刻两次采样）：间隔不是正数，四项缺失，不除以零。
func TestTCPRatesNeedAPositiveInterval(t *testing.T) {
	fsys := tcpProc(snmpTCP("100", "200", "1000", "10"))
	c := &Collector{Host: hostProc(fsys, func() (uint64, uint64, error) { return 0, 0, nil }), Clock: clock.NewFake(time.Unix(0, 0))}
	c.Metrics(c.Identify())
	fsys["proc/net/snmp"] = &fstest.MapFile{Data: []byte(snmpTCP("110", "210", "1100", "11"))}
	if m, _ := c.Metrics(c.Identify()); m == nil || m.TcpActiveOpensPs != nil || m.TcpOutSegsPs != nil {
		t.Fatal("a non-positive interval must leave the rates unset")
	}
}

// 读不出的一次四项缺失并记 TCP_STATS 失败，但保留旧基线：下一次成功读数的差分覆盖两个间隔，速率仍正确。
func TestTCPRatesKeepBaselineAcrossReadFailure(t *testing.T) {
	fsys := tcpProc(snmpTCP("100", "200", "1000", "10"))
	clk := clock.NewFake(time.Unix(0, 0))
	c := &Collector{Host: hostProc(fsys, func() (uint64, uint64, error) { return 0, 0, nil }), Clock: clk}
	c.Metrics(c.Identify())
	delete(fsys, "proc/net/snmp")
	clk.Advance(4 * time.Second)
	if m, err := c.Metrics(c.Identify()); m == nil || m.TcpActiveOpensPs != nil || err == nil || !strings.Contains(err.Error(), "tcp_stats") {
		t.Fatalf("unreadable snmp: metrics %v err %v, want rates unset and a tcp_stats failure", m, err)
	}
	fsys["proc/net/snmp"] = &fstest.MapFile{Data: []byte(snmpTCP("180", "200", "1800", "10"))}
	clk.Advance(4 * time.Second)
	if m, _ := c.Metrics(c.Identify()); m.GetTcpActiveOpensPs() != 10 || m.GetTcpOutSegsPs() != 100 {
		t.Fatalf("after recovery active=%v out=%v, want 80/8 = 10 and 800/8 = 100", m.GetTcpActiveOpensPs(), m.GetTcpOutSegsPs())
	}
}

// 文件读不出：四项缺失并记 TCP_STATS 失败；平台不提供（errNoTCPStats）：四项缺失、不记失败。
func TestTCPStatsFailureCategory(t *testing.T) {
	c := &Collector{Host: hostProc(fstest.MapFS{}, func() (uint64, uint64, error) { return 0, 0, nil }), Clock: clock.NewFake(time.Unix(0, 0))}
	_, err := c.Metrics(c.Identify())
	if err == nil || !strings.Contains(err.Error(), "tcp_stats") {
		t.Fatalf("err = %v, want the tcp_stats failure named", err)
	}
	if !containsComponent(c.Facts(c.Identify()).GetDiagnostics().GetFailedCollectors(), heronv1.CollectionComponent_COLLECTION_COMPONENT_TCP_STATS) {
		t.Fatal("missing /proc/net/snmp must be reported as a TCP_STATS failure")
	}
	h := &noTCPHost{ifacesOnly: ifacesOnly{list: []ifaceCounters{{"eth0", 1, 1}}}}
	c = &Collector{Host: h, Clock: clock.NewFake(time.Unix(0, 0))}
	c.Metrics(c.Identify())
	if containsComponent(c.Facts(c.Identify()).GetDiagnostics().GetFailedCollectors(), heronv1.CollectionComponent_COLLECTION_COMPONENT_TCP_STATS) {
		t.Fatal("a platform without tcp stats must not report a failure")
	}
}

type noTCPHost struct{ ifacesOnly }

func (h *noTCPHost) tcpStats() (tcpCounters, error) { return tcpCounters{}, errNoTCPStats }

func containsComponent(parts []heronv1.CollectionComponent, want heronv1.CollectionComponent) bool {
	for _, p := range parts {
		if p == want {
			return true
		}
	}
	return false
}
