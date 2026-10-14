package store

import (
	"database/sql"
	"testing"
	"time"
)

func TestSecurityAuditFailureRollsBackCredentialChanges(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"password", "reset", "factor"} {
		t.Run(operation, func(t *testing.T) {
			s, clk := open(t)
			ctx := t.Context()
			if err := s.SetAdminPassword(ctx, "original"); err != nil {
				t.Fatal(err)
			}
			var hash [32]byte
			hash[0] = 1
			seedSession(t, s, hash, clk.Now(), clk.Now().Add(time.Hour), "")
			before, err := s.AdminSecurity(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.write(ctx, func(tx *sql.Tx) error {
				_, err := tx.Exec(`CREATE TRIGGER reject_security_audit BEFORE INSERT ON alert_event BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			switch operation {
			case "password":
				err = s.SetAdminPassword(ctx, "replacement")
			case "reset":
				err = s.ResetAdminSecurity(ctx)
			case "factor":
				before.Event = &AlertEvent{Transition: TransitionAuthChanged, At: clk.Now(), Summary: "认证方式变更"}
				err = s.CommitAdminSecurity(ctx, before, `{"secret":"SECRET"}`, true, nil, clk.Now(), clk.Now().Add(time.Hour), "")
			}
			if err == nil {
				t.Fatal("credential change ignored failed audit")
			}
			after, err := s.AdminSecurity(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if before.PasswordHash != after.PasswordHash || before.Data != after.Data || before.Generation != after.Generation {
				t.Fatal("audit failure committed credential state")
			}
			if _, ok := lookupSession(t, s, hash); !ok {
				t.Fatal("audit failure revoked session without committing audit")
			}
		})
	}
}
