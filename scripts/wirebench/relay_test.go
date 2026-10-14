package main

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"testing/iotest"
	"time"
)

// startRelay 在回环上起一个转发到 target 的中继，返回它的地址、读回事件的函数与停止函数。
// 停止后 Serve 已返回：全部连接的 close 事件都已写出。logBytes 给出事件日志的原始字节。
func startRelay(t *testing.T, target string, obs func(*EventLog) Observer) (addr string, stop func() []Event, logBytes func() []byte) {
	t.Helper()
	var buf syncBuffer
	log := NewEventLog(&buf)
	r := &Relay{Target: target, Log: log}
	if obs != nil {
		o := obs(log)
		r.NewObserver = func(uint64) Observer { return o }
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- r.Serve(l) }()
	stopped := false
	stop = func() []Event {
		if !stopped {
			l.Close()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			stopped = true
		}
		ev, err := ReadEvents(bytes.NewReader(buf.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		return ev
	}
	t.Cleanup(func() { stop() })
	return l.Addr().String(), stop, buf.Bytes
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) Bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return bytes.Clone(s.b.Bytes())
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

// connTotals 从事件里按连接与方向加总 data 字节，并取 close 事件的合计。
type connTotals struct {
	dataUp, dataDown   int64
	closeUp, closeDown int64
	closed             bool
	peer               string
}

func totalsByConn(t *testing.T, events []Event) map[uint64]*connTotals {
	t.Helper()
	out := map[uint64]*connTotals{}
	get := func(id uint64) *connTotals {
		if out[id] == nil {
			out[id] = &connTotals{}
		}
		return out[id]
	}
	for _, e := range events {
		c := get(e.Conn)
		switch e.Ev {
		case "open":
			c.peer = e.Peer
		case "data":
			if c.closed {
				t.Fatalf("conn %d: data event after close", e.Conn)
			}
			switch e.Dir {
			case dirUp:
				c.dataUp += e.N
			case dirDown:
				c.dataDown += e.N
			default:
				t.Fatalf("conn %d: data event without a direction: %+v", e.Conn, e)
			}
		case "close":
			c.closed, c.closeUp, c.closeDown = true, e.Up, e.Down
		}
	}
	return out
}

// TestRelayCountsExactBytesPerConnectionAndDirection 让三条连接各自双向传已知长度的随机字节。服务端在读到
// 客户端的 EOF（客户端半关闭）之后才分几段写回应答再关闭：这是"关闭前的最后一段"——一侧先结束后另一侧
// 继续传的尾段。逐连接核对：两端实际收到的字节与发出的逐字节相同；中继按方向记下的 data 合计、close 事件
// 的合计都等于实际字节数。
func TestRelayCountsExactBytesPerConnectionAndDirection(t *testing.T) {
	type plan struct{ req, resp []int } // 每次写的段长
	plans := []plan{
		{req: []int{1, 517, 70000}, resp: []int{3, 16384, 9}},
		{req: []int{100000}, resp: []int{1}},
		{req: []int{7}, resp: []int{250000, 1, 4096}},
	}
	srv, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	type served struct {
		got  []byte
		sent []byte
	}
	results := make(chan served, len(plans))
	go func() {
		for {
			c, err := srv.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				got, err := io.ReadAll(c)
				if err != nil {
					t.Error(err)
					return
				}
				// 服务端不知道是哪条连接，按请求总长对回计划（三个计划的请求总长互不相同）。
				var p plan
				for _, cand := range plans {
					if total(cand.req) == len(got) {
						p = cand
					}
				}
				var sent []byte
				for _, n := range p.resp {
					b := randomBytes(n)
					if _, err := c.Write(b); err != nil {
						t.Error(err)
						return
					}
					sent = append(sent, b...)
					time.Sleep(5 * time.Millisecond)
				}
				results <- served{got: got, sent: sent}
			}(c)
		}
	}()
	addr, stop, _ := startRelay(t, srv.Addr().String(), nil)

	type client struct {
		sent, got []byte
		local     string
	}
	clients := make([]client, len(plans))
	var wg sync.WaitGroup
	for i, p := range plans {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := net.Dial("tcp", addr)
			if err != nil {
				t.Error(err)
				return
			}
			defer c.Close()
			clients[i].local = c.LocalAddr().String()
			for _, n := range p.req {
				b := randomBytes(n)
				if _, err := c.Write(b); err != nil {
					t.Error(err)
					return
				}
				clients[i].sent = append(clients[i].sent, b...)
				time.Sleep(5 * time.Millisecond)
			}
			if err := c.(*net.TCPConn).CloseWrite(); err != nil {
				t.Error(err)
				return
			}
			got, err := io.ReadAll(c)
			if err != nil {
				t.Error(err)
				return
			}
			clients[i].got = got
		}()
	}
	wg.Wait()
	byLen := map[int]served{}
	for range plans {
		s := <-results
		byLen[len(s.got)] = s
	}
	events := stop()
	totals := totalsByConn(t, events)
	if len(totals) != len(plans) {
		t.Fatalf("relay saw %d connections, want %d", len(totals), len(plans))
	}
	for i, c := range clients {
		s := byLen[len(c.sent)]
		if !bytes.Equal(s.got, c.sent) {
			t.Fatalf("client %d: server received %d bytes that differ from the %d sent", i, len(s.got), len(c.sent))
		}
		if !bytes.Equal(c.got, s.sent) {
			t.Fatalf("client %d: received %d bytes, server sent %d", i, len(c.got), len(s.sent))
		}
		var ct *connTotals
		for _, tt := range totals {
			if tt.peer == c.local {
				ct = tt
			}
		}
		if ct == nil {
			t.Fatalf("client %d (%s): no open event with its address", i, c.local)
		}
		want := fmt.Sprintf("up=%d down=%d", len(c.sent), len(s.sent))
		if got := fmt.Sprintf("up=%d down=%d", ct.dataUp, ct.dataDown); got != want {
			t.Errorf("client %d: data events %s, want %s", i, got, want)
		}
		if !ct.closed {
			t.Errorf("client %d: no close event", i)
		}
		if got := fmt.Sprintf("up=%d down=%d", ct.closeUp, ct.closeDown); got != want {
			t.Errorf("client %d: close totals %s, want %s", i, got, want)
		}
	}
}

func total(xs []int) int {
	n := 0
	for _, x := range xs {
		n += x
	}
	return n
}

// TestPumpCountsDataReturnedWithEOF：io.Reader 可以在带回最后一段数据的同一次调用里返回 EOF。
// iotest.DataErrReader 正是这样交付的，最后一段必须照样写出并计入。
func TestPumpCountsDataReturnedWithEOF(t *testing.T) {
	data := randomBytes(100000)
	a, b := net.Pipe()
	got := make(chan []byte, 1)
	go func() {
		all, _ := io.ReadAll(b)
		got <- all
	}()
	var counted int
	pump(a, iotest.DataErrReader(iotest.OneByteReader(bytes.NewReader(data))), func(p []byte, _ int64) { counted += len(p) })
	received := <-got
	if !bytes.Equal(received, data) {
		t.Fatalf("forwarded %d bytes, want the %d written", len(received), len(data))
	}
	if counted != len(data) {
		t.Fatalf("counted %d bytes, want %d", counted, len(data))
	}
}
