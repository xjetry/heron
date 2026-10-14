package store

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/xjetry/heron/internal/clock"
)

func TestDatabasePathsAreNotURIComponents(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, separator string }{{"hash", "#"}, {"query", "?"}, {"percent", "%23"}, {"space", " "}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, suffix := range []string{"one", "two"} {
				path := filepath.Join(dir, "hub"+tc.separator+suffix+".db")
				s, err := Open(path, clock.Real(), slog.Default(), MigrateSchema)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { s.Close() })
				nodes, err := s.ListNodes(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if len(nodes) != 0 {
					t.Fatalf("independent path %q reused another database: %v", path, nodes)
				}
				if _, _, err := s.CreateNode(t.Context(), NewNode{Name: suffix}, hash(1)); err != nil {
					t.Fatal(err)
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				reopened, err := Open(path, clock.Real(), slog.Default(), MigrateSchema)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { reopened.Close() })
				nodes, err = reopened.ListNodes(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if len(nodes) != 1 || nodes[0].Name != suffix {
					t.Fatalf("path %q did not preserve its own data: %v", path, nodes)
				}
			}
			for _, suffix := range []string{"one", "two"} {
				path := filepath.Join(dir, "hub"+tc.separator+suffix+".db")
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("literal database path %q missing: %v", path, err)
				}
			}
		})
	}
}
