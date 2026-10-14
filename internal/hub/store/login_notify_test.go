package store

import (
	"database/sql"
	"errors"
	"slices"
	"testing"
	"time"
)

func TestLoginNotifyDeleteIsAtomic(t *testing.T) {
	t.Parallel()
	s, _, cs, _ := alertFixture(t)
	ids := []int64{cs[0].ID, cs[1].ID}
	if _, err := s.SaveSettings(t.Context(), SettingsUpdate{LoginChannels: &ids}); err != nil {
		t.Fatal(err)
	}
	// 设置写失败时，渠道删除和列表摘除必须一起回滚。
	if err := s.write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`CREATE TRIGGER reject_login_setting BEFORE UPDATE ON setting WHEN NEW.key='notify.login_channels' BEGIN SELECT RAISE(ABORT,'blocked setting update'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteNotifyChannel(t.Context(), cs[0].ID); err == nil {
		t.Fatal("channel deletion succeeded despite failed settings update")
	}
	settings, err := s.Settings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	channels, err := s.ListNotifyChannels(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(settings.LoginChannelIDs, ids) || len(channels) != 2 {
		t.Fatalf("channel/settings transaction partially committed: %+v %+v", settings, channels)
	}
}

func TestLoginNotifyEventTransaction(t *testing.T) {
	t.Parallel()
	s, _, cs, _ := alertFixture(t)
	ids := []int64{cs[0].ID, cs[1].ID}
	if _, err := s.SaveSettings(t.Context(), SettingsUpdate{LoginChannels: &ids}); err != nil {
		t.Fatal(err)
	}
	if err := s.write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`CREATE TRIGGER reject_login_delivery BEFORE INSERT ON alert_delivery WHEN NEW.channel_id=2 BEGIN SELECT RAISE(ABORT,'blocked delivery'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_, err := s.RecordLoginEvent(t.Context(), AlertEvent{At: s.clk.Now(), Transition: TransitionLoginSuccess})
	if err == nil {
		t.Fatal("event insertion succeeded despite failed delivery insert")
	}
	assertAlertRows(t, s, "alert_event", "1=1", 0)
	assertAlertRows(t, s, "alert_delivery", "1=1", 0)
}

// 系统事件的种类由 transition 决定；写侧各收各的 transition，被拒时什么都不写。库里 0/0 的行因此都带系统
// 事件的 transition、规则事件的行都不带，投递队列按 transition 推出的标签才与 0/0 一致。
func TestSystemEventTransitionsStayWithTheirWriter(t *testing.T) {
	t.Parallel()
	for tr, want := range map[Transition]string{
		TransitionLoginSuccess: SystemKindLogin, TransitionLoginLocked: SystemKindLogin,
		TransitionBackupFailed: SystemKindBackup, TransitionBackupRecovered: SystemKindBackup, TransitionBackupDisabled: SystemKindBackup,
		TransitionTrafficReport: SystemKindTrafficReport, TransitionFiring: "", TransitionRecovered: "", "": "",
	} {
		if kind, ok := SystemEventKind(tr); kind != want || ok != (want != "") {
			t.Errorf("SystemEventKind(%q) = %q, %v; want %q", tr, kind, ok, want)
		}
	}
	s, ids, cs, _ := alertFixture(t)
	channels := []int64{cs[0].ID}
	if _, err := s.SaveSettings(t.Context(), SettingsUpdate{LoginChannels: &channels}); err != nil {
		t.Fatal(err)
	}
	r := saveRule(t, s, AlertRule{Name: "offline", Kind: KindOffline, AllNodes: true})
	for _, tr := range []Transition{TransitionFiring, TransitionRecovered, TransitionBackupFailed, TransitionTrafficReport, ""} {
		if _, err := s.RecordLoginEvent(t.Context(), AlertEvent{At: s.clk.Now(), Transition: tr}); err == nil {
			t.Errorf("login event accepted transition %q", tr)
		}
	}
	for _, tr := range []Transition{TransitionFiring, TransitionRecovered, TransitionLoginLocked, TransitionTrafficReport, ""} {
		if _, err := s.RecordBackupEvent(t.Context(), tr, "backup", s.clk.Now()); err == nil {
			t.Errorf("backup event accepted transition %q", tr)
		}
	}
	for _, tr := range []Transition{TransitionLoginSuccess, TransitionLoginLocked, TransitionBackupFailed, TransitionBackupRecovered, TransitionBackupDisabled, TransitionTrafficReport} {
		if _, err := s.RecordTransition(t.Context(), r.ID, ids[0], StateFiring, "", time.Time{}, AlertEvent{At: s.clk.Now(), Transition: tr}, systemTargets(channels)); err == nil {
			t.Errorf("rule event accepted the system transition %q", tr)
		}
	}
	assertAlertRows(t, s, "alert_event", "1=1", 0)
	assertAlertRows(t, s, "alert_state", "1=1", 0)
}

// 系统事件与规则事件共用 recordAlertEvent 写事件与投递：每个渠道在同一写事务里核对存在。列表里残留不存在的 ID
// （只可能来自 hub 之外改库，saveChannelIDs 与删渠道维持列表只含存在的渠道）时整条事件失败、什么都不写，不会留下
// 指向不存在渠道的投递。另写一份插入语句、漏掉引用检查的写者在这里红。
func TestSystemEventsCheckChannelReferences(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		list  NotifyList
		write func(s *Store) error
	}{
		"login": {LoginNotifyList, func(s *Store) error {
			_, err := s.RecordLoginEvent(t.Context(), AlertEvent{At: s.clk.Now(), Transition: TransitionLoginSuccess})
			return err
		}},
		"backup": {BackupNotifyList, func(s *Store) error {
			_, err := s.RecordBackupEvent(t.Context(), TransitionBackupFailed, "config 失败", s.clk.Now())
			return err
		}},
		"traffic report": {TrafficReportNotifyList, func(s *Store) error {
			_, err := s.RecordTrafficReportEvent(t.Context(), []ReportPeriod{{ReportDaily, day(2026, 1, 1)}}, "流量报告")
			return err
		}},
	} {
		t.Run(name, func(t *testing.T) {
			s, _, cs, _ := alertFixture(t)
			if err := s.write(t.Context(), func(tx *sql.Tx) error { return putChannelIDs(tx, c.list, []int64{cs[0].ID, 999}) }); err != nil {
				t.Fatal(err)
			}
			var missing NotFoundError
			if err := c.write(s); !errors.As(err, &missing) || missing.Kind != ObjectNotifyChannel || missing.ID != 999 {
				t.Fatalf("event with a missing channel in %s: err = %v, want NotFoundError for channel 999", c.list, err)
			}
			assertAlertRows(t, s, "alert_event", "1=1", 0)
			assertAlertRows(t, s, "alert_delivery", "1=1", 0)
			// 流量报告的周期标记与事件同一事务：事件没写成，这一期就没发过。
			assertAlertRows(t, s, "maintenance_state", "1=1", 0)
		})
	}
}
