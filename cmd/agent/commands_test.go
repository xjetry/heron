package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"connectrpc.com/connect"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/gen/heron/v1/heronv1connect"
	"github.com/xjetry/heron/internal/agent/client"
	"github.com/xjetry/heron/internal/agentconfig"
)

type registerHub struct {
	heronv1connect.UnimplementedAgentServiceHandler
	calls atomic.Int32
}

func (h *registerHub) Register(context.Context, *connect.Request[heronv1.RegisterRequest]) (*connect.Response[heronv1.RegisterResponse], error) {
	h.calls.Add(1)
	return connect.NewResponse(&heronv1.RegisterResponse{NodeId: 7, Token: "fresh"}), nil
}

func startRegisterHub(t *testing.T) (*registerHub, string) {
	t.Helper()
	h := &registerHub{}
	mux := http.NewServeMux()
	mux.Handle(heronv1connect.NewAgentServiceHandler(h))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return h, srv.URL
}

// 明文非 loopback 地址在发请求之前被拒：key 不出线、窗口名额不消耗、配置不落盘。
func TestRegisterRejectsPlainHTTPBeforeSending(t *testing.T) {
	var reached atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Store(true) }))
	defer srv.Close()
	// 同一个服务换成非 loopback 的写法：若校验不在请求之前，请求会经 localhost 解析到达它。
	hub := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)
	cfg := filepath.Join(t.TempDir(), "config.json")
	err := runRegister([]string{"--hub", hub, "--key", "k", "--name", "n", "--config", cfg})
	if err == nil || !strings.Contains(err.Error(), "uses plain http") {
		t.Fatalf("err = %v, want the plain-http refusal", err)
	}
	if reached.Load() {
		t.Fatal("the request reached the hub before the address was checked")
	}
	if _, err := os.Stat(cfg); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("config written: %v", err)
	}
}

func TestRegisterInsecureHTTPIsRecorded(t *testing.T) {
	h, url := startRegisterHub(t)
	hub := strings.Replace(url, "127.0.0.1", "localhost", 1)
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := runRegister([]string{"--hub", hub, "--key", "k", "--name", "n", "--config", cfgPath, "--insecure-http"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := client.LoadConfig(cfgPath)
	if err != nil || !cfg.InsecureHTTP || cfg.Token != "fresh" || h.calls.Load() != 1 {
		t.Fatalf("cfg=%+v err=%v calls=%d", cfg, err, h.calls.Load())
	}
}

// 重新注册换的是 hub 身份，宿主机的本地探测策略沿用。
func TestRegisterKeepsLocalProbePolicy(t *testing.T) {
	_, url := startRegisterHub(t)
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	old := agentconfig.Config{Hub: "https://old", Token: "stale", ProbeAllow: []string{"127.0.0.0/8"}, ProbeDeny: []string{"10.0.0.0/8"}}
	if err := agentconfig.Save(cfgPath, old); err != nil {
		t.Fatal(err)
	}
	if err := runRegister([]string{"--hub", url, "--key", "k", "--name", "n", "--config", cfgPath}); err != nil {
		t.Fatal(err)
	}
	cfg, err := client.LoadConfig(cfgPath)
	if err != nil || cfg.Token != "fresh" || cfg.Hub != url || cfg.InsecureHTTP ||
		!slices.Equal(cfg.ProbeAllow, old.ProbeAllow) || !slices.Equal(cfg.ProbeDeny, old.ProbeDeny) {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
}

func TestRegisterRefusesUnreadableExistingConfig(t *testing.T) {
	h, url := startRegisterHub(t)
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"hub":"https://h","probe_dney":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runRegister([]string{"--hub", url, "--key", "k", "--config", cfgPath}); err == nil || !strings.Contains(err.Error(), "existing config") {
		t.Fatalf("err = %v, want the existing-config error", err)
	}
	if h.calls.Load() != 0 {
		t.Fatal("registered although the existing config could not be carried over")
	}
}

func configure(t *testing.T, cfgPath string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := runConfigure(append([]string{"--config", cfgPath}, args...), &out)
	return out.String(), err
}

// 升级后被 run 拒绝的明文配置由 configure 一条命令修正，之后 run 能加载；只改显式给出的项。
func TestConfigureFixesPlainHTTPConfig(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := agentconfig.Save(cfgPath, agentconfig.Config{Hub: "http://10.0.0.1:8080", Token: "t", Name: "n", ProbeDeny: []string{"10.9.0.0/16"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.LoadConfig(cfgPath); err == nil {
		t.Fatal("precondition: run must reject the plain http config")
	}
	out, err := configure(t, cfgPath, "--insecure-http=true")
	if err != nil || !strings.Contains(out, "insecure_http=true") || !strings.Contains(out, "probe_deny=[10.9.0.0/16]") {
		t.Fatalf("out=%q err=%v", out, err)
	}
	cfg, err := client.LoadConfig(cfgPath)
	if err != nil || !cfg.InsecureHTTP || cfg.Token != "t" || cfg.Name != "n" || !slices.Equal(cfg.ProbeDeny, []string{"10.9.0.0/16"}) {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
}

func TestConfigureListsAndValidation(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := agentconfig.Save(cfgPath, agentconfig.Config{Hub: "https://h", Token: "t", InsecureHTTP: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := configure(t, cfgPath, "--probe-allow", "127.0.0.0/8, ::1/128", "--probe-deny", "10.0.0.0/8"); err != nil {
		t.Fatal(err)
	}
	cfg, _ := client.LoadConfig(cfgPath)
	if !slices.Equal(cfg.ProbeAllow, []string{"127.0.0.0/8", "::1/128"}) || !slices.Equal(cfg.ProbeDeny, []string{"10.0.0.0/8"}) || !cfg.InsecureHTTP {
		t.Fatalf("cfg=%+v", cfg)
	}
	if _, err := configure(t, cfgPath, "--probe-deny", ""); err != nil {
		t.Fatal(err)
	}
	cfg, _ = client.LoadConfig(cfgPath)
	if cfg.ProbeDeny != nil || !slices.Equal(cfg.ProbeAllow, []string{"127.0.0.0/8", "::1/128"}) {
		t.Fatalf("empty --probe-deny must clear only that list: %+v", cfg)
	}
	before, _ := os.ReadFile(cfgPath)
	for _, args := range [][]string{
		{"--probe-allow", "10.1.2.3/8"},
		{"--probe-allow", "10.0.0.0/8", "--probe-deny", "10.0.0.0/8"},
		{"--insecure-http=false", "--probe-deny", "x"},
		{"stray"},
	} {
		if _, err := configure(t, cfgPath, args...); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
	if after, _ := os.ReadFile(cfgPath); !bytes.Equal(before, after) {
		t.Fatalf("a rejected configure changed the file:\n%s\n%s", before, after)
	}
}

// 关掉明文放行时，整份配置按 run 的规则重新校验：不会写出一份 run 起不来的配置。
func TestConfigureRejectsResultRunWouldRefuse(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := agentconfig.Save(cfgPath, agentconfig.Config{Hub: "http://10.0.0.1:8080", Token: "t", InsecureHTTP: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := configure(t, cfgPath, "--insecure-http=false"); err == nil || !strings.Contains(err.Error(), "uses plain http") {
		t.Fatalf("err = %v", err)
	}
	if cfg, err := client.LoadConfig(cfgPath); err != nil || !cfg.InsecureHTTP {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
}

// 命令的最终错误行有上限，按写出的整行计（前缀、标记、换行都在内）：register 的错误可能带着 hub 应答里的文本。
func TestErrorLineIsBounded(t *testing.T) {
	short := errors.New("load config: boom")
	if got := errorLine(short); got != "error: load config: boom\n" {
		t.Fatalf("short error changed: %q", got)
	}
	fits := strings.Repeat("a", maxErrorLine-len("error: \n"))
	if got := errorLine(errors.New(fits)); len(got) != maxErrorLine || strings.Contains(got, "truncated") {
		t.Fatalf("an error that fits exactly was altered: %d bytes", len(got))
	}
	for _, long := range []string{fits + "a", strings.Repeat("é", 10000), "register: " + strings.Repeat("x", 10000)} {
		got := errorLine(errors.New(long))
		if len(got) > maxErrorLine || !strings.HasSuffix(got, "…(truncated)\n") || !strings.HasPrefix(got, "error: ") || !utf8.ValidString(got) {
			t.Fatalf("error line is %d bytes (limit %d), valid=%v, tail %q", len(got), maxErrorLine, utf8.ValidString(got), got[max(0, len(got)-20):])
		}
	}
}
