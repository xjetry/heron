package store

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/theme"
)

func themeZIP(t *testing.T, id, version string, sdk int) []byte {
	t.Helper()
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	for _, f := range []ThemeFile{{Path: "theme.json", Content: []byte(fmt.Sprintf(`{"id":%q,"name":%q,"version":%q,"sdk":%d}`, id, id, version, sdk))}, {Path: "index.html", Content: []byte(version)}, {Path: "app.js", Content: []byte(version + " js")}} {
		w, err := z.Create(f.Path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(f.Content); err != nil {
			t.Fatal(err)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func installZIP(t *testing.T, s *Store, content []byte) Theme {
	t.Helper()
	pkg, err := theme.Parse(content)
	if err != nil {
		t.Fatal(err)
	}
	files := make([]ThemeFile, len(pkg.Files))
	for i, f := range pkg.Files {
		files[i] = ThemeFile{Path: f.Path, Content: f.Content}
	}
	got, err := s.PutTheme(t.Context(), Theme{ID: pkg.Manifest.ID, Name: pkg.Manifest.Name, Version: pkg.Manifest.Version, SDK: pkg.Manifest.SDK, UploadedAt: time.Unix(100, 0)}, files, content, false, 20)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func seedLegacyTheme(t *testing.T, db *sql.DB, content []byte) {
	t.Helper()
	pkg, err := theme.Parse(content)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO theme(id,name,version,preview,uploaded_at,enabled) VALUES(?,?,?,?,100,1)", pkg.Manifest.ID, pkg.Manifest.Name, pkg.Manifest.Version, pkg.Manifest.Preview); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO theme_package(theme_id,content,revision) VALUES(?,?,10)", pkg.Manifest.ID, content); err != nil {
		t.Fatal(err)
	}
	for _, f := range pkg.Files {
		if _, err := db.Exec("INSERT INTO theme_file(theme_id,path,content) VALUES(?,?,?)", pkg.Manifest.ID, f.Path, f.Content); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMigrationFromV21ArchivesLegacyTheme(t *testing.T) {
	t.Parallel()
	content := themeZIP(t, "legacy", "one", 0)
	s := migrateFrom(t, 21, func(t *testing.T, db *sql.DB) { seedLegacyTheme(t, db, content) })
	list, err := s.ListThemes(t.Context())
	if err != nil || len(list) != 1 || list[0].SDK != 0 || list[0].Enabled || list[0].Digest != fmt.Sprintf("%x", sha256.Sum256(content)) {
		t.Fatalf("legacy migration=%+v %v", list, err)
	}
	assertArchivedTheme(t, s, list[0])
	config, dir := filepath.Join(t.TempDir(), "config.db"), t.TempDir()
	packages, err := s.SnapshotConfigWithThemes(t.Context(), config, dir)
	if err != nil || len(packages) != 1 {
		t.Fatalf("archive backup=%+v %v", packages, err)
	}
	restored := restoreThemesFixture(t, config, dir)
	list, err = restored.ListThemes(t.Context())
	if err != nil || len(list) != 1 || list[0].SDK != 0 {
		t.Fatalf("archive restore=%+v %v", list, err)
	}
	assertArchivedTheme(t, restored, list[0])
}

func assertArchivedTheme(t *testing.T, s *Store, version Theme) {
	t.Helper()
	if err := s.EnableTheme(t.Context(), version.ID, version.Digest); err == nil {
		t.Fatal("legacy archive enabled")
	}
	if _, _, err := s.ThemePreview(t.Context(), version.ID, version.Digest); err == nil {
		t.Fatal("legacy archive previewed")
	}
	current, _, err := s.ThemeSelection(t.Context())
	if err != nil || current.ID != "" {
		t.Fatalf("legacy archive publicly selected=%+v %v", current, err)
	}
	entries, err := s.ThemeBackupEntries(t.Context())
	if err != nil || len(entries) != 1 {
		t.Fatalf("archive backup entries=%+v %v", entries, err)
	}
	if content, err := s.ThemeBackupContent(t.Context(), version.ID, version.Digest, entries[0].Revision); err != nil || len(content) == 0 {
		t.Fatalf("archived bytes unavailable=%d %v", len(content), err)
	}
}

func restoreThemesFixture(t *testing.T, config, dir string) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "restored.db")
	if _, err := Restore(t.Context(), path, config, "", dir, time.Unix(1000, 0), slog.Default()); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path, clock.Real(), slog.Default(), RequireCurrentSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestThemeSnapshotRestoresAllVersionsAndSelection(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	a := installZIP(t, s, themeZIP(t, "a", "one", 1))
	b := installZIP(t, s, themeZIP(t, "a", "two", 1))
	c := installZIP(t, s, themeZIP(t, "b", "three", 1))
	for _, v := range []Theme{a, b} {
		if err := s.EnableTheme(t.Context(), v.ID, v.Digest); err != nil {
			t.Fatal(err)
		}
	}
	config, dir := filepath.Join(t.TempDir(), "config.db"), t.TempDir()
	packages, err := s.SnapshotConfigWithThemes(t.Context(), config, dir)
	if err != nil || len(packages) != 3 {
		t.Fatalf("packages=%+v %v", packages, err)
	}
	restored := restoreThemesFixture(t, config, dir)
	list, err := restored.ListThemes(t.Context())
	if err != nil || len(list) != 3 {
		t.Fatalf("restored versions=%+v %v", list, err)
	}
	current, previous, err := restored.ThemeSelection(t.Context())
	if err != nil || current.Digest != b.Digest || previous.Digest != a.Digest {
		t.Fatalf("restored selection=%+v %+v %v", current, previous, err)
	}
	for _, version := range []Theme{a, b, c} {
		got, content, err := restored.ThemeVersionFile(t.Context(), version.ID, version.Digest, "index.html")
		if err != nil || got.Version != version.Version || string(content) != version.Version {
			t.Fatalf("restored resource=%+v %q %v", got, content, err)
		}
	}
	if err := restored.EnableTheme(t.Context(), c.ID, c.Digest); err != nil {
		t.Fatal(err)
	}
	missing := restoreThemesFixture(t, config, "")
	current, previous, err = missing.ThemeSelection(t.Context())
	if err != nil || current.ID != "" || previous.ID != "" {
		t.Fatalf("missing packages remained selected=%+v %+v %v", current, previous, err)
	}
	if err := missing.EnableTheme(t.Context(), b.ID, b.Digest); err != ErrThemeContentMissing {
		t.Fatalf("missing package activation=%v", err)
	}
	installZIP(t, missing, themeZIP(t, "a", "two", 1))
	if err := missing.EnableTheme(t.Context(), b.ID, b.Digest); err != nil {
		t.Fatalf("re-upload did not restore missing files: %v", err)
	}
}

func TestThemeReuploadRemovesMissingArchivePlaceholder(t *testing.T) {
	t.Parallel()
	s := migrateFrom(t, 21, func(t *testing.T, db *sql.DB) {
		if _, err := db.Exec("INSERT INTO theme(id,name,version,preview,uploaded_at,enabled) VALUES('a','A','old','',1,1)"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("INSERT INTO theme_file(theme_id,path,content) VALUES('a','index.html',x'00')"); err != nil {
			t.Fatal(err)
		}
	})
	a := installZIP(t, s, themeZIP(t, "a", "one", 1))
	list, err := s.ListThemes(t.Context())
	if err != nil || len(list) != 1 || list[0].Digest != a.Digest {
		t.Fatalf("placeholder occupied version slot=%+v %v", list, err)
	}
	var orphan int
	if err := s.r.QueryRow("SELECT COUNT(*) FROM theme_file WHERE digest=''").Scan(&orphan); err != nil || orphan != 0 {
		t.Fatalf("placeholder files=%d %v", orphan, err)
	}
	for _, version := range []string{"two", "three"} {
		installZIP(t, s, themeZIP(t, "a", version, 1))
	}
}

func TestDeleteThemeClearsOnlyItsSelections(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	a := installZIP(t, s, themeZIP(t, "a", "one", 1))
	b := installZIP(t, s, themeZIP(t, "b", "two", 1))
	for _, version := range []Theme{a, b} {
		if err := s.EnableTheme(t.Context(), version.ID, version.Digest); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeleteTheme(t.Context(), a.ID); err != nil {
		t.Fatal(err)
	}
	current, previous, err := s.ThemeSelection(t.Context())
	if err != nil || current.Digest != b.Digest || previous.ID != "" {
		t.Fatalf("uninstall previous=%+v %+v %v", current, previous, err)
	}
	if err := s.DeleteTheme(t.Context(), b.ID); err != nil {
		t.Fatal(err)
	}
	current, _, err = s.ThemeSelection(t.Context())
	if err != nil || current.ID != "" {
		t.Fatalf("uninstall current did not fall back=%+v %v", current, err)
	}
	for _, table := range []string{"theme", "theme_version", "theme_file", "theme_package"} {
		var n int
		if err := s.r.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("uninstall left %s count=%d %v", table, n, err)
		}
	}
}

func TestLegacyThemeSnapshotMigration(t *testing.T) {
	t.Parallel()
	for _, format := range []bool{false, true} {
		t.Run(fmt.Sprint(format), func(t *testing.T) {
			path, db := frozenSchemaFixture(t, 21)
			content := themeZIP(t, "legacy", "old", 0)
			seedLegacyTheme(t, db, content)
			config, dir := filepath.Join(t.TempDir(), "config.db"), t.TempDir()
			legacySnapshot(t, path, config, "config", 21, format)
			name := "legacy.zip"
			if format {
				name = fmt.Sprintf("%x.zip", sha256.Sum256(content))
			}
			if err := os.WriteFile(filepath.Join(dir, name), content, 0600); err != nil {
				t.Fatal(err)
			}
			restored := restoreThemesFixture(t, config, dir)
			list, err := restored.ListThemes(t.Context())
			if err != nil || len(list) != 1 {
				t.Fatalf("legacy snapshot=%+v %v", list, err)
			}
			assertArchivedTheme(t, restored, list[0])
		})
	}
}

func TestThemeRestoreRejectsUnmatchedIdentityAndVersion(t *testing.T) {
	t.Parallel()
	for _, query := range []string{"DELETE FROM theme", "INSERT INTO theme(id) VALUES('orphan')", "DELETE FROM snapshot_theme"} {
		t.Run(query, func(t *testing.T) {
			s, _ := open(t)
			installZIP(t, s, themeZIP(t, "a", "one", 1))
			config := filepath.Join(t.TempDir(), "config.db")
			if err := s.SnapshotConfig(t.Context(), config); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", config)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(query); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(t.TempDir(), "target.db")
			_, err = Restore(t.Context(), target, config, "", "", time.Now(), slog.Default())
			if err == nil || !strings.Contains(err.Error(), "snapshot theme references do not match configuration") {
				t.Fatalf("unmatched references accepted: %v", err)
			}
			if _, err := os.Stat(target); !os.IsNotExist(err) {
				t.Fatalf("invalid snapshot created target: %v", err)
			}
		})
	}
}

func TestLegacyMetricsSnapshotRejectsUnknownFormat(t *testing.T) {
	t.Parallel()
	path, _ := frozenSchemaFixture(t, 21)
	s, _ := open(t)
	config, metrics := filepath.Join(t.TempDir(), "config.db"), filepath.Join(t.TempDir(), "metrics.db")
	if err := s.SnapshotConfig(t.Context(), config); err != nil {
		t.Fatal(err)
	}
	legacySnapshot(t, path, metrics, "metrics", 21, true)
	db, err := sql.Open("sqlite", metrics)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE snapshot_meta SET format_version=99"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = Restore(t.Context(), filepath.Join(t.TempDir(), "target.db"), config, metrics, "", time.Now(), slog.Default())
	if err == nil || !strings.Contains(err.Error(), "unsupported format_version=99") {
		t.Fatalf("unknown metrics format accepted: %v", err)
	}
}

// 旧快照按当时的表结构生成，不调用只面向当前 schema 的生产快照代码。
func legacySnapshot(t *testing.T, source, target, layer string, version int, format bool) {
	t.Helper()
	db, err := sql.Open("sqlite", dsn(source, "&mode=ro"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("ATTACH DATABASE ? AS snap", target); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	tables := []string{"metric_1m", "metric_5m", "metric_1h", "probe_1m", "probe_5m", "probe_1h", "rollup_state", "maintenance_state"}
	if layer == "config" {
		tables = []string{"node", "node_facts", "traffic", "probe_task", "probe_task_node", "probe_meta", "alert_rule", "alert_rule_node", "alert_rule_channel", "alert_state", "alert_event", "alert_delivery", "notify_channel", "setting", "admin", "admin_security", "api_token", "tag", "node_tag", "theme", "restore_record", "probe_task_tag", "alert_rule_tag"}
	}
	for _, table := range tables {
		if _, err := tx.Exec("CREATE TABLE snap." + table + " AS SELECT * FROM main." + table); err != nil {
			t.Fatal(err)
		}
	}
	for _, query := range []string{"CREATE TABLE snap.sequence_seed(id INTEGER PRIMARY KEY AUTOINCREMENT)", "DROP TABLE snap.sequence_seed", "INSERT INTO snap.sqlite_sequence SELECT * FROM main.sqlite_sequence", "CREATE TABLE snap.snapshot_meta(schema_version INTEGER,taken_at INTEGER,layer TEXT)"} {
		if _, err := tx.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec("INSERT INTO snap.snapshot_meta VALUES(?,100,?)", version, layer); err != nil {
		t.Fatal(err)
	}
	if format {
		if _, err := tx.Exec("ALTER TABLE snap.snapshot_meta ADD COLUMN format_version INTEGER NOT NULL DEFAULT 2"); err != nil {
			t.Fatal(err)
		}
		if layer == "config" {
			if _, err := tx.Exec("CREATE TABLE snap.snapshot_theme(theme_id TEXT PRIMARY KEY,sha256 TEXT NOT NULL)"); err != nil {
				t.Fatal(err)
			}
			rows, err := tx.Query("SELECT t.id,p.content FROM main.theme t LEFT JOIN main.theme_package p ON p.theme_id=t.id")
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var id string
				var content []byte
				if err := rows.Scan(&id, &content); err != nil {
					t.Fatal(err)
				}
				digest := ""
				if len(content) > 0 {
					digest = fmt.Sprintf("%x", sha256.Sum256(content))
				}
				if _, err := tx.Exec("INSERT INTO snap.snapshot_theme VALUES(?,?)", id, digest); err != nil {
					t.Fatal(err)
				}
			}
			if err := rows.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
