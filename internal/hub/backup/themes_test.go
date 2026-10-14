package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/xjetry/heron/internal/hub/s3"
	"github.com/xjetry/heron/internal/hub/store"
)

func immutableThemeKey(content string) string {
	return fmt.Sprintf("tenant/theme/sha256/%x.zip", sha256.Sum256([]byte(content)))
}

func TestThemeSnapshotUsesImmutablePackages(t *testing.T) {
	m, _, base, _ := setup(t)
	themeStorage(m, base)
	installTheme(t, m, "old zip")
	tick(t, m)
	if got := string(base.objects[immutableThemeKey("old zip")]); got != "old zip" {
		t.Fatalf("immutable package missing: %q", got)
	}
	installTheme(t, m, "new zip")
	tick(t, m)
	deleteThemeVersions(t, m, "a")
	tick(t, m)
	for _, content := range []string{"old zip", "new zip"} {
		if string(base.objects[immutableThemeKey(content)]) != content {
			t.Fatalf("historical package lost: %s", content)
		}
	}
	seenPackage := false
	for _, call := range base.calls {
		if call == "upload "+immutableThemeKey("old zip") {
			seenPackage = true
		}
		if strings.HasPrefix(call, "upload tenant/config/") {
			if !seenPackage {
				t.Fatal("snapshot published before referenced package")
			}
			break
		}
	}
}

type themeObjects struct {
	mu       sync.Mutex
	base     *fakeObjects
	onUpload func()
	changed  chan string
	budgets  map[string]time.Duration
}

func (o *themeObjects) PutObject(ctx context.Context, key string, r io.ReadSeeker) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !strings.Contains(key, "/theme/") {
		return o.base.PutObject(ctx, key, r)
	}
	// base 的字段归 base.mu 管（见 fakeObjects）；锁序固定为 o.mu → base.mu，base 的方法不取 o.mu。
	o.base.mu.Lock()
	err := o.base.call("upload", key)
	o.base.mu.Unlock()
	if err != nil {
		return err
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if d, ok := ctx.Deadline(); ok {
		o.budgets["upload"] = time.Until(d)
	}
	if o.onUpload != nil {
		o.onUpload()
	}
	o.base.mu.Lock()
	o.base.objects[key] = b
	o.base.mu.Unlock()
	if o.changed != nil {
		o.changed <- "put " + key
	}
	return nil
}

func (o *themeObjects) ListObjectsV2(ctx context.Context, prefix string, n int) ([]s3.Object, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if strings.Contains(prefix, "/theme/sha256/") {
		if d, ok := ctx.Deadline(); ok {
			o.budgets["list"] = time.Until(d)
		}
	}
	return o.base.ListObjectsV2(ctx, prefix, n)
}

func (o *themeObjects) DeleteObject(ctx context.Context, key string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.base.DeleteObject(ctx, key); err != nil {
		return err
	}
	if strings.Contains(key, "/theme/") && o.changed != nil {
		o.changed <- "delete " + key
	}
	return nil
}

func themeStorage(m *Manager, base *fakeObjects) *themeObjects {
	o := &themeObjects{base: base, budgets: map[string]time.Duration{}}
	m.newClient = func(s3.Config) (objectStore, error) { return o, nil }
	return o
}

func installTheme(t *testing.T, m *Manager, content string) {
	t.Helper()
	_, err := m.st.PutTheme(t.Context(), store.Theme{ID: "a", Name: "A", SDK: 1, UploadedAt: time.Now()},
		[]store.ThemeFile{{Path: "index.html", Content: []byte(content)}}, []byte(content), false, 20)
	if err != nil {
		t.Fatal(err)
	}
}

type themeBackup struct {
	Revision int64
	Content  []byte
	Uploaded bool
}

func deleteThemeVersions(t *testing.T, m *Manager, id string) {
	t.Helper()
	versions, err := m.st.ListThemes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range versions {
		if version.ID == id {
			if err := m.st.DeleteThemeVersion(t.Context(), id, version.Digest); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func themePackage(t *testing.T, m *Manager, want string) themeBackup {
	t.Helper()
	entries, err := m.st.ThemeBackupEntries(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.ID == "a" && entry.Digest == fmt.Sprintf("%x", sha256.Sum256([]byte(want))) {
			content, err := m.st.ThemeBackupContent(t.Context(), entry.ID, entry.Digest, entry.Revision)
			if err != nil {
				t.Fatal(err)
			}
			return themeBackup{Revision: entry.Revision, Content: content, Uploaded: entry.Uploaded}
		}
	}
	t.Fatal("theme a missing from backup entries")
	return themeBackup{}
}

func TestThemeSyncReconcilesAndReplaces(t *testing.T) {
	m, clk, base, _ := setup(t)
	o := themeStorage(m, base)
	installTheme(t, m, "original zip")
	base.objects["tenant/theme/deleted.zip"] = []byte("old")
	base.objects["foreign/theme/keep.zip"] = []byte("foreign")
	tick(t, m)
	if got := string(base.objects[immutableThemeKey("original zip")]); got != "original zip" {
		t.Fatalf("uploaded package=%q", got)
	}
	if _, ok := base.objects["tenant/theme/deleted.zip"]; !ok {
		t.Fatal("historical theme object was deleted")
	}
	if got := string(base.objects["foreign/theme/keep.zip"]); got != "foreign" {
		t.Fatalf("foreign object changed: %q", got)
	}
	if !themePackage(t, m, "original zip").Uploaded {
		t.Fatal("successful upload was not recorded")
	}
	if d := o.budgets["upload"]; d <= 5*time.Minute || d > 5*time.Minute+time.Second {
		t.Fatalf("theme upload deadline=%v", d)
	}
	if d := o.budgets["list"]; d <= 4*time.Minute || d > 5*time.Minute {
		t.Fatalf("theme list deadline=%v", d)
	}
	base.calls = nil
	clk.Advance(5 * time.Minute)
	tick(t, m)
	for _, c := range base.calls {
		if c == "upload "+immutableThemeKey("original zip") {
			t.Fatal("unchanged package uploaded again")
		}
	}
	installTheme(t, m, "replacement zip")
	if themePackage(t, m, "replacement zip").Uploaded {
		t.Fatal("replacement did not reset uploaded")
	}
	tick(t, m)
	if got := string(base.objects[immutableThemeKey("replacement zip")]); got != "replacement zip" {
		t.Fatalf("replacement not uploaded: %q", got)
	}
	delete(base.objects, immutableThemeKey("replacement zip"))
	clk.Advance(5 * time.Minute)
	tick(t, m)
	if got := string(base.objects[immutableThemeKey("replacement zip")]); got != "replacement zip" {
		t.Fatalf("missing object not repaired: %q", got)
	}
	if _, err := m.st.SaveSettings(t.Context(), store.SettingsUpdate{Backup: &store.BackupSettingsUpdate{Endpoint: "https://example.test", Bucket: "other", Region: "auto", AccessKey: "key", Prefix: "other"}}); err != nil {
		t.Fatal(err)
	}
	clk.Advance(5 * time.Minute)
	tick(t, m)
	if got := string(base.objects[strings.Replace(immutableThemeKey("replacement zip"), "tenant/", "other/", 1)]); got != "replacement zip" {
		t.Fatalf("new target not populated: %q", got)
	}
}

func TestThemeSyncConcurrentWrite(t *testing.T) {
	for _, recreate := range []bool{false, true} {
		name := "overwrite"
		if recreate {
			name = "delete-reinstall"
		}
		t.Run(name, func(t *testing.T) {
			m, _, base, _ := setup(t)
			o := themeStorage(m, base)
			installTheme(t, m, "old zip")
			old := themePackage(t, m, "old zip")
			o.onUpload = func() {
				o.onUpload = nil
				if recreate {
					deleteThemeVersions(t, m, "a")
				}
				installTheme(t, m, "new zip")
			}
			tick(t, m)
			published := false
			for key, data := range base.objects {
				if !strings.HasPrefix(key, "tenant/config/") {
					continue
				}
				published = true
				path := filepath.Join(t.TempDir(), "snapshot.db")
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				db, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				var digest string
				err = db.QueryRow("SELECT sha256 FROM snapshot_theme WHERE theme_id='a'").Scan(&digest)
				db.Close()
				if err != nil || digest != fmt.Sprintf("%x", sha256.Sum256([]byte("old zip"))) {
					t.Fatalf("concurrent write changed frozen snapshot reference: digest=%q err=%v", digest, err)
				}
			}
			if !published {
				t.Fatal("configuration snapshot was not published")
			}
			p := themePackage(t, m, "new zip")
			if p.Uploaded || p.Revision == old.Revision || string(p.Content) != "new zip" {
				t.Fatalf("in-flight write marked new package uploaded: %+v old=%d", p, old.Revision)
			}
			tick(t, m)
			if got := string(base.objects[immutableThemeKey("new zip")]); got != "new zip" || !themePackage(t, m, "new zip").Uploaded {
				t.Fatalf("next synchronization did not upload new package: %q", got)
			}
		})
	}
}

func TestThemeSyncFailureAndLostWake(t *testing.T) {
	for _, stage := range []string{"upload", "list"} {
		t.Run(stage, func(t *testing.T) {
			m, clk, base, sink := setup(t)
			themeStorage(m, base)
			tick(t, m)
			last := status(t, m).Config.LastSuccess
			configKeys := func() int {
				count := 0
				for key := range base.objects {
					if strings.HasPrefix(key, "tenant/config/") {
						count++
					}
				}
				return count
			}
			published := configKeys()
			installTheme(t, m, "zip")
			<-m.st.ThemeChanges()
			base.objects["tenant/theme/orphan.zip"] = nil
			base.failLayer, base.failStage = "theme", stage
			clk.Advance(5 * time.Minute)
			tick(t, m)
			s := status(t, m).Config
			if configKeys() != published {
				t.Fatal("snapshot with unavailable theme reference was published")
			}
			if s.Failure != "theme_"+stage+"/http_status" || s.Since.IsZero() || !s.LastSuccess.Equal(last) || len(sink.events) != 1 {
				t.Fatalf("theme failure not in config state: %+v events=%d", s, len(sink.events))
			}
			if since, err := m.st.BackupFailingSince(t.Context()); err != nil || !since.Equal(s.Since) {
				t.Fatalf("theme failure marker not persisted: %v %v", since, err)
			}
			clk.Advance(5 * time.Minute)
			tick(t, m)
			if len(sink.events) != 1 {
				t.Fatalf("repeated theme failure notified: %d", len(sink.events))
			}
			base.failStage = ""
			clk.Advance(5 * time.Minute)
			tick(t, m)
			if got := string(base.objects[immutableThemeKey("zip")]); got != "zip" {
				t.Fatalf("lost wake not repaired by period: %q", got)
			}
			if s := status(t, m).Config; s.Failure != "" || len(sink.events) != 2 || sink.events[1].Transition != store.TransitionBackupRecovered {
				t.Fatalf("theme recovery not in config state: %+v events=%v", s, sink.events)
			}
			if since, err := m.st.BackupFailingSince(t.Context()); err != nil || !since.IsZero() {
				t.Fatalf("theme failure marker not recovered: %v %v", since, err)
			}
		})
	}
}

func TestThemeSyncDisabledThenConfigured(t *testing.T) {
	m, _, base, _ := setup(t)
	themeStorage(m, base)
	if _, err := m.st.SaveSettings(t.Context(), store.SettingsUpdate{Backup: &store.BackupSettingsUpdate{}}); err != nil {
		t.Fatal(err)
	}
	installTheme(t, m, "zip")
	base.objects["tenant/theme/orphan.zip"] = nil
	tick(t, m)
	if len(base.calls) != 0 {
		t.Fatalf("disabled theme sync touched objects: %v", base.calls)
	}
	if _, err := m.st.SaveSettings(t.Context(), store.SettingsUpdate{Backup: &store.BackupSettingsUpdate{Endpoint: "https://example.test", Bucket: "backups", Region: "auto", AccessKey: "key", Prefix: "tenant"}}); err != nil {
		t.Fatal(err)
	}
	tick(t, m)
	if got := string(base.objects[immutableThemeKey("zip")]); got != "zip" {
		t.Fatalf("later configuration did not upload package: %q", got)
	}
}

func TestThemeSyncRunWakesAfterCommit(t *testing.T) {
	m, _, base, _ := setup(t)
	o := themeStorage(m, base)
	tick(t, m)
	o.changed = make(chan string, 4)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); m.Run(ctx) }()
	defer func() { cancel(); <-done }()
	wait := func(want string) {
		t.Helper()
		select {
		case got := <-o.changed:
			if got != want {
				t.Fatalf("wake operation=%q want=%q", got, want)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("committed theme did not wake manager: %s", want)
		}
	}
	installTheme(t, m, "zip")
	wait("put " + immutableThemeKey("zip"))
	deleteThemeVersions(t, m, "a")
	cancel()
	<-done
	// 删除当前主题不能删除仍被已发布快照引用的原包。
	if string(base.objects[immutableThemeKey("zip")]) != "zip" {
		t.Fatal("historical package deleted")
	}
}

func TestThemeSyncRecordFailure(t *testing.T) {
	m, _, base, sink := setup(t)
	themeStorage(m, base)
	installTheme(t, m, "zip")
	execFixtureSQL(t, base.databasePath, "CREATE TRIGGER reject_mark BEFORE UPDATE OF uploaded ON theme_package BEGIN SELECT RAISE(ABORT, 'mark rejected'); END")
	tick(t, m)
	if s := status(t, m).Config; s.Failure != "theme_record" || !s.LastSuccess.IsZero() || len(sink.events) != 1 {
		t.Fatalf("theme record failure hidden: %+v events=%d", s, len(sink.events))
	}
}

func TestThemeFailureSharesMarkerAndDisabledLifecycle(t *testing.T) {
	m, _, base, sink := setup(t)
	themeStorage(m, base)
	installTheme(t, m, "zip")
	execFixtureSQL(t, base.databasePath, `INSERT INTO setting VALUES ('backup.config_failing_since','x')`)
	tick(t, m)
	if s, err := m.Status(t.Context()); err != nil || s.Config.Failure != "marker" || len(sink.events) != 1 {
		t.Fatalf("theme wake bypassed marker preparation: %+v err=%v events=%v", s, err, sink.events)
	}
	base.failLayer, base.failStage = "theme", "upload"
	tick(t, m)
	if s := status(t, m).Config; s.Failure != "theme_upload/http_status" || len(sink.events) != 1 {
		t.Fatalf("theme failure did not continue marker lifecycle: %+v events=%v", s, sink.events)
	}
	if _, err := m.st.SaveSettings(t.Context(), store.SettingsUpdate{Backup: &store.BackupSettingsUpdate{}}); err != nil {
		t.Fatal(err)
	}
	tick(t, m)
	if s := status(t, m).Config; s.Failure != "" || len(sink.events) != 2 || sink.events[1].Transition != store.TransitionBackupDisabled {
		t.Fatalf("theme failure did not close on disable: %+v events=%v", s, sink.events)
	}
}

func TestThemeSyncPreservesUnmanagedKeys(t *testing.T) {
	m, _, base, _ := setup(t)
	keys := []string{"README.txt", "a.zip.bak", "archive/a.zip", "A (1).zip"}
	for _, key := range keys {
		base.objects["tenant/theme/"+key] = []byte("keep")
	}
	tick(t, m)
	for _, key := range keys {
		if string(base.objects["tenant/theme/"+key]) != "keep" {
			t.Errorf("unmanaged theme key deleted: %s", key)
		}
	}
}

func TestThemeSyncMissingPackageIsNotFailure(t *testing.T) {
	for _, files := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "legacy"}[files], func(t *testing.T) {
			m, _, base, sink := setup(t)
			installTheme(t, m, "zip")
			execFixtureSQL(t, base.databasePath, "DELETE FROM theme_package")
			if !files {
				execFixtureSQL(t, base.databasePath, "DELETE FROM theme_file")
			}
			base.objects["tenant/theme/a.zip"] = []byte("only copy")
			tick(t, m)
			s := status(t, m)
			if s.Config.Failure != "" || s.Config.LastSuccess.IsZero() || len(sink.events) != 0 || len(s.ThemesWithoutPackage) != 1 || s.ThemesWithoutPackage[0] != "a" || string(base.objects["tenant/theme/a.zip"]) != "only copy" {
				t.Fatalf("missing package treated as failure or lost: %+v events=%v remote=%q", s, sink.events, base.objects["tenant/theme/a.zip"])
			}
		})
	}
}

func TestThemeSyncContinuesAfterPackageFailure(t *testing.T) {
	m, _, base, _ := setup(t)
	themeStorage(m, base)
	installTheme(t, m, "a")
	if _, err := m.st.PutTheme(t.Context(), store.Theme{ID: "b", SDK: 1}, []store.ThemeFile{{Path: "index.html", Content: []byte("b")}}, []byte("b"), false, 20); err != nil {
		t.Fatal(err)
	}
	execFixtureSQL(t, base.databasePath, "CREATE TRIGGER reject_a BEFORE UPDATE OF uploaded ON theme_package WHEN NEW.theme_id='a' BEGIN SELECT RAISE(ABORT,'reject a'); END")
	base.objects["tenant/theme/orphan.zip"] = []byte("old")
	tick(t, m)
	if s := status(t, m).Config; s.Failure != "theme_record" || string(base.objects[immutableThemeKey("b")]) != "b" {
		t.Fatalf("theme failure blocked later package: %+v package=%q", s, base.objects[immutableThemeKey("b")])
	}
	if _, ok := base.objects["tenant/theme/orphan.zip"]; !ok {
		t.Fatal("historical package deleted")
	}
}

func TestThemeMissingPackageStartupWarning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _, base, _ := setup(t)
		installTheme(t, m, "a")
		execFixtureSQL(t, base.databasePath, "DELETE FROM theme_package")
		var output bytes.Buffer
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		m.log = slog.New(slog.NewTextHandler(&output, nil))
		done := make(chan struct{})
		go func() { defer close(done); m.Run(ctx) }()
		// synctest 推进真实 ticker 使用的虚拟时间；先等首轮完成，再跨过两个每秒判定周期。
		synctest.Wait()
		time.Sleep(2500 * time.Millisecond)
		cancel()
		<-done
		if n := strings.Count(output.String(), "level=WARN"); n != 1 {
			t.Fatalf("missing-package warning logged %d times over several ticks: %s", n, output.String())
		}
		if !strings.Contains(output.String(), "theme=a") {
			t.Fatalf("missing package startup warning=%s", output.String())
		}
	})
}

func TestThemeSyncDeleteDuringUpload(t *testing.T) {
	m, _, base, sink := setup(t)
	o := themeStorage(m, base)
	installTheme(t, m, "a")
	if _, err := m.st.PutTheme(t.Context(), store.Theme{ID: "b", SDK: 1}, []store.ThemeFile{{Path: "index.html", Content: []byte("b")}}, []byte("b"), false, 20); err != nil {
		t.Fatal(err)
	}
	o.onUpload = func() {
		o.onUpload = nil
		deleteThemeVersions(t, m, "b")
	}
	tick(t, m)
	if s := status(t, m).Config; s.Failure != "" || len(sink.events) != 0 {
		t.Fatalf("concurrent deletion raised false failure: %+v events=%v", s, sink.events)
	}
}

func TestThemeWakeKeepsSnapshotFailure(t *testing.T) {
	m, clk, base, sink := setup(t)
	themeStorage(m, base)
	tick(t, m)
	last := status(t, m).Config.LastSuccess
	base.failLayer, base.failStage = "config", "upload"
	clk.Advance(5 * time.Minute)
	tick(t, m)
	base.calls = nil
	installTheme(t, m, "zip")
	clk.Advance(time.Second)
	tick(t, m)
	retried := false
	for _, call := range base.calls {
		if strings.HasPrefix(call, "upload tenant/config/") {
			retried = true
		}
	}
	if s := status(t, m).Config; !retried || s.Failure != "upload/http_status" || !s.LastSuccess.Equal(last) || len(sink.events) != 1 {
		t.Fatalf("wake skipped snapshot or cleared failure: retried=%t state=%+v events=%v", retried, s, sink.events)
	}
}
