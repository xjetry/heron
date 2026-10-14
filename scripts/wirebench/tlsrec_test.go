package main

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

const testName = "hub.wirebench.test"

func testTLSConfigs(t *testing.T) (server, client *tls.Config) {
	t.Helper()
	dir := t.TempDir()
	if err := writeCerts(dir, []string{testName}); err != nil {
		t.Fatal(err)
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "chain.pem"), filepath.Join(dir, "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	caPEM, err := os.ReadFile(filepath.Join(dir, "ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("ca.pem holds no certificate")
	}
	// 关掉会话票据：服务端在客户端 Finished 之后就不再主动发任何记录，握手结束时两个方向的累计字节
	// 就是握手本身，可以拿来独立核对 handshakeSplit。
	server = &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"h2", "http/1.1"}, SessionTicketsDisabled: true}
	client = &tls.Config{RootCAs: pool, ServerName: testName, NextProtos: []string{"h2", "http/1.1"}}
	return server, client
}

// rawObserver 原样留下每个方向的字节，供逐字节重放。
type rawObserver struct {
	mu       sync.Mutex
	up, down []byte
	inner    Observer
}

func (o *rawObserver) Data(conn uint64, dir string, b []byte, read int64) {
	o.mu.Lock()
	if dir == dirUp {
		o.up = append(o.up, b...)
	} else {
		o.down = append(o.down, b...)
	}
	o.mu.Unlock()
	o.inner.Data(conn, dir, b, read)
}

func (o *rawObserver) Closed(conn uint64) { o.inner.Closed(conn) }

// TestTLSRecordsTileTheStreamAndSplitTheHandshake 经中继跑一次真实的 TLS 1.3 握手与应用数据：
//   - 每个方向切出的记录长度（含记录头）加起来等于该方向中继计到的字节：切分没有漏段、没有错位；
//   - handshakeSplit 给出的两个方向的握手字节，等于握手刚完成、任何应用数据发出之前两个方向的累计字节；
//   - 两个 hello 解析出 SNI、ALPN 与 TLS 1.3。
func TestTLSRecordsTileTheStreamAndSplitTheHandshake(t *testing.T) {
	serverCfg, clientCfg := testTLSConfigs(t)
	l, err := tls.Listen("tcp", "127.0.0.1:0", serverCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	handshaken := make(chan struct{})
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		if err := c.(*tls.Conn).Handshake(); err != nil {
			t.Error(err)
			return
		}
		close(handshaken)
		if _, err := io.ReadAll(c); err != nil {
			t.Error(err)
			return
		}
		for range 3 {
			if _, err := c.Write(randomBytes(20000)); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	var raw *rawObserver
	addr, stop, _ := startRelay(t, l.Addr().String(), func(log *EventLog) Observer {
		raw = &rawObserver{inner: &TLSObserver{Log: log}}
		return raw
	})
	c, err := tls.Dial("tcp", addr, clientCfg)
	if err != nil {
		t.Fatal(err)
	}
	<-handshaken
	// 服务端已读完客户端 Finished，此后到客户端写应用数据之前两个方向都不再有字节；等中继把已转发的
	// 字节记完再取累计。
	time.Sleep(100 * time.Millisecond)
	raw.mu.Lock()
	hsUp, hsDown := len(raw.up), len(raw.down)
	raw.mu.Unlock()
	if _, err := c.Write(randomBytes(30000)); err != nil {
		t.Fatal(err)
	}
	if err := c.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(c); err != nil {
		t.Fatal(err)
	}
	c.Close()
	events := stop()

	var conn uint64
	recSum := map[string]int64{}
	dataSum := map[string]int64{}
	var hellos []HelloInfo
	for _, e := range events {
		conn = e.Conn
		switch e.Ev {
		case "rec":
			recSum[e.Dir] += e.N
		case "data":
			dataSum[e.Dir] += e.N
		case "hello":
			hellos = append(hellos, *e.Hello)
		}
	}
	for _, dir := range []string{dirUp, dirDown} {
		if recSum[dir] == 0 || recSum[dir] != dataSum[dir] {
			t.Errorf("%s: records add up to %d bytes, relay forwarded %d", dir, recSum[dir], dataSum[dir])
		}
	}
	up, down, ok := handshakeSplit(events, conn)
	if !ok || up != int64(hsUp) || down != int64(hsDown) {
		t.Errorf("handshakeSplit = %d/%d (ok=%v), bytes on the wire when the handshake completed = %d/%d", up, down, ok, hsUp, hsDown)
	}
	if len(hellos) != 2 {
		t.Fatalf("got %d hellos, want ClientHello and ServerHello: %+v", len(hellos), hellos)
	}
	for _, h := range hellos {
		switch h.Msg {
		case "client_hello":
			if h.SNI != testName || !slices.Equal(h.ALPN, []string{"h2", "http/1.1"}) || h.Version != "TLS 1.3" || len(h.KeyShares) == 0 {
				t.Errorf("client hello = %+v", h)
			}
		case "server_hello":
			if h.Version != "TLS 1.3" || h.Cipher == "" || len(h.KeyShares) != 1 {
				t.Errorf("server hello = %+v", h)
			}
		default:
			t.Errorf("unexpected hello %+v", h)
		}
	}

	// 同一份字节逐字节喂给新的解析器，切出的记录序列必须与按中继读到的分段切出的相同：记录头跨段时也不错位。
	for _, dir := range []struct {
		name string
		b    []byte
	}{{dirUp, raw.up}, {dirDown, raw.down}} {
		var want, got []int
		var p1, p2 recordParser
		p1.feed(dir.b, func(_ byte, n int) { want = append(want, n) }, func(HelloInfo) {})
		for i := range dir.b {
			p2.feed(dir.b[i:i+1], func(_ byte, n int) { got = append(got, n) }, func(HelloInfo) {})
		}
		if len(want) == 0 || !slices.Equal(want, got) {
			t.Errorf("%s: one-byte feeding split %v, whole feeding split %v", dir.name, got, want)
		}
	}
}

// TestRecordParserStopsOnNonTLS：明文流不是 TLS 记录，解析器不应把随机字节当长度切出记录。
func TestRecordParserStopsOnNonTLS(t *testing.T) {
	var p recordParser
	n := 0
	p.feed([]byte("POST /heron.v1.AgentService/Report HTTP/1.1\r\n"), func(byte, int) { n++ }, func(HelloInfo) {})
	if n != 0 || !p.bad {
		t.Fatalf("parsed %d records from plain HTTP (bad=%v)", n, p.bad)
	}
}
