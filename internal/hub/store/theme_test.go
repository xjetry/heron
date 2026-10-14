package store

import (
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

var schemaV15 = append(slices.Clone(schemaV14),
	"CREATE TABLE tag (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL, name_fold TEXT NOT NULL UNIQUE)",
	"CREATE TABLE node_tag (node_id INTEGER NOT NULL, tag_id INTEGER NOT NULL, PRIMARY KEY (node_id, tag_id)) WITHOUT ROWID",
	"CREATE INDEX node_tag_by_tag ON node_tag (tag_id)")

func TestThemePreviewAllocationIgnoresUnrelatedFiles(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	version, err := s.PutTheme(t.Context(), Theme{ID: "preview", Name: "Preview", SDK: 1, Preview: "preview.png"}, []ThemeFile{
		{Path: "index.html", Content: []byte("index")}, {Path: "preview.png", Content: []byte("image")}, {Path: "unused.bin", Content: []byte{}},
	}, []byte("archive"), false, 20)
	if err != nil {
		t.Fatal(err)
	}
	read := func() {
		t.Helper()
		meta, content, err := s.ThemePreview(t.Context(), version.ID, version.Digest)
		if err != nil || meta.Digest != version.Digest || string(content) != "image" {
			t.Fatalf("preview=%+v %q %v", meta, content, err)
		}
	}
	allocated := func() uint64 {
		read()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		for range 5 {
			read()
		}
		runtime.ReadMemStats(&after)
		return (after.TotalAlloc - before.TotalAlloc) / 5
	}
	small := allocated()
	if err := s.write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE theme_file SET content=zeroblob(?) WHERE path='unused.bin'", 8<<20)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if large := allocated(); large > small+(2<<20) {
		t.Fatalf("preview allocated unrelated file: small=%d large=%d", small, large)
	}
}

func TestMigrationFromV15AddsThemeTables(t *testing.T) {
	t.Parallel()
	migrated := migrateFrom(t, 15, func(t *testing.T, db *sql.DB) {
		if _, err := db.Exec("INSERT INTO node(id,name,token_hash,created_at) VALUES(7,'kept',x'00',1)"); err != nil {
			t.Fatal(err)
		}
	})
	if n, err := migrated.GetNode(t.Context(), 7); err != nil || n.Name != "kept" {
		t.Fatalf("node after migration=%+v %v", n, err)
	}
	fresh, _ := open(t)
	for _, s := range []*Store{migrated, fresh} {
		a := putTheme(t, s, "a", "index.html")
		if err := s.EnableTheme(t.Context(), a.ID, a.Digest); err != nil {
			t.Fatal(err)
		}
		for _, query := range []string{"INSERT INTO theme_selection(id) VALUES(2)", "UPDATE theme_selection SET current_id='' WHERE id=1"} {
			if err := s.write(t.Context(), func(tx *sql.Tx) error { _, err := tx.Exec(query); return err }); err == nil || !strings.Contains(err.Error(), "CHECK constraint failed") {
				t.Fatalf("invalid selection accepted: %v", err)
			}
		}
	}
}

func putTheme(t *testing.T, s *Store, id string, paths ...string) Theme {
	t.Helper()
	var files []ThemeFile
	for _, p := range paths {
		files = append(files, ThemeFile{Path: p, Content: []byte(id + ":" + p)})
	}
	th, err := s.PutTheme(t.Context(), Theme{ID: id, Name: "Theme " + id, Version: "1", SDK: 1, UploadedAt: time.Unix(100, 0)}, files, []byte("original zip "+id), false, 20)
	if err != nil {
		t.Fatal(err)
	}
	return th
}

func themeFiles(t *testing.T, s *Store, id string) map[string]string {
	t.Helper()
	rows, err := s.r.Query("SELECT path,content FROM theme_file WHERE theme_id=?", id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var p, c string
		if err := rows.Scan(&p, &c); err != nil {
			t.Fatal(err)
		}
		out[p] = c
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestThemeSelectionRetainsRollbackAndPublishedVersions(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	a := putTheme(t, s, "a", "index.html", "old.js")
	if err := s.EnableTheme(ctx, a.ID, a.Digest); err != nil {
		t.Fatal(err)
	}
	b, err := s.PutTheme(ctx, Theme{ID: "a", Name: "New", Version: "2", SDK: 1}, []ThemeFile{{Path: "index.html", Content: []byte("new")}}, []byte("new zip"), true, 20)
	if err != nil {
		t.Fatal(err)
	}
	current, previous, err := s.ThemeSelection(ctx)
	if err != nil || current.Digest != a.Digest || previous.ID != "" || b.Enabled {
		t.Fatalf("installation changed selection: %+v %+v %+v %v", current, previous, b, err)
	}
	if err := s.EnableTheme(ctx, b.ID, b.Digest); err != nil {
		t.Fatal(err)
	}
	if err := s.EnableTheme(ctx, b.ID, b.Digest); err != nil {
		t.Fatal(err)
	}
	current, previous, err = s.ThemeSelection(ctx)
	if err != nil || current.Digest != b.Digest || previous.Digest != a.Digest || !current.Published || !previous.Published {
		t.Fatalf("selection=%+v %+v %v", current, previous, err)
	}
	for _, version := range []Theme{a, b} {
		if err := s.DeleteThemeVersion(ctx, version.ID, version.Digest); !errors.Is(err, ErrThemeInUse) {
			t.Fatalf("deleting protected version=%v", err)
		}
	}
	_, oldFile, err := s.ThemeVersionFile(ctx, a.ID, a.Digest, "old.js")
	if err != nil || string(oldFile) != "a:old.js" {
		t.Fatalf("old resource=%q %v", oldFile, err)
	}
	if err := s.EnableTheme(ctx, a.ID, a.Digest); err != nil {
		t.Fatal(err)
	}
	current, previous, err = s.ThemeSelection(ctx)
	if err != nil || current.Digest != a.Digest || previous.Digest != b.Digest {
		t.Fatalf("rollback=%+v %+v %v", current, previous, err)
	}
	if err := s.EnableTheme(ctx, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteThemeVersion(ctx, b.ID, b.Digest); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ThemeVersionFile(ctx, b.ID, b.Digest, "index.html"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted version read=%v", err)
	}
}

func TestThemeInstallIdempotentAndAtomic(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	a := putTheme(t, s, "a", "index.html")
	before, _ := s.ListThemes(t.Context())
	_, err := s.PutTheme(t.Context(), Theme{ID: "a", Name: "Broken", SDK: 1}, []ThemeFile{{Path: "dup", Content: []byte("a")}, {Path: "dup", Content: []byte("b")}}, []byte("broken"), true, 20)
	if err == nil || !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		t.Fatalf("duplicate file failure=%v", err)
	}
	after, _ := s.ListThemes(t.Context())
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("failed installation changed metadata=%+v", after)
	}
	got, err := s.PutTheme(t.Context(), Theme{ID: "a", Name: "Not the original", SDK: 1}, []ThemeFile{{Path: "changed"}}, []byte("original zip a"), true, 20)
	if err != nil || got != a {
		t.Fatalf("same digest changed immutable metadata=%+v %v", got, err)
	}
	if f := themeFiles(t, s, "a"); !reflect.DeepEqual(f, map[string]string{"index.html": "a:index.html"}) {
		t.Fatalf("same digest changed files=%v", f)
	}
	if _, err := s.PutTheme(t.Context(), Theme{ID: "missing", SDK: 1}, []ThemeFile{{Path: "index.html"}}, []byte("missing"), true, 20); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing update=%v", err)
	}
}

func TestThemeLimitsAreAtomic(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	var wg sync.WaitGroup
	errs := make(chan error, 30)
	for i := range 30 {
		wg.Go(func() {
			_, err := s.PutTheme(ctx, Theme{ID: fmt.Sprintf("t%d", i), SDK: 1}, []ThemeFile{{Path: "index.html", Content: []byte{}}}, []byte(fmt.Sprintf("zip%d", i)), false, 20)
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	success := 0
	for err := range errs {
		if err == nil {
			success++
		} else if !errors.Is(err, ErrThemeLimit) {
			t.Fatal(err)
		}
	}
	if success != 20 {
		t.Fatalf("concurrent theme installs=%d want 20", success)
	}
	list, _ := s.ListThemes(ctx)
	id := list[0].ID
	errCh := make(chan error, 10)
	for i := range 10 {
		wg.Go(func() {
			_, err := s.PutTheme(ctx, Theme{ID: id, SDK: 1}, []ThemeFile{{Path: "index.html", Content: []byte{}}}, []byte(fmt.Sprintf("other%d", i)), true, 20)
			errCh <- err
		})
	}
	wg.Wait()
	close(errCh)
	success = 0
	for err := range errCh {
		if err == nil {
			success++
		} else if !errors.Is(err, ErrThemeVersionLimit) {
			t.Fatal(err)
		}
	}
	if success != 2 {
		t.Fatalf("concurrent additional versions=%d want 2", success)
	}
}

func TestThemeGenerationAndWholePackage(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	a := putTheme(t, s, "a", "index.html", "a.js")
	b := putTheme(t, s, "b", "index.html", "b.js")
	base := s.ThemeGeneration()
	if err := s.EnableTheme(ctx, "missing", "bad"); !errors.Is(err, ErrNotFound) || s.ThemeGeneration() != base {
		t.Fatalf("failed selection advanced generation: %v", err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 1)
	wg.Go(func() {
		for range 100 {
			for _, version := range []Theme{a, b} {
				if err := s.EnableTheme(ctx, version.ID, version.Digest); err != nil {
					errs <- err
					return
				}
			}
		}
	})
	for range 200 {
		g := s.ThemeGeneration()
		current, _, err := s.ThemeSelection(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if g < base {
			t.Fatalf("generation regressed=%d", g)
		}
		if current.ID != "" {
			meta, content, err := s.ThemeVersionFile(ctx, current.ID, current.Digest, "index.html")
			if err != nil {
				t.Fatal(err)
			}
			if meta.ID != current.ID || meta.Digest != current.Digest || string(content) != current.ID+":index.html" {
				t.Fatalf("mixed resource=%+v %q", meta, content)
			}
		}
	}
	wg.Wait()
	select {
	case err := <-errs:
		t.Fatal(err)
	default:
	}
	if s.ThemeGeneration() != base+200 {
		t.Fatalf("generation=%d want %d", s.ThemeGeneration(), base+200)
	}
}
