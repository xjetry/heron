package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/gen/heron/v1/heronv1connect"
	"github.com/xjetry/heron/internal/agentconfig"
	"github.com/xjetry/heron/internal/testwait"
)

// smokeHub 是冒烟用的最小 hub：第一个 Report 响应下发一份钉住证书指纹的 HTTPS 任务，
// 记录此后收到的全部上报。
type smokeHub struct {
	tasks *heronv1.ProbeTasks

	mu      sync.Mutex
	reports []*heronv1.ReportRequest
}

func (h *smokeHub) Register(context.Context, *connect.Request[heronv1.RegisterRequest]) (*connect.Response[heronv1.RegisterResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}

func (h *smokeHub) Report(_ context.Context, req *connect.Request[heronv1.ReportRequest]) (*connect.Response[heronv1.ReportResponse], error) {
	if req.Header().Get("Authorization") != "Bearer smoke-token" {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("bad token"))
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reports = append(h.reports, req.Msg)
	resp := &heronv1.ReportResponse{ReportIntervalMs: 1000}
	if len(h.reports) == 1 {
		resp.Tasks = h.tasks
	}
	return connect.NewResponse(resp), nil
}

func (h *smokeHub) GetRelease(context.Context, *connect.Request[heronv1.GetReleaseRequest]) (*connect.Response[heronv1.GetReleaseResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}

// findProbeResult 返回最近一次上报里指定任务的结果；还没有则 ok=false。
func (h *smokeHub) findProbeResult(taskID uint64) (*heronv1.ProbeResult, *heronv1.ReportRequest, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := len(h.reports) - 1; i >= 0; i-- {
		for _, r := range h.reports[i].GetProbeResults() {
			if r.GetTaskId() == taskID {
				return r, h.reports[i], true
			}
		}
	}
	return nil, nil, false
}

// 二进制冒烟：真实构建的 heron-agent 跑真实采集器与真实 TLS 目标，端到端确认钉住任务的成功
// 结果带 cert_not_after、回显任务身份、上报声明 PROBE_CERT_PIN 与清单摘要。不走 mock hub 客户端，
// 链条上的每一环（配置加载、策略、调度、探测、编码、上报）都是真实代码。
func TestBinarySmokePinnedHTTPSuccessCarriesCertNotAfter(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer target.Close()
	pin := sha256.Sum256(target.Certificate().RawSubjectPublicKeyInfo)
	configID := []byte("smoke-config-id0") // 恰 16 字节
	hub := &smokeHub{tasks: &heronv1.ProbeTasks{Version: 1, Tasks: []*heronv1.ProbeTask{{
		Id:             1,
		Kind:           heronv1.ProbeKind_PROBE_KIND_HTTP,
		Target:         target.URL,
		IntervalS:      5,
		TimeoutMs:      2000,
		CertSpkiSha256: pin[:],
		ConfigId:       configID,
	}}}}
	mux := http.NewServeMux()
	mux.Handle(heronv1connect.NewAgentServiceHandler(hub))
	hubSrv := httptest.NewServer(mux)
	defer hubSrv.Close()

	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.json")
	// 探测目标是回环上的测试服务：默认拒绝集管辖回环，放行要显式写出。
	if err := agentconfig.Save(cfgPath, agentconfig.Config{Hub: hubSrv.URL, Token: "smoke-token", Name: "smoke", ProbeAllow: []string{"127.0.0.0/8"}}); err != nil {
		t.Fatal(err)
	}

	bin := filepath.Join(tmp, "heron-agent")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build agent: %v\n%s", err, out)
	}
	cmd := exec.Command(bin, "run", "--config", cfgPath)
	var stderr safeBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// 失败也必须停止上报：不能让上一个测试的 agent 继续向下一个测试的 hub 发包。
	stopped := false
	defer func() {
		if !stopped {
			cmd.Process.Kill()
			cmd.Wait()
		}
	}()

	testwait.Until(t, 20*time.Millisecond, func() bool {
		_, _, ok := hub.findProbeResult(1)
		return ok
	}, "agent did not report a result for the pinned task; agent stderr:\n%s", testwait.When(stderr.String))
	cmd.Process.Signal(syscall.SIGTERM)
	waitErr := cmd.Wait()
	stopped = true

	res, report, ok := hub.findProbeResult(1)
	if !ok {
		t.Fatal("no probe result captured")
	}
	if res.GetRttUs() == 0 {
		t.Fatalf("result = %v, want success (rtt_us)", res)
	}
	if res.GetCertNotAfterS() != target.Certificate().NotAfter.Unix() {
		t.Fatalf("cert_not_after_s = %d, want %d", res.GetCertNotAfterS(), target.Certificate().NotAfter.Unix())
	}
	if !slices.Equal(res.GetTaskConfigId(), configID) {
		t.Fatalf("task_config_id = %x, want %x", res.GetTaskConfigId(), configID)
	}
	if len(report.GetTasksDigest()) != 32 {
		t.Fatalf("tasks_digest = %x, want 32 bytes after receiving the task list", report.GetTasksDigest())
	}
	if waitErr != nil {
		t.Fatalf("agent did not exit cleanly on SIGTERM: %v\nstderr:\n%s", waitErr, stderr.String())
	}
}

// safeBuffer 给子进程 stderr 用：agent 退出后测试才读，Wait 之前的写与读之间有 testwait 轮询，
// 需要自己的锁。
type safeBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.buf)+len(p) > 64<<10 {
		return 0, fmt.Errorf("smoke agent stderr exceeds 64KiB")
	}
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
