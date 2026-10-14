package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// exchange 是一条外侧连接上被空闲间隔分开的一段往返。agent 的上报间隔至少是 MinTTL/3（约 3.3 s），一次上报的
// 往返在毫秒量级，所以按 gap 切开后一段就是一次上报；服务端主动发的帧（空闲关闭前的 GOAWAY）自成一段，
// 没有对应的内侧请求。
type exchange struct {
	Conn       uint64 `json:"conn"`
	Index      int    `json:"index"` // 在本连接里的序号，0 是建立连接的那一段
	Start, End int64  `json:"-"`
	Up         int64  `json:"up"`
	Down       int64  `json:"down"`
	UpRecs     int    `json:"up_recs"`
	DownRecs   int    `json:"down_recs"`
	// HandshakeUp、HandshakeDown 只在连接的第一段非零：见 handshakeSplit。
	HandshakeUp   int64       `json:"handshake_up,omitempty"`
	HandshakeDown int64       `json:"handshake_down,omitempty"`
	HTTP          *HTTPRecord `json:"http,omitempty"`
	Phase         string      `json:"phase"`
	Class         string      `json:"class"`
}

type connInfo struct {
	ID        uint64
	Peer      string
	Open      int64
	Close     int64 // 0：运行结束时仍打开
	Up, Down  int64
	Exchanges []*exchange
	Hellos    []HelloInfo
}

type phase struct {
	T     int64
	Label string
}

// readPhases 读编排写下的阶段边界，每行 "<unix 纳秒> <标签>"。
func readPhases(r io.Reader) ([]phase, error) {
	var out []phase
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		ts, label, ok := strings.Cut(line, " ")
		if !ok {
			return nil, fmt.Errorf("phase line %q: want \"<unix-nanos> <label>\"", line)
		}
		t, err := strconv.ParseInt(ts, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("phase line %q: %w", line, err)
		}
		out = append(out, phase{T: t, Label: label})
	}
	return out, sc.Err()
}

func phaseAt(phases []phase, t int64) string {
	label := "-"
	for _, p := range phases {
		if p.T <= t {
			label = p.Label
		}
	}
	return label
}

// buildConns 把外侧中继的事件按连接归拢并切成往返段。
func buildConns(events []Event, gap time.Duration) []*connInfo {
	byID := map[uint64]*connInfo{}
	var order []uint64
	get := func(id uint64) *connInfo {
		c, ok := byID[id]
		if !ok {
			c = &connInfo{ID: id}
			byID[id] = c
			order = append(order, id)
		}
		return c
	}
	last := map[uint64]int64{}
	for _, e := range events {
		c := get(e.Conn)
		switch e.Ev {
		case "open":
			c.Open, c.Peer = e.T, e.Peer
		case "close":
			c.Close, c.Up, c.Down = e.T, e.Up, e.Down
		case "hello":
			if e.Hello != nil {
				c.Hellos = append(c.Hellos, *e.Hello)
			}
		case "data":
			var ex *exchange
			if n := len(c.Exchanges); n > 0 && e.T-last[e.Conn] <= int64(gap) {
				ex = c.Exchanges[n-1]
			} else {
				ex = &exchange{Conn: e.Conn, Index: len(c.Exchanges), Start: e.T}
				c.Exchanges = append(c.Exchanges, ex)
			}
			last[e.Conn] = e.T
			ex.End = e.T
			if e.Dir == dirUp {
				ex.Up += e.N
			} else {
				ex.Down += e.N
			}
		case "rec":
			if n := len(c.Exchanges); n > 0 {
				ex := c.Exchanges[n-1]
				if e.Dir == dirUp {
					ex.UpRecs++
				} else {
					ex.DownRecs++
				}
			}
		}
	}
	out := make([]*connInfo, 0, len(order))
	for _, id := range order {
		out = append(out, byID[id])
	}
	return out
}

// handshakeSplit 给出一条连接里 TLS 1.3 完整握手的字节：客户端从 ClientHello 到它的 Finished（客户端发出的
// 第一条 application_data 类型记录：TLS 1.3 的加密握手记录对外都标为 application_data，不带客户端证书时
// 客户端的加密握手只有 Finished 这一条），服务端是客户端 Finished 之前发出的全部记录（ServerHello 直到
// 服务端 Finished）。后者成立的前提是服务端不发 0.5-RTT 数据：Go 的 crypto/tls 服务端（Caddy）与
// OpenSSL（nginx）在 HTTP/2 上都是收到客户端 Finished 才开始写 SETTINGS，日志里服务端第一批记录与
// 客户端 Finished 之间的先后可以核对这一点。会话票据（NewSessionTicket）在客户端 Finished 之后发出，
// 计入第一段往返而不计入这里。
func handshakeSplit(events []Event, conn uint64) (up, down int64, ok bool) {
	for _, e := range events {
		if e.Conn != conn || e.Ev != "rec" {
			continue
		}
		if e.Dir == dirDown {
			down += e.N
			continue
		}
		up += e.N
		if e.Type == recApplicationData {
			return up, down, true
		}
	}
	return 0, 0, false
}

// matchHTTP 把内侧的每次请求配给外侧时间上包含它的往返段：反代收到完整请求之后才向 hub 发出，
// 内侧请求解析完成的时刻必然落在外侧这一段的首字节与末字节之间（两个中继在同一台宿主上取时钟）。
func matchHTTP(conns []*connInfo, tap []Event) (unmatched []HTTPRecord) {
	var all []*exchange
	for _, c := range conns {
		all = append(all, c.Exchanges...)
	}
	for _, e := range tap {
		if e.Ev != "http" || e.HTTP == nil {
			continue
		}
		var hit *exchange
		for _, ex := range all {
			if ex.Start <= e.HTTP.ReqT && e.HTTP.ReqT <= ex.End && ex.HTTP == nil {
				hit = ex
				break
			}
		}
		if hit == nil {
			unmatched = append(unmatched, *e.HTTP)
			continue
		}
		h := *e.HTTP
		hit.HTTP = &h
	}
	return unmatched
}

// classify 给每段往返一个类别。按"这一段比稳态多出了什么"分：新连接（握手）、携带 Facts、应答带清单，
// 其余带上报的是稳态。一段同时属于多类时按这个顺序取第一个：新连接的那段必然也带首次 Facts。
func classify(ex *exchange) string {
	switch {
	case ex.HTTP == nil:
		return "no-request"
	case ex.HTTP.Path != reportPath:
		return "other:" + ex.HTTP.Path
	case ex.Index == 0:
		return "new-connection"
	case ex.HTTP.Report != nil && ex.HTTP.Report.Facts > 0:
		return "facts"
	case ex.HTTP.Reply != nil && ex.HTTP.Reply.Tasks > 0:
		return "tasks"
	}
	return "steady"
}

type stat struct {
	N                      int
	Min, Median, Max, Mean float64
}

func stats(xs []float64) stat {
	if len(xs) == 0 {
		return stat{}
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	sum := 0.0
	for _, x := range s {
		sum += x
	}
	med := s[len(s)/2]
	if len(s)%2 == 0 {
		med = (s[len(s)/2-1] + s[len(s)/2]) / 2
	}
	return stat{N: len(s), Min: s[0], Median: med, Max: s[len(s)-1], Mean: sum / float64(len(s))}
}

func runAnalyze(args []string, w io.Writer) error {
	fs := flag.NewFlagSet("analyze", flag.ContinueOnError)
	relayPath := fs.String("relay", "", "outer relay event log")
	tapPath := fs.String("tap", "", "inner tap event log")
	phasesPath := fs.String("phases", "", "phase boundaries written by run.sh")
	gap := fs.Duration("gap", time.Second, "idle gap that separates two exchanges on one connection")
	nodes := fs.Int("nodes", 500, "node count for the projection")
	detail := fs.String("detail", "", "write every exchange as JSON lines here")
	trafficPath := fs.String("traffic", "", "hub traffic totals written by run.sh, one \"<unix-nanos> <label> <rx> <tx>\" per line")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *relayPath == "" || *tapPath == "" {
		return errors.New("-relay and -tap are required")
	}
	outer, err := readEventFile(*relayPath)
	if err != nil {
		return err
	}
	inner, err := readEventFile(*tapPath)
	if err != nil {
		return err
	}
	var phases []phase
	if *phasesPath != "" {
		f, err := os.Open(*phasesPath)
		if err != nil {
			return err
		}
		phases, err = readPhases(f)
		f.Close()
		if err != nil {
			return err
		}
	}
	conns := buildConns(outer, *gap)
	unmatched := matchHTTP(conns, inner)
	for _, c := range conns {
		if len(c.Exchanges) > 0 {
			if up, down, ok := handshakeSplit(outer, c.ID); ok {
				c.Exchanges[0].HandshakeUp, c.Exchanges[0].HandshakeDown = up, down
			}
		}
		for _, ex := range c.Exchanges {
			ex.Phase = phaseAt(phases, ex.Start)
			ex.Class = classify(ex)
		}
	}
	if *detail != "" {
		if err := writeDetail(*detail, conns); err != nil {
			return err
		}
	}
	report(w, conns, unmatched, *nodes)
	if *trafficPath != "" {
		f, err := os.Open(*trafficPath)
		if err != nil {
			return err
		}
		snaps, err := readTrafficSnapshots(f)
		f.Close()
		if err != nil {
			return err
		}
		trafficReport(w, inner, snaps)
	}
	return nil
}

// trafficSnap 是 hub 在某一时刻对本节点累计记下的字节（GetTraffic 的 total_rx、total_tx）。
type trafficSnap struct {
	T      int64
	Label  string
	Rx, Tx int64
}

func readTrafficSnapshots(r io.Reader) ([]trafficSnap, error) {
	var out []trafficSnap
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 4 {
			return nil, fmt.Errorf("traffic line %q: want \"<unix-nanos> <label> <rx> <tx>\"", line)
		}
		var s trafficSnap
		var err error
		s.Label = f[1]
		for i, dst := range []*int64{&s.T, &s.Rx, &s.Tx} {
			if *dst, err = strconv.ParseInt(f[[]int{0, 2, 3}[i]], 10, 64); err != nil {
				return nil, fmt.Errorf("traffic line %q: %w", line, err)
			}
		}
		out = append(out, s)
	}
	return out, sc.Err()
}

// trafficReport 把相邻两次 hub 快照之间"agent 报的计数涨了多少"与"hub 记了多少"并排列出。两边按同一组上报对齐：
// 快照时刻之前最后一次被接受的上报（tap 读到请求的时刻 ReqT 早于快照）是 hub 在那一刻已经入账的最后一次。窗口内
// 第一次出现的网卡，agent 的合计从它已有的计数算起，hub 只从它出现之后算起，这部分列在 first seen；其余差额就是
// hub 没记上的字节。快照与上报同时发生时这一次可能落在任一侧，差额会在相邻两行里一正一负。
func trafficReport(w io.Writer, tap []Event, snaps []trafficSnap) {
	var reports []*HTTPRecord
	for _, e := range tap {
		if h := e.HTTP; h != nil && h.Path == reportPath && h.Status == 200 && h.Counters != nil && h.Counters.RxTotal != nil && h.Counters.TxTotal != nil {
			reports = append(reports, h)
		}
	}
	slices.SortFunc(reports, func(a, b *HTTPRecord) int { return int(a.ReqT - b.ReqT) })
	// before 是 t 之前最后一次上报的下标，没有则为 -1。
	before := func(t int64) int {
		i := -1
		for j, r := range reports {
			if r.ReqT < t {
				i = j
			}
		}
		return i
	}
	fmt.Fprintln(w, "\n## Traffic: counters reported vs bytes the hub accounted (between consecutive hub snapshots)")
	fmt.Fprintln(w, "| from | to | reports | reported rx | hub rx | unaccounted rx | reported tx | hub tx | unaccounted tx | first seen (name rx/tx) |")
	fmt.Fprintln(w, "|---|---|---:|---:|---:|---:|---:|---:|---:|---|")
	for i := 1; i < len(snaps); i++ {
		a, b := snaps[i-1], snaps[i]
		lo, hi := before(a.T), before(b.T)
		if hi < 0 {
			continue
		}
		// hub 对第一次上报只建基线：窗口开始时还没有上报，就从窗口里的第一次算起。
		if lo < 0 {
			lo = 0
		}
		base, last := reports[lo].Counters, reports[hi].Counters
		repRx, repTx := int64(*last.RxTotal-*base.RxTotal), int64(*last.TxTotal-*base.TxTotal)
		hubRx, hubTx := b.Rx-a.Rx, b.Tx-a.Tx
		var seen []string
		for j := lo + 1; j <= hi; j++ {
			prev := map[string]bool{}
			for _, n := range reports[j-1].Counters.Interfaces {
				prev[n.Name] = true
			}
			for _, n := range reports[j].Counters.Interfaces {
				if len(prev) > 0 && !prev[n.Name] {
					seen = append(seen, fmt.Sprintf("%s %d/%d", n.Name, n.Rx, n.Tx))
				}
			}
		}
		first := "-"
		if len(seen) > 0 {
			first = strings.Join(seen, ", ")
		}
		fmt.Fprintf(w, "| %s | %s | %d | %d | %d | %d | %d | %d | %d | %s |\n", a.Label, b.Label, hi-lo, repRx, hubRx, repRx-hubRx, repTx, hubTx, repTx-hubTx, first)
	}
}

func readEventFile(path string) ([]Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ReadEvents(f)
}

func writeDetail(path string, conns []*connInfo) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	for _, c := range conns {
		for _, ex := range c.Exchanges {
			if err := enc.Encode(ex); err != nil {
				f.Close()
				return err
			}
		}
	}
	return f.Close()
}

func report(w io.Writer, conns []*connInfo, unmatched []HTTPRecord, nodes int) {
	fmt.Fprintln(w, "## TLS hellos (first connection)")
	for _, c := range conns {
		for _, h := range c.Hellos {
			b, _ := json.Marshal(h)
			fmt.Fprintf(w, "conn %d: %s\n", c.ID, b)
		}
		if len(c.Hellos) > 0 {
			break
		}
	}
	fmt.Fprintln(w, "\n## Connections")
	fmt.Fprintln(w, "| conn | peer | open | lifetime | exchanges | reports | up B | down B | handshake up/down B | closed |")
	fmt.Fprintln(w, "|---:|---|---|---:|---:|---:|---:|---:|---|---|")
	end := int64(0)
	for _, c := range conns {
		end = max(end, c.Close)
		for _, ex := range c.Exchanges {
			end = max(end, ex.End)
		}
	}
	for _, c := range conns {
		reports := 0
		var up, down int64
		for _, ex := range c.Exchanges {
			up += ex.Up
			down += ex.Down
			if ex.HTTP != nil && ex.HTTP.Path == reportPath {
				reports++
			}
		}
		closed, life := "open at end", end-c.Open
		if c.Close != 0 {
			closed, life = "closed", c.Close-c.Open
			// close 事件里的合计是中继计数的总账；与逐段相加不一致说明有字节没有落进任何一段。
			if c.Up != up || c.Down != down {
				closed += fmt.Sprintf(" (MISMATCH: close totals %d/%d)", c.Up, c.Down)
			}
		}
		hs := "-"
		if len(c.Exchanges) > 0 && c.Exchanges[0].HandshakeUp > 0 {
			hs = fmt.Sprintf("%d / %d", c.Exchanges[0].HandshakeUp, c.Exchanges[0].HandshakeDown)
		}
		fmt.Fprintf(w, "| %d | %s | %s | %s | %d | %d | %d | %d | %s | %s |\n", c.ID, c.Peer, time.Unix(0, c.Open).UTC().Format(time.TimeOnly),
			time.Duration(life).Round(time.Second), len(c.Exchanges), reports, up, down, hs, closed)
	}
	if len(unmatched) > 0 {
		fmt.Fprintf(w, "\nWARNING: %d inner requests matched no outer exchange\n", len(unmatched))
		for _, h := range unmatched {
			fmt.Fprintf(w, "  %s %s at %s\n", h.Method, h.Path, time.Unix(0, h.ReqT).UTC().Format(time.StampMicro))
		}
	}

	type key struct{ phase, class string }
	groups := map[key][]*exchange{}
	var keys []key
	for _, c := range conns {
		for _, ex := range c.Exchanges {
			k := key{ex.Phase, ex.Class}
			if _, ok := groups[k]; !ok {
				keys = append(keys, k)
			}
			groups[k] = append(groups[k], ex)
		}
	}
	col := func(xs []*exchange, f func(*exchange) float64) stat {
		var v []float64
		for _, ex := range xs {
			v = append(v, f(ex))
		}
		return stats(v)
	}
	fmt.Fprintln(w, "\n## Per exchange, by phase and class")
	fmt.Fprintln(w, "wire = outer relay (TLS records incl. HTTP/2 frames); body = protobuf message (inner hop, decompressed); overhead = wire - body, medians.")
	fmt.Fprintln(w, "| phase | class | n | up wire min/med/max/mean | down wire min/med/max/mean | req body med | reply body med | up overhead med | down overhead med | probe results med | interval ms |")
	fmt.Fprintln(w, "|---|---|---:|---|---|---:|---:|---:|---:|---:|---:|")
	for _, k := range keys {
		xs := groups[k]
		up := col(xs, func(e *exchange) float64 { return float64(e.Up) })
		down := col(xs, func(e *exchange) float64 { return float64(e.Down) })
		req := col(xs, func(e *exchange) float64 {
			if e.HTTP == nil {
				return 0
			}
			return float64(e.HTTP.ReqBody)
		})
		// agent 那一侧收到的应答正文是解码前的消息本身（agent 不声明压缩），取解压后的长度，不取内侧这一跳的字节。
		resp := col(xs, func(e *exchange) float64 {
			if e.HTTP == nil || e.HTTP.Reply == nil {
				return 0
			}
			return float64(e.HTTP.Reply.Body)
		})
		upOver := col(xs, func(e *exchange) float64 {
			if e.HTTP == nil {
				return 0
			}
			return float64(e.Up - int64(e.HTTP.ReqBody))
		})
		downOver := col(xs, func(e *exchange) float64 {
			if e.HTTP == nil || e.HTTP.Reply == nil {
				return 0
			}
			return float64(e.Down - int64(e.HTTP.Reply.Body))
		})
		pr := col(xs, func(e *exchange) float64 {
			if e.HTTP == nil || e.HTTP.Report == nil {
				return 0
			}
			return float64(e.HTTP.Report.ProbeResults)
		})
		iv := col(xs, func(e *exchange) float64 {
			if e.HTTP == nil || e.HTTP.Reply == nil {
				return 0
			}
			return float64(e.HTTP.Reply.IntervalMs)
		})
		fmt.Fprintf(w, "| %s | %s | %d | %s | %s | %.0f | %.0f | %.0f | %.0f | %.0f | %.0f |\n", k.phase, k.class, up.N, fmtStat(up), fmtStat(down), req.Median, resp.Median, upOver.Median, downOver.Median, pr.Median, iv.Median)
	}

	// 全文只在首次和索要时出现，另列出现次数，避免中位数掩盖少数请求。
	fmt.Fprintln(w, "\n## ReportRequest field bytes, steady exchanges (median per field; tag and length prefix included)")
	fmt.Fprintln(w, "| phase | body | metrics | net totals (in metrics) | probe_results | facts_hash | update | update_hash | tasks_digest | net_interfaces | with update |")
	fmt.Fprintln(w, "|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|")
	for _, k := range keys {
		if k.class != "steady" {
			continue
		}
		xs := groups[k]
		f := func(get func(*ReportInfo) int) float64 {
			return col(xs, func(e *exchange) float64 {
				if e.HTTP == nil || e.HTTP.Report == nil {
					return 0
				}
				return float64(get(e.HTTP.Report))
			}).Median
		}
		with := func(get func(*ReportInfo) int) string {
			n, of := 0, 0
			for _, e := range xs {
				if e.HTTP == nil || e.HTTP.Report == nil {
					continue
				}
				of++
				if get(e.HTTP.Report) > 0 {
					n++
				}
			}
			return fmt.Sprintf("%d/%d", n, of)
		}
		fmt.Fprintf(w, "| %s | %.0f | %.0f | %.0f | %.0f | %.0f | %.0f | %.0f | %.0f | %.0f | %s |\n", k.phase,
			f(func(r *ReportInfo) int { return r.Body }), f(func(r *ReportInfo) int { return r.Metrics }), f(func(r *ReportInfo) int { return r.NetTotals }), f(func(r *ReportInfo) int { return r.ProbeBytes }),
			f(func(r *ReportInfo) int { return r.FactsHash }), f(func(r *ReportInfo) int { return r.Update }),
			f(func(r *ReportInfo) int { return r.UpdateHash }), f(func(r *ReportInfo) int { return r.TasksDigest }), f(func(r *ReportInfo) int { return r.NetInterfaces }),
			with(func(r *ReportInfo) int { return r.Update }))
	}

	fmt.Fprintln(w, "\n## ReportResponse field bytes, steady exchanges (median per field; tag and length prefix included)")
	fmt.Fprintln(w, "| phase | body | interval | tasks | detection | update | with want_facts | with want_update |")
	fmt.Fprintln(w, "|---|---:|---:|---:|---:|---:|---:|---:|")
	for _, k := range keys {
		if k.class != "steady" {
			continue
		}
		xs := groups[k]
		f := func(get func(*ReplyInfo) int) float64 {
			return col(xs, func(e *exchange) float64 {
				if e.HTTP == nil || e.HTTP.Reply == nil {
					return 0
				}
				return float64(get(e.HTTP.Reply))
			}).Median
		}
		var wantFacts, wantUpdate, of int
		for _, e := range xs {
			if e.HTTP == nil || e.HTTP.Reply == nil {
				continue
			}
			of++
			if e.HTTP.Reply.WantFacts {
				wantFacts++
			}
			if e.HTTP.Reply.WantUpdate {
				wantUpdate++
			}
		}
		fmt.Fprintf(w, "| %s | %.0f | %.0f | %.0f | %.0f | %.0f | %d/%d | %d/%d |\n", k.phase,
			f(func(r *ReplyInfo) int { return r.Body }), f(func(r *ReplyInfo) int { return r.Interval }), f(func(r *ReplyInfo) int { return r.Tasks }),
			f(func(r *ReplyInfo) int { return r.Detection }), f(func(r *ReplyInfo) int { return r.Update }),
			wantFacts, of, wantUpdate, of)
	}

	// 随主机变化的值：最后一份带 Facts 的上报与最后一份带全文的更新状态，原样列出（都不是凭据）。
	var lastFacts, lastUpdate *HTTPRecord
	var lastTasks *exchange
	var reports, withUpdate int
	for _, c := range conns {
		for _, ex := range c.Exchanges {
			if ex.HTTP == nil {
				continue
			}
			if ex.HTTP.Facts != nil {
				lastFacts = ex.HTTP
			}
			if ex.HTTP.Report != nil {
				reports++
			}
			if ex.HTTP.Update != nil {
				lastUpdate = ex.HTTP
				withUpdate++
			}
			if ex.HTTP.Reply != nil && ex.HTTP.Reply.Tasks > 0 {
				lastTasks = ex
			}
		}
	}
	fmt.Fprintln(w, "\n## Host-dependent values as reported")
	if lastFacts != nil {
		b, _ := json.Marshal(lastFacts.Facts)
		fmt.Fprintf(w, "facts (%d bytes in the request): %s\n", lastFacts.Report.Facts, b)
	}
	if lastUpdate != nil {
		b, _ := json.Marshal(lastUpdate.Update)
		fmt.Fprintf(w, "update status (%d bytes; carried by %d of %d report requests): %s\n", lastUpdate.Report.Update, withUpdate, reports, b)
	}
	if lastTasks != nil {
		h := lastTasks.HTTP
		per := map[[2]int]int{}
		for _, t := range h.Tasks {
			per[[2]int{t.Bytes, t.TargetLen}]++
		}
		fmt.Fprintf(w, "last task list sent: %d tasks, %d bytes in the reply (wire down %d); per task bytes/target length -> count: %v\n", h.Reply.TaskCount, h.Reply.Tasks, lastTasks.Down, per)
	}

	fmt.Fprintf(w, "\n## Projection to %d nodes (steady exchanges of each phase; mean bytes; 30-day month)\n", nodes)
	fmt.Fprintln(w, "| phase | interval s | hub requests/s | up kbit/s | down kbit/s | per node per month MB (up+down) |")
	fmt.Fprintln(w, "|---|---:|---:|---:|---:|---:|")
	for _, k := range keys {
		if k.class != "steady" {
			continue
		}
		xs := groups[k]
		up := col(xs, func(e *exchange) float64 { return float64(e.Up) }).Mean
		down := col(xs, func(e *exchange) float64 { return float64(e.Down) }).Mean
		ivMs := col(xs, func(e *exchange) float64 {
			if e.HTTP == nil || e.HTTP.Reply == nil {
				return 0
			}
			return float64(e.HTTP.Reply.IntervalMs)
		}).Median
		if ivMs == 0 {
			continue
		}
		iv := ivMs / 1000
		perSec := float64(nodes) / iv
		month := 30 * 86400 / iv
		fmt.Fprintf(w, "| %s | %.3f | %.1f | %.1f | %.1f | %.1f |\n", k.phase, iv, perSec, perSec*up*8/1000, perSec*down*8/1000, (up+down)*month/1e6)
	}
}

func fmtStat(s stat) string {
	return fmt.Sprintf("%.0f / %.0f / %.0f / %.1f", s.Min, s.Median, s.Max, s.Mean)
}
