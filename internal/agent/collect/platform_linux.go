//go:build linux

package collect

import (
	"golang.org/x/sys/unix"

	"github.com/xjetry/heron/internal/clock"
)

// NewPlatform 构造本平台的 Collector。hostRoot 为空时被观测主机就是本进程的根 "/"；非空是显式的宿主根
// （容器里的 --host-root），构造时做一次启动自检（checkHostRoot），任何不满足都返回 HostRootError。
// 两种情形走同一条代码路径，区别只在初始化策略（openRoot）与是否自检。
func NewPlatform(version string, clk clock.Clock, netInclude, netExclude []string, hostRoot string) (*Collector, error) {
	host, err := newProcFS(hostRoot, unix.Openat2)
	if err != nil {
		return nil, err
	}
	return &Collector{Host: host, Clock: clk, NetInclude: netInclude, NetExclude: netExclude, Version: version}, nil
}

// newProcFS 打开宿主根与读者根并在显式宿主根上自检；任一步失败都在返回前关闭已打开的根。
func newProcFS(hostRoot string, openat2 openat2Func) (*ProcFS, error) {
	dir, explicit := hostRoot, hostRoot != ""
	if !explicit {
		dir = "/"
	}
	root, err := openRoot(dir, explicit, openat2)
	if err != nil {
		return nil, err
	}
	reader, err := openRoot("/", false, openat2)
	if err != nil {
		root.Close()
		return nil, err
	}
	if explicit {
		if err := checkHostRoot(dir, root, reader); err != nil {
			root.Close()
			reader.Close()
			return nil, err
		}
	}
	return &ProcFS{Root: root, Reader: reader}, nil
}

// DefaultNetExclude 是本平台未给 --net-exclude 时不计入流量的网卡。
func DefaultNetExclude() []string { return linuxNetExclude }
