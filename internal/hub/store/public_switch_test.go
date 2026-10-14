package store

import (
	"database/sql"
	"log/slog"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/clock"
)

func TestPublicSwitchPersistenceAndMemory(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "hub.db")
	clk := clock.NewFake(time.Now())
	s, err := Open(path, clk, slog.Default(), MigrateSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if !s.PublicEnabled() {
		t.Fatal("new store gate is closed")
	}
	for _, enabled := range []bool{false, true, false} {
		if _, err := s.SaveSettings(t.Context(), SettingsUpdate{PublicEnabled: &enabled}); err != nil {
			t.Fatal(err)
		}
		var raw string
		if err := s.r.QueryRow("SELECT value FROM setting WHERE key = 'site.public_enabled'").Scan(&raw); err != nil {
			t.Fatal(err)
		}
		want := "0"
		if enabled {
			want = "1"
		}
		if raw != want || s.PublicEnabled() != enabled {
			t.Fatalf("saved gate: raw=%q memory=%v, want %s/%v", raw, s.PublicEnabled(), want, enabled)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		s, err = Open(path, clk, slog.Default(), RequireCurrentSchema)
		if err != nil {
			t.Fatal(err)
		}
		if s.PublicEnabled() != enabled {
			t.Fatalf("reopened gate=%v, want %v", s.PublicEnabled(), enabled)
		}
	}
}

// 内存副本必须等于库里最近一次提交的总闸。runWriter 串行提交，但各调用方醒来后的发布顺序不受它约束：两次保存
// 背靠背提交时，后提交的一方可以先发布，最后发布的就不是最后提交的，分叉一直留到下一次显式保存总闸。
// 每轮先用一个阻塞的写占住写协程，让两次相反的保存排进队列，放行后它们背靠背提交。持 siteWriteMu 时第二次保存
// 要等第一次发布完才入队，排不进去，所以等它入队的轮询以次数为界。GOMAXPROCS 取 1：醒来的调用方没有别的处理器
// 可用，通常要等写协程让出处理器才运行，错序才稳定出现；处理器多时它往往被空闲的处理器立刻取走，在下一次提交之前
// 就发布完了，自由并发的保存能否撞上错序随机器负载与是否开 -race 大幅波动。
// 这依赖当前 runtime 的调度行为：单处理器上写协程先后唤醒 A、B 且不让出时，调度器先运行最后被唤醒的 B
// （-race 下随机化，约一半），于是 B 先发布、A 后发布，库里是 B 而内存是 A。runtime 若改了这一点，去锁时
// 本用例只会变绿而不会误红，届时要换一种制造错序的手法。
//
// 不并行：GOMAXPROCS 是进程级设置，并行用例会在它取 1 的期间被压到单处理器上，它们的写协程也会和这里争唯一的处理器，
// 错序依赖的"写协程不让出"不再成立。
func TestPublicSwitchMemoryMatchesDatabaseUnderConcurrentSaves(t *testing.T) {
	prev := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(prev)
	s, _ := open(t)
	const rounds = 200
	mismatches := 0
	for round := range rounds {
		release, blocking := make(chan struct{}), make(chan struct{})
		s.writeAsync(func(*sql.Tx) error { close(blocking); <-release; return nil }, nil)
		<-blocking
		var wg sync.WaitGroup
		for queued, enabled := range []bool{round%2 == 0, round%2 != 0} {
			wg.Go(func() {
				if _, err := s.SaveSettings(t.Context(), SettingsUpdate{PublicEnabled: &enabled}); err != nil {
					t.Error(err)
				}
			})
			for n := 0; n < 100 && len(s.writes) <= queued; n++ {
				runtime.Gosched()
			}
		}
		close(release)
		wg.Wait()
		st, err := s.SiteSettings(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if st.PublicEnabled != s.PublicEnabled() {
			mismatches++
		}
	}
	if mismatches > 0 {
		t.Fatalf("memory differs from the committed gate after %d of %d rounds", mismatches, rounds)
	}
}

func TestPublicSwitchFailedSaveKeepsMemory(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	if err := s.write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.Exec("CREATE TRIGGER reject_gate BEFORE INSERT ON setting WHEN NEW.key = 'site.public_enabled' BEGIN SELECT RAISE(ABORT, 'gate rejected'); END")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_, err := s.SaveSettings(t.Context(), SettingsUpdate{Appearance: &SiteAppearance{Theme: "auto"}, PublicEnabled: new(bool)})
	if err == nil || !strings.Contains(err.Error(), "gate rejected") {
		t.Fatalf("save err=%v", err)
	}
	if !s.PublicEnabled() {
		t.Fatal("failed save published closed gate")
	}
	var count int
	if err := s.r.QueryRow("SELECT COUNT(*) FROM setting").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("failed save left %d rows", count)
	}
}

func TestPublicSwitchInvalidStoredValueRefused(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "hub.db")
	s, err := Open(path, clock.NewFake(time.Now()), slog.Default(), MigrateSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.Exec("INSERT INTO setting (key, value) VALUES ('site.public_enabled', 'invalid')")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, clock.NewFake(time.Now()), slog.Default(), RequireCurrentSchema)
	if reopened != nil {
		reopened.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "site.public_enabled must be 0 or 1") {
		t.Fatalf("invalid gate accepted: %v", err)
	}
}
