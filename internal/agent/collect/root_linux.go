//go:build linux

package collect

import (
	"errors"
	"io/fs"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// 根对象的全部打开参数。dirRoot 的每一次打开都经 openat，openat 只接受下面两组标志之一，
// 根目录本身只用 rootDirFlags 打开一次：没有任何写标志，agent 不写被观测的树由此承载——
// 容器的 -v …:ro 是否对子挂载递归取决于 Docker 版本（同一内核上 20.10.24 不递归、29.9.0 递归），不能依赖。
const (
	// rootReadFlags 打开要读内容的文件与目录。
	rootReadFlags = unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOCTTY
	// rootPathFlags 只为 fstat / fstatfs 取一个引用：O_PATH 不要求读权限，与 stat(2) 对权限的要求相同，
	// 存在却不可读的文件（如非 root 看 proc/1/environ）照样 stat 得到。
	rootPathFlags = unix.O_PATH | unix.O_CLOEXEC
	rootDirFlags  = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC
	// rootResolve 让解析停在根内：绝对符号链接与 ".." 都以根目录为界；magic link（proc/<pid>/root、
	// cwd、exe、fd/*、ns/*）一律拒绝，它们会把解析带出根。proc/self、proc/net 是普通符号链接：
	// 宿主 procfs 实例按宿主的 pid 命名空间解析 self，得到的是读者自己的进程，proc/self/mountinfo
	// 因而是读者的挂载表，proc/net 是读者网络命名空间的表。
	rootResolve = unix.RESOLVE_IN_ROOT | unix.RESOLVE_NO_MAGICLINKS
)

// rootRetries 是 openat 对 EAGAIN 与 EINTR 的重试上限。RESOLVE_IN_ROOT 在解析 ".." 期间撞上系统里
// 并发的 rename 或挂载变化时返回 EAGAIN，由调用者重试（openat2(2)）；sysfs 的网卡、块设备条目都是带
// ".." 的相对链接，常规读取就会经过这条路径。EINTR 的重试与 os.Open 相同。
const rootRetries = 16

type openat2Func func(dirfd int, path string, how *unix.OpenHow) (int, error)

// dirRoot 是 Root 在 Linux 上的实现：持有根目录的 fd，每次打开都相对它。
//
// 初始化策略在 openRoot 里定一次，之后不变：openat2 可用就始终用它；只有未显式指定的根（"/"）在内核
// 没有 openat2（ENOSYS，内核 < 5.6）时改用普通 openat——根是 "/" 时 RESOLVE_IN_ROOT 与普通解析结果相同，
// 这是 5.6 之前内核上的既有行为。显式的根没有等价的普通解析（绝对链接会落到容器自己的根里），所以拒绝启动。
type dirRoot struct {
	fd      int
	plain   bool
	openat2 openat2Func
}

// openRoot 打开 dir 作为根并探测一次 openat2。显式的根（--host-root）的任何失败都是 HostRootError；
// 默认根只对 ENOSYS 回退，EPERM（seccomp 拦截）、EINVAL 等原样报错，不当成缺文件、不扩大回退范围。
// 失败时已打开的根 fd 在返回前关闭。
func openRoot(dir string, explicit bool, openat2 openat2Func) (*dirRoot, error) {
	fd, err := unix.Open(dir, rootDirFlags, 0)
	if err != nil {
		err = &fs.PathError{Op: "open", Path: dir, Err: err}
		if explicit {
			return nil, &HostRootError{Dir: dir, Err: err, Hint: hintMountRootFor(dir)}
		}
		return nil, err
	}
	r := &dirRoot{fd: fd, openat2: openat2}
	pfd, err := r.openat2(fd, ".", &unix.OpenHow{Flags: rootPathFlags, Resolve: rootResolve})
	switch {
	case err == nil:
		unix.Close(pfd)
		return r, nil
	case errors.Is(err, unix.ENOSYS) && !explicit:
		r.plain = true
		return r, nil
	}
	unix.Close(fd)
	err = &fs.PathError{Op: "openat2", Path: dir, Err: err}
	switch {
	case !explicit:
		return nil, err
	case errors.Is(err, unix.ENOSYS):
		return nil, &HostRootError{Dir: dir, Err: err, Hint: hintKernel}
	default:
		return nil, &HostRootError{Dir: dir, Err: err, Hint: hintSeccomp}
	}
}

// openat 是 dirRoot 唯一的打开路径；flags 只能是 rootReadFlags 或 rootPathFlags。
func (r *dirRoot) openat(op, name string, flags int) (int, error) {
	if !fs.ValidPath(name) {
		return -1, &fs.PathError{Op: op, Path: name, Err: fs.ErrInvalid}
	}
	var err error
	for range rootRetries {
		var fd int
		if r.plain {
			fd, err = unix.Openat(r.fd, name, flags, 0)
		} else {
			fd, err = r.openat2(r.fd, name, &unix.OpenHow{Flags: uint64(flags), Resolve: rootResolve})
		}
		if err == nil {
			return fd, nil
		}
		if !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EINTR) {
			break
		}
	}
	return -1, &fs.PathError{Op: op, Path: name, Err: err}
}

func (r *dirRoot) Open(name string) (fs.File, error) {
	fd, err := r.openat("open", name, rootReadFlags)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

// Stat 让 fs.Stat 不经读打开：存在却不可读的文件也 stat 得到，与 os.DirFS 相同。
func (r *dirRoot) Stat(name string) (fs.FileInfo, error) {
	fd, err := r.openat("stat", name, rootPathFlags)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	return f.Stat()
}

func (r *dirRoot) StatID(name string) (string, uint64, error) {
	fd, err := r.openat("stat", name, rootPathFlags)
	if err != nil {
		return "", 0, err
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return "", 0, &fs.PathError{Op: "stat", Path: name, Err: err}
	}
	return devKey(st.Dev), st.Ino, nil
}

func (r *dirRoot) FSType(name string) (uint64, error) {
	fd, err := r.openat("statfs", name, rootPathFlags)
	if err != nil {
		return 0, err
	}
	defer unix.Close(fd)
	var st unix.Statfs_t
	if err := unix.Fstatfs(fd, &st); err != nil {
		return 0, &fs.PathError{Op: "statfs", Path: name, Err: err}
	}
	return uint64(st.Type), nil
}

func (r *dirRoot) Usage() (uint64, uint64, error) {
	var st unix.Statfs_t
	if err := unix.Fstatfs(r.fd, &st); err != nil {
		return 0, 0, &fs.PathError{Op: "statfs", Path: ".", Err: err}
	}
	total, used := statfsUsage(&st)
	return total, used, nil
}

// Close 关闭根 fd；重复调用无副作用。
func (r *dirRoot) Close() error {
	if r.fd < 0 {
		return nil
	}
	err := unix.Close(r.fd)
	r.fd = -1
	return err
}

// devKey 把 userspace 的 st_dev 编码折成 "major:minor"（与 mountinfo 第 3 列、
// findmnt 的 MAJ:MIN 同构）。Go 的 Sys() 直接给内核译好的 dev_t，不经过 glibc。
func devKey(dev uint64) string {
	return strconv.FormatUint(uint64(unix.Major(dev)), 10) + ":" + strconv.FormatUint(uint64(unix.Minor(dev)), 10)
}
