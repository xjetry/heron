package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSecurityResetRequiresExistingDatabaseAndConfirmation(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "missing.db")
	for _, args := range [][]string{{"--db", path}, {"--db", path, "--yes"}} {
		if err := runSecurityReset(args); err == nil {
			t.Fatalf("reset accepted %v", args)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("reset created missing database: %v", err)
		}
	}
}

func TestSecurityResetClearsFactorsAndSessions(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "hub.db")
	st, a, err := openOffline(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := a.SetPassword(t.Context(), "a sufficiently long password"); err != nil {
		t.Fatal(err)
	}
	before, err := st.AdminSecurity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var session [32]byte
	session[0] = 1
	now := time.Now()
	if err := st.CommitAdminSecurity(t.Context(), before, `{"secret":"TEST","recovery":["digest"],"passkeys":[]}`, false, &session, now, now.Add(24*time.Hour), ""); err != nil {
		t.Fatal(err)
	}
	if err := runSecurityReset([]string{"--db", path, "--yes"}); err != nil {
		t.Fatal(err)
	}
	after, err := st.AdminSecurity(t.Context())
	if err != nil || after.Data != "{}" || after.PasswordHash != before.PasswordHash || after.Generation <= before.Generation {
		t.Fatalf("reset state: %+v, %v", after, err)
	}
	sessions, err := st.Sessions(t.Context())
	if err != nil || len(sessions) != 0 {
		t.Fatalf("sessions survived reset: %+v, %v", sessions, err)
	}
}
