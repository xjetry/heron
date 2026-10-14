package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// TestExchangesAlignWithInnerRequests 用合成日志钉住分析的三步：按空闲间隔切段、内侧请求按时间配给外侧的段、
// 段的分类；close 合计与逐段相加不一致时报表必须标出来。
func TestExchangesAlignWithInnerRequests(t *testing.T) {
	ms := int64(time.Millisecond)
	outer := []Event{
		{T: 0, Ev: "open", Conn: 1, Peer: "10.0.0.2:5000"},
		{T: 1 * ms, Ev: "data", Conn: 1, Dir: dirUp, N: 1500},
		{T: 3 * ms, Ev: "data", Conn: 1, Dir: dirDown, N: 2300},
		{T: 4 * ms, Ev: "data", Conn: 1, Dir: dirUp, N: 600},
		{T: 9 * ms, Ev: "data", Conn: 1, Dir: dirDown, N: 140},
		// 10 秒后的第二次上报：带 Facts。
		{T: 10000 * ms, Ev: "data", Conn: 1, Dir: dirUp, N: 650},
		{T: 10006 * ms, Ev: "data", Conn: 1, Dir: dirDown, N: 127},
		// 20 秒后的第三次上报：应答带清单。
		{T: 20000 * ms, Ev: "data", Conn: 1, Dir: dirUp, N: 440},
		{T: 20005 * ms, Ev: "data", Conn: 1, Dir: dirDown, N: 512},
		{T: 30000 * ms, Ev: "close", Conn: 1, Up: 3190, Down: 3079},
	}
	inner := []Event{
		// 落在任何一段之外的请求：报表要列为未配上，而不是硬塞给还空着的哪一段。排在最前，配对若不看时间就会抢走第一段。
		{Ev: "http", HTTP: &HTTPRecord{Path: reportPath, ReqT: 15000 * ms, Report: &ReportInfo{}}},
		{Ev: "http", HTTP: &HTTPRecord{Path: reportPath, ReqT: 5 * ms, Report: &ReportInfo{Facts: 200}}},
		{Ev: "http", HTTP: &HTTPRecord{Path: reportPath, ReqT: 10003 * ms, Report: &ReportInfo{Facts: 210}}},
		{Ev: "http", HTTP: &HTTPRecord{Path: reportPath, ReqT: 20002 * ms, Report: &ReportInfo{}, Reply: &ReplyInfo{Tasks: 385}}},
	}
	conns := buildConns(outer, time.Second)
	if len(conns) != 1 || len(conns[0].Exchanges) != 3 {
		t.Fatalf("want 1 connection with 3 exchanges, got %+v", conns)
	}
	want := [][2]int64{{2100, 2440}, {650, 127}, {440, 512}}
	for i, ex := range conns[0].Exchanges {
		if ex.Up != want[i][0] || ex.Down != want[i][1] {
			t.Errorf("exchange %d: up/down %d/%d, want %d/%d", i, ex.Up, ex.Down, want[i][0], want[i][1])
		}
	}
	unmatched := matchHTTP(conns, inner)
	if len(unmatched) != 1 || unmatched[0].ReqT != 15000*ms {
		t.Errorf("unmatched = %+v, want only the request at 15 s", unmatched)
	}
	var classes []string
	for _, ex := range conns[0].Exchanges {
		classes = append(classes, classify(ex))
	}
	if got := strings.Join(classes, ","); got != "new-connection,facts,tasks" {
		t.Errorf("classes = %s", got)
	}
	var buf bytes.Buffer
	report(&buf, conns, unmatched, 500)
	if strings.Contains(buf.String(), "MISMATCH") {
		t.Errorf("report flags a mismatch for consistent totals:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "1 inner requests matched no outer exchange") {
		t.Errorf("report does not list the unmatched request:\n%s", buf.String())
	}
	// close 合计多出一段没有落进任何往返的字节：报表必须标出。
	conns[0].Up += 7
	buf.Reset()
	report(&buf, conns, nil, 500)
	if !strings.Contains(buf.String(), "MISMATCH") {
		t.Errorf("report does not flag close totals that differ from the exchanges:\n%s", buf.String())
	}
}

// TestReportCountsUpdateCarriage：稳态里 update 全文只在被索要的那一次出现时，字段表要按实际出现计次，不能写成
// 每个请求都带；metrics 里的合计与摘要同样按实际出现计次；应答表列出 want_update 的次数与声明的能力集合。
func TestReportCountsUpdateCarriage(t *testing.T) {
	ms := int64(time.Millisecond)
	outer := []Event{{T: 0, Ev: "open", Conn: 1, Peer: "10.0.0.2:5000"}}
	var inner []Event
	for i := range 4 {
		at := int64(i) * 10000 * ms
		outer = append(outer, Event{T: at + ms, Ev: "data", Conn: 1, Dir: dirUp, N: 400}, Event{T: at + 3*ms, Ev: "data", Conn: 1, Dir: dirDown, N: 60})
		r := &ReportInfo{Body: 300, UpdateHash: 9}
		// 第 1 段带合计与摘要（例如超出列表上限），其余只带列表。
		if i == 1 {
			r.NetTotals = 77
		}
		reply := &ReplyInfo{Body: 20}
		var update *UpdateInfo
		switch i {
		case 2:
			reply.WantUpdate = true
		case 3:
			r.Update = 90
			update = &UpdateInfo{Reason: "container"}
		}
		inner = append(inner, Event{Ev: "http", HTTP: &HTTPRecord{Path: reportPath, ReqT: at + 2*ms, Report: r, Reply: reply, Update: update}})
	}
	outer = append(outer, Event{T: 40000 * ms, Ev: "close", Conn: 1, Up: 1600, Down: 240})
	conns := buildConns(outer, time.Second)
	if unmatched := matchHTTP(conns, inner); len(unmatched) != 0 {
		t.Fatalf("unmatched %+v", unmatched)
	}
	for _, ex := range conns[0].Exchanges {
		ex.Phase, ex.Class = phaseAt(nil, ex.Start), classify(ex)
	}
	var buf bytes.Buffer
	report(&buf, conns, nil, 500)
	out := buf.String()
	// 第 0 段是新连接，稳态是其余三段：其中一段带全文，三段都带摘要，一段带合计；应答里一次 want_update。
	for _, want := range []string{
		"| - | 300 | 0 | 0 | 0 | 0 | 0 | 9 | 0 | 0 | 1/3 |",
		"| - | 20 | 0 | 0 | 0 | 0 | 0/3 | 1/3 |",
		"update status (90 bytes; carried by 1 of 4 report requests)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
}

// TestTrafficReportAlignsReportsWithHubSnapshots 钉住 hub 快照与上报的对齐：每一行按"快照之前最后一次被接受的上报"
// 取 agent 报的合计，第一次上报只是基线；窗口里新出现的网卡单独列出它首次出现时已有的计数，差额减去它就是 hub
// 没记上的字节。被拒的上报不算。
func TestTrafficReportAlignsReportsWithHubSnapshots(t *testing.T) {
	sec := int64(time.Second)
	u := func(v uint64) *uint64 { return &v }
	rep := func(at int64, status int, rx, tx uint64, nics ...NicCounter) Event {
		return Event{Ev: "http", HTTP: &HTTPRecord{Path: reportPath, Status: status, ReqT: at, Report: &ReportInfo{},
			Counters: &CounterInfo{RxTotal: u(rx), TxTotal: u(tx), Interfaces: nics}}}
	}
	tap := []Event{
		rep(21*sec, 200, 165, 137, NicCounter{"eth0", 160, 130}, NicCounter{"eth1", 5, 7}),
		rep(1*sec, 200, 100, 100, NicCounter{"eth0", 100, 100}),
		rep(11*sec, 200, 150, 120, NicCounter{"eth0", 150, 120}),
		rep(25*sec, 400, 999999, 999999, NicCounter{"eth0", 999999, 999999}),
		rep(31*sec, 200, 176, 148, NicCounter{"eth0", 170, 140}, NicCounter{"eth1", 6, 8}),
	}
	snaps, err := readTrafficSnapshots(strings.NewReader("500000000 T0 0 0\n15000000000 nic-T0 50 20\n\n40000000000 end 71 41\n"))
	if err != nil || len(snaps) != 3 {
		t.Fatalf("snapshots %+v %v", snaps, err)
	}
	var buf bytes.Buffer
	trafficReport(&buf, tap, snaps)
	for _, row := range []string{
		"| T0 | nic-T0 | 1 | 50 | 50 | 0 | 20 | 20 | 0 | - |",
		"| nic-T0 | end | 2 | 26 | 21 | 5 | 28 | 21 | 7 | eth1 5/7 |",
	} {
		if !strings.Contains(buf.String(), row) {
			t.Errorf("traffic report lacks %q:\n%s", row, buf.String())
		}
	}
	if _, err := readTrafficSnapshots(strings.NewReader("1 T0 5\n")); err == nil {
		t.Error("short traffic line accepted")
	}
}
