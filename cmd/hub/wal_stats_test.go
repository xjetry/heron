package main

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/store"
)

type statsFixture struct {
	wal   store.WALObservation
	calls int
}

func (f *statsFixture) Close() error { return nil }

func (f *statsFixture) StorageStats(context.Context) (store.StorageStats, error) {
	f.calls++
	if f.calls != 1 {
		return store.StorageStats{}, fmt.Errorf("stats observed more than once")
	}
	return store.StorageStats{SQLObservedAt: 122, WAL: f.wal}, nil
}

func TestStatsWALUsesSingleObservation(t *testing.T) {
	t.Parallel()
	zero, size := int64(0), int64(12345)
	for _, tc := range []struct {
		name string
		wal  store.WALObservation
		want string
	}{
		{"present", store.WALObservation{ObservedAt: 123, Bytes: &size}, "wal.observed_at: 123\nwal.state: present\nwal.bytes: 12345\n"},
		{"empty", store.WALObservation{ObservedAt: 123, Bytes: &zero}, "wal.observed_at: 123\nwal.state: present\nwal.bytes: 0\n"},
		{"absent", store.WALObservation{ObservedAt: 123, Absent: true}, "wal.observed_at: 123\nwal.state: absent (no WAL file)\n"},
		{"error", store.WALObservation{ObservedAt: 123, Error: "permission\ndenied"}, "wal.observed_at: 123\nwal.state: unknown\nwal.error: \"permission\\ndenied\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := &statsFixture{wal: tc.wal}
			var out strings.Builder
			if err := runStatsWithSource([]string{"--db", filepath.Join(t.TempDir(), "unobserved.db")}, &out, func(string) (storageStatsSource, error) { return fixture, nil }); err != nil {
				t.Fatal(err)
			}
			_, got, ok := strings.Cut(out.String(), "wal.observed_at: ")
			if !strings.Contains(out.String(), "sql.observed_at: 122\n") {
				t.Fatalf("SQL observation must come from the Store result: %q", out.String())
			}
			if fixture.calls != 1 || !ok || "wal.observed_at: "+got != tc.want {
				t.Fatalf("WAL output = %q, want %q; calls=%d", got, tc.want, fixture.calls)
			}
		})
	}
}

// 写入后不再修改库，让真实 CLI 与 API 可对照同一个固定 WAL 长度；观测墙钟仍各自取值。
func TestStatsCLIAndAPIMatchFrozenWAL(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "stats.db")
	clk := clock.NewFake(time.Unix(1700000000, 0))
	log := slog.Default()
	st, err := store.Open(path, clk, log, store.MigrateSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, _, err := st.CreateNode(t.Context(), store.NewNode{Name: "wal"}, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	_, svc := newTestServicesOn(t, st, clk)
	resp, err := svc.GetStorageStats(t.Context(), connect.NewRequest(&heronv1.GetStorageStatsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now().Unix()
	output, err := hubCommand(t, "stats", "--db", path).Output()
	if err != nil {
		t.Fatal(err)
	}
	after := time.Now().Unix()
	fields := map[string]string{}
	for _, line := range strings.Split(string(output), "\n") {
		key, value, ok := strings.Cut(line, ": ")
		if ok {
			fields[key] = value
		}
	}
	wal := resp.Msg.Wal
	if wal == nil || wal.GetBytes() == 0 || fields["wal.state"] != "present" || fields["wal.bytes"] != strconv.FormatUint(wal.GetBytes(), 10) {
		t.Fatalf("CLI WAL = %v, API WAL = %v", fields, wal)
	}
	at, err := strconv.ParseInt(fields["wal.observed_at"], 10, 64)
	if err != nil || at < before || at > after || wal.ObservedAt != clk.Now().Unix() {
		t.Fatalf("CLI observed_at=%s (bounds %d..%d), API=%v", fields["wal.observed_at"], before, after, wal)
	}
}
