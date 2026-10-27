//go:build linux

package tcpstall

import (
	"net"

	"golang.org/x/sys/unix"
)

func bound(c *net.TCPConn, ms int) error {
	raw, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	if err := raw.Control(func(fd uintptr) {
		serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_USER_TIMEOUT, ms)
	}); err != nil {
		return err
	}
	return serr
}
