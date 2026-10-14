package store

import (
	"crypto/sha256"
	"database/sql"
	"fmt"

	"github.com/xjetry/heron/internal/hub/theme"
)

// 冻结 DDL 独立于当前 schema，后续演进不能反过来改变旧库的迁移输入。
var migrationThemeV22 = []string{
	`CREATE TABLE theme (id TEXT PRIMARY KEY)`,
	`CREATE TABLE theme_version (theme_id TEXT NOT NULL,digest TEXT NOT NULL,name TEXT NOT NULL,version TEXT NOT NULL,preview TEXT NOT NULL,uploaded_at INTEGER NOT NULL,sdk INTEGER NOT NULL,published INTEGER NOT NULL DEFAULT 0 CHECK (published IN (0, 1)),repository TEXT NOT NULL DEFAULT '',release TEXT NOT NULL DEFAULT '',asset TEXT NOT NULL DEFAULT '',PRIMARY KEY(theme_id,digest))`,
	`CREATE TABLE theme_selection (id INTEGER PRIMARY KEY CHECK (id = 1),current_id TEXT NOT NULL DEFAULT '',current_digest TEXT NOT NULL DEFAULT '',previous_id TEXT NOT NULL DEFAULT '',previous_digest TEXT NOT NULL DEFAULT '',CHECK ((current_id = '') = (current_digest = '')),CHECK ((previous_id = '') = (previous_digest = '')))`,
	`INSERT INTO theme_selection(id) VALUES(1)`,
}

var migrationThemeContentV22 = []string{
	`CREATE TABLE theme_package (theme_id TEXT NOT NULL,digest TEXT NOT NULL,content BLOB NOT NULL,revision INTEGER NOT NULL CHECK (revision > 0),uploaded INTEGER NOT NULL DEFAULT 0 CHECK (uploaded IN (0, 1)),PRIMARY KEY(theme_id,digest))`,
	`CREATE TABLE theme_file (theme_id TEXT NOT NULL,digest TEXT NOT NULL,path TEXT NOT NULL,content BLOB NOT NULL,PRIMARY KEY(theme_id,digest,path))`,
}

func migrateThemeVersions(tx *sql.Tx) error {
	for _, stmt := range []string{"ALTER TABLE theme RENAME TO theme_old", "ALTER TABLE theme_package RENAME TO theme_package_old", "ALTER TABLE theme_file RENAME TO theme_file_old", "DROP INDEX theme_enabled"} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	if err := execAll(append(append([]string{}, migrationThemeV22...), migrationThemeContentV22...))(tx); err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT t.id,t.name,t.version,t.preview,t.uploaded_at,t.enabled,p.content,p.revision,p.uploaded
		FROM theme_old t LEFT JOIN theme_package_old p ON p.theme_id=t.id ORDER BY t.id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var t Theme
		var at int64
		var content []byte
		var revision, uploaded sql.NullInt64
		if err := rows.Scan(&t.ID, &t.Name, &t.Version, &t.Preview, &at, &t.Enabled, &content, &revision, &uploaded); err != nil {
			return err
		}
		if revision.Valid {
			t.Digest = fmt.Sprintf("%x", sha256.Sum256(content))
			pkg, err := theme.Parse(content)
			if err != nil {
				return fmt.Errorf("migrate theme %q: %w", t.ID, err)
			}
			if pkg.Manifest.ID != t.ID {
				return fmt.Errorf("migrate theme %q: package id mismatch", t.ID)
			}
			t.SDK = pkg.Manifest.SDK
		}
		if _, err := tx.Exec("INSERT INTO theme(id) VALUES(?)", t.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO theme_version(theme_id,digest,name,version,preview,uploaded_at,sdk,published) VALUES(?,?,?,?,?,?,?,?)`, t.ID, t.Digest, t.Name, t.Version, t.Preview, at, t.SDK, t.Enabled && t.SDK == theme.SDKVersion && t.Digest != ""); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO theme_file(theme_id,digest,path,content) SELECT theme_id,?,path,content FROM theme_file_old WHERE theme_id=?`, t.Digest, t.ID); err != nil {
			return err
		}
		if revision.Valid {
			if _, err := tx.Exec("INSERT INTO theme_package(theme_id,digest,content,revision,uploaded) VALUES(?,?,?,?,?)", t.ID, t.Digest, content, revision.Int64, uploaded.Int64); err != nil {
				return err
			}
		}
		if t.Enabled && t.Digest != "" && theme.CheckExecutable(t.SDK) == nil {
			if _, err := tx.Exec("UPDATE theme_selection SET current_id=?,current_digest=? WHERE id=1", t.ID, t.Digest); err != nil {
				return err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, table := range []string{"theme_old", "theme_file_old", "theme_package_old"} {
		if _, err := tx.Exec("DROP TABLE " + table); err != nil {
			return err
		}
	}
	return nil
}

// 分层快照不含原包；旧摘要清单可保留版本身份，但 SDK 必须等实际原包校验后才能确定。
func migrateThemeSnapshot(tx *sql.Tx) error {
	var formatColumn int
	if err := tx.QueryRow("SELECT count(*) FROM pragma_table_info('snapshot_meta') WHERE name='format_version'").Scan(&formatColumn); err != nil {
		return err
	}
	if formatColumn == 1 {
		var format int
		if err := tx.QueryRow("SELECT format_version FROM snapshot_meta").Scan(&format); err != nil {
			return err
		}
		if format != 2 {
			return fmt.Errorf("unsupported snapshot format_version=%d", format)
		}
		var unmatched int
		if err := tx.QueryRow(`SELECT (SELECT count(*) FROM theme WHERE id NOT IN(SELECT theme_id FROM snapshot_theme))+(SELECT count(*) FROM snapshot_theme WHERE theme_id NOT IN(SELECT id FROM theme))`).Scan(&unmatched); err != nil {
			return err
		}
		if unmatched != 0 {
			return fmt.Errorf("snapshot theme references do not match configuration")
		}
	}
	if _, err := tx.Exec("ALTER TABLE theme RENAME TO theme_old"); err != nil {
		return err
	}
	if err := execAll(migrationThemeV22)(tx); err != nil {
		return err
	}
	if _, err := tx.Exec("INSERT INTO theme(id) SELECT id FROM theme_old"); err != nil {
		return err
	}
	if formatColumn == 1 {
		if _, err := tx.Exec("ALTER TABLE snapshot_theme RENAME TO snapshot_theme_old"); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO theme_version(theme_id,digest,name,version,preview,uploaded_at,sdk,published)
			SELECT t.id,s.sha256,t.name,t.version,t.preview,t.uploaded_at,0,0 FROM theme_old t JOIN snapshot_theme_old s ON s.theme_id=t.id`); err != nil {
			return err
		}
	} else {
		if _, err := tx.Exec(`INSERT INTO theme_version(theme_id,digest,name,version,preview,uploaded_at,sdk,published) SELECT id,'',name,version,preview,uploaded_at,0,0 FROM theme_old`); err != nil {
			return err
		}
		if _, err := tx.Exec("ALTER TABLE snapshot_meta ADD COLUMN format_version INTEGER NOT NULL DEFAULT 3"); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("CREATE TABLE snapshot_theme(theme_id TEXT NOT NULL,digest TEXT NOT NULL,sha256 TEXT NOT NULL,PRIMARY KEY(theme_id,digest))"); err != nil {
		return err
	}
	if _, err := tx.Exec("INSERT INTO snapshot_theme SELECT theme_id,digest,digest FROM theme_version"); err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE snapshot_meta SET format_version=3"); err != nil {
		return err
	}
	if _, err := tx.Exec("DROP TABLE theme_old"); err != nil {
		return err
	}
	if formatColumn == 1 {
		if _, err := tx.Exec("DROP TABLE snapshot_theme_old"); err != nil {
			return err
		}
	}
	return nil
}
