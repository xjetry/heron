package main

import (
	"database/sql"
	"testing"
)

func removeV31Metrics(t *testing.T, db *sql.DB) {
	t.Helper()
	restoreExec(t, db, "DROP TABLE node_coverage; ALTER TABLE metric_1m DROP COLUMN reported; ALTER TABLE metric_1m DROP COLUMN observed")
	for _, table := range []string{"metric_5m", "metric_1h"} {
		for _, column := range []string{"minutes", "observed", "both"} {
			restoreExec(t, db, "ALTER TABLE "+table+" DROP COLUMN "+column)
		}
	}
}
