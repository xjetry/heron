package deploy

import (
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// realCurlEnv 把替身表里的 curl 换成记录参数后转调真实 curl 的包装：下载重试与超时用例直接跑真实 curl，
// 既能通过 calls 核对传出的超时与协议开关，又用真实网络行为覆盖退出码与 -w 输出；sleep 仍用替身，
// 五次重试不需要真的等满重试间隔。
func realCurlEnv(t *testing.T, in installer) *env {
	t.Helper()
	curlBin, err := exec.LookPath("curl")
	if err != nil {
		t.Fatalf("curl not found on PATH: %v", err)
	}
	e := in.fresh(t)
	var base map[string]string
	switch in.script {
	case "install.sh":
		base = linuxStubs
	case "install-macos.sh":
		base = stubs
	case "install-hub.sh":
		base = hubStubs
	default:
		t.Fatalf("unknown script %s", in.script)
	}
	stubs := maps.Clone(base)
	stubs["curl"] = "#!/bin/sh\necho \"curl $*\" >> \"$STUB_STATE/calls\"\nexec " + curlBin + " \"$@\"\n"
	e.bin = stubBin(t, stubs)
	return e
}

// servePackage 把 e.dist 里的包文件照文件名原样发给客户端。
func servePackage(w http.ResponseWriter, r *http.Request, dist string) {
	data, err := os.ReadFile(filepath.Join(dist, filepath.Base(r.URL.Path)))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// rawCloseListener 监听本地 TCP：前 failCount 次接受连接后立刻关闭（零字节响应，真实 curl 以 52 或 56 退出），
// 之后的连接按 HTTP 转发到 upstream。
func rawCloseListener(t *testing.T, failCount int32, upstream *httptest.Server) (string, *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var attempts atomic.Int32
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			n := attempts.Add(1)
			if n <= failCount {
				// 读掉请求头后不回任何字节直接关连接，复现建连/握手后零字节断开（curl 52/56）。
				_ = conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
				buf := make([]byte, 1024)
				_, _ = conn.Read(buf)
				conn.Close()
				continue
			}
			go func(c net.Conn) {
				defer c.Close()
				up, err := net.Dial("tcp", strings.TrimPrefix(upstream.URL, "http://"))
				if err != nil {
					return
				}
				defer up.Close()
				done := make(chan struct{}, 2)
				go func() {
					buf := make([]byte, 16*1024)
					for {
						n, err := c.Read(buf)
						if n > 0 {
							_, _ = up.Write(buf[:n])
						}
						if err != nil {
							done <- struct{}{}
							return
						}
					}
				}()
				go func() {
					buf := make([]byte, 16*1024)
					for {
						n, err := up.Read(buf)
						if n > 0 {
							_, _ = c.Write(buf[:n])
						}
						if err != nil {
							done <- struct{}{}
							return
						}
					}
				}()
				<-done
			}(conn)
		}
	}()
	return "http://" + ln.Addr().String(), &attempts
}

// 建连后零字节立即断开与 HTTP 502/503/504 属于瞬时故障：三个安装脚本都要以 5 秒间隔重试，直到成功或满 5 次放弃；
// 每次调用真 curl 都要带上默认的连接超时（30s）与低速中止（1024 B/s、60s）。
func TestDownloadRetriesOnZeroByteDisconnectAnd5xx(t *testing.T) {
	t.Parallel()
	forEachInstaller(t, func(t *testing.T, in installer) {
		t.Run("zero-byte disconnect succeeds on 3rd try", func(t *testing.T) {
			t.Parallel()
			e := realCurlEnv(t, in)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				servePackage(w, r, e.dist)
			}))
			defer upstream.Close()
			baseURL, attempts := rawCloseListener(t, 2, upstream)
			out, code := in.install(e, baseURL)
			if code != 0 {
				t.Fatalf("install should succeed after retries, exit %d:\n%s", code, out)
			}
			// install.sh 与 install-hub.sh 还会再下载更新器包（1 次），至少发出 3 次请求。
			if got := attempts.Load(); got < 3 {
				t.Fatalf("expected at least 3 attempts (2 disconnects + success), got %d:\n%s", got, out)
			}
			if !strings.Contains(out, "retrying in 5s (attempt 2 of 5)") || !strings.Contains(out, "retrying in 5s (attempt 3 of 5)") {
				t.Errorf("expected retry log lines, got:\n%s", out)
			}
			if index(e.calls(), "sleep 5") < 0 {
				t.Errorf("expected 5s retry delay, calls %q", e.calls())
			}
			i := index(e.calls(), "curl ")
			if i < 0 || !strings.Contains(e.calls()[i], "--connect-timeout 30") ||
				!strings.Contains(e.calls()[i], "--speed-limit 1024") || !strings.Contains(e.calls()[i], "--speed-time 60") {
				t.Errorf("expected default timeout flags on curl, calls %q", e.calls())
			}
		})

		t.Run("HTTP 503 retries and succeeds", func(t *testing.T) {
			t.Parallel()
			e := realCurlEnv(t, in)
			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if hits.Add(1) <= 2 {
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = w.Write([]byte("temporarily unavailable"))
					return
				}
				servePackage(w, r, e.dist)
			}))
			defer srv.Close()
			out, code := in.install(e, srv.URL)
			if code != 0 {
				t.Fatalf("install should succeed after 503 retries, exit %d:\n%s", code, out)
			}
			if got := hits.Load(); got < 3 {
				t.Fatalf("expected at least 3 hits, got %d:\n%s", got, out)
			}
			if !strings.Contains(out, "HTTP 503") || !strings.Contains(out, "retrying in 5s (attempt 3 of 5)") {
				t.Errorf("expected HTTP 503 retry message, got:\n%s", out)
			}
		})

		t.Run("persistent 503 gives up after 5 tries", func(t *testing.T) {
			t.Parallel()
			e := realCurlEnv(t, in)
			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte("still down"))
			}))
			defer srv.Close()
			out, code := in.install(e, srv.URL)
			if code == 0 {
				t.Fatalf("persistent 503 must fail:\n%s", out)
			}
			if got := hits.Load(); got != 5 {
				t.Fatalf("expected exactly 5 attempts, got %d:\n%s", got, out)
			}
			if !strings.Contains(out, "failed 5 times (HTTP 503); giving up") {
				t.Errorf("expected giving-up message, got:\n%s", out)
			}
		})
	})
}

// 收到部分正文后断开说明线路通但慢而不稳，重下大概率再断：不重试，并提示改用 --base-url 离线安装。
// 404 与连不上属于重来结果不变的故障：同样不重试，且不误报"收到部分正文后断开"。
func TestDownloadDoesNotRetryPartialBodyOrPermanentErrors(t *testing.T) {
	t.Parallel()
	forEachInstaller(t, func(t *testing.T, in installer) {
		t.Run("partial body disconnect", func(t *testing.T) {
			t.Parallel()
			e := realCurlEnv(t, in)
			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				hj, ok := w.(http.Hijacker)
				if !ok {
					t.Error("response writer does not support hijacking")
					return
				}
				conn, rw, err := hj.Hijack()
				if err != nil {
					return
				}
				_, _ = rw.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 10000\r\n\r\n")
				_, _ = rw.Write(make([]byte, 512))
				_ = rw.Flush()
				conn.Close()
			}))
			defer srv.Close()
			out, code := in.install(e, srv.URL)
			if code == 0 {
				t.Fatalf("partial body must fail:\n%s", out)
			}
			if got := hits.Load(); got != 1 {
				t.Fatalf("partial body disconnect must not retry, got %d requests:\n%s", got, out)
			}
			if strings.Contains(out, "retrying in") {
				t.Errorf("partial body must not retry:\n%s", out)
			}
			if !strings.Contains(out, "broke off after part of the file arrived") || !strings.Contains(out, "--base-url") {
				t.Errorf("expected offline-install hint for partial body, got:\n%s", out)
			}
		})

		t.Run("HTTP 404 does not retry", func(t *testing.T) {
			t.Parallel()
			e := realCurlEnv(t, in)
			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte("not found"))
			}))
			defer srv.Close()
			out, code := in.install(e, srv.URL)
			if code == 0 {
				t.Fatalf("404 must fail:\n%s", out)
			}
			if got := hits.Load(); got != 1 {
				t.Fatalf("404 must not retry, got %d requests:\n%s", got, out)
			}
			if strings.Contains(out, "broke off after part of the file arrived") {
				t.Errorf("404 must not be reported as partial body:\n%s", out)
			}
		})

		t.Run("connection refused does not retry", func(t *testing.T) {
			t.Parallel()
			e := realCurlEnv(t, in)
			// 占一个本地端口随即关掉，拿到一个未监听端口触发连接拒绝（curl 7）。
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			addr := ln.Addr().String()
			ln.Close()
			out, code := in.install(e, "http://"+addr)
			if code == 0 {
				t.Fatalf("connection refused must fail:\n%s", out)
			}
			var curlCalls int
			for _, c := range e.calls() {
				if strings.HasPrefix(c, "curl ") {
					curlCalls++
				}
			}
			if curlCalls != 1 {
				t.Fatalf("connection refused must not retry, got %d curl calls:\n%s", curlCalls, out)
			}
		})
	})
}

// 超时开关实测生效：把测试副本的超时常量缩到 1 秒后跑真 curl。
// - 连接超时：对端接受 TCP 却不回 TLS ServerHello，--connect-timeout 在约 1 秒后以退出码 28 终止且不重试。
// - 低速中止：服务器先发少量字节再卡住，--speed-limit/--speed-time 在约 1 秒后以退出码 28 终止、不重试并提示离线安装。
func TestDownloadTimeoutsEnforcedByRealCurl(t *testing.T) {
	t.Parallel()
	in := installers[0]

	t.Run("connect timeout during TLS handshake", func(t *testing.T) {
		t.Parallel()
		e := realCurlEnv(t, in)
		b, err := os.ReadFile(e.script)
		if err != nil {
			t.Fatal(err)
		}
		patched := strings.Replace(string(b), "DL_CONNECT_TIMEOUT=30", "DL_CONNECT_TIMEOUT=1", 1)
		if patched == string(b) {
			t.Fatal("DL_CONNECT_TIMEOUT=30 not found in script")
		}
		if err := os.WriteFile(e.script, []byte(patched), 0o755); err != nil {
			t.Fatal(err)
		}

		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		var accepted atomic.Int32
		var acceptedAt atomic.Int64
		done := make(chan struct{})
		defer close(done)
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				accepted.Add(1)
				acceptedAt.CompareAndSwap(0, time.Now().UnixNano())
				go func(conn net.Conn) {
					defer conn.Close()
					<-done
				}(c)
			}
		}()

		out, code := in.install(e, "https://"+ln.Addr().String())
		finishedAt := time.Now()
		if code == 0 {
			t.Fatalf("stalled TLS handshake must fail:\n%s", out)
		}
		if got := accepted.Load(); got != 1 {
			t.Fatalf("connect timeout must not retry, accepted %d connections:\n%s", got, out)
		}
		// 从服务端收到建连起算耗时，排除并发跑全套脚本测试时拉起 sh 与探测环境带来的排队延迟；
		// 15 秒远小于脚本默认值 30 秒，仍能照出 --connect-timeout 失效。
		if wait := finishedAt.Sub(time.Unix(0, acceptedAt.Load())); wait > 15*time.Second {
			t.Fatalf("connect timeout took too long after accept: %v\n%s", wait, out)
		}
	})

	t.Run("low-speed stall abort", func(t *testing.T) {
		t.Parallel()
		e := realCurlEnv(t, in)
		b, err := os.ReadFile(e.script)
		if err != nil {
			t.Fatal(err)
		}
		patched := strings.Replace(string(b), "DL_STALL_TIME=60", "DL_STALL_TIME=1", 1)
		if patched == string(b) {
			t.Fatal("DL_STALL_TIME=60 not found in script")
		}
		if err := os.WriteFile(e.script, []byte(patched), 0o755); err != nil {
			t.Fatal(err)
		}

		var hits atomic.Int32
		var hitAt atomic.Int64
		done := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			hitAt.CompareAndSwap(0, time.Now().UnixNano())
			w.Header().Set("Content-Length", "100000")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(make([]byte, 128))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			select {
			case <-done:
			case <-r.Context().Done():
			}
		}))
		defer srv.Close()
		defer close(done)

		out, code := in.install(e, srv.URL)
		finishedAt := time.Now()
		if code == 0 {
			t.Fatalf("stalled transfer must fail:\n%s", out)
		}
		if got := hits.Load(); got != 1 {
			t.Fatalf("low-speed abort must not retry, got %d hits:\n%s", got, out)
		}
		// 从服务端发出首包并开始停顿起算耗时，排除并发测试起进程的排队；15 秒远小于默认 60 秒。
		if wait := finishedAt.Sub(time.Unix(0, hitAt.Load())); wait > 15*time.Second {
			t.Fatalf("low-speed abort took too long after stall: %v\n%s", wait, out)
		}
		if !strings.Contains(out, "broke off after part of the file arrived") || !strings.Contains(out, "--base-url") {
			t.Errorf("stalled partial transfer must advise offline install, got:\n%s", out)
		}
	})
}

// is_https 不区分大小写：大写/混合大小写的 HTTPS:// 同样加 --proto '=https' --proto-redir '=https'，
// 在 install.sh 里 --hub HTTPS:// 也触发缺失 CA 证书包时的安装（OpenRC 路径不因 systemd 更新器强制装 CA，
// 因而能单独照出 --hub 的 scheme 判定）。
func TestIsHTTPSCaseInsensitive(t *testing.T) {
	t.Parallel()
	t.Run("base-url passes --proto =https for uppercase HTTPS", func(t *testing.T) {
		t.Parallel()
		forEachInstaller(t, func(t *testing.T, in installer) {
			for _, scheme := range []string{"HTTPS://", "HtTpS://"} {
				e := in.fresh(t)
				e.put("etc/ssl/certs/ca-certificates.crt", "")
				out, code := in.install(e, scheme+"mirror.example.test/rel")
				if code == 0 {
					t.Fatalf("%s: fileCurl must reject https:\n%s", scheme, out)
				}
				i := index(e.calls(), "curl ")
				if i < 0 || !strings.Contains(e.calls()[i], "--proto =https --proto-redir =https") {
					t.Fatalf("%s: expected --proto =https on curl, calls %q\n%s", scheme, e.calls(), out)
				}
			}
		})
	})

	t.Run("install.sh installs CA bundle for uppercase --hub HTTPS on OpenRC", func(t *testing.T) {
		t.Parallel()
		e := newOpenRCHost(t, true)
		os.Remove(filepath.Join(e.root, "etc/ssl/certs/ca-certificates.crt"))
		e.linuxRelease("amd64", "v1")
		out, code := e.run("--hub", "HTTPS://hub.example.test", "--key", "k", "--base-url", "file://"+e.dist)
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		if index(e.calls(), "apt-get install -y ca-certificates") < 0 {
			t.Fatalf("uppercase --hub HTTPS:// must trigger CA installation, calls %q", e.calls())
		}
	})
}

// pathWithoutCurl 构造不含 curl 的系统工具目录，供 wget 分支用例屏蔽宿主 /usr/bin/curl。
func pathWithoutCurl(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	tools := []string{
		"sh", "awk", "grep", "sed", "cat", "cp", "mv", "rm", "mkdir", "chmod",
		"ln", "tr", "wc", "tar", "sha256sum", "mktemp", "dirname", "find",
		"install", "touch", "sort", "cut", "readlink", "head", "tail", "uname",
	}
	for _, name := range tools {
		p, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		_ = os.Symlink(p, filepath.Join(dir, name))
	}
	return dir
}

// wget 分支（install.sh 与 install-hub.sh）：GNU wget 传 --tries=1 -q -T 60，busybox wget 不传 --tries；
// 输出文件为空时重试，已写出部分字节后失败则不重试并提示 --base-url。
func TestWgetBranchFlagsAndRetry(t *testing.T) {
	t.Parallel()
	sysDir := pathWithoutCurl(t)

	for _, in := range []installer{installers[0], installers[2]} {
		for _, busybox := range []bool{false, true} {
			mode := "gnu"
			if busybox {
				mode = "busybox"
			}
			t.Run(in.script+"/"+mode, func(t *testing.T) {
				t.Parallel()
				var base map[string]string
				if in.script == "install.sh" {
					base = linuxStubs
				} else {
					base = hubStubs
				}
				stubs := maps.Clone(base)
				delete(stubs, "curl")
				helpOut := "GNU Wget 1.21"
				if busybox {
					helpOut = "BusyBox v1.37.0 multi-call binary"
				}
				stubs["wget"] = `#!/bin/sh
cat > /dev/null
if [ "$1" = --help ]; then echo "` + helpOut + `"; exit 0; fi
echo "wget $*" >> "$STUB_STATE/calls"
out=""; url=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -O) out=$2; shift 2;;
    -T) shift 2;;
    -*) shift;;
    *) url=$1; shift;;
  esac
done
count_file=$STUB_STATE/wget-count
n=0; [ ! -f "$count_file" ] || n=$(cat "$count_file")
n=$((n + 1)); echo "$n" > "$count_file"
if [ -n "${STUB_WGET_PARTIAL-}" ]; then
  printf 'partial-bytes' > "$out"
  exit 1
fi
if [ -n "${STUB_WGET_FAIL_FIRST-}" ] && [ "$n" -le "$STUB_WGET_FAIL_FIRST" ]; then
  exit 1
fi
cp "${url#file://}" "$out"
`
				t.Run("zero-byte failure retries and succeeds", func(t *testing.T) {
					t.Parallel()
					e := in.fresh(t)
					e.bin = stubBin(t, stubs)
					e.vars = []string{"PATH=" + e.bin + string(os.PathListSeparator) + sysDir, "STUB_WGET_FAIL_FIRST=2"}
					out, code := in.install(e, "file://"+e.dist)
					if code != 0 {
						t.Fatalf("wget should succeed on 3rd try, exit %d:\n%s", code, out)
					}
					i := index(e.calls(), "wget ")
					if i < 0 {
						t.Fatalf("expected wget call, calls %q", e.calls())
					}
					call := e.calls()[i]
					if !strings.Contains(call, "-T 60") {
						t.Errorf("expected -T 60 in %q", call)
					}
					hasTries := strings.Contains(call, "--tries=1")
					if busybox && hasTries {
						t.Errorf("busybox wget must not receive --tries=1: %q", call)
					}
					if !busybox && !hasTries {
						t.Errorf("GNU wget must receive --tries=1: %q", call)
					}
					if !strings.Contains(out, "retrying in 5s (attempt 3 of 5)") {
						t.Errorf("expected retry log, got:\n%s", out)
					}
				})

				t.Run("partial file does not retry", func(t *testing.T) {
					t.Parallel()
					e := in.fresh(t)
					e.bin = stubBin(t, stubs)
					e.vars = []string{"PATH=" + e.bin + string(os.PathListSeparator) + sysDir, "STUB_WGET_PARTIAL=1"}
					out, code := in.install(e, "file://"+e.dist)
					if code == 0 {
						t.Fatalf("partial wget must fail:\n%s", out)
					}
					var wgetCalls int
					for _, c := range e.calls() {
						if strings.HasPrefix(c, "wget ") {
							wgetCalls++
						}
					}
					if wgetCalls != 1 {
						t.Fatalf("partial wget must not retry, got %d calls:\n%s", wgetCalls, out)
					}
					if !strings.Contains(out, "broke off after part of the file arrived") || !strings.Contains(out, "--base-url") {
						t.Errorf("expected offline-install hint, got:\n%s", out)
					}
				})
			})
		}
	}
}

// 启动时清理超过一天的暂存文件：只删符合 "<目标>.tmp.<纯数字>" 命名且不是符号链接的旧文件，
// 保留一天以内的暂存、非数字后缀文件以及符号链接（及其指向的目标）。
func TestStaleTempFilesCleanup(t *testing.T) {
	t.Parallel()
	oldTime := time.Now().Add(-25 * time.Hour)
	freshTime := time.Now().Add(-1 * time.Hour)

	setMtime := func(t *testing.T, p string, tm time.Time) {
		t.Helper()
		if err := os.Chtimes(p, tm, tm); err != nil {
			t.Fatal(err)
		}
	}

	targets := map[string][]string{
		"install.sh":       {"usr/local/bin/heron-agent", "usr/local/bin/heron-updater-agent", "etc/heron-update-agent/config.json"},
		"install-hub.sh":   {"usr/local/bin/heron-hub", "usr/local/bin/heron-updater-hub"},
		"install-macos.sh": {"usr/local/bin/heron-agent"},
	}

	forEachInstaller(t, func(t *testing.T, in installer) {
		e := in.fresh(t)
		var shouldDelete, shouldKeep []string
		symTarget := filepath.Join(e.root, "usr/local/bin/keep-target")
		e.put("usr/local/bin/keep-target", "precious")
		setMtime(t, symTarget, oldTime)
		shouldKeep = append(shouldKeep, "usr/local/bin/keep-target")

		for _, rel := range targets[in.script] {
			oldRel := rel + ".tmp.12345"
			e.put(oldRel, "stale")
			setMtime(t, filepath.Join(e.root, oldRel), oldTime)
			shouldDelete = append(shouldDelete, oldRel)

			freshRel := rel + ".tmp.67890"
			e.put(freshRel, "recent")
			setMtime(t, filepath.Join(e.root, freshRel), freshTime)
			shouldKeep = append(shouldKeep, freshRel)

			nonNumRel := rel + ".tmp.keep1"
			e.put(nonNumRel, "other")
			setMtime(t, filepath.Join(e.root, nonNumRel), oldTime)
			shouldKeep = append(shouldKeep, nonNumRel)

			symRel := rel + ".tmp.99999"
			if err := os.Symlink(symTarget, filepath.Join(e.root, symRel)); err != nil {
				t.Fatal(err)
			}
			shouldKeep = append(shouldKeep, symRel)
		}

		out, code := in.install(e, "file://"+e.dist)
		if code != 0 {
			t.Fatalf("install exit %d:\n%s", code, out)
		}
		for _, rel := range shouldDelete {
			if e.exists(rel) {
				t.Errorf("stale temp file %s must be removed", rel)
			}
		}
		for _, rel := range shouldKeep {
			if _, err := os.Lstat(filepath.Join(e.root, rel)); err != nil {
				t.Errorf("file %s must be kept: %v", rel, err)
			}
		}
		// 确认当前安装器的 .tmp.$$ 在退出时也已清理，没有遗留新的数字暂存。
		for _, rel := range targets[in.script] {
			matches, _ := filepath.Glob(filepath.Join(e.root, rel+".tmp.*"))
			for _, m := range matches {
				relMatch, _ := filepath.Rel(e.root, m)
				if !slices.Contains(shouldKeep, relMatch) {
					t.Errorf("unexpected leftover temp file %s", relMatch)
				}
			}
		}
	})
}
