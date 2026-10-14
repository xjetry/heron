package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func missingDB(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "missing.db")
}

func assertNotCreated(t *testing.T, db string, err error) {
	t.Helper()
	if _, statErr := os.Stat(db); !os.IsNotExist(statErr) {
		t.Fatalf("database was created: stat %v; err %v", statErr, err)
	}
	if err == nil || !strings.Contains(err.Error(), "does not exist") || !strings.Contains(err.Error(), db) || !strings.Contains(err.Error(), "--db") {
		t.Fatalf("err = %v, want a missing-database error naming %s and --db", err, db)
	}
}

func TestOfflineCommandsDoNotCreateAMissingDatabase(t *testing.T) {
	t.Parallel()
	var out, errOut strings.Builder
	cases := []struct {
		name string
		run  func(db string) error
	}{
		{"token list", func(db string) error {
			return runTokenWith([]string{"list", "--db", db}, &out, &errOut)
		}},
		{"token revoke --all", func(db string) error {
			return runTokenWith([]string{"revoke", "--all", "--db", db}, &out, &errOut)
		}},
		{"stats", func(db string) error { return runStatsWith([]string{"--db", db}, io.Discard) }},
		{"node list", func(db string) error { return runNode([]string{"list", "--db", db}) }},
		{"node delete", func(db string) error { return runNode([]string{"delete", "--id", "1", "--db", db}) }},
		{"node rotate-token", func(db string) error { return runNode([]string{"rotate-token", "--id", "1", "--db", db}) }},
		{"window close", func(db string) error { return runWindow([]string{"close", "--db", db}) }},
		{"window show", func(db string) error { return runWindow([]string{"show", "--db", db}) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := missingDB(t)
			assertNotCreated(t, db, c.run(db))
		})
	}
}

func TestStateEstablishingCommandsCreateAMissingDatabase(t *testing.T) {
	t.Parallel()
	t.Run("passwd", func(t *testing.T) {
		db := missingDB(t)
		err := runPasswdWith([]string{"--db", db}, pipeWith(t, "a sufficiently long password\n"), io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if _, statErr := os.Stat(db); statErr != nil {
			t.Fatalf("passwd did not create the database: %v", statErr)
		}
	})
	t.Run("window open", func(t *testing.T) {
		db := missingDB(t)
		if err := runWindow([]string{"open", "--db", db, "--ttl", "10m", "--max", "2"}); err != nil {
			t.Fatal(err)
		}
		if _, statErr := os.Stat(db); statErr != nil {
			t.Fatalf("window open did not create the database: %v", statErr)
		}
	})
	t.Run("node create", func(t *testing.T) {
		db := missingDB(t)
		if err := runNode([]string{"create", "--db", db, "--name", "n"}); err != nil {
			t.Fatal(err)
		}
		if _, statErr := os.Stat(db); statErr != nil {
			t.Fatalf("node create did not create the database: %v", statErr)
		}
	})
}
