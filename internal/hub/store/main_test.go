package store

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/clock"
)

// 本包的用例默认并行（t.Parallel）。一个用例能并行的前提，缺一条就不加并写明原因：
//   - 不与别的用例共享假时钟或 Store：各自经 open、migrateFrom 等夹具在自己的 t.TempDir 里建库；
//   - 不用 t.Setenv，不改进程级设置（GOMAXPROCS、slog 默认 logger、全局驱动表里已注册的名字）；
//   - 不读写包级可变状态；包级只放只读夹具（frozenSchemaFixture 的各版本模板、freshSchema），建成之后谁都不写；
//   - 断言里的耗时阈值（上界或比值）远大于负载能造成的停顿：负载下一次调度停顿可达几十毫秒，阈值在这个量级的
//     不并行；阈值在百毫秒以上且比被量操作的正常耗时大两个数量级的，或只用来区分"等满了某个超时"与"没等"的
//     （如 drainTimeout，缺陷路径至少要等满它），可以并行。
//
// 不并行的用例在 go test 里先于全部并行用例串行跑完，两类不会重叠。

// fixtureDir 存放包级只读夹具（模板库），TestMain 在全部用例结束后删除。
var fixtureDir string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "heron-store-fixtures-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fixtureDir = dir
	code := m.Run()
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

// probeHistoryRows 是模板库 probe_1h 里的行数：要覆盖读额度用例用到的最大行数（CountStopsAtLimit 的
// 800000，其余用例都在额度 quotaRowsPerSeries×MaxTasksPerNode = 768000 附近）。
const probeHistoryRows = 800000

// buildProbeHistoryTemplate 在 t.TempDir 里建一个模板库并返回路径：当前 schema，probe_1h 里有经
// fillProbeRows(node 1, task 1) 灌入的 probeHistoryRows 行，其余表与刚建成的库相同。读额度的用例要越过 768000 行的
// 额度，每个用例在 -race 下各自灌一遍要几十秒，灌的又是同一份数据，所以只灌一次，用例复制文件。模板随 t 的
// TempDir 删除，t 的并行子测试全部结束之后才删，所以只把路径交给 t 的子测试（见 TestReadQuotaOnProbeHistory）。
//
// 复制只拷主文件：Close 关掉最后一个连接时 SQLite 把 WAL 检查点进主文件并删掉 WAL，这里核对 WAL 确实不在，
// 不在才说明主文件已经是完整的库。
func buildProbeHistoryTemplate(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "probe-history.db")
	s, err := Open(path, clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)), slog.New(slog.NewTextHandler(io.Discard, nil)), MigrateSchema)
	if err != nil {
		t.Fatalf("probe history template: %v", err)
	}
	fillErr := fillProbeRows(t.Context(), s.w, "probe_1h", 1, 1, probeHistoryRows)
	if err := errors.Join(fillErr, s.Close()); err != nil {
		t.Fatalf("probe history template: %v", err)
	}
	if _, err := os.Stat(path + "-wal"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("probe history template: WAL still present after Close (stat err %v); copying the main file alone would lose rows", err)
	}
	return path
}

// openProbeHistory 打开模板库 tpl 的一份副本，probe_1h 截到前 rows 行，结果与在新库上 fillProbeRows(node 1, task 1, rows)
// 相同：fillProbeRows 的第 g 行（从 0 数）ts 为 (g+1)×3600、task 只取决于 g，所以前 rows 行恰是 ts ≤ rows×3600 的行。
// 时钟与 open 相同。副本在 t.TempDir 里，用例可以随意写。
func openProbeHistory(t *testing.T, tpl string, rows int64) (*Store, *clock.Fake) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "t.db")
	copyFile(t, tpl, path)
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	s, err := Open(path, clk, slog.Default(), MigrateSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if rows > probeHistoryRows {
		t.Fatalf("probe history template has %d rows, want at least %d", probeHistoryRows, rows)
	}
	// 核对删掉的行数：模板口径与 probeHistoryRows 对不上时当场失败，不让用例在错的行数上断言。
	res, err := s.w.Exec("DELETE FROM probe_1h WHERE node_id = 1 AND ts > ?", rows*3600)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := res.RowsAffected(); err != nil || n != probeHistoryRows-rows {
		t.Fatalf("truncating probe history from %d to %d rows deleted %d (err %v), want %d", probeHistoryRows, rows, n, err, probeHistoryRows-rows)
	}
	return s, clk
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}
