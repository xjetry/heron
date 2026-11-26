package store

import (
	"database/sql"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/clock"
)

// schemaV56 冻结 v56 的完整 DDL：v55 加上迁移 56 重建的操作记录表与按时刻的索引。
var schemaV56 = append(slices.Clone(schemaV55), migrationV56Config...)

// operationColumnsV56 是迁移 56 给操作记录加的列；伪造早于 56 的配置快照时撤掉它们（removeV56Config）。
var operationColumnsV56 = []string{"principal", "session_id", "source", "method", "resource_key"}

// removeV56Config 把当前版本导出的配置快照撤回迁移 56 之前的操作记录列集合。快照里的表是 CREATE TABLE AS 拷出的，
// 没有约束，可以直接删列。
func removeV56Config(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, column := range operationColumnsV56 {
		if _, err := db.Exec("ALTER TABLE operation DROP COLUMN " + column); err != nil {
			t.Fatal(column, err)
		}
	}
}

// legacyTokenIdentity 是种子里 API token 回执的 owner_key：迁移之前 token 的回执以它的身份（token_hash 的十六进制）为键。
const legacyTokenIdentity = "ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789"

// seedLegacyReceipts 写两条迁移 56 之前的回执：一条会话的、一条 API token 的。那时只有 ExecuteChange 写回执。
func seedLegacyReceipts(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, q := range []string{
		`INSERT INTO operation (owner_key,owner_id,request_id,request_hash,action,resource_id,before_json,after_json,committed_at) VALUES ('session',0,'by-session','h1','update_node',3,'{"node":[]}','{"node":[{"id":3}]}',100)`,
		`INSERT INTO operation (owner_key,owner_id,request_id,request_hash,action,resource_id,before_json,after_json,committed_at) VALUES ('` + legacyTokenIdentity + `',7,'by-token','h2','delete_node',4,'','',90)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
}

// assertLegacyReceipts 核对旧回执迁移后的样子：主体由 owner_key 推出，方法是 ExecuteChange，会话 id、来源与文本对象
// 是空串（当时没有记录），其余列原样；会话与 token 各自仍能按幂等键取回自己的回执。
func assertLegacyReceipts(t *testing.T, s *Store) {
	t.Helper()
	all, err := s.ListOperations(sessionContext(t.Context()), OperationFilter{Limit: 10})
	if err != nil || len(all) != 2 {
		t.Fatalf("migrated receipts: %+v %v", all, err)
	}
	want := map[string]struct {
		principal PrincipalKind
		owner     int64
		action    string
	}{"by-session": {PrincipalSession, 0, "update_node"}, "by-token": {PrincipalAPIToken, 7, "delete_node"}}
	for _, o := range all {
		w := want[o.RequestID]
		if o.Principal != w.principal || o.OwnerID != w.owner || o.Action != w.action || o.Method != "ExecuteChange" || o.SessionID != "" || o.Source != "" || o.ResourceKey != "" {
			t.Errorf("migrated receipt %s: %+v", o.RequestID, o)
		}
	}
	if all[0].RequestID != "by-session" || all[0].AfterJSON != `{"node":[{"id":3}]}` {
		t.Errorf("receipts out of order or details lost: %+v", all)
	}
	if o, err := s.FindOperation(sessionContext(t.Context()), 0, "by-session"); err != nil || o.RequestHash != "h1" {
		t.Errorf("session receipt lookup after migration: %+v %v", o, err)
	}
	token := tokenContext(t.Context(), APIToken{ID: 7, Identity: legacyTokenIdentity})
	if o, err := s.FindOperation(token, 7, "by-token"); err != nil || o.RequestHash != "h2" {
		t.Errorf("token receipt lookup after migration: %+v %v", o, err)
	}
	if own, err := s.ListOperations(token, OperationFilter{Limit: 10}); err != nil || len(own) != 1 || own[0].RequestID != "by-token" {
		t.Errorf("token sees after migration: %+v %v", own, err)
	}
}

func TestMigrationFromV55KeepsReceiptsQueryable(t *testing.T) {
	t.Parallel()
	s := migrateFrom(t, 55, seedLegacyReceipts)
	assertLegacyReceipts(t, s)
	// principal 没有默认值且只收三种取值：漏写主体或写错主体的插入在库里就被拒绝。
	for _, q := range []string{
		`INSERT INTO operation (owner_key,owner_id,request_id,request_hash,action,resource_id,before_json,after_json,committed_at,session_id,source,method,resource_key) VALUES ('session',0,'no-principal','','x',0,'','',1,'','','','')`,
		`INSERT INTO operation (owner_key,owner_id,request_id,request_hash,action,resource_id,before_json,after_json,committed_at,principal,session_id,source,method,resource_key) VALUES ('session',0,'bad-principal','','x',0,'','',1,'admin','','','','')`,
	} {
		if _, err := s.w.Exec(q); err == nil || !strings.Contains(err.Error(), "constraint") {
			t.Errorf("%s: %v, want a constraint failure", q, err)
		}
	}
}

// 55 的配置快照恢复到 56：快照迁移给配置层的操作记录加列并按同一规则回填，旧回执照常可查。
func TestRestoreConfigSnapshotFrom55KeepsReceipts(t *testing.T) {
	t.Parallel()
	path, db := frozenSchemaFixture(t, 55)
	seedLegacyReceipts(t, db)
	source := &Store{path: path, clk: clock.NewFake(time.Unix(2_000_000_000, 0))}
	config := filepath.Join(t.TempDir(), "config.db")
	if err := source.snapshot(t.Context(), config, "config", configSnapshotTables, nil); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target.db")
	if _, err := Restore(t.Context(), target, config, "", "", time.Now(), slog.Default()); err != nil {
		t.Fatal(err)
	}
	s, err := Open(target, clock.Real(), slog.Default(), RequireCurrentSchema)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	assertLegacyReceipts(t, s)
}

// 当前版本快照里主体不合规的记录：预检拒绝整份快照。恢复按 INSERT OR IGNORE 取并集，不在预检拒绝的话这条记录会被
// 静默跳过，目标库照常恢复，看起来什么都没丢。
func TestRestoreRejectsOperationWithUnknownPrincipal(t *testing.T) {
	t.Parallel()
	for name, update := range map[string]string{
		"unknown principal":       "UPDATE operation SET principal='admin'",
		"session principal owner": "UPDATE operation SET owner_key='ABCD'",
		"token principal owner":   "UPDATE operation SET principal='api_token'",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, _ := open(t)
			if err := s.ReorderNodes(WithAudit(sessionContext(t.Context()), &Audit{}), nil); err != nil {
				t.Fatal(err)
			}
			config := filepath.Join(t.TempDir(), "config.db")
			if err := s.SnapshotConfig(t.Context(), config); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", config)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(update); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			_, err = Restore(t.Context(), filepath.Join(t.TempDir(), "target.db"), config, "", "", time.Now(), slog.Default())
			if err == nil || !strings.Contains(err.Error(), "does not match its owner") {
				t.Fatalf("restore: %v, want the operation principal rejected", err)
			}
		})
	}
}
