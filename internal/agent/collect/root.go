package collect

import (
	"errors"
	"fmt"
	"io/fs"
	"strconv"
)

// Root 是一棵目录树的只读入口：ProcFS 对这棵树的每一次读取都经它，名字一律相对根
// （fs.ValidPath：不以 "/" 开头、没有 ".." 段），实现对不合法的名字报 fs.ErrInvalid。
// Linux 的实现是 dirRoot（root_linux.go）：以 openat2 的 RESOLVE_IN_ROOT 在根内解析，
// 所以根目录不是 "/" 时，树里的绝对符号链接也落在根内。
type Root interface {
	fs.FS
	// StatID 取 name 的 st_dev（折成与 mountinfo 第 3 列同构的 "major:minor"）与 st_ino。
	StatID(name string) (dev string, ino uint64, err error)
	// FSType 取 name 所在文件系统的 statfs 类型。
	FSType(name string) (uint64, error)
	// Usage 取根目录所在文件系统的总量与已用量（字节），口径见 statfsUsage。
	Usage() (total, used uint64, err error)
	Close() error
}

// HostRootError 是显式给出的宿主根不满足契约：根打不开、内核或 seccomp 不让 openat2 在根内解析、
// 根下没有宿主的 procfs、或本进程不在宿主的网络命名空间里。agent 拒绝启动，cmd/agent 以退出码 2 退出；
// Hint 点名缺的运行参数。
type HostRootError struct {
	Dir  string
	Err  error
	Hint string
}

func (e *HostRootError) Error() string {
	return "--host-root " + e.Dir + ": " + e.Err.Error() + "; " + e.Hint
}

func (e *HostRootError) Unwrap() error { return e.Err }

// 宿主根自检与初始化失败的提示。容器运行约定（宿主根 bind 到 --host-root、--network host）是
// 这些提示的来源；文案有外部读者（镜像冒烟按原文匹配），改动时同步。
const (
	hintMountRoot   = "mount the host root there read-only (docker run -v /:%s:ro)"
	hintHostNetwork = "run the container in the host network namespace (docker run --network host)"
	hintKernel      = "--host-root needs openat2, which Linux provides from 5.6"
	hintSeccomp     = "openat2 must be allowed (a seccomp profile may be blocking it)"
)

func hintMountRootFor(dir string) string { return fmt.Sprintf(hintMountRoot, dir) }

// netnsProbe 是自检比较网络命名空间用的 sysfs 节点。sysfs 的超级块按挂载者的网络命名空间分实例，
// 每个命名空间的 lo 是独立的 net_device、独立的 kernfs 节点，所以两个根下的这个节点 (st_dev, st_ino)
// 相同当且仅当两边的 sysfs 绑定同一个网络命名空间；进程自己的 /sys 由运行时在进程所在的网络命名空间里
// 挂载，宿主根下的 sys 是宿主的。网卡增删不影响 lo。
const netnsProbe = "sys/class/net/lo"

// checkHostRoot 是显式宿主根的启动自检，任一读取失败都按失败处理：
//  1. 根下 proc/stat 可打开：宿主根已挂进来，而且带着宿主的 /proc。
//  2. reader（进程自己的根）与 root 下 netnsProbe 的 (st_dev, st_ino) 相等：本进程在宿主的网络命名空间里。
//     连接数、hostname、ICMP 可用性、本机与公网地址都随读者的网络命名空间走，命名空间不对它们会静默地
//     报成容器自己的。
//
// pid、cgroup 命名空间不检查：宿主根下的 procfs 与 cgroup2 实例本身给出宿主视图。
func checkHostRoot(dir string, root, reader Root) error {
	f, err := root.Open("proc/stat")
	if err != nil {
		return &HostRootError{Dir: dir, Err: fmt.Errorf("the host's proc/stat is not readable under it: %w", err), Hint: hintMountRootFor(dir)}
	}
	f.Close()
	rdev, rino, err := reader.StatID(netnsProbe)
	if err != nil {
		return &HostRootError{Dir: dir, Err: fmt.Errorf("cannot identify this process's network namespace: %w", err), Hint: hintHostNetwork}
	}
	hdev, hino, err := root.StatID(netnsProbe)
	if err != nil {
		return &HostRootError{Dir: dir, Err: fmt.Errorf("cannot identify the host's network namespace: %w", err), Hint: hintHostNetwork}
	}
	if rdev != hdev || rino != hino {
		return &HostRootError{Dir: dir, Err: errors.New("this process is not in the host network namespace (" + netnsProbe + " is " +
			rdev + "/" + strconv.FormatUint(rino, 10) + " in the process root, " + hdev + "/" + strconv.FormatUint(hino, 10) + " under the host root)"), Hint: hintHostNetwork}
	}
	return nil
}
