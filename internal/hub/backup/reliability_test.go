package backup

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/hub/s3"
	"github.com/xjetry/heron/internal/hub/store"
)

func restart(m *Manager, objects *fakeObjects, sink *eventSink) *Manager {
	r := New(m.st, sink, m.clk, slog.Default())
	r.newClient = func(s3.Config) (objectStore, error) { return objects, nil }
	return r
}

func TestRestartFailureTransitions(t *testing.T) {
	for _, recover := range []bool{false, true} {
		t.Run(map[bool]string{false: "failing", true: "recovered"}[recover], func(t *testing.T) {
			m, clk, objects, sink := setup(t)
			objects.failLayer, objects.failStage = "config", "upload"
			tick(t, m)
			start := clk.Now()
			if recover {
				objects.failStage = ""
			}
			m = restart(m, objects, sink)
			clk.Advance(time.Second)
			tick(t, m)
			events, err := m.st.ListAlertEvents(t.Context(), 0, 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			f, r := 0, 0
			for _, ev := range events {
				if ev.Transition == store.TransitionBackupFailed {
					f++
				} else if ev.Transition == store.TransitionBackupRecovered {
					r++
				}
			}
			wantR := 0
			if recover {
				wantR = 1
			}
			if f != 1 || r != wantR || len(sink.events) != 1+wantR || (!recover && !status(t, m).Config.Since.Equal(start)) {
				t.Errorf("restart transition firing=%d recovered=%d enqueued=%d status=%+v", f, r, len(sink.events), status(t, m).Config)
			}
		})
	}
}

func TestEventRetryKeepsOriginalFailureTime(t *testing.T) {
	m, clk, objects, sink := setup(t)
	start := clk.Now()
	objects.failLayer, objects.failStage = "config", "upload"
	execFixtureSQL(t, objects.databasePath, `CREATE TRIGGER reject_event BEFORE INSERT ON alert_event BEGIN SELECT RAISE(ABORT,'rejected'); END`)
	_ = m.Tick(t.Context())
	execFixtureSQL(t, objects.databasePath, "DROP TRIGGER reject_event")
	clk.Advance(5 * time.Minute)
	tick(t, m)
	m = restart(m, objects, sink)
	tick(t, m)
	if got := status(t, m).Config.Since; !got.Equal(start) {
		t.Errorf("event retry reset original failure time: got=%s want=%s", got, start)
	}
}

func TestRestartSchedulesFromSuccess(t *testing.T) {
	for _, age := range []time.Duration{-time.Hour, 2 * time.Minute, 48 * time.Hour} {
		t.Run(age.String(), func(t *testing.T) {
			m, clk, objects, sink := setup(t)
			tick(t, m)
			clk.SetWall(clk.Now().Add(age))
			m = restart(m, objects, sink)
			objects.calls = nil
			tick(t, m)
			wait := max(time.Duration(0), min(5*time.Minute, 5*time.Minute-age))
			if wait > 0 {
				clk.Advance(wait - time.Second)
				tick(t, m)
				if len(objects.calls) != 0 {
					t.Errorf("restart uploaded before persisted deadline: %v", objects.calls)
				}
				clk.Advance(time.Second)
				tick(t, m)
			}
			if len(objects.calls) == 0 {
				t.Error("restart did not upload at clamped persisted deadline")
			}
		})
	}
}

func TestRetentionProtectsCurrentAndIgnoresManual(t *testing.T) {
	m, clk, objects, _ := setup(t)
	one := uint32(1)
	if _, err := m.st.SaveSettings(t.Context(), store.SettingsUpdate{Backup: &store.BackupSettingsUpdate{Endpoint: "https://example.test", Bucket: "backups", Region: "auto", AccessKey: "key", Prefix: "tenant", ConfigKeep: &one}}); err != nil {
		t.Fatal(err)
	}
	manual := "tenant/config/0000-manual.db"
	objects.objects[manual] = nil
	tick(t, m)
	clk.Advance(5 * time.Minute)
	clk.SetWall(clk.Now().Add(-10 * time.Minute))
	tick(t, m)
	current := "tenant/config/" + clk.Now().Format("20060102T150405.000000000Z") + ".db"
	var backups int
	for k := range objects.objects {
		if strings.HasPrefix(k, "tenant/config/") && k != manual {
			backups++
		}
	}
	_, kept := objects.objects[current]
	if !kept || backups != 1 {
		t.Errorf("retention lost current upload or kept excess: current=%s objects=%v", current, objects.calls)
	}
	if _, ok := objects.objects[manual]; !ok {
		t.Error("retention deleted non-timestamp db")
	}
}

type inFlightObjects struct {
	entered chan struct{}
	configs chan struct{}
}

func (b *inFlightObjects) PutObject(ctx context.Context, key string, _ io.ReadSeeker) error {
	if strings.Contains(key, "/metrics/") {
		close(b.entered)
		<-ctx.Done()
		return ctx.Err()
	}
	b.configs <- struct{}{}
	return nil
}
func (*inFlightObjects) ListObjectsV2(context.Context, string, int) ([]s3.Object, error) {
	return nil, nil
}
func (*inFlightObjects) DeleteObject(context.Context, string) error { return nil }

func TestMetricsInFlightDoesNotBlockConfig(t *testing.T) {
	m, clk, _, _ := setup(t)
	b := &inFlightObjects{entered: make(chan struct{}), configs: make(chan struct{}, 16)}
	m.newClient = func(s3.Config) (objectStore, error) { return b, nil }
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); m.Run(ctx) }()
	defer func() { cancel(); <-done }()
	select {
	case <-b.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("metrics upload did not start")
	}
	for n := 0; n < 7; n++ {
		select {
		case <-b.configs:
		case <-time.After(3 * time.Second):
			t.Fatalf("config layer blocked by metrics upload at period %d", n)
		}
		// Status 可先从数据库读到成功时间；runMu[0] 由 tickLayerWithWake 持有到调度记账结束。
		// 推进假钟必须等该锁释放，不能把持久化成功当成本轮已完成，否则 lastAttempt 会读到下一周期。
		deadline := time.Now().Add(3 * time.Second)
		for !m.runMu[0].TryLock() {
			if time.Now().After(deadline) {
				t.Fatalf("config layer did not finish at period %d", n)
			}
			time.Sleep(time.Millisecond)
		}
		func() {
			defer m.runMu[0].Unlock()
			if !status(t, m).Config.LastSuccess.Equal(clk.Now()) {
				t.Fatalf("config success was not recorded at period %d", n)
			}
			if n < 6 {
				clk.Advance(5 * time.Minute)
			}
		}()
	}
}

type cancelObjects struct {
	*fakeObjects
	cancel context.CancelFunc
}

func (c *cancelObjects) PutObject(ctx context.Context, _ string, _ io.ReadSeeker) error {
	c.cancel()
	<-ctx.Done()
	return &s3.Error{Kind: "transport", Detail: "context canceled"}
}
func TestCanceledUploadIsNotFailure(t *testing.T) {
	m, _, objects, sink := setup(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	m.newClient = func(s3.Config) (objectStore, error) { return &cancelObjects{objects, cancel}, nil }
	_ = m.Tick(ctx)
	events, err := m.st.ListAlertEvents(t.Context(), 0, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 || len(sink.events) != 0 || status(t, m).Config.Failure != "" {
		t.Errorf("canceled upload recorded failure: events=%v status=%+v", events, status(t, m))
	}
}

func TestBackupDirectoryAndStartupCleanup(t *testing.T) {
	m, _, objects, _ := setup(t)
	root := filepath.Dir(objects.databasePath)
	// 残留按生产的命名方式造：随机串的形态若变了，清理认不出它，这里会红。
	_, prefix := m.st.BackupScratch()
	stale, err := os.MkdirTemp(root, prefix+"*")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "snapshot.db"), []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
	tick(t, m)
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale snapshot directory remains: %v", err)
	}
	if len(objects.paths) != 2 {
		t.Fatalf("snapshots not created beside database: %v", objects.paths)
	}
	for _, p := range objects.paths {
		if filepath.Dir(filepath.Dir(p)) != root {
			t.Errorf("snapshot outside database disk: %s", p)
		}
	}
}

func TestSettingsFailureRateLimitedAndRecovers(t *testing.T) {
	m, clk, objects, sink := setup(t)
	var logs bytes.Buffer
	m.log = slog.New(slog.NewTextHandler(&logs, nil))
	execFixtureSQL(t, objects.databasePath, `INSERT INTO setting VALUES ('backup.config_interval_s','0')`)
	// 日志间隔 5 分钟，每轮推进 10 秒观察到的限频与逐秒相同：一天 288 行。
	for n := 0; n < 8640; n++ {
		tick(t, m)
		clk.Advance(10 * time.Second)
	}
	s, statusErr := m.Status(t.Context())
	if statusErr != nil || s.Config.Failure != "settings" || len(sink.events) != 1 || strings.Count(logs.String(), "backup failed") != 288 {
		t.Errorf("settings failure not bounded/notified: status=%+v err=%v events=%d logs=%d", s, statusErr, len(sink.events), strings.Count(logs.String(), "backup failed"))
	}
	execFixtureSQL(t, objects.databasePath, `UPDATE setting SET value='300' WHERE key='backup.config_interval_s'`)
	tick(t, m)
	if status(t, m).Config.Failure != "" || len(sink.events) != 2 || sink.events[1].Transition != store.TransitionBackupRecovered {
		t.Errorf("settings recovery missing: status=%+v events=%v", status(t, m), sink.events)
	}
}

type timedObjects struct {
	*fakeObjects
	budget time.Duration
}

func (o *timedObjects) PutObject(ctx context.Context, key string, body io.ReadSeeker) error {
	deadline, _ := ctx.Deadline()
	o.budget = time.Until(deadline)
	return o.fakeObjects.PutObject(ctx, key, body)
}

func TestRemoteFailureDiagnosticsAndBudget(t *testing.T) {
	m, _, objects, _ := setup(t)
	var logs bytes.Buffer
	m.log = slog.New(slog.NewTextHandler(&logs, nil))
	objects.failLayer, objects.failStage = "config", "upload"
	tick(t, m)
	if status(t, m).Config.StatusCode != 503 || !strings.Contains(logs.String(), "status_code=503") || !strings.Contains(logs.String(), "SECRET") || strings.Contains(logs.String(), "https://") {
		t.Errorf("remote diagnostic incomplete: status=%+v logs=%s", status(t, m), logs.String())
	}
	cfg, err := m.st.BackupSettings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	timed := &timedObjects{fakeObjects: objects}
	m.newClient = func(s3.Config) (objectStore, error) { return timed, nil }
	for _, period := range []uint32{3600, 86400, 604800} {
		cfg.MetricsIntervalS = period
		category, _, _ := m.perform(t.Context(), cfg, "metrics", 1)
		want := time.Duration(period-300) * time.Second
		if category != "" || timed.budget > want || timed.budget < want-time.Second {
			t.Errorf("metrics upload budget=%s period=%d category=%s", timed.budget, period, category)
		}
	}
}
