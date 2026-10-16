package update

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/xjetry/heron/internal/releasesig"
	"golang.org/x/sys/unix"
)

type systemMachine struct {
	role, dir, run, bin, unit string
	uid, gid                  int
}

func (m *systemMachine) command(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "/usr/bin/systemctl", args...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"}
	b, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("systemctl: %w: %s", err, boundedError(string(b)))
	}
	return strings.TrimSpace(string(b)), nil
}

func (m *systemMachine) Current(ctx context.Context) (string, error) {
	props, err := m.command(ctx, "show", m.unit, "--property=User,Group,ExecStart,ExecStartPre,ExecStartPost,ExecStop,ExecStopPost,DropInPaths,MainPID")
	if err != nil {
		return "", err
	}
	p := map[string]string{}
	for line := range strings.SplitSeq(props, "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok {
			p[k] = v
		}
	}
	if p["User"] != "heron-"+m.role || p["Group"] != "heron-"+m.role || p["ExecStartPre"] != "" || p["ExecStartPost"] != "" || p["ExecStop"] != "" || p["ExecStopPost"] != "" || !strings.Contains(p["ExecStart"], "path="+m.bin+" ;") {
		return "", errors.New("unsupported custom systemd service; reinstall the official service")
	}
	pid, err := strconv.Atoi(p["MainPID"])
	if err != nil || pid <= 0 {
		return "", errors.New("service has no running main process")
	}
	actual, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil || actual != m.bin {
		return "", errors.New("service is not running the managed binary")
	}
	args, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return "", err
	}
	parts := strings.Split(strings.TrimRight(string(args), "\x00"), "\x00")
	key, want, verb := "config", agentConfigPath, "run"
	if m.role == "hub" {
		key, want, verb = "db", "/var/lib/heron/heron.db", "serve"
	}
	if len(parts) < 2 || parts[1] != verb {
		return "", errors.New("unsupported service command")
	}
	if !fixedArgument(parts[2:], key, want) {
		return "", errors.New("online update requires the installer's fixed data/config path")
	}
	f, err := safeFile(m.bin, 0)
	if err != nil {
		return "", err
	}
	f.Close()
	cmd := exec.CommandContext(ctx, m.bin, "version")
	// 版本查询不需要 root 权限；执行文件必须先确认由 root 管理且不可被服务用户写入。
	cmd.SysProcAttr = credentials(m.uid, m.gid)
	return commandOutput(cmd, m.bin)
}

// Go flag 同时接受单横线与双横线；按实际参数顺序解析，不能漏掉后面的别名覆盖或位置参数截断。
func fixedArgument(args []string, key, want string) bool {
	count := 0
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") || arg == "--" {
			return false
		}
		name, value, hasValue := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "-"), "=")
		if !hasValue {
			i++
			if i >= len(args) {
				return false
			}
			value = args[i]
		}
		if name == key {
			count++
			if value != want {
				return false
			}
		}
	}
	return count == 1
}

func (m *systemMachine) Stage(b []byte) (string, error) {
	if err := atomicWrite(filepath.Join(m.dir, "candidate"), b, 0700); err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
func (m *systemMachine) Stop(ctx context.Context) error {
	_, err := m.command(ctx, "stop", m.unit)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		var st unix.Stat_t
		if err := unix.Stat("/proc/"+entry.Name(), &st); err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			return err
		}
		if int(st.Uid) == m.uid {
			return errors.New("service user still owns a process after stop; refusing to modify its files")
		}
	}
	return nil
}
func (m *systemMachine) Start(ctx context.Context) error {
	if _, err := m.command(ctx, "start", "--no-block", m.unit); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	for {
		s, err := m.command(ctx, "show", m.unit, "--property=MainPID", "--value")
		if err == nil {
			pid, _ := strconv.Atoi(s)
			actual, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
			if err == nil && actual == m.bin {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return errors.New("service did not start the managed executable")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (m *systemMachine) Gate(closed bool) error {
	path := filepath.Join(m.dir, "pending")
	if closed {
		return atomicWrite(path, []byte("pending\n"), 0644)
	}
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	d, err := os.Open(m.dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (m *systemMachine) Backup() error {
	if err := copySafe(m.bin, filepath.Join(m.dir, "previous"), 0, 0700); err != nil {
		return err
	}
	present := []string{}
	if m.role == "hub" {
		for _, name := range []string{"heron.db", "heron.db-wal", "heron.db-shm"} {
			err := copySafe(filepath.Join("/var/lib/heron", name), filepath.Join(m.dir, name), m.uid, 0600)
			if errors.Is(err, os.ErrNotExist) && name != "heron.db" {
				continue
			}
			if err != nil {
				return err
			}
			present = append(present, name)
		}
	}
	b, _ := json.Marshal(present)
	return atomicWrite(filepath.Join(m.dir, "database.json"), b, 0600)
}

func (m *systemMachine) Install() error {
	return copySafe(filepath.Join(m.dir, "candidate"), m.bin, 0, 0755)
}

func (m *systemMachine) Restore() error {
	if m.role == "hub" {
		b, err := os.ReadFile(filepath.Join(m.dir, "database.json"))
		if err != nil {
			return err
		}
		var present []string
		if err = json.Unmarshal(b, &present); err != nil {
			return err
		}
		// 通过已打开且不跟随链接的目录描述符恢复；root 不打开服务用户可替换的目标文件。
		d, err := unix.Open("/var/lib/heron", unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		defer unix.Close(d)
		for _, name := range []string{"heron.db", "heron.db-wal", "heron.db-shm"} {
			if err = unix.Unlinkat(d, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
				return err
			}
		}
		for _, name := range present {
			if name != "heron.db" && name != "heron.db-wal" && name != "heron.db-shm" {
				return errors.New("invalid backup manifest")
			}
			f, err := os.Open(filepath.Join(m.dir, name))
			if err != nil {
				return err
			}
			fd, err := unix.Openat(d, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
			if err != nil {
				f.Close()
				return err
			}
			out := os.NewFile(uintptr(fd), name)
			_, err = io.Copy(out, f)
			f.Close()
			if err == nil {
				err = out.Chown(m.uid, m.gid)
			}
			if err == nil {
				err = out.Sync()
			}
			err = errors.Join(err, out.Close())
			if err != nil {
				return err
			}
		}
		if err = unix.Fsync(d); err != nil {
			return err
		}
	}
	return copySafe(filepath.Join(m.dir, "previous"), m.bin, 0, 0755)
}

func (m *systemMachine) Verify(ctx context.Context, pid int, digest string) error {
	s, err := m.command(ctx, "show", m.unit, "--property=MainPID", "--value")
	if err != nil {
		return err
	}
	if s != strconv.Itoa(pid) || pid <= 0 {
		return errors.New("readiness must come from the service main process")
	}
	f, err := os.Open(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != digest {
		return errors.New("running executable does not match verified release")
	}
	return nil
}

func safeFile(path string, uid int) (*os.File, error) {
	d, err := unix.Open(filepath.Dir(path), unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(d)
	fd, err := unix.Openat(d, filepath.Base(path), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	var st unix.Stat_t
	if err = unix.Fstat(fd, &st); err != nil {
		f.Close()
		return nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 || int(st.Uid) != uid || st.Mode&0022 != 0 {
		f.Close()
		return nil, errors.New("managed file must be regular, single-link, owned by its service identity and not group/world writable")
	}
	return f, nil
}

const (
	// agent 配置属服务用户、由 root 更新器解析：限量读取，文件被换成巨大文件也不会让 root 进程耗尽内存。
	maxAgentConfig  = 1 << 20
	maxSourceConfig = 4 << 10
)

// readSafe 经 safeFile（目录与文件都不跟随链接、普通文件、单链接、属主为 uid、不可被组与其他用户写）打开后限量读取。
func readSafe(path string, uid int, limit int64) ([]byte, error) {
	f, err := safeFile(path, uid)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s exceeds %d bytes", path, limit)
	}
	return b, nil
}

func copySafe(src, dst string, uid int, mode os.FileMode) error {
	f, err := safeFile(src, uid)
	if err != nil {
		return err
	}
	defer f.Close()
	out, err := os.CreateTemp(filepath.Dir(dst), ".heron-update-")
	if err != nil {
		return err
	}
	defer os.Remove(out.Name())
	err = out.Chmod(mode)
	if err == nil {
		_, err = io.Copy(out, f)
	}
	if err == nil {
		err = out.Sync()
	}
	err = errors.Join(err, out.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(out.Name(), dst); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(dst))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

type peerKey struct{}

// Serve 运行本机更新器。log 收事务引擎的状态迁移与失败原因（见 Engine.log），cmd/updater 指向 stderr。
func Serve(ctx context.Context, role string, log *slog.Logger) error {
	return serve(ctx, role, log, NewOfficialSource(), releasesig.Trusted())
}

// serve 的来源与公钥由调用方给出：正式入口只传固定官方源与 releasesig 的常量公钥，
// 隔离验收的测试程序传受控来源与测试公钥（accept_linux_test.go）。
func serve(ctx context.Context, role string, log *slog.Logger, official source, keys []ed25519.PublicKey) error {
	if role != "hub" && role != "agent" {
		return errors.New("role must be hub or agent")
	}
	if os.Geteuid() != 0 {
		return errors.New("updater requires root")
	}
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		return errors.New("online updates require systemd")
	}
	u, err := user.Lookup("heron-" + role)
	if err != nil {
		return err
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	m := &systemMachine{role: role, uid: uid, gid: gid, dir: "/var/lib/heron-update-" + role, run: "/run/heron-update-" + role, bin: "/usr/local/bin/heron-" + role, unit: "heron-" + role + ".service"}
	for _, dir := range []string{m.dir, m.run} {
		if err = os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		info, err := os.Lstat(dir)
		if err != nil {
			return err
		}
		var stat unix.Stat_t
		if err = unix.Lstat(dir, &stat); err != nil {
			return err
		}
		if !info.IsDir() || stat.Uid != 0 || stat.Mode&0022 != 0 {
			return errors.New("updater directories must be root-owned and not writable by other users")
		}
	}
	if err = os.Chmod(m.run, 0755); err != nil {
		return err
	}
	if err = os.Chmod(m.dir, 0755); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(m.dir, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return errors.New("updater is already running")
	}
	arch := runtime.GOARCH
	if arch == "arm" {
		arch = "armv7"
	}
	// hub 来源的 agent 配置按服务用户属主检查读取；来源配置是 root 的决定，按 root 属主检查。
	hub := NewHubSource(func() ([]byte, error) { return readSafe(agentConfigPath, uid, maxAgentConfig) })
	choice := chooseSource(role, func() ([]byte, error) { return readSafe(sourceConfigPath, 0, maxSourceConfig) }, official, hub)
	e, err := newEngine(ctx, log, filepath.Join(m.dir, "state.json"), role, arch, choice, keys, m)
	if err != nil {
		return err
	}
	path := Socket(role)
	if err = os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	defer l.Close()
	if err = os.Chown(path, 0, gid); err != nil {
		return err
	}
	if err = os.Chmod(path, 0660); err != nil {
		return err
	}
	srv := &http.Server{ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 5 * time.Second,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			uc, ok := c.(*net.UnixConn)
			if !ok {
				return ctx
			}
			raw, err := uc.SyscallConn()
			if err != nil {
				return ctx
			}
			var peer *unix.Ucred
			_ = raw.Control(func(fd uintptr) { peer, _ = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) })
			return context.WithValue(ctx, peerKey{}, peer)
		},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			peer, _ := r.Context().Value(peerKey{}).(*unix.Ucred)
			if peer == nil || r.Method != http.MethodPost {
				http.Error(w, "unauthorized", http.StatusForbidden)
				return
			}
			if r.URL.Path == "/maintenance" {
				if peer.Uid != 0 {
					http.Error(w, "unauthorized", http.StatusForbidden)
					return
				}
				if err := e.enterMaintenance(); err != nil {
					http.Error(w, err.Error(), http.StatusConflict)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte("{}"))
				return
			}
			if int(peer.Uid) != uid {
				http.Error(w, "unauthorized", http.StatusForbidden)
				return
			}
			dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
			dec.DisallowUnknownFields()
			var out any
			var err error
			switch r.URL.Path {
			case "/status":
				out = e.status()
			case "/submit":
				var q Request
				err = dec.Decode(&q)
				if err == nil {
					err = trailingJSON(dec)
				}
				if err == nil {
					out, err = e.submit(q)
				}
			case "/ready":
				var q struct {
					Version string `json:"version"`
				}
				err = dec.Decode(&q)
				if err == nil {
					err = trailingJSON(dec)
				}
				if err == nil {
					err = e.ready(r.Context(), int(peer.Pid), q.Version)
					out = e.status()
				}
			default:
				http.NotFound(w, r)
				return
			}
			if err != nil {
				http.Error(w, boundedError(err.Error()), http.StatusConflict)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(out)
		}),
	}
	go func() { <-ctx.Done(); _ = srv.Close() }()
	err = srv.Serve(l)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
