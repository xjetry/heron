package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/xjetry/heron/internal/releasesig"
	"github.com/xjetry/heron/internal/releasesig/sigtest"
)

var (
	fetchLinux  = []string{"amd64", "arm64", "armv7", "386", "riscv64"}
	fetchDarwin = []string{"amd64", "arm64"}
)

// newServer 按路径 /<version>/<名字> 返回表里的字节，requests 统计收到的请求数（预发布版本必须在校验阶段
// 就被拒绝，一个请求都不该发出）。
func newServer(t *testing.T, files map[string][]byte) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	var mu sync.Mutex
	mux := http.NewServeMux()
	for path, body := range files {
		mux.HandleFunc("/"+path, func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			requests.Add(1)
			mu.Unlock()
			w.Write(body)
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &requests
}

// bundleSums 生成 sha256sum 格式的清单：agent 组每个 tar 包名配一个任意合法摘要，两个安装脚本配真实摘要，
// drop 指定的名字不写。清单经 sigtest.Sign 签名后交给服务端。
func bundleSums(t *testing.T, installers map[string][]byte, drop string) []byte {
	t.Helper()
	var b strings.Builder
	for _, name := range agentBundle(fetchLinux, fetchDarwin) {
		if name == drop {
			continue
		}
		if body, ok := installers[name]; ok {
			sum := sha256.Sum256(body)
			fmt.Fprintf(&b, "%s  %s\n", hex.EncodeToString(sum[:]), name)
			continue
		}
		fmt.Fprintf(&b, "%s  %s\n", strings.Repeat("a", 64), name)
	}
	return []byte(b.String())
}

// serveRelease 把 sums（签名按 signVersion 计算）与安装脚本放进服务端，返回可作 base 用的地址与请求计数。
func serveRelease(t *testing.T, version, signVersion string, sums []byte, installers map[string][]byte) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	files := map[string][]byte{
		version + "/SHA256SUMS":     sums,
		version + "/SHA256SUMS.sig": sigtest.Sign(signVersion, sums),
	}
	for name, body := range installers {
		files[version+"/"+name] = body
	}
	return newServer(t, files)
}

// fetchInto 在临时目录上执行 fetchInstallers：出错时目录里不得写出任何文件。
func fetchInto(t *testing.T, srv *httptest.Server, version string) error {
	t.Helper()
	pub, _ := sigtest.Key()
	dir := t.TempDir()
	err := fetchInstallers(context.Background(), srv.Client(), srv.URL+"/", []ed25519.PublicKey{pub}, version, dir, fetchLinux, fetchDarwin)
	if err != nil {
		entries, readErr := os.ReadDir(dir)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if len(entries) != 0 {
			t.Errorf("%s: dir has entries after failure: %v", t.Name(), entries)
		}
	}
	return err
}

func testInstallers() map[string][]byte {
	return map[string][]byte{
		"install.sh":       []byte("#!/bin/sh\n# linux installer v1.2.3\n"),
		"install-macos.sh": []byte("#!/bin/sh\n# macos installer v1.2.3\n"),
	}
}

func TestFetchWritesVerifiedInstallers(t *testing.T) {
	installers := testInstallers()
	srv, _ := serveRelease(t, "v1.2.3", "v1.2.3", bundleSums(t, installers, ""), installers)
	dir := t.TempDir()
	pub, _ := sigtest.Key()
	if err := fetchInstallers(context.Background(), srv.Client(), srv.URL+"/", []ed25519.PublicKey{pub}, "v1.2.3", dir, fetchLinux, fetchDarwin); err != nil {
		t.Fatalf("fetchInstallers: %v", err)
	}
	for name, want := range installers {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: fetched %q, want the server bytes verbatim", name, got)
		}
	}
}

func TestFetchRejectsSignatureForAnotherVersion(t *testing.T) {
	installers := testInstallers()
	// 签名对 v1.2.2 的清单有效：被签消息里的版本不是 v1.2.3，对本次取回的验签必须失败。
	srv, _ := serveRelease(t, "v1.2.3", "v1.2.2", bundleSums(t, installers, ""), installers)
	if err := fetchInto(t, srv, "v1.2.3"); err == nil || !strings.Contains(err.Error(), "verify") {
		t.Fatalf("fetchInstallers with a signature for another version = %v, want a verification error", err)
	}
}

func TestFetchRejectsIncompleteAgentBundle(t *testing.T) {
	installers := testInstallers()
	const missing = "heron-agent_linux_riscv64.tar.gz"
	srv, _ := serveRelease(t, "v1.2.3", "v1.2.3", bundleSums(t, installers, missing), installers)
	if err := fetchInto(t, srv, "v1.2.3"); err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("fetchInstallers with an incomplete bundle = %v, want an error naming %s", err, missing)
	}
}

func TestFetchRejectsInstallerHashMismatch(t *testing.T) {
	installers := testInstallers()
	sums := bundleSums(t, installers, "")
	tampered := make(map[string][]byte, len(installers))
	for name, body := range installers {
		tampered[name] = body
	}
	// 服务端的 install.sh 与清单里的摘要不符：传输不能参与信任，摘要不符即失败。
	tampered["install.sh"] = append(bytes.Clone(installers["install.sh"]), []byte("# tampered\n")...)
	srv, _ := serveRelease(t, "v1.2.3", "v1.2.3", sums, tampered)
	if err := fetchInto(t, srv, "v1.2.3"); err == nil || !strings.Contains(err.Error(), "install.sh") {
		t.Fatalf("fetchInstallers with a tampered installer = %v, want an error naming install.sh", err)
	}
}

func TestFetchRejectsDuplicateSumsEntry(t *testing.T) {
	installers := testInstallers()
	sums := append(bytes.Clone(bundleSums(t, installers, "")), []byte(fmt.Sprintf("%s  install.sh\n", strings.Repeat("a", 64)))...)
	srv, _ := serveRelease(t, "v1.2.3", "v1.2.3", sums, installers)
	if err := fetchInto(t, srv, "v1.2.3"); err == nil || !strings.Contains(err.Error(), "install.sh") {
		t.Fatalf("fetchInstallers with a duplicated sums entry = %v, want an error naming install.sh", err)
	}
}

func TestFetchRejectsNonStableVersion(t *testing.T) {
	installers := testInstallers()
	srv, requests := serveRelease(t, "v1.2.3", "v1.2.3", bundleSums(t, installers, ""), installers)
	if err := fetchInto(t, srv, "v1.2.3-rc.1"); err == nil || !strings.Contains(err.Error(), "stable") {
		t.Fatalf("fetchInstallers for a prerelease = %v, want an error about a stable release", err)
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("prerelease rejection made %d HTTP requests, want 0", n)
	}
}

func TestFetchRejectsOversizedSums(t *testing.T) {
	installers := testInstallers()
	files := map[string][]byte{
		"v1.2.3/SHA256SUMS":     bytes.Repeat([]byte("a"), releasesig.MaxSums+1),
		"v1.2.3/SHA256SUMS.sig": sigtest.Sign("v1.2.3", []byte("x")),
	}
	for name, body := range installers {
		files["v1.2.3/"+name] = body
	}
	srv, _ := newServer(t, files)
	if err := fetchInto(t, srv, "v1.2.3"); err == nil || !strings.Contains(err.Error(), "SHA256SUMS") {
		t.Fatalf("fetchInstallers with an oversized sums = %v, want an error about SHA256SUMS", err)
	}
}
