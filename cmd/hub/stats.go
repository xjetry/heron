package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/xjetry/heron/internal/hub/store"
)

func runStats(args []string) error { return runStatsWith(args, os.Stdout) }

// runStatsWith 打印库的逻辑大小、每张表的行数与存储健康读数；数据来自 store.StorageStats，与
// AdminService.GetStorageStats 同源。除首行 db_bytes 外，不是表行数的读数都在表行数之后、键都带点号
// （sql.observed_at、表名.oldest、表名.watermark、prune.finished_at、rollup.finished_at、cleanup.pending、schema_backups.state、
// schema_backups.error、schema_backup.<seq>），不会与"表名: 行数"混淆：
// 解析方（scripts/e2e.sh 与 stats_health_test）按"键不带点号且值为整数"认表行，新增读数的键不带点号就会被当成一张表。
// sql.observed_at 是行数、逻辑大小与健康读数所在 SQL 快照的时刻（Unix 秒），与 wal.observed_at 不是同一时刻。缺失的值写 none。
// 离线命令不知道运行中 hub 的保留期配置，所以只给原值，不给标红结论（标红见 GetStorageStats）。
// wal.* 与 schema_backup* 原样展示 store 的单次文件观测；present（含零字节）、absent、unknown 分开，不另读文件系统。
// schema_backups.state 为 listed 时下面的 schema_backup.<seq> 行就是全部备份（可以没有），为 unknown 时列举失败、原因见 schema_backups.error。
// 本命令自己打开库：hub 未运行时观测到的是这次打开建立的 -wal（正常关闭过的库上为 0 字节）。
func runStatsWith(args []string, out io.Writer) error {
	return runStatsWithSource(args, out, func(path string) (storageStatsSource, error) {
		st, _, err := openOffline(path, false)
		return st, err
	})
}

type storageStatsSource interface {
	StorageStats(context.Context) (store.StorageStats, error)
	Close() error
}

func runStatsWithSource(args []string, out io.Writer, open func(string) (storageStatsSource, error)) error {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	db := fs.String("db", "heron.db", "SQLite database path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := open(*db)
	if err != nil {
		return err
	}
	defer st.Close()
	stats, err := st.StorageStats(context.Background())
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "db_bytes: %d\n", stats.DBBytes)
	for _, t := range stats.Tables {
		fmt.Fprintf(out, "%s: %d\n", t.Name, t.Rows)
	}
	fmt.Fprintf(out, "sql.observed_at: %d\n", stats.SQLObservedAt)
	for _, h := range stats.Series {
		fmt.Fprintf(out, "%s.oldest: %s\n", h.Table, orNone(h.Oldest))
		if h.Watermark != nil {
			fmt.Fprintf(out, "%s.watermark: %d\n", h.Table, *h.Watermark)
		}
	}
	fmt.Fprintf(out, "%s.finished_at: %s\n", store.MaintenancePrune, orNone(stats.LastPrune))
	fmt.Fprintf(out, "%s.finished_at: %s\n", store.MaintenanceRollup, orNone(stats.LastRollup))
	// 尚未完成的历史清理作业数，与表行数同一 SQL 快照。
	fmt.Fprintf(out, "cleanup.pending: %d\n", stats.CleanupPending)
	// 迁移前备份按 seq 从新到旧，每份一行；列举失败时打印错误项而不退出：其余读数仍然有效，"没有备份行"不等于没有备份。
	if stats.SchemaBackupsError != "" {
		fmt.Fprintln(out, "schema_backups.state: unknown")
		fmt.Fprintf(out, "schema_backups.error: %q\n", stats.SchemaBackupsError)
	} else {
		fmt.Fprintln(out, "schema_backups.state: listed")
		for _, b := range stats.SchemaBackups {
			fmt.Fprintf(out, "schema_backup.%d: path=%q schema_version=%d bytes=%d modified_at=%d\n", b.Seq, b.Path, b.SchemaVersion, b.Bytes, b.ModifiedAt)
		}
	}
	fmt.Fprintf(out, "wal.observed_at: %d\n", stats.WAL.ObservedAt)
	switch {
	case stats.WAL.Bytes != nil:
		fmt.Fprintln(out, "wal.state: present")
		fmt.Fprintf(out, "wal.bytes: %d\n", *stats.WAL.Bytes)
	case stats.WAL.Absent:
		fmt.Fprintln(out, "wal.state: absent (no WAL file)")
	default:
		fmt.Fprintln(out, "wal.state: unknown")
		fmt.Fprintf(out, "wal.error: %q\n", stats.WAL.Error)
	}
	return nil
}

func orNone(v *int64) string {
	if v == nil {
		return "none"
	}
	return strconv.FormatInt(*v, 10)
}
