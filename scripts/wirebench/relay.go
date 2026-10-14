package main

import (
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// 方向以发起连接的一侧为准：up 是客户端发往服务端（agent → 反代、反代 → hub），down 反之。
const (
	dirUp   = "up"
	dirDown = "down"
)

// Observer 看到中继转发出去的每一段字节。中继只把已经写进对端的字节交给它：没写出去的字节不在线路上，
// 不能计入线上字节。read 是中继从来源读到这段字节的时刻（Unix 纳秒），早于把它写给对端：中继在转发之后才写
// 事件日志、才调用观察器，二者在负载下都可能慢，对端的应答可以先于这次调用绕回客户端，所以需要时刻的观察器
// 只能用 read，不能在调用里自己取时间。同一连接同一方向的调用串行，两个方向各在自己的 goroutine 里调用。
type Observer interface {
	Data(conn uint64, dir string, b []byte, read int64)
	// Closed 在两个方向都结束之后调用一次，此后不再有该连接的 Data。
	Closed(conn uint64)
}

// Relay 把 Listen 上接受的每条 TCP 连接原样转发到 Target，按连接、按方向计数并写事件日志。
// 计的是中继两侧之间真实流过的字节，含 TLS 记录头与 AEAD 标签、HTTP/2 帧头与 HPACK 头块：中继不解密，
// 也不改写任何字节，所以放在 agent 与反代之间时计到的就是公网那一段的线上字节（不含 TCP/IP 头）。
type Relay struct {
	Target string
	Log    *EventLog
	// NewObserver 为每条连接给出一个观察者，可为空；中继本身的计数不依赖它。
	NewObserver func(conn uint64) Observer
	// Dial 为空时用 net.Dial；测试经它把目标换成内存里的对端。
	Dial func(network, addr string) (net.Conn, error)

	next atomic.Uint64
	wg   sync.WaitGroup
}

// Serve 接受连接直到 l 关闭，返回前等全部已接受的连接结束：调用方据此知道事件日志已经写完整。
func (r *Relay) Serve(l net.Listener) error {
	defer r.wg.Wait()
	for {
		c, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		id := r.next.Add(1)
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			r.handle(id, c)
		}()
	}
}

func (r *Relay) handle(id uint64, client net.Conn) {
	defer client.Close()
	r.Log.Write(Event{Ev: "open", Conn: id, Peer: client.RemoteAddr().String()})
	dial := r.Dial
	if dial == nil {
		dial = net.Dial
	}
	server, err := dial("tcp", r.Target)
	if err != nil {
		r.Log.Write(Event{Ev: "close", Conn: id, Err: err.Error()})
		return
	}
	defer server.Close()
	var obs Observer
	if r.NewObserver != nil {
		obs = r.NewObserver(id)
	}
	var up, down atomic.Int64
	record := func(dir string, total *atomic.Int64) func([]byte, int64) {
		return func(b []byte, read int64) {
			total.Add(int64(len(b)))
			r.Log.Write(Event{T: read, Ev: "data", Conn: id, Dir: dir, N: int64(len(b))})
			if obs != nil {
				obs.Data(id, dir, b, read)
			}
		}
	}
	// 两个方向各自转发到读端 EOF，再把 EOF 以半关闭传给对端，另一方向照常继续：一侧先关写的连接（客户端发完请求
	// 后 shutdown(SHUT_WR)、服务端随后才回最后一段）只有这样才不丢尾段。连接在两个方向都结束后才算关闭，
	// close 事件里的合计因而包含关闭前的最后一段。
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		pump(server, client, record(dirUp, &up))
	}()
	go func() {
		defer wg.Done()
		pump(client, server, record(dirDown, &down))
	}()
	wg.Wait()
	if obs != nil {
		obs.Closed(id)
	}
	r.Log.Write(Event{Ev: "close", Conn: id, Up: up.Load(), Down: down.Load()})
}

// pump 把 src 读到的字节写进 dst，每段写出的字节连同读到它的时刻交给 rec，读端结束后半关闭 dst 的写方向。
// Read 可以在返回数据的同一次调用里带回 EOF（io.Reader 的约定），所以先处理 n 再看错误；反过来写会漏掉最后一段。
// 写出失败时只记已写出的部分并停止：对端已经不收，后面的字节不会上线。
func pump(dst net.Conn, src io.Reader, rec func([]byte, int64)) {
	buf := make([]byte, 32<<10)
	for {
		n, rerr := src.Read(buf)
		read := clock().UnixNano()
		if n > 0 {
			w, werr := dst.Write(buf[:n])
			if w > 0 {
				rec(buf[:w], read)
			}
			if werr != nil {
				break
			}
		}
		if rerr != nil {
			break
		}
	}
	closeWrite(dst)
}

// closeWrite 半关闭 c 的写方向；不支持半关闭的连接整体关闭，对端同样读到 EOF。
func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}

// clock 是事件时间戳的来源，测试替换它以得到确定的时间。
var clock = func() time.Time { return time.Now() }
