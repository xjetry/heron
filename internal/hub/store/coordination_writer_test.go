package store

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/clock"
)

// openPair 在同一个库上打开 hub 自己的 Store 与一个库外写者的 Store，两者与运行中的 hub 和离线子命令同形。
func openPair(t *testing.T) (hub, external *Store, path string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "t.db")
	hub, err := Open(path, clock.Real(), slog.Default(), MigrateSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hub.Close() })
	external, err = Open(path, clock.Real(), slog.Default(), RequireCurrentSchema, ExternalWriter())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { external.Close() })
	return hub, external, path
}

func wantGeneration(t *testing.T, s *Store, want uint64) {
	t.Helper()
	if g, err := s.OfflineGeneration(t.Context()); err != nil || g != want {
		t.Fatalf("offline generation = %d, %v; want %d", g, err, want)
	}
}

// 每个提交的库外写事务推进一次，回滚的不推进，hub 自己的写永不推进；同步、异步与类型化的写方法都经 runWriter。
func TestExternalWriterAdvancesOfflineGenerationPerCommit(t *testing.T) {
	t.Parallel()
	hub, external, _ := openPair(t)
	ctx := t.Context()
	if _, _, err := hub.CreateNode(ctx, NewNode{Name: "online"}, hash(1)); err != nil {
		t.Fatal(err)
	}
	wantGeneration(t, hub, 0)

	if _, _, err := external.CreateNode(ctx, NewNode{Name: "offline"}, hash(2)); err != nil {
		t.Fatal(err)
	}
	wantGeneration(t, hub, 1)

	refused := errors.New("refused")
	if err := external.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec("DELETE FROM node"); err != nil {
			return err
		}
		return refused
	}); !errors.Is(err, refused) {
		t.Fatalf("failing write = %v, want %v", err, refused)
	}
	wantGeneration(t, hub, 1)

	done := make(chan error, 1)
	external.writeAsync(func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE node SET note = 'async' WHERE name = 'offline'")
		return err
	}, func(err error) { done <- err })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	wantGeneration(t, hub, 2)

	if err := external.SetTokenHash(ctx, 2, hash(2), hash(3)); err != nil {
		t.Fatal(err)
	}
	wantGeneration(t, external, 3)
	if nodes, err := hub.ListNodes(ctx); err != nil || len(nodes) != 2 {
		t.Fatalf("rolled-back external write leaked: nodes %v, %v", nodes, err)
	}
}

// 推进失败（协调行缺失）时整个库外写事务回滚：不推进的库外写入运行中的 hub 永远看不到，宁可让离线命令报错。
func TestExternalWriteRollsBackWithoutCoordinationRow(t *testing.T) {
	t.Parallel()
	hub, external, _ := openPair(t)
	ctx := t.Context()
	if err := hub.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec("DELETE FROM hub_coordination")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := external.CreateNode(ctx, NewNode{Name: "offline"}, hash(2)); err == nil {
		t.Fatal("external write committed without advancing the offline generation")
	}
	if nodes, err := hub.ListNodes(ctx); err != nil || len(nodes) != 0 {
		t.Fatalf("external write without advance left nodes %v, %v", nodes, err)
	}
	// hub 自己的写不碰协调行，缺行不影响它。
	if _, _, err := hub.CreateNode(ctx, NewNode{Name: "online"}, hash(1)); err != nil {
		t.Fatal(err)
	}
}

// 运行中的 hub 与离线子命令是同一个库的两个写者，谁先开写事务都一样：先开的一方读过之后，另一方要提交就得等它提交，
// 先开的一方随后的写不得因快照已被对方的提交淘汰而失败（SQLITE_BUSY_SNAPSHOT 不经 busy_timeout 等待，直接报错）。
func TestWriterThatReadFirstIsNotRefusedByConcurrentWriter(t *testing.T) {
	t.Parallel()
	for _, externalFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("external_first=%v", externalFirst), func(t *testing.T) {
			t.Parallel()
			hub, external, _ := openPair(t)
			ctx := t.Context()
			if _, _, err := hub.CreateNode(ctx, NewNode{Name: "n"}, hash(1)); err != nil {
				t.Fatal(err)
			}
			first, second := hub, external
			if externalFirst {
				first, second = external, hub
			}
			secondDone := make(chan error, 1)
			var secondErr error
			returned := false
			err := first.write(ctx, func(tx *sql.Tx) error {
				var n int
				if err := tx.QueryRow("SELECT COUNT(*) FROM node").Scan(&n); err != nil {
					return err
				}
				go func() {
					secondDone <- second.write(ctx, func(tx *sql.Tx) error {
						_, err := tx.Exec("UPDATE node SET note = 'second' WHERE name = 'n'")
						return err
					})
				}()
				// 另一方若能插进这个事务的读与写之间提交，几毫秒内就会返回；等待取远短于 busyTimeout 的一段，
				// 它被挡住时这段时间之后本事务提交、它随即拿到锁，仍在它自己的 busyTimeout 之内。
				select {
				case secondErr = <-secondDone:
					returned = true
				case <-time.After(busyTimeout / 5):
				}
				_, err := tx.Exec("UPDATE node SET note = 'first' WHERE name = 'n'")
				return err
			})
			if !returned {
				secondErr = <-secondDone
			}
			if err != nil {
				t.Fatalf("writer that read first: %v", err)
			}
			if secondErr != nil {
				t.Fatalf("concurrent writer: %v", secondErr)
			}
			var note string
			if err := hub.r.QueryRowContext(ctx, "SELECT note FROM node WHERE name = 'n'").Scan(&note); err != nil || note != "second" {
				t.Fatalf("note = %q, %v; want the concurrent writer's value, written after the first commit", note, err)
			}
		})
	}
}
