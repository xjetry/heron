package store

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/metric"
)

// schemaV46 冻结 v46 的完整 DDL：v45 加上节点资料版本表、种子行与推进它的触发器（迁移 46 的那一组语句）。
var schemaV46 = append(slices.Clone(schemaV45), migrationV46Config...)

func revisionOf(t *testing.T, s *Store) uint64 {
	t.Helper()
	v, err := nodeListRevision(t.Context(), s.r)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// v45 的库升上来后有版本表与种子行，触发器随即生效：迁移之后的第一次资料写入就推进。
func TestMigrationFromV45AddsNodeListRevision(t *testing.T) {
	t.Parallel()
	s := migrateFrom(t, 45, seedMinuteRow)
	if v := revisionOf(t, s); v != 0 {
		t.Fatalf("迁移后的版本 = %d，应为种子 0", v)
	}
	if _, err := s.UpdateNodeTasks(t.Context(), 7, NodeEdit{Name: "renamed", TrafficResetDay: 1}); err != nil {
		t.Fatal(err)
	}
	if v := revisionOf(t, s); v == 0 {
		t.Fatal("迁移后改节点名，版本应推进")
	}
}

// nodeWithFacts 建一个带 facts 的节点，返回库与节点 id；每个用例自己一份库，改主键的用例不影响别人。
func nodeWithFacts(t *testing.T) (*Store, int64) {
	t.Helper()
	s, _ := open(t)
	id, _, err := s.CreateNode(t.Context(), NewNode{Name: "n"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	f := &heronv1.Facts{Hostname: "h", Os: "linux", Kernel: "6", Arch: "amd64", Virtualization: "kvm", CpuModel: "c", CpuCores: 2, AgentVersion: "v1.0.0"}
	if err := s.UpsertFacts(t.Context(), id, 1, f); err != nil {
		t.Fatal(err)
	}
	return s, id
}

// 列归类：true 表示这一列出现在 ListNodes 的内容里（直接或经推导），改它必须推进版本；false 的列与理由：
//   - node.last_seen_at：实时状态，面板从快照取，分钟刷出每分钟写它；
//   - node.token_hash：节点凭据，不在 ListNodes 里；
//   - node_facts.facts_hash、facts_rev：agent 对整份 Facts 算的摘要与 hub 的持久化字段集合版本，不在 ListNodes 里。
//
// 两张表的列由 PRAGMA 枚举，清单之外的列直接判红：加列必须在这里归类，并改 schema.go 里对应触发器的 WHEN。
var nodeListColumns = map[string]map[string]bool{
	"node": {
		"id": true, "name": true, "sort_order": true, "public": true, "note": true, "offline_grace_s": true,
		"traffic_reset_day": true, "token_hash": false, "created_at": true, "last_seen_at": false,
		"price": true, "currency": true, "billing_cycle": true, "expires_on": true, "auto_renew": true,
		"last_source": true, "country": true, "country_ip": true, "country_pin": true, "maintenance": true,
		"public_remark": true, "traffic_quota_bytes": true, "traffic_quota_mode": true,
		"ipv4_pin": true, "ipv6_pin": true, "rented": true,
	},
	"node_facts": {
		"node_id": true, "facts_hash": false, "hostname": true, "os": true, "kernel": true, "arch": true,
		"virtualization": true, "cpu_model": true, "cpu_cores": true, "agent_version": true, "icmp_available": true,
		"updated_at": true, "network": true, "diagnostics": true, "execution": true, "facts_rev": false,
	},
	// api_token：ListNodes 对 token 的结果只取决于范围，nodeScopeSQL 按 id 找到这一行、读 all_nodes（显式节点在
	// api_token_node，那张表的任何写入都推进）。名称、哈希、时间、权限位与有效期都不改变范围。
	"api_token": {
		"id": true, "all_nodes": true, "name": false, "token_hash": false, "created_at": false,
		"last_used_at": false, "permissions": false, "expires_at": false,
	},
}

func TestNodeListRevisionColumnClassification(t *testing.T) {
	t.Parallel()
	probe, _ := open(t)
	for table, classes := range nodeListColumns {
		rows, err := probe.r.Query("SELECT name FROM pragma_table_info(?)", table)
		if err != nil {
			t.Fatal(err)
		}
		var columns []string
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err != nil {
				t.Fatal(err)
			}
			columns = append(columns, c)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		if len(columns) == 0 {
			t.Fatalf("%s: PRAGMA 没有读到任何列", table)
		}
		for _, column := range columns {
			visible, ok := classes[column]
			if !ok {
				t.Errorf("%s.%s 没有归类：在 nodeListColumns 里归类，并同步 schema.go 里 %s 的 UPDATE 触发器", table, column, table)
				continue
			}
			t.Run(table+"."+column, func(t *testing.T) {
				t.Parallel()
				s, id := nodeWithFacts(t)
				key := "id"
				switch table {
				case "node_facts":
					key = "node_id"
				case "api_token":
					// 指定节点的 token：all_nodes 为 0，下面 +1 改成 1 仍满足 CHECK (all_nodes IN (0, 1))。
					tok, err := s.CreateAPIToken(t.Context(), "scoped", sha256.Sum256([]byte("scoped")), time.Unix(1, 0), 100, &TokenGrant{NodeIDs: []int64{id}}, time.Time{})
					if err != nil {
						t.Fatal(err)
					}
					id = tok.ID
				}
				before := revisionOf(t, s)
				mustExec(t, s, fmt.Sprintf("UPDATE %s SET %s = %s WHERE %s = ?", table, column, column, key), id)
				if after := revisionOf(t, s); after != before {
					t.Fatalf("写回原值也推进了版本（%d → %d）：WHEN 必须按新旧值比较", before, after)
				}
				mustExec(t, s, fmt.Sprintf(`UPDATE %[1]s SET %[2]s = CASE typeof(%[2]s) WHEN 'integer' THEN %[2]s + 1 WHEN 'text' THEN %[2]s || 'x'
					WHEN 'blob' THEN randomblob(32) ELSE 1 END WHERE %[3]s = ?`, table, column, key), id)
				if bumped := revisionOf(t, s) != before; bumped != visible {
					t.Fatalf("改 %s.%s 后版本推进 = %v，归类要求 %v", table, column, bumped, visible)
				}
			})
		}
	}
}

// "指定节点、列表为空"改为"全站"不碰任何 api_token_node 行，只有 api_token 上的触发器能让条件读取看到变化。
func TestNodeListRevisionBumpsWhenTokenScopeWidens(t *testing.T) {
	t.Parallel()
	s, _ := nodeWithFacts(t)
	tok, err := s.CreateAPIToken(t.Context(), "narrow", sha256.Sum256([]byte("narrow")), time.Unix(1, 0), 100, &TokenGrant{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithPrincipal(t.Context(), tok)
	first, err := s.ReadNodeList(ctx, NodeFilter{}, func(uint64) bool { return false })
	if err != nil || len(first.Nodes) != 0 {
		t.Fatalf("empty explicit scope listed nodes: %+v %v", first, err)
	}
	mustExec(t, s, "UPDATE api_token SET all_nodes = 1 WHERE id = ?", tok.ID)
	second, err := s.ReadNodeList(ctx, NodeFilter{}, func(r uint64) bool { return r == first.Revision })
	if err != nil || second.Unchanged || len(second.Nodes) != 1 {
		t.Fatalf("widened scope still reported unchanged: %+v %v", second, err)
	}
}

func mustExec(t *testing.T, s *Store, query string, args ...any) {
	t.Helper()
	if err := s.write(t.Context(), func(tx *sql.Tx) error { _, err := tx.Exec(query, args...); return err }); err != nil {
		t.Fatal(err)
	}
}

// 行的增删与关联表的任何写入都推进：节点、facts、节点标签、标签改名或删除、token 的节点范围。
func TestNodeListRevisionBumpsOnMembership(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		do   func(t *testing.T, s *Store, id int64)
	}{
		{"create node", func(t *testing.T, s *Store, _ int64) {
			if _, _, err := s.CreateNode(t.Context(), NewNode{Name: "m"}, hash(2)); err != nil {
				t.Fatal(err)
			}
		}},
		{"delete node", func(t *testing.T, s *Store, id int64) {
			if err := s.DeleteNode(t.Context(), id); err != nil {
				t.Fatal(err)
			}
		}},
		{"delete facts", func(t *testing.T, s *Store, id int64) { mustExec(t, s, "DELETE FROM node_facts WHERE node_id = ?", id) }},
		{"set tags", func(t *testing.T, s *Store, id int64) {
			if _, err := s.UpdateNodeTasks(t.Context(), id, NodeEdit{Name: "n", TrafficResetDay: 1, Tags: []string{"a"}}); err != nil {
				t.Fatal(err)
			}
		}},
		{"rename tag", func(t *testing.T, s *Store, _ int64) { mustExec(t, s, "UPDATE tag SET name = 'B' WHERE name = 'b'") }},
		{"delete tag", func(t *testing.T, s *Store, _ int64) {
			if err := s.DeleteTag(t.Context(), "b"); err != nil {
				t.Fatal(err)
			}
		}},
		{"grant token node", func(t *testing.T, s *Store, id int64) {
			mustExec(t, s, "INSERT INTO api_token_node (token_id, node_id) VALUES (99, ?)", id)
		}},
		{"revoke token node", func(t *testing.T, s *Store, _ int64) {
			mustExec(t, s, "DELETE FROM api_token_node WHERE token_id = 98")
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s, id := nodeWithFacts(t)
			// "rename tag"、"delete tag" 要先有标签 b；"revoke token node" 要先有一行范围。
			if _, err := s.UpdateNodeTasks(t.Context(), id, NodeEdit{Name: "n", TrafficResetDay: 1, Tags: []string{"b"}}); err != nil {
				t.Fatal(err)
			}
			mustExec(t, s, "INSERT INTO api_token_node (token_id, node_id) VALUES (98, ?)", id)
			before := revisionOf(t, s)
			c.do(t, s, id)
			if after := revisionOf(t, s); after == before {
				t.Fatalf("%s 之后版本仍是 %d", c.name, before)
			}
		})
	}
}

// 分钟刷出每分钟改每个上报过的节点的 last_seen_at：来源不变时不推进，来源变了推进（显示值与国家跟着变）。
func TestMinuteFlushBumpsOnlyWhenSourceChanges(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	ctx := t.Context()
	id, _, err := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	flush := func(ts int64, source string) {
		t.Helper()
		row := metric.Row{NodeID: id, TS: ts, CoverageStart: 600, Bucket: metric.NewBucket(), LastSeen: clk.Now(), Source: source}
		if _, err := s.WriteMinuteBatch(ctx, metric.Batch{Rows: []metric.Row{row}}); err != nil {
			t.Fatal(err)
		}
	}
	flush(600, "203.0.113.7")
	settled := revisionOf(t, s)
	clk.Advance(time.Minute)
	flush(660, "203.0.113.7")
	if v := revisionOf(t, s); v != settled {
		t.Fatalf("来源不变的分钟刷出推进了版本：%d → %d", settled, v)
	}
	clk.Advance(time.Minute)
	flush(720, "198.51.100.9")
	if v := revisionOf(t, s); v == settled {
		t.Fatal("来源变了，版本应推进")
	}
}

func TestReadNodeListSkipsRowsWhenUnchanged(t *testing.T) {
	t.Parallel()
	s, id := nodeWithFacts(t)
	ctx := t.Context()
	full, err := s.ReadNodeList(ctx, NodeFilter{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if full.Unchanged || len(full.Nodes) != 1 || full.Nodes[0].ID != id {
		t.Fatalf("无条件读取 = %+v", full)
	}
	var asked uint64
	same, err := s.ReadNodeList(ctx, NodeFilter{}, func(rev uint64) bool { asked = rev; return rev == full.Revision })
	if err != nil {
		t.Fatal(err)
	}
	if asked != full.Revision || !same.Unchanged || same.Nodes != nil || same.Revision != full.Revision {
		t.Fatalf("版本未变时应只回版本：asked=%d %+v", asked, same)
	}
	if _, err := s.UpdateNodeTasks(ctx, id, NodeEdit{Name: "m", TrafficResetDay: 1}); err != nil {
		t.Fatal(err)
	}
	changed, err := s.ReadNodeList(ctx, NodeFilter{}, func(rev uint64) bool { return rev == full.Revision })
	if err != nil {
		t.Fatal(err)
	}
	if changed.Unchanged || len(changed.Nodes) != 1 || changed.Nodes[0].Name != "m" || changed.Revision == full.Revision {
		t.Fatalf("改名后应整份返回新版本：%+v", changed)
	}
}

// 同一个版本号只能对应一份内容：版本与节点行若读自两个快照，并发改名插进两次读之间时，同一个版本会配上两个名字。
func TestNodeListRevisionAndRowsComeFromOneSnapshot(t *testing.T) {
	s, id := nodeWithFacts(t)
	ctx := t.Context()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	defer func() { close(stop); wg.Wait() }()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := s.UpdateNodeTasks(ctx, id, NodeEdit{Name: fmt.Sprint("n", i), TrafficResetDay: 1}); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	// 写者插进两次读之间才验证了东西：GOMAXPROCS 至少 2，读满 minReads 次且见过至少两个版本才停，到时限仍只见一个判为空过。
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(max(2, runtime.GOMAXPROCS(0))))
	const minReads = 500
	deadline := time.Now().Add(10 * time.Second)
	seen := map[uint64]string{}
	for reads := 0; reads < minReads || (len(seen) < 2 && time.Now().Before(deadline)); reads++ {
		list, err := s.ReadNodeList(ctx, NodeFilter{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		name := list.Nodes[0].Name
		if prev, ok := seen[list.Revision]; ok && prev != name {
			t.Fatalf("版本 %d 先后配上 %q 与 %q：版本与节点行不是同一个快照", list.Revision, prev, name)
		}
		seen[list.Revision] = name
	}
	if len(seen) < 2 {
		t.Fatal("期间只见到一个版本，写者没有插进来，用例空过")
	}
}

// Tags 与 Untagged 同时给出没有合法含义（带有所选标签与没有标签的交集为空）：存储层显式拒绝，不按其中一个放宽。
func TestReadNodeListRejectsTagsWithUntagged(t *testing.T) {
	t.Parallel()
	s, _ := nodeWithFacts(t)
	if _, err := s.ReadNodeList(t.Context(), NodeFilter{Tags: []string{"db"}, Untagged: true}, nil); err == nil {
		t.Fatal("Tags 与 Untagged 同时给出应报错")
	}
}

// 恢复覆盖已有的库时整表删除再插入节点行，触发器随之推进版本：恢复后的版本大于恢复前，不会与旧标签页手里的版本相等。
// （恢复要求 hub 已停止，重启后纪元另换；这里钉的是库里这一层。）
func TestRestoreAdvancesNodeListRevision(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	ctx := t.Context()
	if _, _, err := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1)); err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(t.TempDir(), "config.db")
	if err := s.SnapshotConfig(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	// 先把计数推到远高于一次恢复搬运的推进量：恢复若把计数归零再推进，结果会落回这之下。
	if err := s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE node_list_revision SET revision = revision + 1000 WHERE id = 1")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	before := revisionOf(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(ctx, s.path, snapshot, "", "", clk.Now(), slog.Default()); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(s.path, clk, slog.Default(), RequireCurrentSchema)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if after := revisionOf(t, reopened); after <= before {
		t.Fatalf("恢复后版本 = %d，恢复前 %d：恢复的整表搬运应推进版本", after, before)
	}
}
