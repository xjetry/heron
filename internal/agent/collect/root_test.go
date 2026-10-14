package collect

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/agent/collect/collecttest"
	"github.com/xjetry/heron/internal/clock"
)

// deploymentMarkerNames 是 deployenv.Read 读的全部名字：读者根上只允许出现这些。
var deploymentMarkerNames = []string{".dockerenv", "run/.containerenv", "run/systemd/container", "proc/1/environ"}

// 一个完整采集周期（识别、两轮 Metrics、Facts）里 ProcFS 对两个根用到的每个名字都相对根：没有绝对路径、
// 没有 ".."——绝对路径在 openat2 的根内解析下会被当成根内路径，但它说明调用方以为自己在读 "/"，
// 而根是 --host-root 时那是另一棵树。读者根只读部署标识，其余全部经宿主根。
func TestProcFSUsesOnlyRootRelativeNames(t *testing.T) {
	p := hostProc(os.DirFS("testdata/docker-debian"), noDisk)
	reader := collecttest.New(fstest.MapFS{".dockerenv": {}})
	p.Reader = reader
	clk := clock.NewFake(time.Unix(0, 0))
	c := &Collector{Host: p, Clock: clk, Version: "t"}
	c.Metrics(c.Identify())
	clk.Advance(2 * time.Second)
	snap := c.Identify()
	c.Metrics(snap)
	c.Facts(snap)

	host := fixtureOf(p).Names()
	for _, name := range host {
		if !fs.ValidPath(name) || name == "." {
			t.Errorf("host root was asked for %q, which is not a root-relative name", name)
		}
	}
	if !slices.Contains(host, "proc/meminfo") || !slices.Contains(host, "sys/class/net") {
		t.Fatalf("the cycle did not go through the host root (saw %v); the check above would pass vacuously", host)
	}
	got := reader.Names()
	if len(got) == 0 {
		t.Fatal("the reader root was never read; the container-evidence check below would pass vacuously")
	}
	for _, name := range got {
		if !slices.Contains(deploymentMarkerNames, name) {
			t.Errorf("reader root was asked for %q; only deployment markers describe the reader", name)
		}
	}
}

// 宿主根上的标识属于宿主，读者根上的标识属于 agent：执行环境说明只看读者根，两个方向都钉住。
func TestContainerSignalComesFromReaderRoot(t *testing.T) {
	notesWith := func(host, reader fstest.MapFS) []heronv1.ScopeNote {
		p := hostProc(host, noDisk)
		p.Reader = collecttest.New(reader)
		return p.identify().exec.GetNotes()
	}
	base := func() fstest.MapFS {
		return fstest.MapFS{
			"proc/stat":    {Data: statLines("cpu  1 1 1 1 1 1 1 1 0 0\n", 4)},
			"proc/meminfo": {Data: []byte("MemTotal: 1024 kB\nMemAvailable: 512 kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\n")},
			"proc/loadavg": {Data: []byte("0 0 0 1/1 1\n")},
			"proc/cpuinfo": {Data: cpuinfoProcessors(4)},
		}
	}
	// 容器里的 agent 读宿主根：宿主没有任何标识，读者（镜像）有 .dockerenv。
	if n := notesWith(base(), fstest.MapFS{".dockerenv": {}}); !slices.Contains(n, heronv1.ScopeNote_SCOPE_NOTE_CONTAINER_SIGNAL_ON_HOST_ROOT) {
		t.Errorf("notes = %v, want container_signal_on_host_root from the reader's .dockerenv", n)
	}
	// 反方向：宿主自己是 LXC guest、根上有 Docker 留下的标识，读者没有任何标识——不记说明。
	host := base()
	host[".dockerenv"] = &fstest.MapFile{}
	host["run/systemd/container"] = &fstest.MapFile{Data: []byte("lxc\n")}
	host["proc/1/environ"] = &fstest.MapFile{Data: []byte("container=lxc\x00")}
	if n := notesWith(host, fstest.MapFS{}); slices.Contains(n, heronv1.ScopeNote_SCOPE_NOTE_CONTAINER_SIGNAL_ON_HOST_ROOT) {
		t.Errorf("notes = %v: markers under the host root describe the host, not the agent", n)
	}
}

// 虚拟化描述被观测主机：只从宿主根读，读者镜像里的标识与 PID 1 环境不改变它。
func TestVirtualizationIgnoresReaderRoot(t *testing.T) {
	reader := collecttest.New(fstest.MapFS{
		".dockerenv":     {},
		"proc/1/environ": {Data: []byte("container=docker\x00")},
	})
	for name, tc := range map[string]struct {
		host fstest.MapFS
		want string
	}{
		"host is lxc":      {fstest.MapFS{"proc/1/environ": {Data: []byte("container=lxc\x00")}}, "lxc"},
		"host has no hint": {fstest.MapFS{}, ""},
	} {
		p := plainProc(tc.host)
		p.Reader = reader
		if got := p.facts().virtualization; got != tc.want {
			t.Errorf("%s: virtualization = %q, want %q", name, got, tc.want)
		}
	}
}

// netnsRoots 构造自检用的一对根：宿主根下是否有 proc/stat，两个根下 lo 的身份。
func netnsRoots(withProcStat bool, readerLo, hostLo string) (root, reader *collecttest.Root) {
	m := fstest.MapFS{}
	if withProcStat {
		m["proc/stat"] = &fstest.MapFile{Data: []byte("cpu  1 1 1 1\n")}
	}
	id := func(v string) func(string) (string, uint64, error) {
		return func(name string) (string, uint64, error) {
			if name != netnsProbe {
				return "", 0, fs.ErrNotExist
			}
			if v == "" {
				return "", 0, fs.ErrPermission
			}
			dev, ino, _ := strings.Cut(v, "/")
			n := uint64(0)
			for _, c := range ino {
				n = n*10 + uint64(c-'0')
			}
			return dev, n, nil
		}
	}
	root, reader = collecttest.New(m), collecttest.New(fstest.MapFS{})
	root.StatIDFunc, reader.StatIDFunc = id(hostLo), id(readerLo)
	return root, reader
}

func TestCheckHostRoot(t *testing.T) {
	const (
		notMounted = "the host's proc/stat is not readable under it"
		otherNetns = "this process is not in the host network namespace"
		readerErr  = "cannot identify this process's network namespace"
		hostErr    = "cannot identify the host's network namespace"
	)
	mountHint := hintMountRootFor("/hostfs")
	if !strings.Contains(mountHint, "docker run -v /:/hostfs:ro") {
		t.Fatalf("mount hint %q does not name the bind mount", mountHint)
	}
	for name, tc := range map[string]struct {
		procStat         bool
		readerLo, hostLo string
		want, hint       string // want 为空 = 应通过
	}{
		"host network namespace":     {true, "0:376/1423928", "0:376/1423928", "", ""},
		"bridge network":             {true, "0:400/1425825", "0:376/1423928", otherNetns, hintHostNetwork},
		"same device, other inode":   {true, "0:376/1425825", "0:376/1423928", otherNetns, hintHostNetwork},
		"same inode, other device":   {true, "0:400/1423928", "0:376/1423928", otherNetns, hintHostNetwork},
		"host root not mounted":      {false, "0:376/1423928", "0:376/1423928", notMounted, mountHint},
		"mount checked before netns": {false, "0:400/1425825", "0:376/1423928", notMounted, mountHint},
		"reader lo unreadable":       {true, "", "0:376/1423928", readerErr, hintHostNetwork},
		"host lo unreadable":         {true, "0:376/1423928", "", hostErr, hintHostNetwork},
		// 两边都读不出时两个空身份"相等"：读取失败必须先于比较按失败处理。
		"both lo unreadable": {true, "", "", readerErr, hintHostNetwork},
	} {
		root, reader := netnsRoots(tc.procStat, tc.readerLo, tc.hostLo)
		err := checkHostRoot("/hostfs", root, reader)
		if tc.want == "" {
			if err != nil {
				t.Errorf("%s: %v", name, err)
			}
			continue
		}
		var hre *HostRootError
		if !errors.As(err, &hre) {
			t.Errorf("%s: err = %v, want *HostRootError", name, err)
			continue
		}
		if msg := err.Error(); !strings.HasPrefix(msg, "--host-root /hostfs: "+tc.want) || !strings.HasSuffix(msg, "; "+tc.hint) {
			t.Errorf("%s: %q, want %q … %q", name, msg, tc.want, tc.hint)
		}
	}
}

// 采集对文件系统的路径型系统调用只有根对象能发：宿主口径的读数若有一处绕开 Root（直接 stat、statfs、
// open 一个绝对路径），--host-root 下它读到的是容器自己的树，而且不报错。darwin 的磁盘用量是唯一例外：
// darwin 不接受 --host-root。
func TestFilesystemSyscallsOnlyInRootObject(t *testing.T) {
	pathCalls := map[string]map[string]bool{
		"unix":    {"Open": true, "Openat": true, "Openat2": true, "Stat": true, "Lstat": true, "Statfs": true, "Fstatat": true, "Readlink": true, "Access": true, "Faccessat": true},
		"os":      {"Open": true, "OpenFile": true, "Create": true, "Stat": true, "Lstat": true, "ReadFile": true, "ReadDir": true, "DirFS": true, "Readlink": true, "OpenRoot": true, "OpenInRoot": true},
		"syscall": {"Open": true, "Openat": true, "Stat": true, "Lstat": true, "Statfs": true},
	}
	allowed := []string{"platform_darwin.go unix.Statfs", "root_linux.go unix.Open", "root_linux.go unix.Openat"}
	var got []string
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pathCalls[pkg.Name][sel.Sel.Name] {
				got = append(got, name+" "+pkg.Name+"."+sel.Sel.Name)
			}
			return true
		})
	}
	slices.Sort(got)
	if !slices.Equal(got, allowed) {
		t.Fatalf("path-based filesystem calls = %v, want exactly %v", got, allowed)
	}
}

// 根对象只有一条打开路径（dirRoot.openat），它的每个调用者、根目录的那一次打开、openat2 的每个
// OpenHow 都只用约定的只读标志常量；常量本身不含写标志由 root_linux_test 在 Linux 上按位检查。
// 这里按源码检查，任何平台都跑：新增一个带别的标志的打开路径即红。
func TestRootOpensOnlyWithReadOnlyFlags(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "root_linux.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	src := func(e ast.Expr) string {
		var b strings.Builder
		printer.Fprint(&b, token.NewFileSet(), e)
		return b.String()
	}
	counts := map[string]int{}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			sel, ok := n.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch call := src(sel); call {
			case "r.openat":
				counts[call]++
				if a := src(n.Args[2]); a != "rootReadFlags" && a != "rootPathFlags" {
					t.Errorf("r.openat called with flags %s", a)
				}
			case "unix.Open":
				counts[call]++
				if a := src(n.Args[1]); a != "rootDirFlags" {
					t.Errorf("unix.Open called with flags %s", a)
				}
			case "unix.Openat":
				counts[call]++
				if a := src(n.Args[2]); a != "flags" {
					t.Errorf("unix.Openat called with flags %s, want the openat parameter", a)
				}
			}
		case *ast.CompositeLit:
			if src(n.Type) != "unix.OpenHow" {
				return true
			}
			counts["unix.OpenHow"]++
			for _, el := range n.Elts {
				kv := el.(*ast.KeyValueExpr)
				switch key, val := src(kv.Key), src(kv.Value); {
				case key == "Flags" && val != "rootPathFlags" && val != "uint64(flags)":
					t.Errorf("OpenHow.Flags = %s", val)
				case key == "Resolve" && val != "rootResolve":
					t.Errorf("OpenHow.Resolve = %s", val)
				}
			}
		}
		return true
	})
	// 四处 openat 调用（Open、Stat、StatID、FSType）、根目录一次 Open、回退一次 Openat、探测与正常各一个 OpenHow。
	want := map[string]int{"r.openat": 4, "unix.Open": 1, "unix.Openat": 1, "unix.OpenHow": 2}
	if !maps.Equal(counts, want) {
		t.Fatalf("open sites = %v, want %v; a new open path must be added to this check deliberately", counts, want)
	}
}
