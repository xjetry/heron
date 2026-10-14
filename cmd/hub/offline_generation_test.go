package main

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/store"
)

func offlineGeneration(t *testing.T, db string) uint64 {
	t.Helper()
	st, err := store.Open(db, clock.Real(), slog.New(slog.DiscardHandler), store.RequireCurrentSchema)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	g, err := st.OfflineGeneration(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// 每个写库的离线子命令都推进离线变更代数（运行中的 hub 据此重载），只读的不推进（不给 hub 制造无谓的重载）。
// 判定是"库外写者"这一角色，不逐命令挑选：清单以 main 的子命令表为准，restore 要求 hub 已停止、另有用例。
func TestOfflineCommandsAdvanceGenerationOnlyWhenTheyWrite(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		writes bool
		run    func(t *testing.T, db string) error
	}{
		{"node create", true, func(t *testing.T, db string) error { return runNode([]string{"create", "--db", db, "--name", "n2"}) }},
		{"node rotate-token", true, func(t *testing.T, db string) error {
			return runNode([]string{"rotate-token", "--db", db, "--id", "1"})
		}},
		{"node delete", true, func(t *testing.T, db string) error { return runNode([]string{"delete", "--db", db, "--id", "1"}) }},
		{"window open", true, func(t *testing.T, db string) error { return runWindow([]string{"open", "--db", db}) }},
		{"window close", true, func(t *testing.T, db string) error { return runWindow([]string{"close", "--db", db}) }},
		{"token revoke", true, func(t *testing.T, db string) error {
			return runTokenWith([]string{"revoke", "--all", "--db", db}, io.Discard, io.Discard)
		}},
		{"passwd", true, func(t *testing.T, db string) error {
			return runPasswdWith([]string{"--db", db}, pipeWith(t, "a sufficiently long password\n"), io.Discard)
		}},
		{"security-reset", true, func(t *testing.T, db string) error { return runSecurityReset([]string{"--db", db, "--yes"}) }},
		{"node list", false, func(t *testing.T, db string) error { return runNode([]string{"list", "--db", db}) }},
		{"window show", false, func(t *testing.T, db string) error { return runWindow([]string{"show", "--db", db}) }},
		{"token list", false, func(t *testing.T, db string) error {
			return runTokenWith([]string{"list", "--db", db}, io.Discard, io.Discard)
		}},
		{"stats", false, func(t *testing.T, db string) error { return runStatsWith([]string{"--db", db}, io.Discard) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			db := filepath.Join(t.TempDir(), "hub.db")
			if err := runNode([]string{"create", "--db", db, "--name", "n1"}); err != nil {
				t.Fatal(err)
			}
			before := offlineGeneration(t, db)
			if err := c.run(t, db); err != nil {
				t.Fatal(err)
			}
			after := offlineGeneration(t, db)
			if c.writes && after <= before {
				t.Fatalf("%s wrote the database without advancing the offline generation (%d → %d)", c.name, before, after)
			}
			if !c.writes && after != before {
				t.Fatalf("%s only reads, but the offline generation moved %d → %d", c.name, before, after)
			}
		})
	}
}
