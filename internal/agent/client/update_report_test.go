package client

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/gen/heron/v1/heronv1connect"
	"github.com/xjetry/heron/internal/agentwire"
	"github.com/xjetry/heron/internal/hubclient"
	"github.com/xjetry/heron/internal/testwait"
	"github.com/xjetry/heron/internal/update"
	"google.golang.org/protobuf/proto"
)

// scriptedHub 按上报的序号给出应答：replies[i] 为 nil 表示第 i 次上报失败（Unavailable）。超出脚本即停止 Runner。
type scriptedHub struct {
	replies []*heronv1.ReportResponse
	mu      sync.Mutex
	reports []*heronv1.ReportRequest
}

func (h *scriptedHub) Register(context.Context, *connect.Request[heronv1.RegisterRequest]) (*connect.Response[heronv1.RegisterResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}

func (h *scriptedHub) GetRelease(context.Context, *connect.Request[heronv1.GetReleaseRequest]) (*connect.Response[heronv1.GetReleaseResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}

func (h *scriptedHub) Report(_ context.Context, req *connect.Request[heronv1.ReportRequest]) (*connect.Response[heronv1.ReportResponse], error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	i := len(h.reports)
	h.reports = append(h.reports, req.Msg)
	if i >= len(h.replies) || h.replies[i] == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("down"))
	}
	return connect.NewResponse(proto.Clone(h.replies[i]).(*heronv1.ReportResponse)), nil
}

// fakeUpdates 是可在轮次之间改写的本机更新状态。
type fakeUpdates struct {
	mu     sync.Mutex
	status *heronv1.UpdateStatus
}

func (u *fakeUpdates) Snapshot() *heronv1.UpdateStatus {
	u.mu.Lock()
	defer u.mu.Unlock()
	return proto.Clone(u.status).(*heronv1.UpdateStatus)
}

// 状态在构造时就已就绪：等待它的读者立即放行。
func (u *fakeUpdates) Ready() <-chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}

func (u *fakeUpdates) Reported(*heronv1.UpdateTask) {}

func (u *fakeUpdates) set(s *heronv1.UpdateStatus) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.status = s
}

// runScript 让 Runner 对脚本里的每个应答各上报一次；beforeReport[i] 在第 i 次上报之前执行（改写本机更新状态）。
// 返回各次上报的请求。
func runScript(t *testing.T, replies []*heronv1.ReportResponse, beforeReport map[int]func(*fakeUpdates)) []*heronv1.ReportRequest {
	t.Helper()
	r, _ := newRunner(t, &fakeHub{})
	u := &fakeUpdates{status: &heronv1.UpdateStatus{Supported: true, Version: "v0.2.0", Source: "hub"}}
	r.Updates = u
	before := map[int]func(){}
	for i, f := range beforeReport {
		before[i] = func() { f(u) }
	}
	return driveScript(t, r, replies, before)
}

// driveScript 把 r 接到按 replies 应答的 hub 上，对每个应答各上报一次；before[i] 在第 i 次上报之前执行。
// 返回各次上报的请求。
func driveScript(t *testing.T, r *Runner, replies []*heronv1.ReportResponse, before map[int]func()) []*heronv1.ReportRequest {
	t.Helper()
	hub := &scriptedHub{replies: replies}
	mux := http.NewServeMux()
	mux.Handle(heronv1connect.NewAgentServiceHandler(hub))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	r.Client = hubclient.New(srv.URL, 5*time.Second, agentwire.MaxResponseBytes)
	if f := before[0]; f != nil {
		f()
	}
	// 成功与失败的上报之后各睡一次（间隔或退避）；睡第 n 次即已完成第 n 次上报。
	rounds := 0
	r.Sleep = func(context.Context, time.Duration) error {
		rounds++
		if rounds == len(replies) {
			return context.Canceled
		}
		if f := before[rounds]; f != nil {
			f()
		}
		return nil
	}
	if err := r.Run(t.Context()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if len(hub.reports) != len(replies) {
		t.Fatalf("got %d reports, want %d", len(hub.reports), len(replies))
	}
	return hub.reports
}

// full 与 digestOnly 判定一次上报带的形态；摘要必须是所带（或本应所带）状态的 UpdateHash。
func full(r *heronv1.ReportRequest) bool {
	return r.Update != nil && r.UpdateHash != nil && *r.UpdateHash == UpdateHash(r.Update)
}
func digestOnly(r *heronv1.ReportRequest, of *heronv1.UpdateStatus) bool {
	return r.Update == nil && r.UpdateHash != nil && *r.UpdateHash == UpdateHash(of)
}

// 首次带全文，此后只带摘要；状态变化后先报新摘要，被索要后下一次带全文。
func TestRunnerUpdateTextFirstAndWhenWanted(t *testing.T) {
	changed := &heronv1.UpdateStatus{Supported: true, Version: "v0.2.0", Source: "github"}
	reports := runScript(t, []*heronv1.ReportResponse{
		{},
		{},
		{WantUpdate: true},
		{},
		{},
	}, map[int]func(*fakeUpdates){2: func(u *fakeUpdates) { u.set(changed) }})
	if !full(reports[0]) {
		t.Fatalf("first report after start: %v", reports[0])
	}
	if !digestOnly(reports[1], reports[0].Update) {
		t.Fatalf("steady report carried more than the digest: %v", reports[1])
	}
	if !digestOnly(reports[2], changed) || *reports[2].UpdateHash == *reports[1].UpdateHash {
		t.Fatalf("changed status must first go out as a new digest only: %v", reports[2])
	}
	if !full(reports[3]) || !proto.Equal(reports[3].Update, changed) {
		t.Fatalf("report after want_update: %v", reports[3])
	}
	if !digestOnly(reports[4], changed) {
		t.Fatalf("report after the full status was accepted: %v", reports[4])
	}
}

// 失败不会清除上一轮的索要，重试继续携带全文。
func TestUpdateTextRequestSurvivesFailure(t *testing.T) {
	reports := runScript(t, []*heronv1.ReportResponse{{}, {WantUpdate: true}, nil, {}, {}}, nil)
	want := []bool{true, false, true, true, false}
	for i, rep := range reports {
		if full(rep) != want[i] || (!want[i] && !digestOnly(rep, reports[0].Update)) {
			t.Fatalf("report %d full=%v, want %v: %v", i, full(rep), want[i], rep)
		}
	}
}

// blockingUpdater 的 Status 阻塞到被释放，钉住首报与本机更新器第一轮查询的先后关系。
type blockingUpdater struct {
	release chan struct{}
	status  update.Status
}

func (f *blockingUpdater) Status(context.Context) update.Status {
	<-f.release
	return f.status
}

func (f *blockingUpdater) Ready(context.Context, string) error { return nil }

func (f *blockingUpdater) Submit(context.Context, update.Request) (update.Job, error) {
	return update.Job{}, nil
}

// 首报等待第一次本机状态查询完成：释放前没有任何 Report、快照还没有状态；
// 释放后首报携带的全文就是刷新后的状态（不是占位文本），摘要与之相符。
func TestFirstReportWaitsForRefreshedUpdateStatus(t *testing.T) {
	release := make(chan struct{})
	upd := &blockingUpdater{release: release, status: update.Status{Supported: true, Source: "hub"}}
	coord := NewUpdateCoordinator("v0.2.0", slog.New(slog.NewTextHandler(io.Discard, nil)))
	coord.client = upd
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	coordDone := make(chan struct{})
	go func() { coord.Run(ctx); close(coordDone) }()
	// Status 阻塞时 Run 察觉不到取消，先释放再等它退出，避免泄漏到下一个测试。
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		cancel()
		<-coordDone
	})

	hub := &fakeHub{interval: 10000}
	r, _ := newRunner(t, hub)
	r.Updates = coord
	runDone := make(chan struct{})
	go func() { r.Run(ctx); close(runDone) }()

	// 释放前的等待窗口必须远大于无等待路径的全程耗时（本地 httptest，毫秒级），否则钉不住缺陷。
	deadline := time.Now().Add(200 * time.Millisecond)
	for hub.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if hub.count() != 0 {
		t.Fatalf("a report arrived before the first local-updater status refresh completed")
	}
	if s := coord.Snapshot(); s != nil {
		t.Fatalf("snapshot before the first refresh must be nil, got %v", s)
	}

	close(release)
	testwait.Until(t, time.Millisecond, func() bool { return hub.count() >= 1 }, "no report after the status refresh completed")
	cancel()
	<-runDone

	first := hub.received()[0]
	want := update.StatusProto(upd.status, "v0.2.0")
	// full 同时断言摘要与全文相符；proto.Equal 断言内容就是刷新后的状态而非占位文本。
	if !full(first) || !proto.Equal(first.Update, want) {
		t.Fatalf("first report must carry the refreshed status in full with its digest: got %v, want %v", first.Update, want)
	}
}
