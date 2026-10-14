package backup

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/s3"
	"github.com/xjetry/heron/internal/hub/store"
)

// fakeObjects 是内存里的对象存储。Manager.Run 让配置层与指标层各在自己的 goroutine 里上传、列举、删除，
// 两层可同时调用同一个实例，所以每个方法都在 mu 下读写字段。测试直接读字段只能发生在 Run 已退出、
// 或 tickLayer 同步返回之后——此时没有别的调用者，读不经 mu 也不与写并发。
type fakeObjects struct {
	mu                   sync.Mutex
	databasePath         string
	objects              map[string][]byte
	calls                []string
	failLayer, failStage string
	paths                []string
}

// call 记录调用并按 failStage/failLayer 注入失败；调用方必须持有 f.mu。
func (f *fakeObjects) call(stage, key string) error {
	f.calls = append(f.calls, stage+" "+key)
	if stage == f.failStage && strings.Contains(key, "/"+f.failLayer+"/") {
		return &s3.Error{Kind: "http_status", StatusCode: 503, Detail: "SECRET"}
	}
	return nil
}
func (f *fakeObjects) PutObject(_ context.Context, key string, r io.ReadSeeker) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if file, ok := r.(*os.File); ok {
		f.paths = append(f.paths, file.Name())
	}
	if err := f.call("upload", key); err != nil {
		return err
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(string(b), "SQLite format 3") {
		return errors.New("not a SQLite snapshot")
	}
	f.objects[key] = b
	return nil
}
func (f *fakeObjects) ListObjectsV2(_ context.Context, prefix string, _ int) ([]s3.Object, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("list", prefix); err != nil {
		return nil, err
	}
	var out []s3.Object
	// 故意逆序，不让 map 遍历或服务端排序替代被测的保留排序。
	var keys []string
	for k := range f.objects {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	slices.Reverse(keys)
	for _, k := range keys {
		out = append(out, s3.Object{Key: k})
	}
	return out, nil
}
func (f *fakeObjects) DeleteObject(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("delete", key); err != nil {
		return err
	}
	delete(f.objects, key)
	return nil
}

// eventSink 收集告警事件。两层的失败通知可能同时到达，Enqueue 在 mu 下追加；测试读 events 的时机同 fakeObjects。
type eventSink struct {
	mu     sync.Mutex
	events []store.AlertEvent
}

func (s *eventSink) Enqueue(ev store.AlertEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
}

func setup(t *testing.T) (*Manager, *clock.Fake, *fakeObjects, *eventSink) {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 9, 28, 1, 0, 0, 0, time.UTC))
	path := filepath.Join(t.TempDir(), "hub.db")
	st, err := store.Open(path, clk, slog.Default(), store.MigrateSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ch, err := st.SaveNotifyChannel(context.Background(), store.NotifyChannel{Name: "backup", Kind: "webhook", Config: `{"url":"https://example.test"}`})
	if err != nil {
		t.Fatal(err)
	}
	secret := "secret"
	_, err = st.SaveSettings(context.Background(), store.SettingsUpdate{Backup: &store.BackupSettingsUpdate{
		Endpoint: "https://example.test", Bucket: "backups", Region: "auto", AccessKey: "key", Secret: &secret, Prefix: "tenant", Channels: &[]int64{ch.ID},
	}})
	if err != nil {
		t.Fatal(err)
	}
	objects := &fakeObjects{databasePath: path, objects: map[string][]byte{}}
	sink := &eventSink{}
	m := New(st, sink, clk, slog.Default())
	m.newClient = func(s3.Config) (objectStore, error) { return objects, nil }
	return m, clk, objects, sink
}

func status(t *testing.T, m *Manager) Status {
	t.Helper()
	s, err := m.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func tick(t *testing.T, m *Manager) {
	t.Helper()
	if err := m.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPeriodsRetentionAndPersistence(t *testing.T) {
	m, clk, objects, _ := setup(t)
	themeStorage(m, objects)
	start := clk.Now()
	for i := 49; i > 0; i-- {
		objects.objects["tenant/config/"+start.Add(-time.Duration(i)*time.Minute).Format("20060102T150405.000000000Z")+".db"] = nil
	}
	if _, err := m.st.PutTheme(t.Context(), store.Theme{ID: "1", SDK: 1}, []store.ThemeFile{{Path: "index.html", Content: []byte("1")}}, []byte("1"), false, 20); err != nil {
		t.Fatal(err)
	}
	objects.objects["tenant/theme/1.zip"] = nil
	objects.objects["foreign/config/old.db"] = nil
	objects.objects["tenant/config/notes.txt"] = nil
	before := status(t, m)
	if !before.Enabled || !before.Config.LastSuccess.IsZero() || !before.Metrics.LastSuccess.IsZero() {
		t.Fatalf("initial status=%+v", before)
	}
	tick(t, m)
	var deleted []string
	for _, call := range objects.calls {
		if strings.HasPrefix(call, "delete ") {
			deleted = append(deleted, strings.TrimPrefix(call, "delete "))
		}
	}
	want := []string{"tenant/config/" + start.Add(-49*time.Minute).Format("20060102T150405.000000000Z") + ".db", "tenant/config/" + start.Add(-48*time.Minute).Format("20060102T150405.000000000Z") + ".db"}
	if !reflect.DeepEqual(deleted, want) {
		t.Fatalf("retention deleted=%v want oldest=%v", deleted, want)
	}
	for _, key := range []string{"tenant/theme/1.zip", "foreign/config/old.db", "tenant/config/notes.txt"} {
		if _, ok := objects.objects[key]; !ok {
			t.Errorf("retention removed unrelated object %q", key)
		}
	}
	after := status(t, m)
	if !after.Config.LastSuccess.Equal(start) || !after.Metrics.LastSuccess.Equal(start) {
		t.Fatalf("success times=%+v want=%s", after, start)
	}
	restarted := New(m.st, nil, clk, slog.Default())
	if got := status(t, restarted); !got.Config.LastSuccess.Equal(start) || !got.Metrics.LastSuccess.Equal(start) {
		t.Fatalf("persisted success=%+v", got)
	}
	objects.calls = nil
	clk.Advance(299 * time.Second)
	tick(t, m)
	if len(objects.calls) != 0 {
		t.Fatalf("ran before config period: %v", objects.calls)
	}
	clk.Advance(time.Second)
	tick(t, m)
	for _, c := range objects.calls {
		if strings.Contains(c, "/metrics/") {
			t.Fatalf("metrics ran before daily period: %v", objects.calls)
		}
	}
	if len(objects.calls) == 0 {
		t.Fatal("config did not run at period")
	}
	objects.calls = nil
	clk.Advance(24*time.Hour - 300*time.Second)
	tick(t, m)
	if got := status(t, m); !got.Metrics.LastSuccess.Equal(clk.Now()) {
		t.Fatalf("metrics did not run at daily period: %+v", got)
	}
	for _, path := range objects.paths {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("snapshot not cleaned: %s err=%v", path, err)
		}
	}
}

func TestFailureTransitions(t *testing.T) {
	for _, stage := range []string{"upload", "list", "delete"} {
		t.Run(stage, func(t *testing.T) {
			m, clk, objects, sink := setup(t)
			for i := 1; i <= 50; i++ {
				objects.objects["tenant/config/"+clk.Now().Add(-time.Duration(i)*time.Minute).Format("20060102T150405.000000000Z")+".db"] = nil
			}
			objects.failLayer, objects.failStage = "config", stage
			tick(t, m)
			first := status(t, m)
			if first.Config.Failure != stage+"/http_status" || !first.Config.Since.Equal(clk.Now()) || !first.Config.LastSuccess.IsZero() {
				t.Fatalf("failure status=%+v", first.Config)
			}
			if first.Metrics.LastSuccess.IsZero() {
				t.Fatal("config failure prevented metrics backup")
			}
			clk.Advance(5 * time.Minute)
			tick(t, m)
			if got := status(t, m); got.Config.Since != first.Config.Since {
				t.Fatalf("persistent failure changed since: %+v", got)
			}
			if len(sink.events) != 1 {
				t.Fatalf("persistent failure notifications=%d want=1", len(sink.events))
			}
			objects.failStage = ""
			clk.Advance(5 * time.Minute)
			tick(t, m)
			if len(sink.events) != 2 {
				t.Fatalf("recovery notifications=%d want=2", len(sink.events))
			}
			if got := status(t, m); got.Config.Failure != "" || !got.Config.Since.IsZero() || !got.Config.LastSuccess.Equal(clk.Now()) {
				t.Fatalf("recovered status=%+v", got.Config)
			}
			events, err := m.st.ListAlertEvents(context.Background(), 0, 0, 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != 4 {
				t.Fatalf("persisted events=%d want=4 (failure, recovery and two layer successes)", len(events))
			}
			for i, ev := range sink.events {
				want := store.TransitionBackupFailed
				if i == 1 {
					want = store.TransitionBackupRecovered
				}
				if ev.RuleID != 0 || ev.NodeID != 0 || ev.Transition != want || len(ev.Deliveries) != 1 || !strings.Contains(ev.Summary, "config") || strings.Contains(ev.Summary, "SECRET") {
					t.Errorf("event=%+v", ev)
				}
			}
			if !strings.Contains(sink.events[0].Summary, stage) || !strings.Contains(sink.events[1].Summary, "已恢复") {
				t.Errorf("summaries=%+v", sink.events)
			}
			clk.Advance(5 * time.Minute)
			tick(t, m)
			if len(sink.events) != 2 {
				t.Fatal("repeated successful backup notified again")
			}
			for _, path := range objects.paths {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("failed snapshot not cleaned: %s", path)
				}
			}
		})
	}
}

func TestMetricsFailureAndDisable(t *testing.T) {
	m, clk, objects, sink := setup(t)
	objects.failStage, objects.failLayer = "upload", "metrics"
	tick(t, m)
	if len(sink.events) != 0 {
		t.Fatalf("metrics failure sent %d notifications", len(sink.events))
	}
	if got := status(t, m); got.Metrics.Failure != "upload/http_status" || got.Config.LastSuccess.IsZero() {
		t.Fatalf("metrics failure status=%+v", got)
	}
	objects.failStage = ""
	clk.Advance(24 * time.Hour)
	tick(t, m)
	if len(sink.events) != 0 {
		t.Fatalf("metrics recovery sent %d notifications", len(sink.events))
	}
	if got := status(t, m); got.Metrics.Failure != "" {
		t.Fatalf("metrics recovery status=%+v", got)
	}
	_, err := m.st.SaveSettings(context.Background(), store.SettingsUpdate{Backup: &store.BackupSettingsUpdate{}})
	if err != nil {
		t.Fatal(err)
	}
	objects.calls = nil
	clk.Advance(24 * time.Hour)
	tick(t, m)
	if status(t, m).Enabled || len(objects.calls) != 0 {
		t.Fatalf("disabled backup called storage: %v", objects.calls)
	}
}

func TestZeroRetentionDoesNotTouchObjects(t *testing.T) {
	m, _, objects, _ := setup(t)
	cfg, err := m.st.BackupSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got, _, _ := m.perform(context.Background(), cfg, "config", 0); got != "retention_config" || len(objects.calls) != 0 {
		t.Fatalf("zero retention=%s calls=%v", got, objects.calls)
	}
}

func execFixtureSQL(t *testing.T, path, query string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(query); err != nil {
		t.Fatal(err)
	}
}

func TestEventWriteFailureDoesNotBlockMetricsAndRetries(t *testing.T) {
	m, clk, objects, sink := setup(t)
	execFixtureSQL(t, objects.databasePath, `CREATE TRIGGER reject_backup_event BEFORE INSERT ON alert_event WHEN NEW.transition='backup_failed' BEGIN SELECT RAISE(ABORT, 'event rejected'); END`)
	objects.failLayer, objects.failStage = "config", "upload"
	err := m.Tick(t.Context())
	if err == nil || !strings.Contains(err.Error(), "event rejected") {
		t.Fatalf("event failure=%v", err)
	}
	if got := status(t, m); got.Metrics.LastSuccess.IsZero() {
		t.Fatal("event write failure blocked metrics backup")
	}
	if len(sink.events) != 0 {
		t.Fatal("uncommitted event was enqueued")
	}
	execFixtureSQL(t, objects.databasePath, "DROP TRIGGER reject_backup_event")
	clk.Advance(5 * time.Minute)
	tick(t, m)
	if len(sink.events) != 1 {
		t.Fatalf("failed event was not retried: events=%d", len(sink.events))
	}
	objects.failStage = ""
	execFixtureSQL(t, objects.databasePath, `CREATE TRIGGER reject_backup_event BEFORE INSERT ON alert_event WHEN NEW.transition='backup_recovered' BEGIN SELECT RAISE(ABORT, 'event rejected'); END`)
	clk.Advance(5 * time.Minute)
	if err := m.Tick(t.Context()); err == nil {
		t.Fatal("recovery event write error was hidden")
	}
	if len(sink.events) != 1 {
		t.Fatal("uncommitted recovery was enqueued")
	}
	execFixtureSQL(t, objects.databasePath, "DROP TRIGGER reject_backup_event")
	clk.Advance(5 * time.Minute)
	tick(t, m)
	if len(sink.events) != 2 || sink.events[1].Transition != store.TransitionBackupRecovered {
		t.Fatalf("failed recovery was not retried: events=%v", sink.events)
	}
}

func TestBackupRecordFailureIsNotSuccess(t *testing.T) {
	m, clk, objects, sink := setup(t)
	execFixtureSQL(t, objects.databasePath, `CREATE TRIGGER reject_backup_record BEFORE INSERT ON maintenance_state WHEN NEW.name='backup_config' BEGIN SELECT RAISE(ABORT, 'record rejected'); END`)
	tick(t, m)
	s := status(t, m)
	if s.Config.Failure != "record" || !s.Config.LastSuccess.IsZero() || s.Metrics.LastSuccess.IsZero() {
		t.Fatalf("record failure status=%+v", s)
	}
	if len(sink.events) != 1 {
		t.Fatalf("record failure notifications=%d", len(sink.events))
	}
	execFixtureSQL(t, objects.databasePath, "DROP TRIGGER reject_backup_record")
	clk.Advance(5 * time.Minute)
	tick(t, m)
	if s := status(t, m); s.Config.Failure != "" || !s.Config.LastSuccess.Equal(clk.Now()) {
		t.Fatalf("record recovery=%+v", s)
	}
}

func TestSnapshotFailureNotifiesAndCanceledTickDoesNot(t *testing.T) {
	m, _, objects, sink := setup(t)
	execFixtureSQL(t, objects.databasePath, "DROP TABLE node_facts")
	execFixtureSQL(t, objects.databasePath, "DROP TABLE metric_1m")
	tick(t, m)
	s := status(t, m)
	if s.Config.Failure != "snapshot" || s.Metrics.Failure != "snapshot" || len(sink.events) != 1 {
		t.Fatalf("snapshot failure=%+v events=%d", s, len(sink.events))
	}
	m2, _, objects2, sink2 := setup(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := m2.Tick(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled tick=%v", err)
	}
	if len(objects2.calls) != 0 || len(sink2.events) != 0 {
		t.Fatalf("canceled tick performed work: calls=%v events=%v", objects2.calls, sink2.events)
	}
}

func TestPeriodsUseMonotonicClockAndUpdatedSettings(t *testing.T) {
	m, clk, objects, _ := setup(t)
	tick(t, m)
	objects.calls = nil
	clk.SetWall(clk.Now().Add(48 * time.Hour))
	tick(t, m)
	if len(objects.calls) != 0 {
		t.Fatalf("wall jump triggered backup: %v", objects.calls)
	}
	clk.SetWall(clk.Now().Add(-96 * time.Hour))
	clk.Advance(300 * time.Second)
	tick(t, m)
	if len(objects.calls) == 0 {
		t.Fatal("wall rollback delayed config backup")
	}
	objects.calls = nil
	interval := uint32(60)
	_, err := m.st.SaveSettings(t.Context(), store.SettingsUpdate{Backup: &store.BackupSettingsUpdate{Endpoint: "https://example.test", Bucket: "backups", Region: "auto", AccessKey: "key", Prefix: "changed", ConfigIntervalS: &interval}})
	if err != nil {
		t.Fatal(err)
	}
	clk.Advance(59 * time.Second)
	tick(t, m)
	if len(objects.calls) != 0 {
		t.Fatalf("new interval ran early: %v", objects.calls)
	}
	clk.Advance(time.Second)
	tick(t, m)
	if len(objects.calls) == 0 || !strings.HasPrefix(objects.calls[0], "upload changed/config/") {
		t.Fatalf("settings update not applied: %v", objects.calls)
	}
}
