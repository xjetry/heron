//go:build !linux

package tcpstall

import "net"

func bound(*net.TCPConn, int) error { return nil }
