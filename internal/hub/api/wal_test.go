package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/store"
)

func TestWALObservationWireStates(t *testing.T) {
	t.Parallel()
	zero, size := int64(0), int64(12345)
	for _, tc := range []struct {
		name string
		wal  store.WALObservation
		json string
	}{
		{"present", store.WALObservation{ObservedAt: 123, Bytes: &size}, `{"wal":{"observedAt":"123","bytes":"12345"}}`},
		{"empty", store.WALObservation{ObservedAt: 123, Bytes: &zero}, `{"wal":{"observedAt":"123","bytes":"0"}}`},
		{"absent", store.WALObservation{ObservedAt: 123, Absent: true}, `{"wal":{"observedAt":"123","absent":true}}`},
		{"error", store.WALObservation{ObservedAt: 123, Error: "permission denied"}, `{"wal":{"observedAt":"123","error":"permission denied"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := &heronv1.GetStorageStatsResponse{Wal: walObservationProto(tc.wal)}
			want := &heronv1.GetStorageStatsResponse{}
			if err := protojson.Unmarshal([]byte(tc.json), want); err != nil {
				t.Fatal(err)
			}
			if !proto.Equal(got, want) {
				t.Fatalf("WAL projection = %v, want %v", got, want)
			}
			for _, codec := range []struct {
				name      string
				marshal   func(proto.Message) ([]byte, error)
				unmarshal func([]byte, proto.Message) error
			}{{"protobuf", proto.Marshal, proto.Unmarshal}, {"json", protojson.Marshal, protojson.Unmarshal}} {
				data, err := codec.marshal(got)
				if err != nil {
					t.Fatal(err)
				}
				back := &heronv1.GetStorageStatsResponse{}
				if err := codec.unmarshal(data, back); err != nil {
					t.Fatal(err)
				}
				if !proto.Equal(back, want) {
					t.Fatalf("%s roundtrip = %v, want %v", codec.name, back, want)
				}
			}
		})
	}
	old := &heronv1.GetStorageStatsResponse{}
	if err := protojson.Unmarshal([]byte(`{"dbBytes":"123"}`), old); err != nil {
		t.Fatal(err)
	}
	if old.Wal != nil {
		t.Fatalf("response without a wal field acquired an observation: %v", old.Wal)
	}
}

func TestGetStorageStatsIncludesWALObservation(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	h.createNode(t, "wal")
	resp, err := h.admin.GetStorageStats(t.Context(), connect.NewRequest(&heronv1.GetStorageStatsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	w := resp.Msg.Wal
	if w == nil || w.GetBytes() == 0 || w.ObservedAt != h.clk.Now().Unix() {
		t.Fatalf("WAL response = %v, want present with hub time", w)
	}
}

// 迁移前备份与 WAL 上限原样投影到响应：路径、schema、大小、mtime 逐项，按序号从新到旧；上限是 store 的 journal_size_limit。
// 读不了库所在目录（只有 w、x 没有 r：SQLite 仍能读写库和 -wal，列目录失败）时列表为空、错误文本非空，其余读数照常。
func TestGetStorageStatsProjectsSchemaBackups(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	dir := filepath.Dir(h.dbPath)
	mtime := time.Unix(1_750_000_000, 0)
	for name, content := range map[string]string{".before-schema-39.1": "one", ".before-schema-40.2": "second"} {
		p := h.dbPath + name
		if err := os.WriteFile(p, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := h.admin.GetStorageStats(t.Context(), connect.NewRequest(&heronv1.GetStorageStatsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	want := []*heronv1.SchemaBackup{
		{Path: h.dbPath + ".before-schema-40.2", SchemaVersion: 40, Bytes: 6, ModifiedAt: mtime.Unix()},
		{Path: h.dbPath + ".before-schema-39.1", SchemaVersion: 39, Bytes: 3, ModifiedAt: mtime.Unix()},
	}
	got := resp.Msg.SchemaBackups
	if len(got) != len(want) || resp.Msg.SchemaBackupsError != "" {
		t.Fatalf("schema backups = %v, error %q; want %v", got, resp.Msg.SchemaBackupsError, want)
	}
	for i := range want {
		if !proto.Equal(got[i], want[i]) {
			t.Errorf("schema backup %d = %v, want %v", i, got[i], want[i])
		}
	}
	if resp.Msg.WalSizeLimitBytes != 16<<20 {
		t.Errorf("wal_size_limit_bytes = %d, want %d", resp.Msg.WalSizeLimitBytes, 16<<20)
	}

	if os.Geteuid() == 0 {
		t.Skip("root reads any directory; the listing failure needs a non-root user")
	}
	if err := os.Chmod(dir, 0300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0700) })
	resp, err = h.admin.GetStorageStats(t.Context(), connect.NewRequest(&heronv1.GetStorageStatsRequest{}))
	if err != nil {
		t.Fatalf("GetStorageStats with an unreadable directory: %v", err)
	}
	if len(resp.Msg.SchemaBackups) != 0 || !strings.Contains(resp.Msg.SchemaBackupsError, "permission denied") {
		t.Errorf("schema backups = %v, error %q; want none with a permission error", resp.Msg.SchemaBackups, resp.Msg.SchemaBackupsError)
	}
	if resp.Msg.DbBytes == 0 || resp.Msg.WalSizeLimitBytes != 16<<20 || resp.Msg.Wal == nil {
		t.Errorf("other readings missing when listing fails: %v", resp.Msg)
	}
}
