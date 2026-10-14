package store

import (
	"database/sql"
	"slices"
	"testing"
)

// 冻结文本独立于生产迁移，避免结构比较的两端随实现一起改变。
var schemaV19 = append(slices.Clone(schemaV18),
	`CREATE TABLE theme_package (
  theme_id TEXT PRIMARY KEY,
  content BLOB NOT NULL,
  revision INTEGER NOT NULL CHECK (revision > 0),
  uploaded INTEGER NOT NULL DEFAULT 0 CHECK (uploaded IN (0, 1))
)`,
	`ALTER TABLE restore_record ADD COLUMN themes TEXT NOT NULL DEFAULT '{}'`,
)

var schemaV22 = append(slices.Clone(schemaV21),
	`DROP TABLE theme_file`, `DROP TABLE theme_package`, `DROP TABLE theme`,
	`CREATE TABLE theme (id TEXT PRIMARY KEY)`,
	`CREATE TABLE theme_version (theme_id TEXT NOT NULL,digest TEXT NOT NULL,name TEXT NOT NULL,version TEXT NOT NULL,preview TEXT NOT NULL,uploaded_at INTEGER NOT NULL,sdk INTEGER NOT NULL,published INTEGER NOT NULL DEFAULT 0 CHECK(published IN (0,1)),repository TEXT NOT NULL DEFAULT '',release TEXT NOT NULL DEFAULT '',asset TEXT NOT NULL DEFAULT '',PRIMARY KEY(theme_id,digest))`,
	`CREATE TABLE theme_selection (id INTEGER PRIMARY KEY CHECK(id=1),current_id TEXT NOT NULL DEFAULT '',current_digest TEXT NOT NULL DEFAULT '',previous_id TEXT NOT NULL DEFAULT '',previous_digest TEXT NOT NULL DEFAULT '',CHECK((current_id='')=(current_digest='')),CHECK((previous_id='')=(previous_digest='')))`,
	`INSERT INTO theme_selection(id) VALUES(1)`,
	`CREATE TABLE theme_package (theme_id TEXT NOT NULL,digest TEXT NOT NULL,content BLOB NOT NULL,revision INTEGER NOT NULL CHECK(revision>0),uploaded INTEGER NOT NULL DEFAULT 0 CHECK(uploaded IN (0,1)),PRIMARY KEY(theme_id,digest))`,
	`CREATE TABLE theme_file (theme_id TEXT NOT NULL,digest TEXT NOT NULL,path TEXT NOT NULL,content BLOB NOT NULL,PRIMARY KEY(theme_id,digest,path))`,
)

func TestMigrationFromV18PreservesThemesAndRestoreRecords(t *testing.T) {
	t.Parallel()
	migrated := migrateFrom(t, 18, func(t *testing.T, db *sql.DB) {
		for _, stmt := range []string{
			`INSERT INTO theme (id,name,version,preview,uploaded_at,enabled) VALUES ('kept','Kept','1','preview.png',123,1)`,
			`INSERT INTO theme_file (theme_id,path,content) VALUES ('kept','index.html',x'616263')`,
			`INSERT INTO restore_record (id,restored_at,config_taken_at,metrics_taken_at,orphans) VALUES ('old',123,100,90,'{"traffic":2}')`,
		} {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatal(err)
			}
		}
	})
	for _, tc := range []struct {
		name, query, want string
	}{
		{"theme", `SELECT json_array(theme_id,name,version,preview,uploaded_at,sdk,digest) FROM theme_version`, `["kept","Kept","1","preview.png",123,0,""]`},
		{"theme_file", `SELECT json_array(theme_id,path,hex(content)) FROM theme_file`, `["kept","index.html","616263"]`},
		{"restore_record", `SELECT json_array(id,restored_at,config_taken_at,metrics_taken_at,orphans) FROM restore_record`, `["old",123,100,90,"{\"traffic\":2}"]`},
		{"themes_default", `SELECT themes FROM restore_record WHERE id='old'`, `{}`},
		{"no_reconstructed_package", `SELECT count(*) FROM theme_package`, `0`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			if err := migrated.r.QueryRow(tc.query).Scan(&got); err != nil || got != tc.want {
				t.Fatalf("%s after migration: got %q err=%v, want %q", tc.name, got, err, tc.want)
			}
		})
	}
}
