package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/agentwire"
)

// HTTPRecord 是明文一侧（反代 → hub）的一次 HTTP/1.1 往返。只记长度与解码出的结构尺寸，不记任何头的值：
// 请求头里有节点 token，注册请求体里有注册 key，二者都不能进日志。
type HTTPRecord struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Proto  string `json:"proto"`
	// ReqHead、RespHead 是起始行加头部（含结尾空行）的字节数；ReqWire、RespWire 是消息在这一跳上的总字节，
	// 分块编码时含分块框架；ReqBody、RespBody 是去掉传输编码后的正文字节。
	ReqHead  int `json:"req_head"`
	ReqBody  int `json:"req_body"`
	ReqWire  int `json:"req_wire"`
	Status   int `json:"status"`
	RespHead int `json:"resp_head"`
	RespBody int `json:"resp_body"`
	RespWire int `json:"resp_wire"`
	// RespContentEncoding 是应答的 Content-Encoding 值（不含凭据，只有编码名）：agent 不声明压缩，非空即异常。
	RespContentEncoding string      `json:"resp_content_encoding,omitempty"`
	Report              *ReportInfo `json:"report,omitempty"`
	Reply               *ReplyInfo  `json:"reply,omitempty"`
	Err                 string      `json:"err,omitempty"`
	// ReqT 是中继读到请求最后一个字节的时刻（见 Observer），不是解析完成或入队的时刻：解析在自己的
	// goroutine 里异步进行，入队在中继写完日志之后，负载高时二者都可以落后于整次往返。读到的时刻在因果上
	// 晚于外侧这段往返的首字节（反代收到请求才转发）、早于它的末字节（hub 收到请求才应答），analyze 据此配对。
	ReqT     int64          `json:"req_t"`
	Facts    *FactsInfo     `json:"facts,omitempty"`
	Update   *UpdateInfo    `json:"update,omitempty"`
	Tasks    []TaskWireInfo `json:"tasks,omitempty"`
	Counters *CounterInfo   `json:"counters,omitempty"`
}

// ReportInfo 是 ReportRequest 各字段在编码里占的字节，含字段自身的标签与长度前缀；加起来等于 Body。
type ReportInfo struct {
	Body         int `json:"body"`
	Metrics      int `json:"metrics"`
	ProbeResults int `json:"probe_results"`
	ProbeBytes   int `json:"probe_bytes"`

	FactsHash int `json:"facts_hash"`
	Facts     int `json:"facts"`
	Update    int `json:"update"`

	TasksDigest int `json:"tasks_digest"`
	UpdateHash  int `json:"update_hash"`
	// NetInterfaces 是 net_interfaces 各项的编码字节（每项含字段 10 的标签与长度前缀），NetInterfaceCount 是项数；
	// 与 ProbeBytes、ProbeResults 一样，加起来等于 Body 的是字节而不是项数。
	NetInterfaces     int `json:"net_interfaces"`
	NetInterfaceCount int `json:"net_interface_count"`
	// NetTotals 是 metrics 里 net_rx_total、net_tx_total、net_counter_epoch 三项的编码字节（含各自的标签与长度前缀），
	// 已含在 Metrics 里、不另计入 Body。带列表时 agent 总是省略这三项（omitDerivedNetTotals：空列表无法推导，合计仍由 metrics 携带）。
	// hub 准入之后由列表覆盖合计与摘要（normalizeNetTotals），不核对接受到的原值。这一列就是同形样本对照里 metrics 的净变化。
	NetTotals int `json:"net_totals"`
}

// CounterInfo 是这次上报交给 hub 流量账本的计数，按 hub 准入之后看到的样子记：只带逐网卡列表时合计与摘要取
// agentwire.NetInterfaceSummary 的推导值，与 ingest 的补齐同一个函数。网卡名与计数都不是凭据。analyze 用它把
// "agent 报了多少"与"hub 记了多少"按同一组上报对齐。
type CounterInfo struct {
	RxTotal    *uint64      `json:"rx_total,omitempty"`
	TxTotal    *uint64      `json:"tx_total,omitempty"`
	Epoch      string       `json:"epoch,omitempty"`
	Interfaces []NicCounter `json:"interfaces,omitempty"`
}

type NicCounter struct {
	Name string `json:"name"`
	Rx   uint64 `json:"rx"`
	Tx   uint64 `json:"tx"`
}

// FactsInfo 记下随主机变化的 Facts 字段：字符串原值（主机名、系统、内核、CPU 型号、网卡名都不是凭据）与
// 三个子消息的编码字节。
type FactsInfo struct {
	Hostname       string   `json:"hostname"`
	OS             string   `json:"os"`
	Kernel         string   `json:"kernel"`
	Arch           string   `json:"arch"`
	Virtualization string   `json:"virtualization"`
	CPUModel       string   `json:"cpu_model"`
	AgentVersion   string   `json:"agent_version"`
	NetInterfaces  []string `json:"net_interfaces"`
	NetExclude     int      `json:"net_exclude"`
	Network        int      `json:"network"`
	Diagnostics    int      `json:"diagnostics"`
	Execution      int      `json:"execution"`
	IPv4State      string   `json:"ipv4_state"`
	IPv6State      string   `json:"ipv6_state"`
}

type UpdateInfo struct {
	Supported bool   `json:"supported"`
	Reason    string `json:"reason"`
	Version   string `json:"version"`
	Source    string `json:"source"`
}

// ReplyInfo 是 ReportResponse 各字段的编码字节；Body 是解压后的消息长度，即 agent 那一侧收到的正文。两个布尔量
// 为真时各占 2 字节（标签 + 1），为假时不编码。
type ReplyInfo struct {
	Body       int  `json:"body"`
	IntervalMs int  `json:"interval_ms"`
	Interval   int  `json:"interval"`
	Tasks      int  `json:"tasks"`
	TaskCount  int  `json:"task_count"`
	WantFacts  bool `json:"want_facts"`
	Detection  int  `json:"detection"`
	Update     int  `json:"update"`

	WantUpdate bool `json:"want_update"`
}

// TaskWireInfo 是下发清单里一个任务的编码字节与 target 长度：target 是清单里唯一由使用者决定长度的字段。
type TaskWireInfo struct {
	Bytes     int `json:"bytes"`
	TargetLen int `json:"target_len"`
}

const reportPath = "/heron.v1.AgentService/Report"

// HTTPObserver 在中继的明文一侧按 HTTP/1.1 解析每条连接两个方向的字节流。解析在自己的 goroutine 里进行，
// 与中继之间隔着无界队列：解析慢或解析失败都不能让中继停下来等它，否则观察本身就改变了被测的时序。
type HTTPObserver struct {
	Log *EventLog

	mu    sync.Mutex
	conns map[uint64]*httpConn
}

type httpConn struct {
	up, down *byteQueue
	done     sync.WaitGroup
}

func (o *HTTPObserver) conn(id uint64) *httpConn {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.conns == nil {
		o.conns = map[uint64]*httpConn{}
	}
	c, ok := o.conns[id]
	if ok {
		return c
	}
	c = &httpConn{up: newByteQueue(), down: newByteQueue()}
	o.conns[id] = c
	reqs := make(chan pendingRequest, 64)
	c.done.Add(2)
	go func() {
		defer c.done.Done()
		defer close(reqs)
		o.readRequests(id, c.up, reqs)
	}()
	go func() {
		defer c.done.Done()
		o.readResponses(id, c.down, reqs)
	}()
	return c
}

func (o *HTTPObserver) Data(conn uint64, dir string, b []byte, read int64) {
	c := o.conn(conn)
	if dir == dirUp {
		c.up.push(b, read)
	} else {
		c.down.push(b, read)
	}
}

// Closed 等两个解析 goroutine 处理完已收到的全部字节：中继在它返回之后才写 close 事件，
// 一条连接的 http 事件因此都在它的 close 之前。
func (o *HTTPObserver) Closed(conn uint64) {
	c := o.conn(conn)
	c.up.close()
	c.down.close()
	c.done.Wait()
	o.mu.Lock()
	delete(o.conns, conn)
	o.mu.Unlock()
}

type pendingRequest struct {
	req *http.Request
	rec HTTPRecord
}

// countingReader 数从底层读走的字节；减去 bufio 里尚未消费的部分就是消息在线上的位置。
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// parseDelay 只供测试模拟解析 goroutine 被调度延后。
var parseDelay time.Duration

func (o *HTTPObserver) readRequests(conn uint64, q *byteQueue, out chan<- pendingRequest) {
	defer q.drain()
	time.Sleep(parseDelay)
	cr := &countingReader{r: q}
	br := bufio.NewReader(cr)
	pos := 0
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		head := cr.n - br.Buffered() - pos
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return
		}
		wire := cr.n - br.Buffered() - pos
		pos += wire
		rec := HTTPRecord{Method: req.Method, Path: req.URL.Path, Proto: req.Proto, ReqHead: head, ReqBody: len(body), ReqWire: wire, ReqT: q.timeAt(int64(pos))}
		if req.URL.Path == reportPath && strings.HasPrefix(req.Header.Get("Content-Type"), "application/proto") {
			decodeReport(body, &rec)
		}
		out <- pendingRequest{req: req, rec: rec}
	}
}

func (o *HTTPObserver) readResponses(conn uint64, q *byteQueue, reqs <-chan pendingRequest) {
	defer q.drain()
	cr := &countingReader{r: q}
	br := bufio.NewReader(cr)
	pos := 0
	for p := range reqs {
		rec := p.rec
		resp, err := http.ReadResponse(br, p.req)
		if err != nil {
			rec.Err = "response: " + err.Error()
			o.Log.Write(Event{Ev: "http", Conn: conn, HTTP: &rec})
			return
		}
		head := cr.n - br.Buffered() - pos
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			rec.Err = "response body: " + err.Error()
			o.Log.Write(Event{Ev: "http", Conn: conn, HTTP: &rec})
			return
		}
		wire := cr.n - br.Buffered() - pos
		pos += wire
		rec.Status, rec.RespHead, rec.RespBody, rec.RespWire = resp.StatusCode, head, len(body), wire
		rec.RespContentEncoding = resp.Header.Get("Content-Encoding")
		if rec.Path == reportPath && resp.StatusCode == http.StatusOK && strings.HasPrefix(resp.Header.Get("Content-Type"), "application/proto") {
			// 反代在这一跳上可以自己声明压缩：Go 的 http.Transport（Caddy 的上游传输）在请求没带 Accept-Encoding 时
			// 自动加上 gzip，hub 照此压缩应答，Transport 再透明解压后发给 agent。所以这一跳的应答可能是 gzip，
			// agent 那一侧不是；RespBody 记这一跳的字节，解码用解压后的内容。
			switch rec.RespContentEncoding {
			case "":
				decodeReply(body, &rec)
			case "gzip":
				if plain, err := gunzip(body); err != nil {
					rec.Err = "gunzip reply: " + err.Error()
				} else {
					decodeReply(plain, &rec)
				}
			default:
				rec.Err = "reply content encoding " + rec.RespContentEncoding
			}
		}
		o.Log.Write(Event{Ev: "http", Conn: conn, HTTP: &rec})
	}
}

func gunzip(b []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(zr)
}

// fieldSize 是一个已出现的长度前缀字段在外层消息里占的字节：标签、长度前缀与内容。
func fieldSize(num protowire.Number, n int) int {
	return protowire.SizeTag(num) + protowire.SizeBytes(n)
}

func decodeReport(body []byte, rec *HTTPRecord) {
	var m heronv1.ReportRequest
	if err := proto.Unmarshal(body, &m); err != nil {
		rec.Err = "decode report: " + err.Error()
		return
	}
	info := &ReportInfo{Body: len(body)}
	if met := m.Metrics; met != nil {
		info.Metrics = fieldSize(1, proto.Size(met))
		// 三项在 Metrics 里互不相邻也无妨：protobuf 的编码是逐字段拼接，只含这三项的消息的长度就是它们在原消息里占的字节。
		info.NetTotals = proto.Size(&heronv1.Metrics{NetRxTotal: met.NetRxTotal, NetTxTotal: met.NetTxTotal, NetCounterEpoch: met.NetCounterEpoch})
	}
	for _, r := range m.ProbeResults {
		info.ProbeResults++
		info.ProbeBytes += fieldSize(2, proto.Size(r))
	}
	if m.FactsHash != 0 {
		info.FactsHash = protowire.SizeTag(4) + protowire.SizeFixed64()
	}
	if m.Facts != nil {
		info.Facts = fieldSize(5, proto.Size(m.Facts))
		f := m.Facts
		fi := &FactsInfo{Hostname: f.Hostname, OS: f.Os, Kernel: f.Kernel, Arch: f.Arch, Virtualization: f.Virtualization, CPUModel: f.CpuModel, AgentVersion: f.AgentVersion,
			IPv4State: f.GetNetwork().GetIpv4().GetState().String(), IPv6State: f.GetNetwork().GetIpv6().GetState().String()}
		if f.Network != nil {
			fi.Network = fieldSize(10, proto.Size(f.Network))
		}
		if d := f.Diagnostics; d != nil {
			fi.Diagnostics = fieldSize(11, proto.Size(d))
			fi.NetInterfaces = d.NetInterfaces
			fi.NetExclude = len(d.NetExclude)
		}
		if f.Execution != nil {
			fi.Execution = fieldSize(12, proto.Size(f.Execution))
		}
		rec.Facts = fi
	}
	if m.Update != nil {
		info.Update = fieldSize(6, proto.Size(m.Update))
		rec.Update = &UpdateInfo{Supported: m.Update.Supported, Reason: m.Update.Reason, Version: m.Update.Version, Source: m.Update.Source}
	}
	if len(m.TasksDigest) > 0 {
		info.TasksDigest = fieldSize(8, len(m.TasksDigest))
	}
	// optional：给出即编码，值为 0 也占 9 字节。
	if m.UpdateHash != nil {
		info.UpdateHash = protowire.SizeTag(9) + protowire.SizeFixed64()
	}
	for _, c := range m.NetInterfaces {
		info.NetInterfaces += fieldSize(10, proto.Size(c))
		info.NetInterfaceCount++
	}
	rec.Counters = reportedCounters(m.GetMetrics(), m.NetInterfaces)
	rec.Report = info
}

// reportedCounters 按 hub 的准入与补齐给出这次上报的计数；既没有合计也没有列表（网络采集失败）时为 nil。
func reportedCounters(met *heronv1.Metrics, list []*heronv1.NetInterfaceCounter) *CounterInfo {
	c := &CounterInfo{Epoch: met.GetNetCounterEpoch()}
	if met != nil {
		c.RxTotal, c.TxTotal = met.NetRxTotal, met.NetTxTotal
	}
	if len(list) > 0 {
		sum := agentwire.NetInterfaceSummary(list)
		if c.RxTotal == nil {
			c.RxTotal = &sum.Rx
		}
		if c.TxTotal == nil {
			c.TxTotal = &sum.Tx
		}
		if c.Epoch == "" {
			c.Epoch = sum.Epoch
		}
		for _, n := range list {
			c.Interfaces = append(c.Interfaces, NicCounter{Name: n.GetName(), Rx: n.GetRxBytes(), Tx: n.GetTxBytes()})
		}
	}
	if c.RxTotal == nil && c.TxTotal == nil && len(c.Interfaces) == 0 {
		return nil
	}
	return c
}

func decodeReply(body []byte, rec *HTTPRecord) {
	var m heronv1.ReportResponse
	if err := proto.Unmarshal(body, &m); err != nil {
		rec.Err = "decode reply: " + err.Error()
		return
	}
	info := &ReplyInfo{Body: len(body), IntervalMs: int(m.ReportIntervalMs), WantFacts: m.WantFacts, WantUpdate: m.WantUpdate}
	if m.ReportIntervalMs != 0 {
		info.Interval = protowire.SizeTag(1) + protowire.SizeVarint(uint64(m.ReportIntervalMs))
	}
	if m.Tasks != nil {
		info.Tasks = fieldSize(2, proto.Size(m.Tasks))
		info.TaskCount = len(m.Tasks.Tasks)
		for _, t := range m.Tasks.Tasks {
			rec.Tasks = append(rec.Tasks, TaskWireInfo{Bytes: fieldSize(2, proto.Size(t)), TargetLen: len(t.Target)})
		}
	}
	if m.Detection != nil {
		info.Detection = fieldSize(5, proto.Size(m.Detection))
	}
	if m.Update != nil {
		info.Update = fieldSize(4, proto.Size(m.Update))
	}
	rec.Reply = info
}

// byteQueue 是无界的字节队列：push 从不阻塞，Read 在没有数据时等待，close 之后读完剩余数据再返回 EOF。
// 每段带着中继读到它的时刻入队，并记下它在流里的结束位置，timeAt 据此给出流中某个位置的字节是何时读到的。
type byteQueue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buf    bytes.Buffer
	pushed int64
	marks  []queueMark
	closed bool
	// dropped 为真表示读者已放弃（解析失败），此后 push 直接丢弃，不再积累内存。
	dropped bool
}

func newByteQueue() *byteQueue {
	q := &byteQueue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

type queueMark struct{ end, t int64 }

func (q *byteQueue) push(b []byte, read int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.dropped {
		q.buf.Write(b)
		q.pushed += int64(len(b))
		q.marks = append(q.marks, queueMark{end: q.pushed, t: read})
	}
	q.cond.Broadcast()
}

// timeAt 返回流中第 off 个字节（从 1 数）所在那一段被读到的时刻。调用方按位置递增询问，此前的记录随之丢弃。
func (q *byteQueue) timeAt(off int64) int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	i := 0
	for i < len(q.marks) && q.marks[i].end < off {
		i++
	}
	q.marks = q.marks[i:]
	if len(q.marks) == 0 {
		return clock().UnixNano()
	}
	return q.marks[0].t
}

func (q *byteQueue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	q.cond.Broadcast()
}

func (q *byteQueue) drain() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.dropped = true
	q.buf.Reset()
}

func (q *byteQueue) Read(p []byte) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for q.buf.Len() == 0 && !q.closed {
		q.cond.Wait()
	}
	if q.buf.Len() == 0 {
		return 0, io.EOF
	}
	return q.buf.Read(p)
}
