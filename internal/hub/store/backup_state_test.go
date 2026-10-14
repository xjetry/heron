package store

import (
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestBackupMarkerAndEventAtomic(t *testing.T) {
	t.Parallel()
	for _, transition := range []Transition{TransitionBackupFailed, TransitionBackupRecovered, TransitionBackupDisabled} {
		t.Run(string(transition), func(t *testing.T) {
			s, _ := open(t)
			if transition != TransitionBackupFailed {
				if _, err := s.RecordBackupEvent(t.Context(), TransitionBackupFailed, "failed", s.clk.Now()); err != nil {
					t.Fatal(err)
				}
			}
			before, err := s.BackupFailingSince(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			err = s.write(t.Context(), func(tx *sql.Tx) error {
				_, err := tx.Exec(`CREATE TRIGGER reject_backup_event BEFORE INSERT ON alert_event BEGIN SELECT RAISE(ABORT,'event rejected'); END`)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			_, writeErr := s.RecordBackupEvent(t.Context(), transition, "transition", s.clk.Now())
			after, err := s.BackupFailingSince(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if writeErr == nil || !after.Equal(before) {
				t.Errorf("failed event changed marker: before=%s after=%s err=%v", before, after, writeErr)
			}
			err = s.write(t.Context(), func(tx *sql.Tx) error { _, err := tx.Exec("DROP TRIGGER reject_backup_event"); return err })
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.RecordBackupEvent(t.Context(), transition, "transition", s.clk.Now()); err != nil {
				t.Fatal(err)
			}
			after, err = s.BackupFailingSince(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if after.IsZero() != (transition != TransitionBackupFailed) {
				t.Errorf("committed event marker mismatch: transition=%s since=%s", transition, after)
			}
			var value string
			err = s.r.QueryRow("SELECT value FROM setting WHERE key='backup.config_failing_since'").Scan(&value)
			if transition != TransitionBackupFailed && !errors.Is(err, sql.ErrNoRows) {
				t.Errorf("recovery did not delete marker: value=%s err=%v", value, err)
			}
		})
	}
}

func TestBackupEventsUseCurrentChannelsWithoutRuleState(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	c, err := s.SaveNotifyChannel(t.Context(), NotifyChannel{Name: "backup", Kind: ChannelWebhook, Config: `{"url":"https://example.test"}`})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.SaveSettings(t.Context(), SettingsUpdate{Backup: &BackupSettingsUpdate{Channels: &[]int64{c.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	ev, err := s.RecordBackupEvent(t.Context(), TransitionBackupFailed, "config 失败", s.clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	if ev.RuleID != 0 || ev.NodeID != 0 || len(ev.Deliveries) != 1 || ev.Deliveries[0].ChannelID != c.ID {
		t.Fatalf("backup event=%+v", ev)
	}
	batch, err := s.GetDeliveryBatch(t.Context(), ev.Deliveries[0].ID)
	if err != nil || len(batch.Deliveries) != 1 || batch.Deliveries[0].BatchID != ev.Deliveries[0].ID || len(batch.Events) != 1 || batch.Events[0].ID != ev.ID {
		t.Fatalf("backup event must create its own readable batch: batch=%+v err=%v event=%+v", batch, err, ev)
	}
	var states int
	if err := s.r.QueryRow("SELECT count(*) FROM alert_state").Scan(&states); err != nil {
		t.Fatal(err)
	}
	if states != 0 {
		t.Fatalf("backup created %d rule states", states)
	}
	if err := s.DeleteNotifyChannel(t.Context(), c.ID); err != nil {
		t.Fatal(err)
	}
	ev, err = s.RecordBackupEvent(t.Context(), TransitionBackupRecovered, "config 已恢复", s.clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(ev.Deliveries) != 0 {
		t.Fatalf("backup referenced deleted channel: %+v", ev.Deliveries)
	}
	stored, err := s.GetAlertEvent(t.Context(), ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Transition != TransitionBackupRecovered || stored.Summary != "config 已恢复" {
		t.Fatalf("recovery not stored: %+v", stored)
	}
}

func TestBackupSuccessRejectsUnknownLayer(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	if err := s.RecordBackupSuccess(t.Context(), "other"); err == nil {
		t.Fatal("unknown backup layer accepted")
	}
	times, err := s.BackupSuccessTimes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(times) != 0 {
		t.Fatalf("unknown layer created success: %+v", times)
	}
}

// 触发只在调用方没有读到有效标记时发生；库里若留着读不懂的坏值，触发以本次首次失败时刻覆盖它。
func TestBackupFiringOverwritesCorruptMarker(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	err := s.write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.Exec("INSERT INTO setting (key, value) VALUES ('backup.config_failing_since', 'x')")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BackupFailingSince(t.Context()); err == nil {
		t.Fatal("corrupt marker read back without error")
	}
	at := s.clk.Now()
	if _, err := s.RecordBackupEvent(t.Context(), TransitionBackupFailed, "config 层备份失败（marker）", at); err != nil {
		t.Fatal(err)
	}
	if got, err := s.BackupFailingSince(t.Context()); err != nil || !got.Equal(time.Unix(at.Unix(), 0).UTC()) {
		t.Errorf("firing left corrupt marker: got=%s err=%v", got, err)
	}
}
