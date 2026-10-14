package sqlitetest

import (
	"database/sql"
	"fmt"
	"testing"
)

type Column struct {
	Name, Type string
	NotNull    bool
	Default    sql.NullString
	PK         int
}

type SchemaDescription struct {
	Tables  map[string][]Column
	Indexes map[string]IndexDescription
}

type IndexDescription struct {
	Table   string
	Columns []string
	Unique  bool
}

// Describe 比较结构而不是 SQL 原文：空白与注释不改变结构，索引列序与唯一性会改变访问路径或约束。
func Describe(t *testing.T, db *sql.DB) SchemaDescription {
	t.Helper()
	rows, err := db.Query("SELECT name, type, tbl_name FROM sqlite_master WHERE type IN ('table', 'index') AND name NOT LIKE 'sqlite_%' ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	out := SchemaDescription{Tables: map[string][]Column{}, Indexes: map[string]IndexDescription{}}
	for rows.Next() {
		var name, kind, table string
		if err := rows.Scan(&name, &kind, &table); err != nil {
			t.Fatal(err)
		}
		if kind == "table" {
			out.Tables[name] = nil
		} else {
			out.Indexes[name] = IndexDescription{Table: table}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	for table := range out.Tables {
		info, err := db.Query(fmt.Sprintf("PRAGMA table_info(%q)", table))
		if err != nil {
			t.Fatal(err)
		}
		for info.Next() {
			var cid int
			var c Column
			if err := info.Scan(&cid, &c.Name, &c.Type, &c.NotNull, &c.Default, &c.PK); err != nil {
				t.Fatal(err)
			}
			out.Tables[table] = append(out.Tables[table], c)
		}
		if err := info.Err(); err != nil {
			t.Fatal(err)
		}
		if err := info.Close(); err != nil {
			t.Fatal(err)
		}

		indexes, err := db.Query(fmt.Sprintf("PRAGMA index_list(%q)", table))
		if err != nil {
			t.Fatal(err)
		}
		for indexes.Next() {
			var seq int
			var name, origin string
			var unique, partial bool
			if err := indexes.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
				t.Fatal(err)
			}
			if index, ok := out.Indexes[name]; ok {
				index.Unique = unique
				out.Indexes[name] = index
			}
		}
		if err := indexes.Err(); err != nil {
			t.Fatal(err)
		}
		if err := indexes.Close(); err != nil {
			t.Fatal(err)
		}
	}
	for name, index := range out.Indexes {
		info, err := db.Query(fmt.Sprintf("PRAGMA index_info(%q)", name))
		if err != nil {
			t.Fatal(err)
		}
		for info.Next() {
			var seq, cid int
			var column string
			if err := info.Scan(&seq, &cid, &column); err != nil {
				t.Fatal(err)
			}
			for len(index.Columns) <= seq {
				index.Columns = append(index.Columns, "")
			}
			index.Columns[seq] = column
		}
		if err := info.Err(); err != nil {
			t.Fatal(err)
		}
		if err := info.Close(); err != nil {
			t.Fatal(err)
		}
		out.Indexes[name] = index
	}
	return out
}
