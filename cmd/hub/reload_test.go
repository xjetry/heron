package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/gen/heron/v1/heronv1connect"
	"github.com/xjetry/heron/internal/agentwire"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/alert"
	"github.com/xjetry/heron/internal/hub/auth"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/hubclient"
	"github.com/xjetry/heron/internal/testdeps"
	"github.com/xjetry/heron/internal/testwait"
	"google.golang.org/protobuf/proto"
)

// 这些用例按 runServeWith 的三段（parseServeOptions → newHub → run）起 hub：与 runServeWith 是同一条代码路径，
// 只是把重载周期改小，并留住 hub 以便逐个读它的缓存。离线变更一律经 openOffline 或 runNode 写库，与 CLI 同一条路径。

const reloadPassword = "offline reload sufficiently long password"

// reloadLog 收集 hub 的 JSON 日志，供用例等待重载循环走到某一步。
type reloadLog struct{ lockedBuffer }

func (l *reloadLog) entries() []map[string]any {
	var out []map[string]any
	sc := bufio.NewScanner(strings.NewReader(l.String()))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var e map[string]any
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out
}

// find 返回消息为 msg、且 attrs 里每一项都相等的记录（数字按 JSON 解成 float64 比较）。
func (l *reloadLog) find(msg string, attrs map[string]any) []map[string]any {
	var out []map[string]any
	for _, e := range l.entries() {
		if e["msg"] != msg {
			continue
		}
		match := true
		for k, v := range attrs {
			if !reflect.DeepEqual(e[k], v) {
				match = false
			}
		}
		if match {
			out = append(out, e)
		}
	}
	return out
}

func (l *reloadLog) wait(t *testing.T, msg string, attrs map[string]any) {
	t.Helper()
	testwait.Until(t, 5*time.Millisecond, func() bool { return len(l.find(msg, attrs)) > 0 },
		"no log %q with %v; log:\n%v", msg, attrs, testwait.When(l.String))
}

// reloadFixture 在 hub 启动之前建好库：管理员密码，两个预建节点（doomed 之后被离线删除，kept 留着），一次排队中的
// 在线更新，以及一条作用于两个节点的离线告警规则和 doomed 的 firing 状态——告警引擎、更新管理器启动时从库里加载它们。
type reloadFixture struct {
	db                         string
	doomed, kept               int64
	doomedInstall, keptInstall string
	rule                       int64
}

func seedReload(t *testing.T) reloadFixture {
	t.Helper()
	f := reloadFixture{db: filepath.Join(t.TempDir(), "hub.db")}
	if err := runPasswdWith([]string{"--db", f.db}, pipeWith(t, reloadPassword+"\n"), io.Discard); err != nil {
		t.Fatal(err)
	}
	st, a, err := openOffline(f.db, false)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := t.Context()
	if f.doomed, f.doomedInstall, err = a.CreateNode(ctx, store.NewNode{Name: "doomed"}); err != nil {
		t.Fatal(err)
	}
	if f.kept, f.keptInstall, err = a.CreateNode(ctx, store.NewNode{Name: "kept"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveNodeUpdate(ctx, f.doomed, &heronv1.UpdateStatus{Task: &heronv1.UpdateTask{Id: "queued-update", Version: "v9.9.9", State: "queued"}}); err != nil {
		t.Fatal(err)
	}
	rule, err := st.SaveAlertRule(ctx, store.AlertRule{Name: "offline", Kind: store.KindOffline, Enabled: true, NodeIDs: []int64{f.doomed, f.kept}})
	if err != nil {
		t.Fatal(err)
	}
	f.rule = rule.ID
	if err := st.SetAlertState(ctx, rule.ID, f.doomed, store.StateFiring, time.Unix(1, 0), time.Time{}); err != nil {
		t.Fatal(err)
	}
	return f
}

// newReloadHub 解析参数并装配 hub；newHub 在启动屏障处暂停时它也跟着阻塞，调用方据此在协程里调用它。
func newReloadHub(t *testing.T, db string, clk clock.Clock, handler slog.Handler) (*hub, error) {
	opts, err := parseServeOptions([]string{"--db", db, "--listen", "127.0.0.1:0", "--timezone", "UTC"}, noServeEnv)
	if err != nil {
		return nil, err
	}
	opts.offlineReload = 5 * time.Millisecond
	return newHub(opts, clk, slog.New(handler))
}

// runReloadHub 运行 hub 直到用例结束，返回监听地址。
func runReloadHub(t *testing.T, h *hub, handler slog.Handler, logs *reloadLog) string {
	t.Helper()
	h.gate = func(context.Context) error { return nil }
	runInBackground(t, func(ctx context.Context) error { return h.run(ctx, slog.New(handler)) })
	logs.wait(t, "hub listening", nil)
	listen, _ := logs.find("hub listening", nil)[0]["listen"].(string)
	return "http://" + listen
}

func startReloadHub(t *testing.T, f reloadFixture, clk clock.Clock, wrap func(slog.Handler) slog.Handler) (*hub, string, *reloadLog) {
	t.Helper()
	logs := &reloadLog{}
	var handler slog.Handler = slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})
	if wrap != nil {
		handler = wrap(handler)
	}
	h, err := newReloadHub(t, f.db, clk, handler)
	if err != nil {
		t.Fatal(err)
	}
	return h, runReloadHub(t, h, handler, logs), logs
}

func agentClient(url string) heronv1connect.AgentServiceClient {
	return hubclient.New(url, 5*time.Second, agentwire.MaxResponseBytes)
}

func register(ctx context.Context, url, install string) (string, error) {
	resp, err := agentClient(url).Register(ctx, connect.NewRequest(&heronv1.RegisterRequest{Key: install}))
	if err != nil {
		return "", err
	}
	return resp.Msg.Token, nil
}

func report(ctx context.Context, url, token string) error {
	req := connect.NewRequest(&heronv1.ReportRequest{Metrics: &heronv1.Metrics{CpuPct: proto.Float64(42), NetRxTotal: proto.Uint64(1000), NetTxTotal: proto.Uint64(1000)}})
	req.Header().Set("Authorization", "Bearer "+token)
	_, err := agentClient(url).Report(ctx, req)
	return err
}

func adminClient(t *testing.T, url string) heronv1connect.AdminServiceClient {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := heronv1connect.NewAdminServiceClient(&http.Client{Transport: testdeps.OwnedTransport(t), Jar: jar, Timeout: testwait.Bound}, url)
	if _, err := client.Login(t.Context(), connect.NewRequest(&heronv1.LoginRequest{Password: reloadPassword})); err != nil {
		t.Fatal(err)
	}
	return client
}

// offlineCreate 像 heron-hub node create 一样建节点，返回 id 与安装凭据。
func offlineCreate(t *testing.T, db, name string) (int64, string) {
	t.Helper()
	st, a, err := openOffline(db, false)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id, install, err := a.CreateNode(t.Context(), store.NewNode{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	return id, install
}

func confirmedAttrs(t *testing.T, db string) map[string]any {
	t.Helper()
	return map[string]any{"generation": float64(offlineGeneration(t, db))}
}

// 被离线删除的节点生前有在线快照、待刷出的分钟桶、流量账本条目、更新状态、告警状态与规则作用域：重载之后逐一被清，
// 它的运行 token 不再鉴权；留下的节点不受影响。
func TestReloadClearsEveryCacheOfAnOfflineDeletedNode(t *testing.T) {
	t.Parallel()
	f := seedReload(t)
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 30, 0, time.UTC))
	h, url, logs := startReloadHub(t, f, clk, nil)
	ctx := t.Context()
	tokens := map[int64]string{}
	for id, install := range map[int64]string{f.doomed: f.doomedInstall, f.kept: f.keptInstall} {
		token, err := register(ctx, url, install)
		if err != nil {
			t.Fatal(err)
		}
		if err := report(ctx, url, token); err != nil {
			t.Fatal(err)
		}
		tokens[id] = token
	}
	held := func(id int64) map[string]bool {
		_, live := h.live.Get(id)
		_, book := h.book.Get(id)
		state := slices.ContainsFunc(h.alerts.States(), func(s alert.StateView) bool { return s.NodeID == id })
		scope := false
		for _, r := range h.alerts.Rules() {
			if r.ID == f.rule && slices.Contains(r.NodeIDs, id) {
				scope = true
			}
		}
		return map[string]bool{"live": live, "traffic": book, "update": h.updates.Snapshot(id).GetTask() != nil, "alert state": state, "rule scope": scope}
	}
	for _, id := range []int64{f.doomed} {
		for name, ok := range held(id) {
			if !ok {
				t.Fatalf("fixture: node %d has no %s before the offline delete, so clearing it proves nothing", id, name)
			}
		}
	}

	if err := runNode([]string{"delete", "--db", f.db, "--id", fmt.Sprint(f.doomed)}); err != nil {
		t.Fatal(err)
	}
	logs.wait(t, "offline caches reloaded", map[string]any{"removed_nodes": []any{float64(f.doomed)}})
	logs.wait(t, "offline generation confirmed", confirmedAttrs(t, f.db))

	for name, ok := range held(f.doomed) {
		if ok {
			t.Errorf("offline-deleted node still has %s after reload", name)
		}
	}
	if kept := held(f.kept); !kept["live"] || !kept["traffic"] || !kept["rule scope"] {
		t.Errorf("surviving node lost state: %v", kept)
	}
	if err := report(ctx, url, tokens[f.doomed]); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("offline-deleted node's token: report = %v, want Unauthenticated", err)
	}
	if err := report(ctx, url, tokens[f.kept]); err != nil {
		t.Errorf("surviving node's report: %v", err)
	}
	// 待刷出的分钟桶：留着已删节点的桶，刷出时写库一侧会丢掉它并记一条告警。
	h.ingest.Flush(ctx, true)
	if dropped := logs.find("minute row for deleted node dropped", map[string]any{"node": float64(f.doomed)}); len(dropped) != 0 {
		t.Errorf("pending minute bucket of the offline-deleted node survived the reload: %v", dropped)
	}
}

// 启动窗口：读起点代数之后、首次加载之前落下的库外提交必须被看到。屏障停在两步之间的日志上；若实现先加载后读
// 代数，这次提交会被计入起点却不在缓存里，永远不被重载。
func TestReloadSeesCommitBetweenStartupGenerationAndLoad(t *testing.T) {
	t.Parallel()
	f := seedReload(t)
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 30, 0, time.UTC))
	logs := &reloadLog{}
	handler, entered, release := testwait.PauseAtLog(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}), "offline generation read")
	defer release()
	type built struct {
		h   *hub
		err error
	}
	result := make(chan built, 1)
	go func() {
		h, err := newReloadHub(t, f.db, clk, handler)
		result <- built{h, err}
	}()
	select {
	case <-entered:
	case <-time.After(testwait.Bound):
		t.Fatal("startup did not reach the generation read")
	}
	id, install := offlineCreate(t, f.db, "between")
	release()
	b := <-result
	if b.err != nil {
		t.Fatal(b.err)
	}
	url := runReloadHub(t, b.h, handler, logs)
	testwait.Until(t, 5*time.Millisecond, func() bool {
		got, err := register(t.Context(), url, install)
		return err == nil && got != ""
	}, "node %d created between the startup generation read and the first load is never accepted; log:\n%v", id, testwait.When(logs.String))
}

// 重载期间的第二次提交：屏障停在各缓存加载完之后、复核代数之前，此时再提交一次。确认的必须是第二次提交的代数，
// 日志里从不出现中间代的确认；第二次提交的节点最终被认得。
func TestReloadConfirmsOnlyAfterCatchingUpWithACommitDuringReload(t *testing.T) {
	t.Parallel()
	f := seedReload(t)
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 30, 0, time.UTC))
	var entered <-chan struct{}
	var release func()
	_, url, logs := startReloadHub(t, f, clk, func(next slog.Handler) slog.Handler {
		var h slog.Handler
		h, entered, release = testwait.PauseAtLog(next, "offline caches reloaded")
		return h
	})
	defer release()
	_, firstInstall := offlineCreate(t, f.db, "first")
	first := offlineGeneration(t, f.db)
	select {
	case <-entered:
	case <-time.After(testwait.Bound):
		t.Fatal("reload did not reach the pause")
	}
	_, secondInstall := offlineCreate(t, f.db, "second")
	second := offlineGeneration(t, f.db)
	release()
	logs.wait(t, "offline changes arrived during reload", map[string]any{"generation": float64(first), "current": float64(second)})
	logs.wait(t, "offline generation confirmed", map[string]any{"generation": float64(second)})
	if early := logs.find("offline generation confirmed", map[string]any{"generation": float64(first)}); len(early) != 0 {
		t.Fatalf("intermediate generation %d confirmed: %v", first, early)
	}
	for _, install := range []string{firstInstall, secondInstall} {
		if _, err := register(t.Context(), url, install); err != nil {
			t.Fatalf("offline-created node not accepted after catch-up: %v", err)
		}
	}
}

// 部分加载失败：任务缓存加载失败的那一轮已经清掉了离线删除的节点（删除不会丢），confirmed 不动；修好之后下一轮
// 整轮重来并确认。失败用库里缺失的 probe_meta 行制造（hub 与离线命令都不写它，只有加载读它）。
func TestReloadRecoversAfterAFailedLoad(t *testing.T) {
	t.Parallel()
	f := seedReload(t)
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 30, 0, time.UTC))
	h, url, logs := startReloadHub(t, f, clk, nil)
	token, err := register(t.Context(), url, f.doomedInstall)
	if err != nil {
		t.Fatal(err)
	}
	if err := report(t.Context(), url, token); err != nil {
		t.Fatal(err)
	}
	// 运行中的 hub 也在写这个库，直连要等它的写锁。
	raw, err := sql.Open("sqlite", "file:"+f.db+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var version int64
	if err := raw.QueryRow("SELECT version FROM probe_meta WHERE id = 1").Scan(&version); err != nil {
		t.Fatal(err)
	}
	restoreExec(t, raw, "DELETE FROM probe_meta")
	if err := runNode([]string{"delete", "--db", f.db, "--id", fmt.Sprint(f.doomed)}); err != nil {
		t.Fatal(err)
	}
	generation := confirmedAttrs(t, f.db)
	logs.wait(t, "offline reload failed", map[string]any{"step": "reload probe tasks", "generation": generation["generation"]})
	if _, ok := h.live.Get(f.doomed); ok {
		t.Fatal("the failed round did not clear the offline-deleted node; its deletion would be lost once the token map was replaced")
	}
	if got := logs.find("offline generation confirmed", generation); len(got) != 0 {
		t.Fatalf("confirmed despite the failed load: %v", got)
	}
	restoreExec(t, raw, fmt.Sprintf("INSERT INTO probe_meta (id, version) VALUES (1, %d)", version))
	logs.wait(t, "offline generation confirmed", generation)
}

// 在线建删与任务修改和重载交错：重载停在发现变更之后、任何加载之前，此时在线建节点、删节点、建 all_nodes 任务；放行
// 之后 token 映射、任务清单与删除清理都收敛到库的样子。
func TestReloadInterleavedWithOnlineChangesConverges(t *testing.T) {
	t.Parallel()
	f := seedReload(t)
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 30, 0, time.UTC))
	var entered <-chan struct{}
	var release func()
	h, url, logs := startReloadHub(t, f, clk, func(next slog.Handler) slog.Handler {
		var h slog.Handler
		h, entered, release = testwait.PauseAtLog(next, "offline changes detected")
		return h
	})
	defer release()
	ctx := t.Context()
	client := adminClient(t, url)
	keptToken, err := register(ctx, url, f.keptInstall)
	if err != nil {
		t.Fatal(err)
	}
	if err := report(ctx, url, keptToken); err != nil {
		t.Fatal(err)
	}

	offline, offlineInstall := offlineCreate(t, f.db, "offline")
	select {
	case <-entered:
	case <-time.After(testwait.Bound):
		t.Fatal("reload did not reach the pause")
	}
	online, err := client.CreateNode(ctx, connect.NewRequest(&heronv1.CreateNodeRequest{Name: "online"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.DeleteNode(ctx, connect.NewRequest(&heronv1.DeleteNodeRequest{Id: f.kept})); err != nil {
		t.Fatal(err)
	}
	if _, err := client.SaveProbeTask(ctx, connect.NewRequest(&heronv1.SaveProbeTaskRequest{
		Task:     &heronv1.ProbeTask{Kind: heronv1.ProbeKind_PROBE_KIND_TCP, Target: "example.com:443", IntervalS: 60, TimeoutMs: 1000},
		AllNodes: true,
	})); err != nil {
		t.Fatal(err)
	}
	release()
	logs.wait(t, "offline generation confirmed", confirmedAttrs(t, f.db))

	for name, install := range map[string]string{"offline": offlineInstall, "online": online.Msg.Token} {
		if _, err := register(ctx, url, install); err != nil {
			t.Errorf("%s-created node not accepted: %v", name, err)
		}
	}
	if err := report(ctx, url, keptToken); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("online-deleted node's token: report = %v, want Unauthenticated", err)
	}
	if _, ok := h.live.Get(f.kept); ok {
		t.Error("online-deleted node still has a live snapshot")
	}
	tasks, err := client.ListProbeTasks(ctx, connect.NewRequest(&heronv1.ListProbeTasksRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{f.doomed, offline, online.Msg.Node.GetId()}
	slices.Sort(want)
	if len(tasks.Msg.Tasks) != 1 || !slices.Equal(tasks.Msg.Tasks[0].NodeIds, want) {
		t.Fatalf("all_nodes task coverage = %v, want %v", tasks.Msg.Tasks, want)
	}
}

// 安装凭据的跨进程竞争：hub 的映射里是安装凭据 A；离线 rotate-token 换发为 B 并推进代数。在 hub 重载之前（屏障停在
// 发现变更、尚未重建映射处）拿 A 注册被拒，库里仍是 B；重载之后用 B 注册得到运行 token，用它上报成功。
func TestStaleInstallCredentialCannotOverwriteOfflineRotation(t *testing.T) {
	t.Parallel()
	f := seedReload(t)
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 30, 0, time.UTC))
	var entered <-chan struct{}
	var release func()
	_, url, logs := startReloadHub(t, f, clk, func(next slog.Handler) slog.Handler {
		var h slog.Handler
		h, entered, release = testwait.PauseAtLog(next, "offline changes detected")
		return h
	})
	defer release()
	ctx := t.Context()
	st, a, err := openOffline(f.db, false)
	if err != nil {
		t.Fatal(err)
	}
	reissued, err := a.RotateToken(ctx, f.kept)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(testwait.Bound):
		t.Fatal("reload did not reach the pause")
	}
	if _, err := register(ctx, url, f.keptInstall); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("register with the superseded credential before reload: %v, want Unauthenticated", err)
	}
	raw, err := sql.Open("sqlite", "file:"+f.db+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var stored []byte
	if err := raw.QueryRow("SELECT token_hash FROM node WHERE id = ?", f.kept).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if want := auth.HashToken(reissued); !slices.Equal(stored, want[:]) {
		t.Fatal("the superseded credential overwrote the offline rotation")
	}
	release()
	logs.wait(t, "offline generation confirmed", confirmedAttrs(t, f.db))
	token, err := register(ctx, url, reissued)
	if err != nil {
		t.Fatalf("register with the reissued credential after reload: %v", err)
	}
	if err := report(ctx, url, token); err != nil {
		t.Fatalf("report with the token from the reissued credential: %v", err)
	}
}
