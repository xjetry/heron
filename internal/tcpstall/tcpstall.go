// Package tcpstall 给已接受的 TCP 连接设一个"对端停止接收"的上界。
//
// http.Server 的 ReadHeaderTimeout、ReadTimeout 与 IdleTimeout 只约束读请求与 keep-alive 空闲；响应写阻塞在内核发送缓冲上
// 没有任何超时。对端不再读响应（接收窗口一直关着）或不再确认数据时，Write 一直不返回，这条连接的 goroutine 与文件
// 描述符就一直占着；内核自己放弃零窗口探测要十几分钟。Linux 的 TCP_USER_TIMEOUT 让内核在已发出的数据连续这么久没被
// 确认时中止连接，阻塞的 Write 随之返回错误，上层按普通写失败收尾。
package tcpstall

import (
	"errors"
	"math"
	"net"
	"time"
)

// Bound 给 c 设上界：对端连续 d 不确认已发出的数据（含接收窗口一直关着）时内核中止连接。它只管写侧的停滞，
// 不限制一次传输的总时长：只要对端持续确认，多慢都不受影响。d 必须为正且不超过套接字选项的毫秒上限。
// 只对 *net.TCPConn 生效，其它连接类型原样返回 nil。Linux 之外没有对应的套接字选项，Bound 是空操作并返回 nil：
// hub 只部署在 Linux 上，别的平台是开发机。
func Bound(c net.Conn, d time.Duration) error {
	ms, err := millis(d)
	if err != nil {
		return err
	}
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return nil
	}
	return bound(tc, ms)
}

// 套接字选项按无符号 32 位毫秒计；0 表示取消上界，这里不接受。
func millis(d time.Duration) (int, error) {
	ms := d.Milliseconds()
	if ms <= 0 || ms > math.MaxUint32 {
		return 0, errors.New("stall timeout must be between 1ms and 2^32-1 ms: " + d.String())
	}
	return int(ms), nil
}
