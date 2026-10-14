package store

import (
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
)

var schemaV24 = append(slices.Clone(schemaV23), `CREATE TABLE node_update (node_id INTEGER PRIMARY KEY, data TEXT NOT NULL)`)

func TestUpdateMigrationAndDeletedNode(t *testing.T) {
	t.Parallel()
	s := migrateFrom(t, 23, seedMinuteRow)
	status := &heronv1.UpdateStatus{Supported: true, Version: "v0.2.0", Task: &heronv1.UpdateTask{Id: "0123456789abcdef", Version: "v0.3.0", State: "queued", ExpiresAt: 1}}
	if err := s.SaveNodeUpdate(t.Context(), 7, status); err != nil {
		t.Fatal(err)
	}
	rows, err := s.NodeUpdates(t.Context())
	if err != nil || rows[7].GetTask().GetState() != "queued" {
		t.Fatalf("persisted update=%v err=%v", rows, err)
	}
	if err := s.DeleteNode(t.Context(), 7); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveNodeUpdate(t.Context(), 7, status); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted node recreated update: %v", err)
	}
	var count int
	if err := s.r.QueryRow("SELECT count(*) FROM node_update").Scan(&count); err != nil || count != 0 {
		t.Fatalf("deleted update rows=%d err=%v", count, err)
	}
}

// StartNodeUpdate 在写事务里读已出租开关：已出租的机器不装 agent，更新目标不存在，拒绝时点名节点。
// 开关判定与并发改开关的 UpdateNodeTasks 串行化，不靠"界面不显示这个入口"兜底。
func TestStartNodeUpdateRejectsRentedNode(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	rented, _, err := s.CreateNode(t.Context(), NewNode{Name: "leased", Rented: true}, []byte("token-leased"))
	if err != nil {
		t.Fatal(err)
	}
	status := &heronv1.UpdateStatus{Supported: true, Version: "v0.2.0", Task: &heronv1.UpdateTask{Id: "0123456789abcdef", Version: "v0.3.0", State: "queued", ExpiresAt: 1}}
	err = s.StartNodeUpdate(t.Context(), rented, status)
	if !errors.Is(err, ErrNodeRented) {
		t.Fatalf("start rented node: err=%v, want ErrNodeRented", err)
	}
	for _, part := range []string{strconv.FormatInt(rented, 10), "leased", "rented"} {
		if !strings.Contains(err.Error(), part) {
			t.Fatalf("error %q does not name %q", err, part)
		}
	}
	if rows, err := s.NodeUpdates(t.Context()); err != nil || len(rows) != 0 {
		t.Fatalf("rejected start left rows=%v err=%v", rows, err)
	}
	plain, _, err := s.CreateNode(t.Context(), NewNode{Name: "plain"}, []byte("token-plain"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.StartNodeUpdate(t.Context(), plain, status); err != nil {
		t.Fatalf("start plain node: %v", err)
	}
}

func TestRestoreDoesNotReplayUpdates(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	id, _, err := s.CreateNode(t.Context(), NewNode{Name: "test"}, []byte("token"))
	if err != nil {
		t.Fatal(err)
	}
	status := &heronv1.UpdateStatus{Supported: true, Version: "v0.2.0", Task: &heronv1.UpdateTask{Id: "0123456789abcdef", Version: "v0.3.0", State: "queued", ExpiresAt: 1}}
	if err := s.SaveNodeUpdate(t.Context(), id, status); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "config.db")
	if err := s.SnapshotConfig(t.Context(), config); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(t.Context(), s.path, config, "", "", s.clk.Now(), slog.Default()); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(s.path, s.clk, slog.Default(), RequireCurrentSchema)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	rows, err := restored.NodeUpdates(t.Context())
	if err != nil || len(rows) != 0 {
		t.Fatalf("restored update authorization: %v err=%v", rows, err)
	}
}
