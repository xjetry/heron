package store

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/clock"
)

// 非法保留期（零值的事件保留期会以现在为截止点删除此前全部事件）在 Open 时就被拒绝：拿不到 Store，
// 维护循环与清理也就无从以它运行。拒绝发生在迁移之前，不存在的库文件不会被建出来，已有库里的事件原样保留。
func TestOpenRejectsInvalidRetentionBeforeTouchingTheDatabase(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	fresh := filepath.Join(t.TempDir(), "fresh.db")
	if _, err := Open(fresh, clk, slog.Default(), MigrateSchema, WithRetention(Retention{})); err == nil || !strings.Contains(err.Error(), "alert event retention") {
		t.Fatalf("zero retention opened a fresh database: %v", err)
	}
	if _, err := os.Stat(fresh); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("rejected open created the database file: %v", err)
	}

	path := filepath.Join(t.TempDir(), "t.db")
	s, err := Open(path, clk, slog.Default(), MigrateSchema)
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := s.CreateNode(t.Context(), NewNode{Name: "n"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	r := saveRule(t, s, AlertRule{Name: "offline", Kind: KindOffline, AllNodes: true})
	ev := recordEvent(t, s, r.ID, id, nil)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, clk, slog.Default(), MigrateSchema, WithRetention(Retention{})); err == nil || !strings.Contains(err.Error(), "alert event retention") {
		t.Fatalf("zero retention opened an existing database: %v", err)
	}
	s, err = Open(path, clk, slog.Default(), MigrateSchema)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.ListAlertEvents(t.Context(), 0, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != ev.ID {
		t.Fatalf("invalid retention deleted events: %+v", rows)
	}
}

// 生效保留期只经 WithRetention 给出：缺省是 DefaultRetention，给出的值原样成为 Retention()。
func TestOpenUsesTheGivenRetention(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	s, err := Open(filepath.Join(t.TempDir(), "default.db"), clk, slog.Default(), MigrateSchema)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.Retention() != DefaultRetention {
		t.Fatalf("default retention = %+v", s.Retention())
	}
	want := Retention{M1: MinRetentionM1, M5: MinRetentionM5, H1: MinRetentionH1, AlertEvents: MinRetentionAlertEvents}
	c, err := Open(filepath.Join(t.TempDir(), "custom.db"), clk, slog.Default(), MigrateSchema, WithRetention(want))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Retention() != want {
		t.Fatalf("configured retention = %+v, want %+v", c.Retention(), want)
	}
}
