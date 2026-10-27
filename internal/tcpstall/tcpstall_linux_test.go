//go:build linux

package tcpstall

import (
	"net"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func pair(t *testing.T) (server, peer *net.TCPConn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			t.Error(err)
			close(accepted)
			return
		}
		accepted <- c
	}()
	p, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	s, ok := <-accepted
	if !ok {
		t.FailNow()
	}
	t.Cleanup(func() { s.Close(); p.Close() })
	return s.(*net.TCPConn), p.(*net.TCPConn)
}

func TestBoundSetsTheSocketOption(t *testing.T) {
	t.Parallel()
	server, _ := pair(t)
	if err := Bound(server, 1500*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	raw, err := server.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var got int
	var gerr error
	if err := raw.Control(func(fd uintptr) { got, gerr = unix.GetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_USER_TIMEOUT) }); err != nil || gerr != nil {
		t.Fatal(err, gerr)
	}
	if got != 1500 {
		t.Fatalf("TCP_USER_TIMEOUT = %d ms, want 1500", got)
	}
}

// 对端连上后再不读：服务端往它写，内核发送缓冲与对端接收缓冲填满后 Write 阻塞。设了上界的连接在上界之后由内核中止、
// Write 返回错误；对照组（没设）在同样长的时间里仍阻塞——内核自己放弃零窗口探测要十几分钟。
func TestBoundAbortsAWriteThePeerStopsReading(t *testing.T) {
	t.Parallel()
	const bound = time.Second
	const observe = 4 * time.Second
	for _, tc := range []struct {
		name  string
		bound bool
	}{{"bound", true}, {"control", false}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server, _ := pair(t)
			if tc.bound {
				if err := Bound(server, bound); err != nil {
					t.Fatal(err)
				}
			}
			done := make(chan error, 1)
			go func() {
				buf := make([]byte, 1<<20)
				for {
					if _, err := server.Write(buf); err != nil {
						done <- err
						return
					}
				}
			}()
			select {
			case err := <-done:
				if !tc.bound {
					t.Fatalf("the unbounded write ended within %v: %v", observe, err)
				}
				t.Logf("write failed as intended: %v", err)
			case <-time.After(observe):
				if tc.bound {
					t.Fatalf("the bounded write was still blocked %v after a %v bound", observe, bound)
				}
			}
		})
	}
}
