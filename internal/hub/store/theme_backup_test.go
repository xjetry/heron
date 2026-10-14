package store

import (
	"database/sql"
	"reflect"
	"strings"
	"testing"
)

type storedThemePackage struct {
	Content  string
	Revision int64
	Uploaded bool
}

func storedPackage(t *testing.T, s *Store, id string) storedThemePackage {
	t.Helper()
	var p storedThemePackage
	if err := s.r.QueryRow("SELECT content, revision, uploaded FROM theme_package WHERE theme_id = ?", id).Scan(&p.Content, &p.Revision, &p.Uploaded); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestThemePackageStoredAndReplaced(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	putTheme(t, s, "a", "index.html")
	p := storedPackage(t, s, "a")
	if p.Content != "original zip a" || p.Revision <= 0 || p.Uploaded {
		t.Fatalf("initial package=%+v, want original bytes, positive write ID and pending upload", p)
	}
	if err := s.write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE theme_package SET uploaded = 1 WHERE theme_id = 'a'")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	putTheme(t, s, "a", "index.html")
	next := storedPackage(t, s, "a")
	if next.Content != p.Content || next.Revision != p.Revision || !next.Uploaded {
		t.Fatalf("repeated package=%+v previous=%+v, want original write ID and uploaded state", next, p)
	}
}

func TestThemePackageTransactions(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"put", "delete"} {
		t.Run(operation, func(t *testing.T) {
			s, _ := open(t)
			installed := putTheme(t, s, "a", "index.html")
			before := storedPackage(t, s, "a")
			<-s.ThemeChanges()
			themes, err := s.ListThemes(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			files := themeFiles(t, s, "a")
			event := "INSERT"
			if operation == "delete" {
				event = "DELETE"
			}
			if err := s.write(t.Context(), func(tx *sql.Tx) error {
				_, err := tx.Exec("CREATE TRIGGER reject_file BEFORE " + event + " ON theme_file BEGIN SELECT RAISE(ABORT, 'file rejected'); END")
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if operation == "put" {
				_, err = s.PutTheme(t.Context(), Theme{ID: "a", Name: "changed", SDK: 1}, []ThemeFile{{Path: "new", Content: []byte("new")}}, []byte("new zip"), true, 20)
			} else {
				err = s.DeleteThemeVersion(t.Context(), "a", installed.Digest)
			}
			if err == nil || !strings.Contains(err.Error(), "file rejected") {
				t.Fatalf("transaction error=%v", err)
			}
			after, err := s.ListThemes(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if p := storedPackage(t, s, "a"); p != before || !reflect.DeepEqual(themes, after) || !reflect.DeepEqual(files, themeFiles(t, s, "a")) {
				t.Fatalf("failed %s changed package, metadata or files: package=%+v previous=%+v", operation, p, before)
			}
			select {
			case <-s.ThemeChanges():
				t.Fatal("failed transaction woke theme backup")
			default:
			}
		})
	}
}

func TestThemePackageDelete(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	installed := putTheme(t, s, "a", "index.html")
	if err := s.DeleteThemeVersion(t.Context(), "a", installed.Digest); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.r.QueryRow("SELECT COUNT(*) FROM theme_package").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("deleted theme left %d packages", count)
	}
}
