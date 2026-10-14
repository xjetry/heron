package store

import (
	"database/sql"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// 结构签名核对两库的 CHECK 文本，违反写入另行核对 SQLite 的实际拒绝行为；两份 DDL 同时漏掉同一条 CHECK 时，
// 签名仍可能相等，下面的显式清单会报缺项。这条用例按条对照，不按表：
//   - 从新建库与从 v1 迁到当前的库的 sqlite_master 逐条取出每张表的 CHECK 表达式，两库的清单必须相同。只进 schema.go
//     或只进迁移的 CHECK、两边写法不同的 CHECK 都在这里失败。
//   - checkViolations 给每条 CHECK 配一次违反它的写入，按表列出的表达式必须与库里取出的相同。同一张表新加的第二条
//     CHECK 没有自己的写入，也在这里失败。
//   - 每条写入在两库上各执行一次。SQLite 拒绝时点名未命名 CHECK 的表达式原文，据此断言拒绝它的正是配给它的那条，
//     而不是同表的另一条。
func TestEveryCheckConstraintRefusesItsViolation(t *testing.T) {
	t.Parallel()
	migrated := migrateFrom(t, 1, seedMinuteRow)
	fresh, _ := open(t)
	checkViolations := []struct{ table, check, write string }{
		{"api_token", "all_nodes IN (0, 1)", "INSERT INTO api_token (name,token_hash,created_at,all_nodes) VALUES ('x',x'00',0,2)"},
		// 单行表：第二行插不进去。
		{"admin", "id = 1", "INSERT INTO admin (id, password_hash, updated_at) VALUES (2, 'x', 0)"},
		{"admin_security", "id = 1", "INSERT INTO admin_security (id) VALUES (2)"},
		{"probe_meta", "id = 1", "INSERT INTO probe_meta (id, version) VALUES (2, 0)"},
		// 0 与 1 以外的启用值会绕过只看 enabled = 1 的 theme_enabled 索引。
		{"theme_version", "published IN (0, 1)", "INSERT INTO theme_version (theme_id,digest,name,version,preview,uploaded_at,sdk,published) VALUES ('x','d','X','1','',0,1,2)"},
		{"theme_selection", "id = 1", "INSERT INTO theme_selection(id) VALUES(2)"},
		{"theme_selection", "(current_id = '') = (current_digest = '')", "UPDATE theme_selection SET current_id='a'"},
		{"theme_selection", "(previous_id = '') = (previous_digest = '')", "UPDATE theme_selection SET previous_id='a'"},
		{"theme_package", "revision > 0", "INSERT INTO theme_package (theme_id,digest,content,revision) VALUES ('x','d',x'00',0)"},
		{"theme_package", "uploaded IN (0, 1)", "INSERT INTO theme_package (theme_id,digest,content,revision,uploaded) VALUES ('y','d',x'00',1,2)"},
		// 0 是"不限"，负数没有含义。
		{"notify_channel", "rate_per_minute >= 0", "INSERT INTO notify_channel (name, kind, config, created_at, rate_per_minute) VALUES ('x', 'telegram', '{}', 0, -1)"},
		// 批次号是批次第一行的 id，0 不指向任何行。NULL 满足 CHECK，由 NOT NULL 拒绝（TestDeliveryInsertWithoutBatchFails）。
		{"alert_delivery", "batch_id > 0", "INSERT INTO alert_delivery (event_id, channel_id, batch_id) VALUES (1, 1, 0)"},
		{"hub_coordination", "id = 1", "INSERT INTO hub_coordination (id, offline_generation) VALUES (2, 0)"},
		// 代数只增不减，负值没有含义；读侧另把负值当损坏报错（OfflineGeneration）。
		{"hub_coordination", "offline_generation >= 0", "UPDATE hub_coordination SET offline_generation = -1"},
		// 基线计数是截到 MaxInt64 的内核计数，负值没有含义；读侧另按 CheckTrafficBaseline 拒收（快照的表不带 CHECK）。
		{"traffic_baseline", "rx >= 0", "INSERT INTO traffic_baseline (node_id, iface, rx, tx) VALUES (1, '', -1, 0)"},
		{"traffic_baseline", "tx >= 0", "INSERT INTO traffic_baseline (node_id, iface, rx, tx) VALUES (1, '', 0, -1)"},
	}
	freshChecks, migratedChecks := checkConstraints(t, fresh.r), checkConstraints(t, migrated.r)
	if !reflect.DeepEqual(freshChecks, migratedChecks) {
		t.Fatalf("CHECK constraints differ between a fresh and a migrated database:\n fresh:    %v\n migrated: %v", freshChecks, migratedChecks)
	}
	covered := map[string][]string{}
	for _, v := range checkViolations {
		covered[v.table] = append(covered[v.table], v.check)
	}
	for _, checks := range covered {
		slices.Sort(checks)
	}
	if !reflect.DeepEqual(freshChecks, covered) {
		t.Fatalf("CHECK constraints in the schema: %v\nviolating writes cover:        %v\nevery CHECK needs a write of its own that violates it", freshChecks, covered)
	}
	for _, s := range []*Store{fresh, migrated} {
		kind := map[bool]string{true: "migrated", false: "fresh"}[s == migrated]
		for _, v := range checkViolations {
			err := s.write(t.Context(), func(tx *sql.Tx) error {
				_, err := tx.Exec(v.write)
				return err
			})
			if want := "CHECK constraint failed: " + v.check; err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%s on a %s database: %v, want %q", v.write, kind, err, want)
			}
		}
	}
}

var (
	sqlLineComment = regexp.MustCompile(`--[^\n]*`)
	sqlCheck       = regexp.MustCompile(`(?i)\bcheck\s*\(`)
)

// checkConstraints 是每张带 CHECK 的表的 CHECK 表达式，逐条列出（同一张表的几条都在）并排序：ALTER TABLE 加的列排在
// 建表语句末尾，两库里同一组 CHECK 的先后可以不同。表达式取 CHECK 后那对括号里的原文，连续空白压成一个空格。先去掉
// 行注释：DDL 的注释里也会写到 CHECK 这个词。
func checkConstraints(t *testing.T, db *sql.DB) map[string][]string {
	t.Helper()
	rows, err := db.Query("SELECT name, sql FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var name, ddl string
		if err := rows.Scan(&name, &ddl); err != nil {
			t.Fatal(err)
		}
		ddl = sqlLineComment.ReplaceAllString(ddl, "")
		for _, loc := range sqlCheck.FindAllStringIndex(ddl, -1) {
			expr, ok := parenthesized(ddl[loc[1]-1:])
			if !ok {
				t.Fatalf("table %s: no closing parenthesis for the CHECK at byte %d of %q", name, loc[0], ddl)
			}
			out[name] = append(out[name], strings.Join(strings.Fields(expr), " "))
		}
		slices.Sort(out[name])
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// parenthesized 从 s 开头的 ( 找到与它配对的 )，返回两者之间的原文。单引号字符串里的括号不计；SQL 用两个单引号转义
// 一个单引号，按开合各切换一次处理，结果相同。
func parenthesized(s string) (string, bool) {
	depth, quoted := 0, false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case quoted:
			quoted = c != '\''
		case c == '\'':
			quoted = true
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth == 0 {
				return s[1:i], true
			}
		}
	}
	return "", false
}
