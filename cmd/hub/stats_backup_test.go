package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xjetry/heron/internal/hub/store"
)

type backupStatsFixture struct{ stats store.StorageStats }

func (f backupStatsFixture) Close() error { return nil }

func (f backupStatsFixture) StorageStats(context.Context) (store.StorageStats, error) {
	return f.stats, nil
}

// stats 原样打印迁移前备份：列举成功时 listed 加每份一行（没有备份就只有 listed），列举失败时 unknown 加错误项，
// 命令照常输出其余读数并以成功返回。
func TestStatsPrintsSchemaBackups(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		stats store.StorageStats
		want  string
	}{
		{"listed", store.StorageStats{SchemaBackups: []store.SchemaBackup{
			{Path: "/var/lib/heron/heron.db.before-schema-40.3", SchemaVersion: 40, Seq: 3, Bytes: 4096, ModifiedAt: 1700000000},
			{Path: "/var/lib/heron/heron.db.before-schema-39.2", SchemaVersion: 39, Seq: 2, Bytes: 2048, ModifiedAt: 1690000000},
		}}, "schema_backups.state: listed\n" +
			"schema_backup.3: path=\"/var/lib/heron/heron.db.before-schema-40.3\" schema_version=40 bytes=4096 modified_at=1700000000\n" +
			"schema_backup.2: path=\"/var/lib/heron/heron.db.before-schema-39.2\" schema_version=39 bytes=2048 modified_at=1690000000\n"},
		{"none", store.StorageStats{}, "schema_backups.state: listed\n"},
		{"error", store.StorageStats{SchemaBackupsError: "open /var/lib/heron: permission denied"},
			"schema_backups.state: unknown\nschema_backups.error: \"open /var/lib/heron: permission denied\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out strings.Builder
			err := runStatsWithSource([]string{"--db", filepath.Join(t.TempDir(), "unobserved.db")}, &out, func(string) (storageStatsSource, error) { return backupStatsFixture{tc.stats}, nil })
			if err != nil {
				t.Fatalf("stats returned %v, want success", err)
			}
			_, rest, ok := strings.Cut(out.String(), "cleanup.pending: 0\n")
			got, _, ok2 := strings.Cut(rest, "wal.observed_at: ")
			if !ok || !ok2 || got != tc.want {
				t.Errorf("schema backup lines = %q, want %q\nfull output:\n%s", got, tc.want, out.String())
			}
		})
	}
}
