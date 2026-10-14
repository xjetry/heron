package backup

import (
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

// saveBackup 只改备份组，其余设置不动。
func saveBackup(t *testing.T, st *store.Store, u store.BackupSettingsUpdate) {
	t.Helper()
	if _, err := st.SaveSettings(t.Context(), store.SettingsUpdate{Backup: &u}); err != nil {
		t.Fatal(err)
	}
}

func enabledTarget() store.BackupSettingsUpdate {
	return store.BackupSettingsUpdate{Endpoint: "https://example.test", Bucket: "backups", Region: "auto", AccessKey: "key", Prefix: "tenant"}
}

func persistedEvents(t *testing.T, st *store.Store) []store.AlertEvent {
	t.Helper()
	events, err := st.ListAlertEvents(t.Context(), 0, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func hasKey(objects *fakeObjects, prefix string) bool {
	for k := range objects.objects {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}

// 份数 2：两份旧快照之外还有一个排在所有时间戳之后的手工 .db。手工文件若计入份数，会顶掉较新的那份旧快照。
func TestRetentionDoesNotCountManualDB(t *testing.T) {
	m, clk, objects, _ := setup(t)
	two := uint32(2)
	u := enabledTarget()
	u.ConfigKeep = &two
	saveBackup(t, m.st, u)
	manual := "tenant/config/zzzz-manual.db"
	older := "tenant/config/" + clk.Now().Add(-10*time.Minute).Format("20060102T150405.000000000Z") + ".db"
	newer := "tenant/config/" + clk.Now().Add(-5*time.Minute).Format("20060102T150405.000000000Z") + ".db"
	current := "tenant/config/" + clk.Now().Format("20060102T150405.000000000Z") + ".db"
	objects.objects[manual], objects.objects[older], objects.objects[newer] = nil, nil, nil
	tick(t, m)
	var backups []string
	for k := range objects.objects {
		if strings.HasPrefix(k, "tenant/config/") && k != manual {
			backups = append(backups, k)
		}
	}
	_, keptManual := objects.objects[manual]
	_, keptNewer := objects.objects[newer]
	_, keptCurrent := objects.objects[current]
	if len(backups) != 2 || !keptCurrent || !keptNewer || !keptManual {
		t.Errorf("manual .db counted toward keep or deleted: backups=%v keptNewer=%v keptManual=%v calls=%v", backups, keptNewer, keptManual, objects.calls)
	}
}

type sizedObjects struct {
	*fakeObjects
	size   int64
	budget time.Duration
}

func (o *sizedObjects) PutObject(ctx context.Context, key string, body io.ReadSeeker) error {
	deadline, _ := ctx.Deadline()
	o.budget = time.Until(deadline)
	size, err := body.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	if _, err := body.Seek(0, io.SeekStart); err != nil {
		return err
	}
	o.size = size
	return o.fakeObjects.PutObject(ctx, key, body)
}

// 配置层期限按对象体量给：5 分钟起，每开始一个 MiB 加 1 秒。在 PutObject 处读 ctx 截止时间，
// 退化成指标层那样按周期给（约一天）或不计体量都会红。
func TestConfigUploadBudgetFollowsObjectSize(t *testing.T) {
	m, _, objects, _ := setup(t)
	// 把配置层快照撑到数 MiB，按体量加的那部分期限才看得出来。
	execFixtureSQL(t, objects.databasePath, `INSERT INTO node (name, note, token_hash, created_at) VALUES ('padding', hex(randomblob(2097152)), randomblob(32), 0)`)
	sized := &sizedObjects{fakeObjects: objects}
	m.newClient = func(s3.Config) (objectStore, error) { return sized, nil }
	cfg, err := m.st.BackupSettings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	category, _, _ := m.perform(t.Context(), cfg, "config", 1)
	mib := (sized.size + 1<<20 - 1) / (1 << 20)
	want := 5*time.Minute + time.Duration(mib)*time.Second
	if category != "" || mib < 3 || sized.budget > want || sized.budget < want-time.Second {
		t.Errorf("config upload budget=%s want=%s size=%d category=%s", sized.budget, want, sized.size, category)
	}
}

// 重启后首轮之前，状态直接读库里的未恢复标记，不等配置层读回。
func TestStatusShowsPersistedMarkerBeforeFirstTick(t *testing.T) {
	m, clk, objects, sink := setup(t)
	start := clk.Now()
	if _, err := m.st.RecordBackupEvent(t.Context(), store.TransitionBackupFailed, "config 层备份失败（upload）", start); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Hour)
	fresh := restart(m, objects, sink)
	if got := status(t, fresh).Config; got.Failure != "unrecovered" || !got.Since.Equal(start) {
		t.Errorf("status before first tick ignored persisted marker: %+v", got)
	}
}

// 标记被改坏（手工改库或恢复了坏值）：配置层按 marker 类别触发一次并以覆盖写修复标记，指标层照常上传，状态可读。
func TestCorruptMarkerNotifiesOnceWithoutBlockingMetrics(t *testing.T) {
	for _, recovers := range []bool{true, false} {
		t.Run(map[bool]string{true: "recovers", false: "still-failing"}[recovers], func(t *testing.T) {
			m, clk, objects, sink := setup(t)
			execFixtureSQL(t, objects.databasePath, `INSERT INTO setting VALUES ('backup.config_failing_since','x')`)
			start := clk.Now()
			// 两层在 Run 里并发，指标层可能先于配置层修复标记执行；先跑指标层，它若依赖标记就会在这里被挡住。
			for _, i := range []int{1, 0} {
				if err := m.tickLayer(t.Context(), i); err != nil {
					t.Fatal(err)
				}
			}
			s, err := m.Status(t.Context())
			if err != nil || s.Config.Failure != "marker" || !s.Config.Since.Equal(start) || len(sink.events) != 1 || sink.events[0].Transition != store.TransitionBackupFailed {
				t.Fatalf("corrupt marker not notified as config failure: status=%+v err=%v events=%v", s, err, sink.events)
			}
			if !hasKey(objects, "tenant/metrics/") || s.Metrics.LastSuccess.IsZero() {
				t.Fatalf("corrupt marker blocked metrics layer: status=%+v calls=%v", s.Metrics, objects.calls)
			}
			if since, err := m.st.BackupFailingSince(t.Context()); err != nil || !since.Equal(start) {
				t.Fatalf("firing did not repair marker: since=%s err=%v", since, err)
			}
			if !recovers {
				objects.failLayer, objects.failStage = "config", "upload"
			}
			clk.Advance(time.Second)
			tick(t, m)
			if recovers {
				got := status(t, m).Config
				if got.Failure != "" || !got.LastSuccess.Equal(clk.Now()) || len(sink.events) != 2 || sink.events[1].Transition != store.TransitionBackupRecovered {
					t.Fatalf("repaired marker did not let config run and recover: status=%+v events=%v", got, sink.events)
				}
				return
			}
			m = restart(m, objects, sink)
			clk.Advance(time.Second)
			tick(t, m)
			if got := status(t, m).Config; got.Failure != "upload/http_status" || !got.Since.Equal(start) || len(sink.events) != 1 {
				t.Fatalf("repaired marker not honored across restart: status=%+v events=%v", got, sink.events)
			}
		})
	}
}

// 失败中停用：已通知的配置层故障以一条停用事件收尾并清掉标记；未通知的只清状态、不发事件。两层都不再显示旧故障。
func TestDisableEndsFailureTracking(t *testing.T) {
	for _, notified := range []bool{true, false} {
		t.Run(map[bool]string{true: "notified", false: "unnotified"}[notified], func(t *testing.T) {
			m, clk, objects, sink := setup(t)
			objects.failLayer, objects.failStage = "config", "upload"
			execFixtureSQL(t, objects.databasePath, "DROP TABLE metric_1m")
			if notified {
				tick(t, m)
			} else {
				// 触发事件提交不了：配置层故障已观察到，但从未通知。
				execFixtureSQL(t, objects.databasePath, `CREATE TRIGGER reject_event BEFORE INSERT ON alert_event BEGIN SELECT RAISE(ABORT,'rejected'); END`)
				if err := m.Tick(t.Context()); err == nil {
					t.Fatal("rejected firing event was not reported")
				}
				execFixtureSQL(t, objects.databasePath, "DROP TRIGGER reject_event")
			}
			if s := status(t, m); s.Config.Failure != "upload/http_status" || s.Metrics.Failure != "snapshot" {
				t.Fatalf("precondition: both layers failing, status=%+v", s)
			}
			saveBackup(t, m.st, store.BackupSettingsUpdate{Region: "auto"})
			clk.Advance(time.Second)
			tick(t, m)
			wantSink, wantPersisted := 0, 0
			if notified {
				wantSink, wantPersisted = 2, 2
			}
			events := persistedEvents(t, m.st)
			since, err := m.st.BackupFailingSince(t.Context())
			s := status(t, m)
			if s.Enabled || s.Config.Failure != "" || s.Metrics.Failure != "" || err != nil || !since.IsZero() || len(sink.events) != wantSink || len(events) != wantPersisted {
				t.Fatalf("disable did not end failure tracking: status=%+v marker=%s err=%v enqueued=%d persisted=%d", s, since, err, len(sink.events), len(events))
			}
			if notified {
				closing := sink.events[1]
				if closing.Transition != store.TransitionBackupDisabled || !strings.Contains(closing.Summary, "停用") || len(closing.Deliveries) != 1 || events[0].Transition != store.TransitionBackupDisabled {
					t.Errorf("closing event=%+v persisted=%+v", closing, events[0])
				}
			}
			// 停用期间每秒一轮，收尾只发一次；重启后也不复活旧故障。
			clk.Advance(time.Second)
			tick(t, m)
			m = restart(m, objects, sink)
			tick(t, m)
			if s := status(t, m); s.Config.Failure != "" || s.Metrics.Failure != "" || len(sink.events) != wantSink {
				t.Errorf("disabled tracking resumed: status=%+v enqueued=%d", s, len(sink.events))
			}
		})
	}
}

type heldObjects struct {
	*fakeObjects
	entered, release chan struct{}
	dir              string
}

func (h *heldObjects) PutObject(ctx context.Context, key string, r io.ReadSeeker) error {
	if strings.Contains(key, "/config/") {
		if f, ok := r.(*os.File); ok {
			h.dir = filepath.Dir(f.Name())
		}
		close(h.entered)
		<-h.release
	}
	return h.fakeObjects.PutObject(ctx, key, r)
}

// 同一目录下的另一个库 hub.db-2 在途时，hub.db 的启动清理不能删它的暂存目录。
// hub.db-2 的前缀以 hub.db 的前缀开头，只按前缀匹配会误删；hub.db 自己的残留仍要删掉。
func TestStartupCleanupSparesOtherDatabases(t *testing.T) {
	m, clk, objects, _ := setup(t)
	dir, prefix := m.st.BackupScratch()
	own, err := os.MkdirTemp(dir, prefix+"*")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "hub.db-2"), clk, slog.Default(), store.MigrateSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	secret := "secret"
	u := enabledTarget()
	u.Secret = &secret
	saveBackup(t, st, u)
	held := &heldObjects{fakeObjects: &fakeObjects{objects: map[string][]byte{}}, entered: make(chan struct{}), release: make(chan struct{})}
	other := New(st, &eventSink{}, clk, slog.Default())
	other.newClient = func(s3.Config) (objectStore, error) { return held, nil }
	done := make(chan error, 1)
	go func() { done <- other.tickLayer(t.Context(), 0) }()
	<-held.entered
	tick(t, m)
	_, inFlightErr := os.Stat(held.dir)
	close(held.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(own); !os.IsNotExist(err) {
		t.Errorf("own stale scratch remains: %v", err)
	}
	if got := status(t, other).Config; inFlightErr != nil || got.Failure != "" || got.LastSuccess.IsZero() {
		t.Errorf("startup cleanup removed another database's in-flight scratch %s: stat=%v status=%+v", held.dir, inFlightErr, got)
	}
	if len(objects.paths) != 2 {
		t.Errorf("own backups did not run: %v", objects.paths)
	}
}

// 启动准备（读回成功时刻、清本库残留）失败时，配置层按 startup 类别触发一次，指标层跳过；准备成功后照常执行并恢复。
func TestStartupFailureNotifiesAndRetries(t *testing.T) {
	m, clk, objects, sink := setup(t)
	dir, prefix := m.st.BackupScratch()
	stale, err := os.MkdirTemp(dir, prefix+"*")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "snapshot.db"), []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	// 目录不可写，里面的文件删不掉，清理失败。
	if err := os.Chmod(stale, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(stale, 0700) })
	start := clk.Now()
	tick(t, m)
	clk.Advance(time.Second)
	tick(t, m)
	s := status(t, m)
	if s.Config.Failure != "startup" || !s.Config.Since.Equal(start) || len(sink.events) != 1 || len(objects.calls) != 0 {
		t.Fatalf("startup failure not reported through config layer: status=%+v events=%d calls=%v", s, len(sink.events), objects.calls)
	}
	if err := os.Chmod(stale, 0700); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Second)
	tick(t, m)
	s = status(t, m)
	if _, err := os.Stat(stale); !os.IsNotExist(err) || s.Config.Failure != "" || s.Metrics.LastSuccess.IsZero() || len(sink.events) != 2 || sink.events[1].Transition != store.TransitionBackupRecovered {
		t.Errorf("startup retry did not clean and recover: stat=%v status=%+v events=%v", err, s, sink.events)
	}
}

// 重启后配置层先读回标记再做其余准备：已通知的故障遇上设置读不出或启动准备失败，都不重复触发。
func TestRestartReadsMarkerBeforeOtherPrerequisites(t *testing.T) {
	for _, broken := range []string{"settings", "startup"} {
		t.Run(broken, func(t *testing.T) {
			m, clk, objects, sink := setup(t)
			objects.failLayer, objects.failStage = "config", "upload"
			tick(t, m)
			if broken == "settings" {
				execFixtureSQL(t, objects.databasePath, `INSERT INTO setting VALUES ('backup.config_interval_s','0')`)
			} else {
				dir, prefix := m.st.BackupScratch()
				stale, err := os.MkdirTemp(dir, prefix+"*")
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(stale, "snapshot.db"), nil, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(stale, 0500); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.Chmod(stale, 0700) })
			}
			m = restart(m, objects, sink)
			clk.Advance(time.Second)
			tick(t, m)
			if got := status(t, m).Config; got.Failure != broken || len(sink.events) != 1 || len(persistedEvents(t, m.st)) != 2 {
				t.Errorf("restart re-notified a persisted failure: status=%+v enqueued=%d", got, len(sink.events))
			}
		})
	}
}
