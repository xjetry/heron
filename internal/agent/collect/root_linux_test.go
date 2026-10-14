//go:build linux

package collect

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/xjetry/heron/internal/clock"
)

// 根对象的三组打开标志不含任何写语义；TestRootOpensOnlyWithReadOnlyFlags 保证根对象只用这三组。
func TestRootFlagConstantsAreReadOnly(t *testing.T) {
	const write = unix.O_WRONLY | unix.O_RDWR | unix.O_CREAT | unix.O_TRUNC | unix.O_APPEND | unix.O_EXCL
	for name, f := range map[string]int{"rootReadFlags": rootReadFlags, "rootPathFlags": rootPathFlags, "rootDirFlags": rootDirFlags} {
		if f&write != 0 || f&unix.O_TMPFILE == unix.O_TMPFILE {
			t.Errorf("%s = %#x carries a write flag", name, f)
		}
	}
	if rootResolve&(unix.RESOLVE_IN_ROOT|unix.RESOLVE_NO_MAGICLINKS) != unix.RESOLVE_IN_ROOT|unix.RESOLVE_NO_MAGICLINKS {
		t.Fatalf("rootResolve = %#x, want RESOLVE_IN_ROOT|RESOLVE_NO_MAGICLINKS", rootResolve)
	}
}

// 生产根打开的文件在内核里是只读的：写入失败，F_GETFL 的访问模式是 O_RDONLY。
func TestRootFilesAreOpenedReadOnly(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o666); err != nil {
		t.Fatal(err)
	}
	r, err := openRoot(dir, true, unix.Openat2)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	f, err := r.Open("f")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fl, err := unix.FcntlInt(f.(*os.File).Fd(), unix.F_GETFL, 0)
	if err != nil {
		t.Fatal(err)
	}
	if fl&unix.O_ACCMODE != unix.O_RDONLY || fl&(unix.O_APPEND|unix.O_TRUNC) != 0 {
		t.Fatalf("F_GETFL = %#x, want read-only", fl)
	}
	if _, err := f.(*os.File).Write([]byte("y")); err == nil {
		t.Fatal("a file opened through the root accepted a write")
	}
}

// 根内解析：绝对链接、相对链接、链式都落在根内；".." 停在根；循环报 ELOOP；不合法的名字
// （绝对路径、".." 段）在系统调用之前拒绝；magic link 拒绝。
func TestRootResolvesInsideRoot(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "target"), []byte("OUTSIDE"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	// 根内 <outside>/target 有一份 "INSIDE"：绝对链接若按进程根解析会读到 "OUTSIDE"。
	must(os.MkdirAll(filepath.Join(dir, outside), 0o755))
	must(os.WriteFile(filepath.Join(dir, outside, "target"), []byte("INSIDE"), 0o644))
	must(os.Symlink(filepath.Join(outside, "target"), filepath.Join(dir, "abs")))
	must(os.Symlink("abs", filepath.Join(dir, "chain")))
	must(os.Symlink(strings.Repeat("../", 20)+strings.TrimPrefix(filepath.Join(outside, "target"), "/"), filepath.Join(dir, "up")))
	must(os.Symlink("loop2", filepath.Join(dir, "loop1")))
	must(os.Symlink("loop1", filepath.Join(dir, "loop2")))
	r, err := openRoot(dir, true, unix.Openat2)
	must(err)
	defer r.Close()
	for _, name := range []string{"abs", "chain", "up"} {
		b, err := fs.ReadFile(r, name)
		if err != nil || string(b) != "INSIDE" {
			t.Errorf("%s: %q, %v; want the copy inside the root", name, b, err)
		}
	}
	if _, err := r.Open("loop1"); !errors.Is(err, unix.ELOOP) {
		t.Errorf("loop: %v, want ELOOP", err)
	}
	for _, name := range []string{outside + "/target", "../target", "a/../abs", ""} {
		if _, err := r.Open(name); !errors.Is(err, fs.ErrInvalid) {
			t.Errorf("Open(%q) = %v, want fs.ErrInvalid", name, err)
		}
		if _, _, err := r.StatID(name); !errors.Is(err, fs.ErrInvalid) {
			t.Errorf("StatID(%q) = %v, want fs.ErrInvalid", name, err)
		}
	}
	sys, err := openRoot("/", false, unix.Openat2)
	must(err)
	defer sys.Close()
	if b, err := fs.ReadFile(sys, "proc/self/status"); err != nil || !strings.Contains(string(b), "Pid:") {
		t.Errorf("proc/self through the root: %v", err)
	}
	for _, name := range []string{"proc/self/root/etc/hostname", "proc/self/cwd", "proc/self/fd/0", "proc/self/exe"} {
		if _, err := sys.Open(name); !errors.Is(err, unix.ELOOP) {
			t.Errorf("magic link %s: %v, want ELOOP", name, err)
		}
	}
}

// Stat 与 StatID 不要求读权限：与 os.Stat 相同，存在却不可读的文件照样 stat 得到。
func TestRootStatDoesNotNeedReadPermission(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every file; the permission case needs an unprivileged user")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "secret"), nil, 0o000); err != nil {
		t.Fatal(err)
	}
	r, err := openRoot(dir, true, unix.Openat2)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Open("secret"); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("Open = %v, want EACCES (the case is vacuous otherwise)", err)
	}
	if _, err := fs.Stat(r, "secret"); err != nil {
		t.Errorf("Stat: %v", err)
	}
	if _, _, err := r.StatID("secret"); err != nil {
		t.Errorf("StatID: %v", err)
	}
}

func failOpenat2(errno unix.Errno) openat2Func {
	return func(int, string, *unix.OpenHow) (int, error) { return -1, errno }
}

// 初始化策略：显式根遇到任何 openat2 失败都拒绝启动（ENOSYS 说明内核版本，其余说明 seccomp），
// 绝不回退；默认根只对 ENOSYS 回退到普通 openat 并照常读到文件，EPERM、EINVAL 原样报错且不是 HostRootError。
func TestOpenRootInitPolicy(t *testing.T) {
	base := countFDs(t)
	for name, tc := range map[string]struct {
		explicit bool
		errno    unix.Errno
		hint     string // 非空：要 HostRootError 且含此提示
		fallback bool   // 默认根回退后能读
	}{
		"explicit ENOSYS": {true, unix.ENOSYS, hintKernel, false},
		"explicit EPERM":  {true, unix.EPERM, hintSeccomp, false},
		"explicit EINVAL": {true, unix.EINVAL, hintSeccomp, false},
		"default ENOSYS":  {false, unix.ENOSYS, "", true},
		"default EPERM":   {false, unix.EPERM, "", false},
		"default EINVAL":  {false, unix.EINVAL, "", false},
	} {
		r, err := openRoot("/", tc.explicit, failOpenat2(tc.errno))
		if tc.fallback {
			if err != nil {
				t.Errorf("%s: %v, want a fallback root", name, err)
				continue
			}
			if b, err := fs.ReadFile(r, "proc/self/status"); err != nil || !strings.Contains(string(b), "Pid:") {
				t.Errorf("%s: fallback read: %v", name, err)
			}
			r.Close()
			continue
		}
		if err == nil {
			r.Close()
			t.Errorf("%s: openRoot succeeded; openat2 failing with %v must not fall back", name, tc.errno)
			continue
		}
		if !errors.Is(err, tc.errno) {
			t.Errorf("%s: %v does not carry %v", name, err, tc.errno)
		}
		_, isHRE := errors.AsType[*HostRootError](err)
		switch {
		case tc.hint != "" && (!isHRE || !strings.Contains(err.Error(), tc.hint)):
			t.Errorf("%s: %v, want HostRootError naming %q", name, err, tc.hint)
		case tc.hint == "" && isHRE:
			t.Errorf("%s: %v: the default root is not a --host-root contract", name, err)
		}
	}
	if got := countFDs(t); got != base {
		t.Fatalf("open fds %d → %d: a failed initialization leaked the root fd", base, got)
	}
}

func countFDs(t *testing.T) int {
	t.Helper()
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	return len(ents)
}

// 根 fd 的生命周期：构造—关闭反复做不泄漏；构造在任一步失败（根打不开、根下没有 proc/stat、
// 网络命名空间不符）也不泄漏；Collector.Close 关掉两个根。
func TestProcFSRootLifecycle(t *testing.T) {
	notMounted := t.TempDir()
	otherNetns := t.TempDir()
	for _, d := range []string{"proc", "sys/class/net/lo"} {
		if err := os.MkdirAll(filepath.Join(otherNetns, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(otherNetns, "proc/stat"), []byte("cpu  1 1 1 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	base := countFDs(t)
	for range 50 {
		c, err := NewPlatform("t", clock.Real(), nil, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		if got := countFDs(t); got != base+2 {
			t.Fatalf("a constructed collector holds %d fds, want 2 (host root and reader root)", got-base)
		}
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		for _, dir := range []string{filepath.Join(notMounted, "missing"), notMounted, otherNetns} {
			if _, err := NewPlatform("t", clock.Real(), nil, nil, dir); err == nil {
				t.Fatalf("--host-root %s was accepted", dir)
			}
		}
	}
	if got := countFDs(t); got != base {
		t.Fatalf("open fds %d → %d after 50 rounds of construction", base, got)
	}
	// 宿主根已打开、读者根的探测失败：已打开的宿主根也要关掉。
	probes := 0
	secondFails := func(dirfd int, path string, how *unix.OpenHow) (int, error) {
		if probes++; probes%2 == 0 {
			return -1, unix.EPERM
		}
		return unix.Openat2(dirfd, path, how)
	}
	for range 50 {
		if p, err := newProcFS(notMounted, secondFails); err == nil {
			p.close()
			t.Fatal("construction succeeded although the reader root's probe failed")
		}
	}
	if got := countFDs(t); got != base {
		t.Fatalf("open fds %d → %d after the reader root failed to open", base, got)
	}
}

// 显式根上的自检对真实的树：本进程自己的根作为宿主根通过；一棵带 proc/stat 却不是宿主 sysfs 的树
// 被认出"不在宿主网络命名空间"；根目录不存在或没有 proc/stat 是"没挂宿主根"。
func TestHostRootSelfCheckOnRealTrees(t *testing.T) {
	c, err := NewPlatform("t", clock.Real(), nil, nil, "/")
	if err != nil {
		t.Fatalf("this process's own root must pass as a host root: %v", err)
	}
	c.Close()
	fake := t.TempDir()
	for _, d := range []string{"proc", "sys/class/net/lo"} {
		if err := os.MkdirAll(filepath.Join(fake, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(fake, "proc/stat"), []byte("cpu  1 1 1 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	empty := t.TempDir()
	for dir, hint := range map[string]string{
		fake:                            hintHostNetwork,
		empty:                           "docker run -v /:" + empty + ":ro",
		filepath.Join(empty, "missing"): "docker run -v /:" + filepath.Join(empty, "missing") + ":ro",
	} {
		_, err := NewPlatform("t", clock.Real(), nil, nil, dir)
		if _, ok := errors.AsType[*HostRootError](err); !ok || !strings.Contains(err.Error(), hint) {
			t.Errorf("--host-root %s: %v, want HostRootError naming %q", dir, err, hint)
		}
	}
}
