package main

import (
	"context"
	"net"
)

// listenConfig 是 hub 全部 TCP 监听的配置。Go 1.24 起 Linux 上的监听默认请求 MPTCP（ListenConfig.MultipathTCP()
// 默认为 true，go1.27.4 实测），内核不支持或未启用时回退普通 TCP；于是同一个二进制在不同内核上拿到的套接字协议不同。
// hub 不从多路径获益——对端是反代与 agent 的单路径连接——而连接层的写侧上界（tcpstall 设的 TCP_USER_TIMEOUT）
// 只在普通 TCP 上实测过，MPTCP 套接字上它是否生效取决于内核版本。显式关闭，让监听在任何内核上都是普通 TCP，
// 与验证过的形态一致。listen_test.go 的源码扫描保证 hub 不在 listenTCP 之外新建 TCP 监听。
func listenConfig() *net.ListenConfig {
	var lc net.ListenConfig
	lc.SetMultipathTCP(false)
	return &lc
}

// listenTCP 按 listenConfig 在 addr 上建 TCP 监听。地址口径与 net.Listen("tcp", addr) 相同。
func listenTCP(ctx context.Context, addr string) (net.Listener, error) {
	return listenConfig().Listen(ctx, "tcp", addr)
}
