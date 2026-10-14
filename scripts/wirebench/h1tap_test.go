package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/agentwire"
)

const secretToken = "heron_nt_wirebench-test-secret-must-not-leak"

// TestHTTPObserverDecodesReportsWithoutLeakingHeaders 经中继发两次 Report（一次带 Facts，一次不带），hub 的替身
// 回带清单的应答。核对：
//   - 每个解码出的字段字节加起来等于正文长度，Facts 的字节等于独立编码出的值；
//   - 各次请求、应答在这一跳上的字节（头、正文、分块框架）加起来等于中继在该方向计到的字节；
//   - 事件日志里没有请求头里的 token。
func TestHTTPObserverDecodesReportsWithoutLeakingHeaders(t *testing.T) {
	reply := &heronv1.ReportResponse{ReportIntervalMs: 10000, WantFacts: true, WantUpdate: true,
		Tasks: &heronv1.ProbeTasks{Version: 3, Tasks: []*heronv1.ProbeTask{
			{Id: 1, Kind: heronv1.ProbeKind_PROBE_KIND_TCP, Target: "172.30.0.10:18600", IntervalS: 5, TimeoutMs: 1000, ConfigId: bytes.Repeat([]byte{7}, 16)},
			{Id: 2, Kind: heronv1.ProbeKind_PROBE_KIND_HTTP, Target: "https://hub.wirebench.test:18601/healthz", IntervalS: 5, TimeoutMs: 1000, ConfigId: bytes.Repeat([]byte{8}, 16)},
		}}}
	replyBody, err := proto.Marshal(reply)
	if err != nil {
		t.Fatal(err)
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(replyBody); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	// 第二次应答以 gzip 回：反代自己向上游声明压缩时，hub 在这一跳上回的就是这种形状。
	served := 0
	mux := http.NewServeMux()
	mux.HandleFunc(reportPath, func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/proto")
		body := replyBody
		if served == 1 {
			w.Header().Set("Content-Encoding", "gzip")
			body = gz.Bytes()
		}
		served++
		// 先 Flush 再写正文，迫使服务端改用分块编码：分块框架的字节也要落进 RespWire。
		w.(http.Flusher).Flush()
		_, _ = w.Write(body)
	})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux}
	go srv.Serve(l)
	defer srv.Close()
	var raw *rawObserver
	addr, stop, logBytes := startRelay(t, l.Addr().String(), func(log *EventLog) Observer {
		raw = &rawObserver{inner: &HTTPObserver{Log: log}}
		return raw
	})

	facts := &heronv1.Facts{Hostname: "wirebench-agent", Os: "Debian GNU/Linux 12 (bookworm)", Kernel: "7.0.14", Arch: "arm64", CpuCores: 16,
		Diagnostics: &heronv1.AgentDiagnostics{NetInterfaces: []string{"eth0"}, NetInterfacesTotal: 1, ReportIntervalMs: 10000}}
	reqs := []*heronv1.ReportRequest{
		{Metrics: &heronv1.Metrics{BootId: "8d2b5f8e-0c58-4a9e-9a43-5b8b1b0f2b1c", CpuPct: proto.Float64(3.5), MemTotal: proto.Uint64(16 << 30)}, FactsHash: 0x1234567890abcdef, Facts: facts,
			Update: &heronv1.UpdateStatus{Version: "v0.11.1", Reason: "local updater unavailable"}, UpdateHash: proto.Uint64(0x0fedcba987654321)},
		{Metrics: &heronv1.Metrics{BootId: "8d2b5f8e-0c58-4a9e-9a43-5b8b1b0f2b1c", CpuPct: proto.Float64(1.25)}, FactsHash: 0x1234567890abcdef, UpdateHash: proto.Uint64(0), TasksDigest: bytes.Repeat([]byte{1}, 32),
			ProbeResults: []*heronv1.ProbeResult{{TaskId: 1, AgeMs: 1200, Outcome: &heronv1.ProbeResult_RttUs{RttUs: 812}, TaskConfigId: bytes.Repeat([]byte{7}, 16)}},
			// 只带逐网卡列表：合计与摘要按 hub 的补齐取推导值。
			NetInterfaces: []*heronv1.NetInterfaceCounter{{Name: "eth0", RxBytes: 1 << 40, TxBytes: 300}, {Name: "eth1", RxBytes: 5, TxBytes: 7}}},
	}
	// 解析 goroutine 被推迟到整次往返结束之后才开始：ReqT 仍须落在请求发出与应答收到之间。
	parseDelay = 300 * time.Millisecond
	defer func() { parseDelay = 0 }()
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	var sent, answered []int64
	for _, m := range reqs {
		body, err := proto.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequest(http.MethodPost, "http://"+addr+reportPath, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/proto")
		req.Header.Set("Authorization", "Bearer "+secretToken)
		sent = append(sent, time.Now().UnixNano())
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		answered = append(answered, time.Now().UnixNano())
		if _, err := io.ReadAll(resp.Body); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	client.CloseIdleConnections()
	events := stop()

	var got []*HTTPRecord
	var dataUp, dataDown int64
	for _, e := range events {
		switch e.Ev {
		case "http":
			got = append(got, e.HTTP)
		case "data":
			if e.Dir == dirUp {
				dataUp += e.N
			} else {
				dataDown += e.N
			}
		}
	}
	if len(got) != len(reqs) {
		t.Fatalf("got %d http events, want %d", len(got), len(reqs))
	}
	var wireUp, wireDown int64
	for i, h := range got {
		if h.Err != "" || h.Report == nil || h.Reply == nil {
			t.Fatalf("request %d: %+v", i, h)
		}
		if h.ReqT < sent[i] || h.ReqT > answered[i] {
			t.Errorf("request %d: ReqT %d is outside [sent %d, answered %d]", i, h.ReqT, sent[i], answered[i])
		}
		r := h.Report
		if want := proto.Size(reqs[i]); h.ReqBody != want || r.Body != want {
			t.Errorf("request %d: body %d/%d, want %d", i, h.ReqBody, r.Body, want)
		}
		if sum := r.Metrics + r.ProbeBytes + r.FactsHash + r.Facts + r.Update + r.TasksDigest + r.UpdateHash + r.NetInterfaces; sum != r.Body {
			t.Errorf("request %d: fields add up to %d, body is %d: %+v", i, sum, r.Body, r)
		}
		wantFacts := 0
		if reqs[i].Facts != nil {
			wantFacts = fieldSize(5, proto.Size(reqs[i].Facts))
		}
		if r.Facts != wantFacts || r.ProbeResults != len(reqs[i].ProbeResults) {
			t.Errorf("request %d: facts %d results %d, want %d and %d", i, r.Facts, r.ProbeResults, wantFacts, len(reqs[i].ProbeResults))
		}
		// 两次请求的 metrics 都不带合计与摘要（第二次只带列表）。
		if r.NetTotals != 0 {
			t.Errorf("request %d: net totals %d bytes, want 0", i, r.NetTotals)
		}
		wantNics := 0
		for _, c := range reqs[i].NetInterfaces {
			wantNics += fieldSize(10, proto.Size(c))
		}
		if r.NetInterfaces != wantNics || r.NetInterfaceCount != len(reqs[i].NetInterfaces) {
			t.Errorf("request %d: net_interfaces %d bytes in %d entries, want %d in %d", i, r.NetInterfaces, r.NetInterfaceCount, wantNics, len(reqs[i].NetInterfaces))
		}
		hopBody := len(replyBody)
		if i == 1 {
			hopBody = gz.Len()
		}
		if h.RespBody != hopBody || h.Reply.Body != len(replyBody) || h.Reply.TaskCount != 2 || !h.Reply.WantFacts {
			t.Errorf("request %d: reply %+v, resp body %d, want hop body %d and message %d", i, h.Reply, h.RespBody, hopBody, len(replyBody))
		}
		if sum := h.Reply.Interval + h.Reply.Tasks + 2 /* want_facts: 标签 + 1 字节 */ + h.Reply.Detection + h.Reply.Update + 2; /* want_update */ sum != h.Reply.Body {
			t.Errorf("request %d: reply fields add up to %d, body is %d", i, sum, h.Reply.Body)
		}
		if !h.Reply.WantUpdate {
			t.Errorf("request %d: reply want_update %v", i, h.Reply.WantUpdate)
		}
		// update_hash 的有无只看是否给出：两次请求都给了（第二次取值 0），各占标签 + 8 字节。
		if r.UpdateHash != 9 {
			t.Errorf("request %d: update_hash %d bytes, want 9", i, r.UpdateHash)
		}
		// 客户端按 Content-Length 发正文（bytes.Reader），请求在这一跳上只有头与正文。
		if h.ReqHead+h.ReqBody != h.ReqWire || h.ReqHead <= 0 {
			t.Errorf("request %d: head %d + body %d != wire %d", i, h.ReqHead, h.ReqBody, h.ReqWire)
		}
		if h.RespWire <= h.RespHead+h.RespBody {
			t.Errorf("request %d: chunked response wire %d is not larger than head %d + body %d", i, h.RespWire, h.RespHead, h.RespBody)
		}
		wireUp += int64(h.ReqWire)
		wireDown += int64(h.RespWire)
	}
	if got[0].Counters != nil {
		t.Errorf("request without counters recorded %+v", got[0].Counters)
	}
	sum := agentwire.NetInterfaceSummary(reqs[1].NetInterfaces)
	if c := got[1].Counters; c == nil || c.RxTotal == nil || *c.RxTotal != sum.Rx || c.TxTotal == nil || *c.TxTotal != sum.Tx || c.Epoch != sum.Epoch ||
		!slices.Equal(c.Interfaces, []NicCounter{{"eth0", 1 << 40, 300}, {"eth1", 5, 7}}) {
		t.Errorf("list-only counters = %+v, want the derived totals %+v and both interfaces", c, sum)
	}
	if wireUp != dataUp || wireDown != dataDown {
		t.Errorf("parsed messages add up to %d/%d bytes, relay forwarded %d/%d", wireUp, wireDown, dataUp, dataDown)
	}
	// 先确认 token 确实出现在中继转发的字节里（否则"日志里没有"什么也证明不了），再确认日志里没有它。
	if !bytes.Contains(raw.up, []byte(secretToken)) {
		t.Fatal("the token never crossed the relay; the check below would prove nothing")
	}
	if bytes.Contains(logBytes(), []byte(secretToken)) {
		t.Fatal("event log contains the bearer token")
	}
}

// slowWriter 让每次写事件日志都慢下来：负载高时写日志可以比一次往返还慢。
type slowWriter struct{ syncBuffer }

func (w *slowWriter) Write(p []byte) (int, error) {
	time.Sleep(100 * time.Millisecond)
	return w.syncBuffer.Write(p)
}

// TestRequestTimeIsTakenBeforeForwarding：中继转发之后才写日志、才交给观察器。写日志慢到应答已经绕回客户端时，
// 请求的时刻仍须落在请求发出与应答收到之间——它取的是读到字节的时刻，不是观察器被调用的时刻。
func TestRequestTimeIsTakenBeforeForwarding(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(reportPath, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/proto")
		_, _ = w.Write([]byte{0x08, 0x10})
	})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux}
	go srv.Serve(l)
	defer srv.Close()
	var out slowWriter
	log := NewEventLog(&out)
	o := &HTTPObserver{Log: log}
	r := &Relay{Target: l.Addr().String(), Log: log, NewObserver: func(uint64) Observer { return o }}
	rl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- r.Serve(rl) }()
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	body, err := proto.Marshal(&heronv1.ReportRequest{FactsHash: 1})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+rl.Addr().String()+reportPath, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/proto")
	sent := time.Now().UnixNano()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	answered := time.Now().UnixNano()
	client.CloseIdleConnections()
	rl.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	events, err := ReadEvents(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	var got []*HTTPRecord
	for _, e := range events {
		if e.Ev == "http" {
			got = append(got, e.HTTP)
		}
	}
	if len(got) != 1 {
		t.Fatalf("got %d http events, want 1", len(got))
	}
	if got[0].ReqT < sent || got[0].ReqT > answered {
		t.Fatalf("ReqT %d is outside [sent %d, answered %d] (answered %v after sending)", got[0].ReqT, sent, answered, time.Duration(answered-sent))
	}
}

// TestDecodeReportMeasuresNetTotals：metrics 里合计与摘要占的字节单独记下，且只是 metrics 的一部分。数值按编码规则
// 手算：2^40 是 6 字节 varint、300 是 2 字节，各带 1 字节标签；64 字节摘要带 2 字节标签（字段 20）与 1 字节长度。
func TestDecodeReportMeasuresNetTotals(t *testing.T) {
	m := &heronv1.Metrics{BootId: "8d2b5f8e-0c58-4a9e-9a43-5b8b1b0f2b1c", CpuPct: proto.Float64(1),
		NetRxTotal: proto.Uint64(1 << 40), NetTxTotal: proto.Uint64(300), NetCounterEpoch: strings.Repeat("a", 64)}
	body, err := proto.Marshal(&heronv1.ReportRequest{Metrics: m})
	if err != nil {
		t.Fatal(err)
	}
	var rec HTTPRecord
	decodeReport(body, &rec)
	if r := rec.Report; r == nil || r.NetTotals != 7+3+67 || r.Metrics != len(body) {
		t.Fatalf("report info %+v, want net totals 77 within metrics %d", r, len(body))
	}
}
