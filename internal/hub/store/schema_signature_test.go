package store

import (
	"database/sql"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// describe 把库的结构化成一组签名项，迁移后与新建的库、拒绝打开前后的库都按它比较。
//
// pragma 各维度给出逐字段的可读差异：
//   - table_xinfo 的列元数据防止漏列、默认值和主键次序漂移；table_list 的 wr/strict 防止漏掉存储模式和类型约束。
//   - 索引从 index_list 枚举，不能过滤 sqlite_：UNIQUE 自动索引和 WITHOUT ROWID 主键都由 SQLite 管理。
//   - index_xinfo 保留键列、辅助列、排序与校对规则，避免列名相同却访问路径或唯一性语义不同。
//
// 这几个 pragma 看不到只写在 DDL 文本里的语义：AUTOINCREMENT、列级与表级 CHECK、无索引列的 COLLATE、
// REFERENCES 与外键动作、生成列表达式、ON CONFLICT 子句，以及部分/表达式索引的条件与表达式、
// 触发器与视图的定义。它们由各项的 sql 字段承担：table_sql 每张表一项，index_sql 给 pragma
// 描述不全的部分/表达式索引，definition 给触发器与视图。sql 字段一律经 normalizeSQL，
// 同一结构因建法不同（ALTER TABLE ADD COLUMN、注释与缩进）而不同的原文规范化后相同；RENAME 造成的引号差异只在
// 标识符按小写书写时（本包 DDL 的写法）规范化后相同——SQLite 给改名后的表名加双引号，引号内保留大小写，含大写的
// 表名两侧会不同，测试红而不会放过缺陷，不为此放宽引号规则（双引号内容在找不到列时是字符串常量）。
//
// sqlite_sequence 保留结构与存在性，不比较数据：旧库中的序列值本就可能不同于空库。它的存在只说明
// 库里建过 AUTOINCREMENT 表（删掉最后一张自增表后它仍在），说明不了哪张表自增，这由 table_sql 区分。
func describe(t *testing.T, db *sql.DB) []string {
	t.Helper()
	queries := []struct {
		kind string
		sql  string
	}{
		{"object", `SELECT type, name, tbl_name AS owner FROM s`},
		{"table", `SELECT s.name AS table_name, p.type, p.ncol, p.wr, p.strict
			FROM s JOIN pragma_table_list p ON p.name = s.name AND p.schema = 'main'
			WHERE s.type = 'table'`},
		{"table_sql", `SELECT name AS table_name, sql FROM s WHERE type = 'table'`},
		{"column", `SELECT s.name AS table_name, p.cid, p.name, p.type, p."notnull", p.dflt_value, p.pk, p.hidden
			FROM s JOIN pragma_table_xinfo(s.name, 'main') p WHERE s.type = 'table'`},
		{"index", `SELECT s.name AS table_name, i.name AS index_name, i."unique", i.origin,
			CASE i.origin WHEN 'u' THEN 'UNIQUE' WHEN 'pk' THEN 'PRIMARY KEY' ELSE 'CREATE INDEX' END AS constraint_kind,
			i.partial FROM s JOIN pragma_index_list(s.name, 'main') i WHERE s.type = 'table'`},
		{"index_column", `SELECT s.name AS table_name, i.name AS index_name, x.seqno, x.cid, x.name, x.desc, x.coll, x.key
			FROM s JOIN pragma_index_list(s.name, 'main') i
			JOIN pragma_index_xinfo(i.name, 'main') x WHERE s.type = 'table'`},
		{"index_sql", `SELECT s.name AS table_name, i.name AS index_name, d.sql
			FROM s JOIN pragma_index_list(s.name, 'main') i
			JOIN s d ON d.type = 'index' AND d.name = i.name
			WHERE s.type = 'table' AND (i.partial = 1 OR EXISTS
				(SELECT 1 FROM pragma_index_xinfo(i.name, 'main') x WHERE x.cid = -2))`},
		{"definition", `SELECT type, name, tbl_name AS owner, sql FROM s WHERE type IN ('trigger', 'view')`},
	}
	var signature []string
	for _, query := range queries {
		func() {
			rows, err := db.Query(signatureObjects + query.sql)
			if err != nil {
				t.Fatalf("schema %s: %v", query.kind, err)
			}
			defer rows.Close()
			columns, err := rows.Columns()
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				values := make([]sql.NullString, len(columns))
				args := make([]any, len(columns))
				for i := range values {
					args[i] = &values[i]
				}
				if err := rows.Scan(args...); err != nil {
					t.Fatalf("schema %s: %v", query.kind, err)
				}
				fields := []string{query.kind}
				for i, value := range values {
					text := "NULL"
					if value.Valid {
						raw := value.String
						if columns[i] == "sql" {
							raw = normalizeSQL(raw)
						}
						text = strconv.Quote(raw)
					}
					fields = append(fields, columns[i]+"="+text)
				}
				signature = append(signature, strings.Join(fields, " "))
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("schema %s: %v", query.kind, err)
			}
			if err := rows.Close(); err != nil {
				t.Fatal(err)
			}
		}()
	}
	// 对象创建顺序与 index_list.seq 不属于结构，不进签名；列序与 index_xinfo.seqno 属于结构，
	// 作为字段保留在每一项中。比较按集合进行，排序只让差异输出次序稳定、同类项相邻。
	slices.Sort(signature)
	return signature
}

// signatureObjects 定义 describe 各查询共用的对象全集 s。sqlite_stat1..4 是 ANALYZE 写入的查询计划统计，
// PRAGMA optimize 也会按需执行 ANALYZE 建出它们；它们记录数据分布而不是结构。store 当前不执行二者，
// 这里按名字显式排除，签名不依赖"恰好没执行"。sqlite_ 前缀的名字 SQLite 不许用户建对象，按前缀排除不会漏掉用户表。
const signatureObjects = `WITH s AS (SELECT type, name, tbl_name, sql FROM main.sqlite_schema
	WHERE name NOT LIKE 'sqlite\_stat%' ESCAPE '\') `

// normalizeSQL 把 sqlite_schema.sql 的原文化成只随语义变化的文本。同一结构的原文会因建法而异：
// ALTER TABLE ADD COLUMN 把新列定义以 ", " 接在最后一个列定义之后（没有表级约束时即最后的右括号之前；
// 有表级约束时在约束列表之前，alert_state 实测插在 PRIMARY KEY 之前），RENAME 让 SQLite 在建表语句
// 和挂在该表上的索引里把表名写成带双引号的形式，DDL 里还带注释与缩进。规范化只丢弃 SQLite 解析时不起作用的差异：
//   - 注释（-- 到行尾、/* 到 */）与空白：两个非标点记号之间至多留一个空格，标点两侧不留。
//   - 引号与字符串常量之外的 ASCII 大小写：SQLite 按 ASCII 不分大小写解析关键字、标识符、类型名、函数名与校对名。
//   - 标识符的 "…"、`…`、[…] 引号，仅当内容是裸标识符形状时去掉，且内容保留原大小写：双引号内容在解析
//     不到标识符时被 SQLite 当作字符串常量（CHECK (v <> "ABC") 拒绝 'ABC'、放行 'abc'），折叠大小写会混同两者；
//     内容含空白或标点时去掉引号会与多个记号混同。
//
// 字符串常量逐字保留，包括连写两个单引号的转义、内部空白以及形似注释或引号的内容。
func normalizeSQL(text string) string {
	type token struct {
		text     string
		punct    bool // 单个标点字符
		unquoted bool // 去掉了引号的标识符
		spaced   bool // 源文本里与前一记号之间隔着空白或注释
	}
	var tokens []token
	spaced := false
	for i := 0; i < len(text); {
		c := text[i]
		switch {
		case strings.IndexByte(" \t\n\v\f\r", c) >= 0:
			spaced = true
			i++
		case strings.HasPrefix(text[i:], "--"):
			if end := strings.IndexByte(text[i:], '\n'); end >= 0 {
				i += end + 1
			} else {
				i = len(text)
			}
			spaced = true
		case strings.HasPrefix(text[i:], "/*"):
			if end := strings.Index(text[i+2:], "*/"); end >= 0 {
				i += 2 + end + 2
			} else {
				i = len(text)
			}
			spaced = true
		default:
			tok := token{spaced: spaced}
			spaced = false
			switch {
			case c == '\'':
				end := quotedEnd(text, i, '\'')
				tok.text = text[i:end]
				i = end
			case c == '"' || c == '`' || c == '[':
				closer := c
				if c == '[' {
					closer = ']'
				}
				end := quotedEnd(text, i, closer)
				tok.text = text[i:end]
				if end-i >= 2 && text[end-1] == closer && isBareIdentifier(text[i+1:end-1]) {
					tok.text, tok.unquoted = text[i+1:end-1], true
				}
				i = end
			case isIdentifierByte(c):
				end := i
				for end < len(text) && isIdentifierByte(text[end]) {
					end++
				}
				tok.text = asciiLower(text[i:end])
				i = end
			default:
				tok.text, tok.punct = text[i:i+1], true
				i++
			}
			tokens = append(tokens, tok)
		}
	}
	var out strings.Builder
	for k, tok := range tokens {
		// 去掉引号的标识符与相邻的非标点记号之间总留空格：源文本里 "a"b 是列 a 取别名 b，
		// "x"'0a' 是列 x 取别名 '0a'，不能写成标识符 ab 或 blob 常量 x'0a'。
		if k > 0 && !tok.punct && !tokens[k-1].punct && (tok.spaced || tok.unquoted || tokens[k-1].unquoted) {
			out.WriteByte(' ')
		}
		out.WriteString(tok.text)
	}
	return out.String()
}

// quotedEnd 返回从 start 处的引号开始的记号结束位置（闭引号之后）。'…'、"…"、`…` 内连写两个引号
// 表示引号本身，[…] 没有转义。
func quotedEnd(text string, start int, closer byte) int {
	for i := start + 1; i < len(text); i++ {
		if text[i] != closer {
			continue
		}
		if closer != ']' && i+1 < len(text) && text[i+1] == closer {
			i++
			continue
		}
		return i + 1
	}
	return len(text)
}

func isIdentifierByte(c byte) bool {
	return c == '_' || c == '$' || c >= 0x80 || '0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
}

func isBareIdentifier(s string) bool {
	if s == "" || s[0] == '$' || '0' <= s[0] && s[0] <= '9' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isIdentifierByte(s[i]) {
			return false
		}
	}
	return true
}

func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

func TestNormalizeSQL(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{"CREATE TABLE t (\n  -- 注释\n  a INTEGER /* 注释 */ NOT NULL ,\n  b TEXT\n)", "create table t(a integer not null,b text)"},
		{"CHECK (v IN ('A  b', 'it''s -- x /* y */ \"Q\"'))", `check(v in('A  b','it''s -- x /* y */ "Q"'))`},
		{"CREATE INDEX i ON \"Sample\"([V], `w`)", "create index i on Sample(V,w)"},
		{`SELECT "a b", [c.d] FROM t`, `select "a b",[c.d] from t`},
		{`SELECT "a"b, "x"'0a', x'0a'`, `select a b,x '0a',x'0a'`},
	} {
		if got := normalizeSQL(tc.in); got != tc.want {
			t.Errorf("normalizeSQL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// schemaDifference 按集合比较两组签名项，逐项列出只在一侧出现的项，不要求输入有序。
// describe 的每一项都带对象名，列与索引列还带 cid、seqno；SQLite 保证同一库里对象名唯一、
// 同一表的 cid 与同一索引的 seqno 唯一，所以一侧内不会有两条相同的项，按集合比较不丢信息。
func schemaDifference(left, right []string, leftLabel, rightLabel string) string {
	var differences []string
	for _, side := range []struct {
		own, other []string
		label      string
	}{{left, right, leftLabel}, {right, left, rightLabel}} {
		other := make(map[string]bool, len(side.other))
		for _, item := range side.other {
			other[item] = true
		}
		for _, item := range side.own {
			if !other[item] {
				differences = append(differences, fmt.Sprintf("%s有: %s", side.label, item))
			}
		}
	}
	return strings.Join(differences, "\n")
}

func TestSchemaDifferenceIgnoresInputOrder(t *testing.T) {
	t.Parallel()
	got := schemaDifference([]string{"c", "a", "b"}, []string{"d", "b", "a"}, "左", "右")
	if want := "左有: c\n右有: d"; got != want {
		t.Fatalf("schema difference of unsorted inputs = %q, want %q", got, want)
	}
}

func TestSchemaSignatureDistinguishesStructure(t *testing.T) {
	t.Parallel()
	const table = `CREATE TABLE sample (id INTEGER NOT NULL PRIMARY KEY, value TEXT)`
	const tableSQL = `table_sql table_name="sample" sql=`
	const indexSQL = `index_sql table_name="sample" index_name="by_value" sql=`
	for _, tc := range []struct {
		name        string
		left, right string
		want        string
	}{
		{"table_presence", table, table + `; CREATE TABLE extra (id INTEGER)`, `table_name="extra"`},
		{"column_name", table, `CREATE TABLE sample (id INTEGER NOT NULL PRIMARY KEY, other TEXT)`, `name="other"`},
		{"column_type", table, `CREATE TABLE sample (id INTEGER NOT NULL PRIMARY KEY, value BLOB)`, `type="BLOB"`},
		{"not_null", table, `CREATE TABLE sample (id INTEGER NOT NULL PRIMARY KEY, value TEXT NOT NULL)`, `notnull="1"`},
		{"default", table, `CREATE TABLE sample (id INTEGER NOT NULL PRIMARY KEY, value TEXT DEFAULT '')`, `dflt_value="''"`},
		{"null_default", table, `CREATE TABLE sample (id INTEGER NOT NULL PRIMARY KEY, value TEXT DEFAULT NULL)`, `dflt_value="NULL"`},
		{"primary_key_order", `CREATE TABLE sample (a TEXT, b TEXT, PRIMARY KEY (a, b))`, `CREATE TABLE sample (a TEXT, b TEXT, PRIMARY KEY (b, a))`, `pk="2"`},
		{"without_rowid", table, table + ` WITHOUT ROWID`, `wr="1"`},
		{"strict", table, table + ` STRICT`, `strict="1"`},
		{"unique_constraint", table, `CREATE TABLE sample (id INTEGER NOT NULL PRIMARY KEY, value TEXT UNIQUE)`, `index_name="sqlite_autoindex_sample_1" unique="1" origin="u" constraint_kind="UNIQUE"`},
		{"extra_index", table, table + `; CREATE INDEX by_value ON sample(value)`, `index_name="by_value"`},
		{"index_unique", table + `; CREATE INDEX by_value ON sample(value)`, table + `; CREATE UNIQUE INDEX by_value ON sample(value)`, `unique="1"`},
		{"index_column_order", table + `; CREATE INDEX by_value ON sample(id, value)`, table + `; CREATE INDEX by_value ON sample(value, id)`, `seqno="0" cid="1"`},
		{"index_descending", table + `; CREATE INDEX by_value ON sample(value)`, table + `; CREATE INDEX by_value ON sample(value DESC)`, `desc="1"`},
		{"index_collation", table + `; CREATE INDEX by_value ON sample(value)`, table + `; CREATE INDEX by_value ON sample(value COLLATE NOCASE)`, `coll="NOCASE"`},
		{"partial_index", table + `; CREATE INDEX by_value ON sample(value)`, table + `; CREATE INDEX by_value ON sample(value) WHERE id > 0`, `partial="1"`},
		{"partial_predicate", table + `; CREATE INDEX by_value ON sample(value) WHERE id > 0`, table + `; CREATE INDEX by_value ON sample(value) WHERE id > 1`, indexSQL + `"create index by_value on sample(value)where id>1"`},
		{"index_expression", table + `; CREATE INDEX by_value ON sample(lower(value))`, table + `; CREATE INDEX by_value ON sample(upper(value))`, indexSQL + `"create index by_value on sample(upper(value))"`},
		// 字符串常量中的空白是值的一部分，规范化不能折叠它。
		{"predicate_literal_spaces", table + `; CREATE INDEX by_value ON sample(value) WHERE value = 'a b'`, table + `; CREATE INDEX by_value ON sample(value) WHERE value = 'a  b'`, indexSQL + `"create index by_value on sample(value)where value='a  b'"`},
		{"internal_table", `CREATE TABLE sample (id INTEGER PRIMARY KEY)`, `CREATE TABLE sample (id INTEGER PRIMARY KEY AUTOINCREMENT)`, `table_name="sqlite_sequence"`},
		{"trigger_presence", table, table + `; CREATE TRIGGER changed AFTER INSERT ON sample BEGIN UPDATE sample SET value = 'a'; END`, `type="trigger" name="changed"`},
		{"trigger_body", table + `; CREATE TRIGGER changed AFTER INSERT ON sample BEGIN UPDATE sample SET value = 'a'; END`, table + `; CREATE TRIGGER changed AFTER INSERT ON sample BEGIN UPDATE sample SET value = 'b'; END`, `definition type="trigger" name="changed" owner="sample" sql="create trigger changed after insert on sample begin update sample set value='b';end"`},
		{"view_presence", table, table + `; CREATE VIEW visible AS SELECT id FROM sample`, `type="view" name="visible"`},
		{"view_body", table + `; CREATE VIEW visible AS SELECT id FROM sample`, table + `; CREATE VIEW visible AS SELECT value FROM sample`, `definition type="view" name="visible" owner="visible" sql="create view visible as select value from sample"`},
		// 以下语义只写在建表 SQL 里，签名的 pragma 维度不反映它们，由 table_sql 区分。
		// 已有另一张自增表时 sqlite_sequence 两侧都存在，证明不了 sample 是否自增。
		{"autoincrement_second_table", `CREATE TABLE other (id INTEGER PRIMARY KEY AUTOINCREMENT); CREATE TABLE sample (id INTEGER PRIMARY KEY, value TEXT)`, `CREATE TABLE other (id INTEGER PRIMARY KEY AUTOINCREMENT); CREATE TABLE sample (id INTEGER PRIMARY KEY AUTOINCREMENT, value TEXT)`, tableSQL + `"create table sample(id integer primary key autoincrement,value text)"`},
		{"column_check", table, `CREATE TABLE sample (id INTEGER NOT NULL PRIMARY KEY CHECK (id = 1), value TEXT)`, tableSQL + `"create table sample(id integer not null primary key check(id=1),value text)"`},
		{"table_check", `CREATE TABLE sample (lo INTEGER, hi INTEGER)`, `CREATE TABLE sample (lo INTEGER, hi INTEGER, CHECK (lo <= hi))`, tableSQL + `"create table sample(lo integer,hi integer,check(lo<=hi))"`},
		{"column_collation", table, `CREATE TABLE sample (id INTEGER NOT NULL PRIMARY KEY, value TEXT COLLATE NOCASE)`, tableSQL + `"create table sample(id integer not null primary key,value text collate nocase)"`},
		{"foreign_key", `CREATE TABLE parent (id INTEGER PRIMARY KEY); CREATE TABLE sample (id INTEGER PRIMARY KEY, parent_id INTEGER)`, `CREATE TABLE parent (id INTEGER PRIMARY KEY); CREATE TABLE sample (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parent(id) ON DELETE CASCADE)`, tableSQL + `"create table sample(id integer primary key,parent_id integer references parent(id)on delete cascade)"`},
		{"generated_expression", `CREATE TABLE sample (a INTEGER, b INTEGER AS (a + 1))`, `CREATE TABLE sample (a INTEGER, b INTEGER AS (a + 2))`, tableSQL + `"create table sample(a integer,b integer as(a+2))"`},
		{"on_conflict", `CREATE TABLE sample (id INTEGER NOT NULL PRIMARY KEY, value TEXT UNIQUE)`, `CREATE TABLE sample (id INTEGER NOT NULL PRIMARY KEY, value TEXT UNIQUE ON CONFLICT REPLACE)`, tableSQL + `"create table sample(id integer not null primary key,value text unique on conflict replace)"`},
		// 字符串常量中的空白是值的一部分，规范化不能折叠它。
		{"table_literal_spaces", `CREATE TABLE sample (id INTEGER NOT NULL PRIMARY KEY, value TEXT CHECK (value <> 'a b'))`, `CREATE TABLE sample (id INTEGER NOT NULL PRIMARY KEY, value TEXT CHECK (value <> 'a  b'))`, tableSQL + `"create table sample(id integer not null primary key,value text check(value<>'a  b'))"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, left := schemaPolicyFixture(t, []string{tc.left}, 0)
			_, right := schemaPolicyFixture(t, []string{tc.right}, 0)
			// want 取自右侧夹具独有的项。两种参数顺序各比一次，标签跟着夹具走而不跟参数位置走：
			// 右侧独有的项先作第二个参数、再作第一个参数的独有项，两次都必须以"右有"报出。
			for _, order := range []struct {
				first, second           *sql.DB
				firstLabel, secondLabel string
			}{{left, right, "左", "右"}, {right, left, "右", "左"}} {
				diff := schemaDifference(describe(t, order.first), describe(t, order.second), order.firstLabel, order.secondLabel)
				found := false
				for _, line := range strings.Split(diff, "\n") {
					if strings.HasPrefix(line, "右有: ") && strings.Contains(line, tc.want) {
						found = true
					}
				}
				if !found {
					t.Errorf("schema difference with %s first must report 右有 %q:\n%s", order.firstLabel, tc.want, diff)
				}
			}
		})
	}
}

// 结构相同、写法或建法不同的两库签名必须相同。premise 在右侧库上求值且必须为真：它钉住右侧确实带着
// 要被忽略的那种差异，否则两侧相同证明不了签名忽略了它。
func TestSchemaSignatureIgnoresEquivalentSchemas(t *testing.T) {
	t.Parallel()
	const (
		sample  = `CREATE TABLE sample (id INTEGER PRIMARY KEY, value TEXT UNIQUE CHECK (value <> 'a  b'))`
		other   = `CREATE TABLE other (id INTEGER PRIMARY KEY AUTOINCREMENT)`
		byID    = `CREATE INDEX by_id ON sample(id)`
		partial = `CREATE INDEX by_value ON sample(value) WHERE value > ''`
		trigger = `CREATE TRIGGER stamp AFTER INSERT ON sample BEGIN UPDATE sample SET value = 'x  y' WHERE id = new.id; END`
		view    = `CREATE VIEW visible AS SELECT id, value FROM sample WHERE value <> ''`
	)
	for _, tc := range []struct {
		name        string
		left, right []string
		premise     string
	}{
		{
			name:  "creation_order",
			left:  []string{sample, other, byID, partial},
			right: []string{other, sample, partial, byID},
		},
		{
			name: "comments_and_spacing",
			left: []string{sample, partial, trigger, view},
			right: []string{
				`CREATE TABLE sample (
					-- 空白和注释不是表结构
					id   INTEGER PRIMARY KEY ,
					value TEXT /* 行内注释 */ UNIQUE CHECK ( value <> 'a  b' )
				)`,
				"CREATE INDEX by_value ON sample ( value )\n\tWHERE value > ''",
				"CREATE TRIGGER stamp AFTER INSERT ON sample\nBEGIN\n\tUPDATE sample SET value = 'x  y' WHERE id = new.id ;\nEND",
				"CREATE VIEW visible AS\n\tSELECT id , value FROM sample -- 注释\n\tWHERE value <> ''",
			},
		},
		{
			name: "keyword_and_type_case",
			left: []string{sample, partial, trigger, view},
			right: []string{
				`create table sample (id integer primary key, value text unique check (value <> 'a  b'))`,
				`create index by_value on sample(value) where value > ''`,
				`create trigger stamp after insert on sample begin update sample set value = 'x  y' where id = NEW.ID; end`,
				`create view visible as select id, value from sample where value <> ''`,
			},
		},
		{
			name: "identifier_quotes",
			left: []string{sample, partial, trigger, view},
			right: []string{
				"CREATE TABLE \"sample\" ([id] INTEGER PRIMARY KEY, `value` TEXT UNIQUE CHECK (\"value\" <> 'a  b'))",
				`CREATE INDEX "by_value" ON [sample]("value") WHERE [value] > ''`,
				"CREATE TRIGGER `stamp` AFTER INSERT ON \"sample\" BEGIN UPDATE [sample] SET `value` = 'x  y' WHERE \"id\" = new.\"id\"; END",
				`CREATE VIEW [visible] AS SELECT "id", [value] FROM "sample" WHERE "value" <> ''`,
			},
		},
		{
			name:    "added_column",
			left:    []string{`CREATE TABLE sample (id INTEGER PRIMARY KEY, value TEXT NOT NULL DEFAULT '')`},
			right:   []string{"CREATE TABLE sample (\n  id INTEGER PRIMARY KEY -- 注释\n)", `ALTER TABLE sample ADD COLUMN value TEXT NOT NULL DEFAULT ''`},
			premise: `SELECT instr(sql, char(10) || ', value ') > 0 FROM sqlite_schema WHERE name = 'sample'`,
		},
		{
			name: "renamed_table",
			left: []string{`CREATE TABLE sample (id INTEGER PRIMARY KEY, value TEXT)`, partial, `CREATE INDEX by_id ON sample(id) WHERE id > 0`},
			right: []string{
				`CREATE TABLE sample_new (id INTEGER PRIMARY KEY, value TEXT)`,
				`CREATE INDEX by_value ON sample_new(value) WHERE value > ''`,
				`ALTER TABLE sample_new RENAME TO sample`,
				`CREATE INDEX by_id ON sample(id) WHERE id > 0`,
			},
			premise: `SELECT count(*) = 2 FROM sqlite_schema WHERE name IN ('sample', 'by_value') AND instr(sql, '"sample"') > 0`,
		},
		{
			name:    "table_and_sequence_rows",
			left:    []string{sample, other},
			right:   []string{sample, other, `INSERT INTO sample(value) VALUES ('old')`, `INSERT INTO other DEFAULT VALUES`},
			premise: `SELECT (SELECT count(*) FROM sample) = 1 AND (SELECT count(*) FROM sqlite_sequence) = 1`,
		},
		{
			name:    "analyze",
			left:    []string{sample, byID, `INSERT INTO sample(value) VALUES ('old')`},
			right:   []string{sample, byID, `INSERT INTO sample(value) VALUES ('old')`, `ANALYZE`},
			premise: `SELECT count(*) > 0 FROM sqlite_schema WHERE name LIKE 'sqlite\_stat%' ESCAPE '\'`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, left := schemaPolicyFixture(t, tc.left, 0)
			_, right := schemaPolicyFixture(t, tc.right, 0)
			if tc.premise != "" {
				var holds bool
				if err := right.QueryRow(tc.premise).Scan(&holds); err != nil || !holds {
					t.Fatalf("right fixture lacks the difference to ignore: %s = %v, %v", tc.premise, holds, err)
				}
			}
			if diff := schemaDifference(describe(t, left), describe(t, right), "左", "右"); diff != "" {
				t.Fatalf("equivalent schemas differ:\n%s", diff)
			}
		})
	}
}
