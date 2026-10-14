package store

import (
	"database/sql"
	"testing"
)

func TestLoginAuditIndependentOfNotifications(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	for _, transition := range []Transition{TransitionLoginSuccess, TransitionLoginLocked, TransitionLoginFailed, TransitionAuthChanged} {
		ev, err := s.RecordLoginEvent(t.Context(), AlertEvent{Transition: transition, At: s.clk.Now()})
		if err != nil || ev.ID == 0 || len(ev.Deliveries) != 0 {
			t.Fatalf("audit without channels: %+v, %v", ev, err)
		}
		got, err := s.GetAlertEvent(t.Context(), ev.ID)
		if err != nil || got.Transition != transition {
			t.Fatalf("stored audit: %+v, %v", got, err)
		}
	}
	c, err := s.SaveNotifyChannel(t.Context(), NotifyChannel{Name: "audit", Kind: ChannelWebhook, Config: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveSettings(t.Context(), SettingsUpdate{LoginChannels: &[]int64{c.ID}}); err != nil {
		t.Fatal(err)
	}
	for _, transition := range []Transition{TransitionLoginSuccess, TransitionLoginFailed} {
		ev, err := s.RecordLoginEvent(t.Context(), AlertEvent{Transition: transition, At: s.clk.Now()})
		want := 1
		if transition == TransitionLoginFailed {
			want = 0
		}
		if err != nil || ev.ID == 0 || len(ev.Deliveries) != want {
			t.Fatalf("notification policy: %+v, %v", ev, err)
		}
	}
}

func TestBackupSuccessAuditAtomic(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	if err := s.write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`CREATE TRIGGER reject_success BEFORE INSERT ON alert_event BEGIN SELECT RAISE(ABORT,'audit rejected'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordBackupSuccess(t.Context(), "config"); err == nil {
		t.Fatal("audit rejection did not abort success")
	}
	times, err := s.BackupSuccessTimes(t.Context())
	if err != nil || len(times) != 0 {
		t.Fatalf("unrecorded audit advanced success: %v, %v", times, err)
	}
	if err := s.write(t.Context(), func(tx *sql.Tx) error { _, err := tx.Exec("DROP TRIGGER reject_success"); return err }); err != nil {
		t.Fatal(err)
	}
	for _, layer := range []string{"config", "metrics"} {
		if err := s.RecordBackupSuccess(t.Context(), layer); err != nil {
			t.Fatal(err)
		}
	}
	var events, deliveries int
	if err := s.r.QueryRow("SELECT count(*) FROM alert_event WHERE transition='backup_success'").Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := s.r.QueryRow("SELECT count(*) FROM alert_delivery").Scan(&deliveries); err != nil {
		t.Fatal(err)
	}
	times, err = s.BackupSuccessTimes(t.Context())
	if err != nil || len(times) != 2 || events != 2 || deliveries != 0 {
		t.Fatalf("success audit: times=%v events=%d deliveries=%d err=%v", times, events, deliveries, err)
	}
}
