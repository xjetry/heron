package deploy

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// installer 描述一个安装脚本在替身主机上的形态，供三个脚本共用的完整性用例逐个套用：
// 脚本只按写进自己的清单校验下载的包，源码脚本不安装，没有 --version。
type installer struct {
	script string
	// fresh 是一台发布了 v1、尚未安装的主机：dist 里有本机的包，run 执行写入后的脚本。
	fresh func(t *testing.T) *env
	// pkg 是这台主机要下载的包名；build 把 version 的这个包打进 dir，不写入脚本。
	pkg   string
	build func(e *env, dir, version string)
	// creds 是首次安装除 --base-url 之外必需的参数；uninstall 是卸载的完整参数。
	creds, uninstall []string
	// stop 是停服务那一条调用的前缀；bin 是装好的二进制（相对根目录），内容里带打包时的版本号。
	stop, bin string
}

var installers = []installer{
	{
		script: "install.sh",
		fresh: func(t *testing.T) *env {
			e := newLinuxHost(t)
			e.appendTo("etc/passwd", "heron-agent:x:480:480::/nonexistent:/usr/sbin/nologin\n")
			e.appendTo("etc/group", "heron-agent:x:480:\n")
			e.linuxRelease("amd64", "v1")
			return e
		},
		pkg:       "heron-agent_linux_amd64.tar.gz",
		build:     func(e *env, dir, version string) { e.linuxPackage(dir, "amd64", version) },
		creds:     []string{"--hub", "http://hub.test", "--key", "k"},
		uninstall: []string{"--uninstall"},
		stop:      "systemctl stop heron-agent",
		bin:       "usr/local/bin/heron-agent",
	},
	{
		script:    "install-macos.sh",
		fresh:     newEnv,
		pkg:       "heron-agent_darwin_arm64.tar.gz",
		build:     func(e *env, dir, version string) { e.darwinPackage(dir, "arm64", version) },
		creds:     []string{"--hub", "http://hub.test", "--key", "k"},
		uninstall: []string{"--uninstall"},
		stop:      "launchctl bootout",
		bin:       "usr/local/bin/heron-agent",
	},
	{
		script:    "install-hub.sh",
		fresh:     newHubHost,
		pkg:       "heron-hub_linux_amd64.tar.gz",
		build:     func(e *env, dir, version string) { e.hubPackage(dir, version) },
		uninstall: []string{"--uninstall", "--yes"},
		stop:      "systemctl stop heron-hub",
		bin:       "usr/local/bin/heron-hub",
	},
}

func (in installer) install(e *env, baseURL string, extra ...string) (string, int) {
	args := append(slices.Clone(in.creds), "--base-url", baseURL)
	return e.run(append(args, extra...)...)
}

// installed 在 fresh 主机上装好 v1 并在运行，然后发布 v2、清空调用记录，等用例重跑。
func (in installer) installed(t *testing.T) *env {
	t.Helper()
	e := in.fresh(t)
	if out, code := in.install(e, "file://"+e.dist); code != 0 {
		t.Fatalf("first install exit %d:\n%s", code, out)
	}
	in.build(e, e.dist, "v2")
	e.publish("v2")
	e.resetCalls()
	return e
}

// assertNothingDownloaded 断言没有发出任何下载：三个脚本的下载都经 curl（替身 fileCurl 记下每一次调用）。
func assertNothingDownloaded(t *testing.T, e *env, out string) {
	t.Helper()
	if i := index(e.calls(), "curl "); i >= 0 {
		t.Fatalf("downloaded before refusing: %q\n%s", e.calls()[i], out)
	}
}

// assertStillV1 断言正在运行的 v1 没有被碰：没发停服务、二进制还是 v1。
func (in installer) assertStillV1(t *testing.T, e *env, out string) {
	t.Helper()
	if index(e.calls(), in.stop) >= 0 {
		t.Fatalf("the running service was stopped: %q\n%s", e.calls(), out)
	}
	if !strings.Contains(e.file(in.bin), "# v1 ") {
		t.Fatalf("the binary was replaced:\n%s\n%s", e.file(in.bin), out)
	}
}

func forEachInstaller(t *testing.T, f func(t *testing.T, in installer)) {
	for _, in := range installers {
		t.Run(in.script, func(t *testing.T) {
			t.Parallel()
			f(t, in)
		})
	}
}

// 仓库里的源码脚本没有写入清单：在任何下载之前拒绝安装，并指向 release 里的脚本；卸载不下载，照常可用。
func TestSourceScriptRefusesToInstall(t *testing.T) {
	t.Parallel()
	forEachInstaller(t, func(t *testing.T, in installer) {
		e := in.installed(t)
		e.script = e.source
		out, code := in.install(e, "file://"+e.dist)
		if code != 1 || !strings.Contains(out, "this "+in.script+" has no embedded release checksums") || !strings.Contains(out, "https://github.com/xjetry/heron/releases") {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		assertNothingDownloaded(t, e, out)
		in.assertStillV1(t, e, out)
		out, code = e.run(in.uninstall...)
		if code != 0 || !strings.Contains(out, "uninstalled") || e.exists(in.bin) {
			t.Fatalf("uninstall with the source script: exit %d:\n%s", code, out)
		}
	})
}

// dist 里的包换成另一份内容，脚本里的清单没变：按内嵌值校验失败，旧服务照常运行、二进制不被替换。
func TestTamperedPackageIsRefused(t *testing.T) {
	t.Parallel()
	forEachInstaller(t, func(t *testing.T, in installer) {
		e := in.installed(t)
		in.build(e, e.dist, "evil")
		out, code := in.install(e, "file://"+e.dist)
		if code != 1 || !strings.Contains(out, "checksum mismatch for "+in.pkg) {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		in.assertStillV1(t, e, out)
	})
}

// --base-url 指向另一个目录，里面是篡改过的包和与之相符的 SHA256SUMS：这正是内嵌清单要挡住的情形，
// 下载目录里的 SHA256SUMS 不是校验依据。
func TestBaseURLWithMatchingSumsCannotChangeTheBytes(t *testing.T) {
	t.Parallel()
	forEachInstaller(t, func(t *testing.T, in installer) {
		e := in.installed(t)
		mirror := filepath.Join(t.TempDir(), "mirror")
		if err := os.Mkdir(mirror, 0o755); err != nil {
			t.Fatal(err)
		}
		in.build(e, mirror, "evil")
		e.sums(mirror)
		out, code := in.install(e, "file://"+mirror)
		if code != 1 || !strings.Contains(out, "checksum mismatch for "+in.pkg+" from file://"+mirror) {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		in.assertStillV1(t, e, out)
	})
}

// 清单里没有本机要的包：在任何下载之前失败。包本身仍在下载目录里，下载得到；脚本不看目录里有什么。
func TestManifestWithoutThisPackageIsRefused(t *testing.T) {
	t.Parallel()
	forEachInstaller(t, func(t *testing.T, in installer) {
		e := in.installed(t)
		own, other := filepath.Join(e.dist, in.pkg), filepath.Join(e.dist, "probe-other_linux_s390x.tar.gz")
		if err := os.Rename(own, other); err != nil {
			t.Fatal(err)
		}
		e.publish("v2")
		if err := os.Rename(other, own); err != nil {
			t.Fatal(err)
		}
		out, code := in.install(e, "file://"+e.dist)
		if code != 1 || !strings.Contains(out, "release v2 has no embedded checksum for "+in.pkg) {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		assertNothingDownloaded(t, e, out)
		in.assertStillV1(t, e, out)
	})
}

// 没有 --version：给了即报错并指向版本化的脚本 URL，什么都不下载。
func TestVersionFlagIsRefused(t *testing.T) {
	t.Parallel()
	forEachInstaller(t, func(t *testing.T, in installer) {
		e := in.installed(t)
		for _, v := range [][]string{{"--version", "v1"}, {"--version=v1"}, {"--version"}} {
			out, code := in.install(e, "file://"+e.dist, v...)
			if code != 2 || !strings.Contains(out, "has no --version") || !strings.Contains(out, "https://github.com/xjetry/heron/releases/download/<tag>/"+in.script) {
				t.Fatalf("%q: exit %d:\n%s", v, code, out)
			}
		}
		assertNothingDownloaded(t, e, "")
		in.assertStillV1(t, e, "")
	})
}

// 不给 --base-url 时下载目录是脚本所属版本的 release，而不是 latest。
func TestDefaultDownloadDirIsTheEmbeddedVersion(t *testing.T) {
	t.Parallel()
	forEachInstaller(t, func(t *testing.T, in installer) {
		e := in.installed(t)
		// 默认地址是 https，Linux 的两个脚本先确认有 CA 证书包。
		e.put("etc/ssl/certs/ca-certificates.crt", "")
		e.publish("v9.8.7")
		out, code := e.run(in.creds...)
		if code == 0 {
			t.Fatalf("the curl stub refuses https, the install must fail:\n%s", out)
		}
		want := "https://github.com/xjetry/heron/releases/download/v9.8.7/" + in.pkg
		i := index(e.calls(), "curl ")
		if i < 0 || !strings.HasSuffix(e.calls()[i], " "+want) {
			t.Fatalf("want a download of %s, calls %q\n%s", want, e.calls(), out)
		}
	})
}

// agent 的两个脚本：--insecure-http 首次安装交给 register，重跑沿用配置时在停服务之前交给 configure；
// 不给时两条命令行都不带它。
var agentInstallers = installers[:2]

func TestAgentReRegistration(t *testing.T) {
	t.Parallel()
	for _, in := range agentInstallers {
		for _, existing := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/existing=%t", in.script, existing), func(t *testing.T) {
				t.Parallel()
				var e *env
				const config = `{"hub":"old","token":"old-token","probe_interval":"17s"}`
				if existing {
					e = in.installed(t)
					e.put("etc/heron-agent/config.json", config)
				} else {
					e = in.fresh(t)
				}
				out, code := e.run("--base-url", "file://"+e.dist, "--re-register", "--hub", "http://new-hub.test", "--key", "new-key", "--name", "new-name", "--insecure-http")
				if code != 0 || !strings.Contains(out, "heron-agent installed and started") {
					t.Fatalf("exit %d:\n%s", code, out)
				}
				want := "heron-agent register --hub http://new-hub.test --key new-key --config " + e.root + "/etc/heron-agent/config.json --name new-name --insecure-http"
				if !slices.Contains(e.calls(), want) {
					t.Fatalf("want registration %q, calls %q", want, e.calls())
				}
				if existing {
					b, err := os.ReadFile(filepath.Join(e.state, "registration-input"))
					if err != nil || string(b) != config {
						t.Fatalf("register must receive the unchanged existing config: %q, %v", b, err)
					}
					if index(e.calls(), "heron-agent register") > index(e.calls(), in.stop) {
						t.Fatalf("registration must precede stopping the old service: %q", e.calls())
					}
				}
			})
		}
	}
}

func TestAgentUpgradeDoesNotReRegister(t *testing.T) {
	t.Parallel()
	for _, in := range agentInstallers {
		t.Run(in.script, func(t *testing.T) {
			t.Parallel()
			e := in.installed(t)
			config := e.file("etc/heron-agent/config.json")
			out, code := e.run("--base-url", "file://"+e.dist, "--hub", "http://new-hub.test", "--key", "new-key")
			if code != 0 || !strings.Contains(out, "--key ignored") {
				t.Fatalf("exit %d:\n%s", code, out)
			}
			if index(e.calls(), "heron-agent register") >= 0 || e.file("etc/heron-agent/config.json") != config {
				t.Fatalf("an ordinary upgrade must preserve registration and not consume the key: %q", e.calls())
			}
		})
	}
}

func TestAgentReRegistrationRequiresCredentialsBeforeChanges(t *testing.T) {
	t.Parallel()
	for _, in := range agentInstallers {
		for _, args := range [][]string{
			{"--re-register"},
			{"--re-register", "--hub", "http://hub.test"},
			{"--re-register", "--key", "k"},
			{"--re-register", "--hub", "", "--key", "k"},
			{"--re-register", "--hub", "http://hub.test", "--key", ""},
		} {
			t.Run(in.script+"/"+strings.Join(args, "_"), func(t *testing.T) {
				t.Parallel()
				e := in.installed(t)
				config := e.file("etc/heron-agent/config.json")
				out, code := e.run(append(args, "--base-url", "file://"+e.dist)...)
				if code != 2 || !strings.Contains(out, "--hub and --key are required for --re-register") {
					t.Fatalf("exit %d:\n%s", code, out)
				}
				if strings.Join(e.calls(), "") != "" || e.file("etc/heron-agent/config.json") != config {
					t.Fatalf("invalid credentials must be refused before external operations or config changes: %q", e.calls())
				}
				in.assertStillV1(t, e, out)
			})
		}
	}
}

func TestInsecureHTTPReachesRegisterOnFirstInstall(t *testing.T) {
	t.Parallel()
	for _, in := range agentInstallers {
		for _, insecure := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/insecure=%t", in.script, insecure), func(t *testing.T) {
				t.Parallel()
				e := in.fresh(t)
				var extra []string
				want := "heron-agent register --hub http://hub.test --key k --config " + e.root + "/etc/heron-agent/config.json"
				if insecure {
					extra = []string{"--insecure-http"}
					want += " --insecure-http"
				}
				out, code := in.install(e, "file://"+e.dist, extra...)
				if code != 0 {
					t.Fatalf("exit %d:\n%s", code, out)
				}
				c := e.calls()
				if !slices.Contains(c, want) {
					t.Fatalf("want the call %q, got %q", want, c)
				}
				if i := index(c, "heron-agent configure"); i >= 0 {
					t.Fatalf("a first install registers; configure is for reruns: %q", c)
				}
			})
		}
	}
}

func TestInsecureHTTPReachesConfigureBeforeStoppingOnRerun(t *testing.T) {
	t.Parallel()
	forAgents := func(t *testing.T, f func(t *testing.T, in installer)) {
		for _, in := range agentInstallers {
			t.Run(in.script, func(t *testing.T) {
				t.Parallel()
				f(t, in)
			})
		}
	}
	t.Run("given", func(t *testing.T) {
		t.Parallel()
		forAgents(t, func(t *testing.T, in installer) {
			e := in.installed(t)
			out, code := in.install(e, "file://"+e.dist, "--insecure-http")
			if code != 0 {
				t.Fatalf("exit %d:\n%s", code, out)
			}
			c := e.calls()
			want := "heron-agent configure --config " + e.root + "/etc/heron-agent/config.json --insecure-http=true"
			conf, stop := slices.Index(c, want), index(c, in.stop)
			if conf < 0 || stop < 0 || conf > stop {
				t.Fatalf("want %q before %q, calls %q", want, in.stop, c)
			}
			if index(c, "heron-agent register") >= 0 {
				t.Fatalf("a rerun must not register: %q", c)
			}
		})
	})
	t.Run("omitted", func(t *testing.T) {
		t.Parallel()
		forAgents(t, func(t *testing.T, in installer) {
			e := in.installed(t)
			if out, code := in.install(e, "file://"+e.dist); code != 0 {
				t.Fatalf("exit %d:\n%s", code, out)
			}
			if i := index(e.calls(), "heron-agent configure"); i >= 0 {
				t.Fatalf("no --insecure-http, no configure: %q", e.calls())
			}
		})
	})
	t.Run("configure fails", func(t *testing.T) {
		t.Parallel()
		forAgents(t, func(t *testing.T, in installer) {
			e := in.installed(t)
			e.vars = []string{"STUB_CONFIGURE_FAILS=1"}
			out, code := in.install(e, "file://"+e.dist, "--insecure-http")
			if code == 0 || !strings.Contains(out, "configure: invalid config") {
				t.Fatalf("exit %d:\n%s", code, out)
			}
			in.assertStillV1(t, e, out)
		})
	})
}
