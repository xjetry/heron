package store

import (
	"database/sql"
	"strings"
)

// rebuildTable 用 ddlOf 给出的 DDL 重建 name：以 name_new 建出新表，搬运两版都有的列（新列取 DDL
// 默认值），删旧表、改名。走重建而不是 ALTER TABLE ADD COLUMN：描述表生成的 DDL 给每列
// 统一的默认值，而旧版建出的列没有默认值；ADD COLUMN 只能补新列、改不了旧列的定义，
// 迁移后的库却必须与全新库逐列相同（含默认值）。
func rebuildTable(tx *sql.Tx, name string, ddlOf func(table string) string) error {
	tmp := name + "_new"
	if _, err := tx.Exec(ddlOf(tmp)); err != nil {
		return err
	}
	oldCols, err := columnNames(tx, name)
	if err != nil {
		return err
	}
	newCols, err := columnNames(tx, tmp)
	if err != nil {
		return err
	}
	oldSet := make(map[string]bool, len(oldCols))
	for _, c := range oldCols {
		oldSet[c] = true
	}
	var shared []string
	for _, c := range newCols {
		if oldSet[c] {
			shared = append(shared, c)
		}
	}
	list := strings.Join(shared, ", ")
	if _, err := tx.Exec("INSERT INTO " + tmp + " (" + list + ") SELECT " + list + " FROM " + name); err != nil {
		return err
	}
	if _, err := tx.Exec("DROP TABLE " + name); err != nil {
		return err
	}
	_, err = tx.Exec("ALTER TABLE " + tmp + " RENAME TO " + name)
	return err
}

func columnNames(tx *sql.Tx, table string) ([]string, error) {
	rows, err := tx.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}
