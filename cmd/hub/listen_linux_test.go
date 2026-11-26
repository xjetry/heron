//go:build linux

package main

import (
	"net"
	"syscall"
	"testing"
)

// soProtocol 读监听套接字实际的协议号：6 是 TCP，262 是 MPTCP。
func soProtocol(t *testing.T, l net.Listener) int {
	t.Helper()
	rc, err := l.(*net.TCPListener).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var p int
	var serr error
	if err := rc.Control(func(fd uintptr) { p, serr = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_PROTOCOL) }); err != nil {
		t.Fatal(err)
	}
	if serr != nil {
		t.Fatal(serr)
	}
	return p
}

// 内核支持 MPTCP 时 Go 默认的监听是 MPTCP 套接字；listenTCP 在任何内核上都必须是普通 TCP。
// 默认监听的协议号只记日志作对照：它取决于运行测试的内核是否启用 MPTCP。
func TestListenTCPIsPlainTCPOnLinux(t *testing.T) {
	t.Parallel()
	l, err := listenTCP(t.Context(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if got := soProtocol(t, l); got != syscall.IPPROTO_TCP {
		t.Fatalf("listenTCP socket protocol = %d, want %d (TCP)", got, syscall.IPPROTO_TCP)
	}
	def, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer def.Close()
	t.Logf("default net.Listen socket protocol on this kernel: %d", soProtocol(t, def))
}
