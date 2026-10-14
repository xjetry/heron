package store

import (
	"database/sql"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/clock"
)

func day(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

// 这一组整体替换：给出即五项与渠道列表一起写，缺席即不动。从未保存过时全部关闭、时刻为 0。
func TestTrafficReportSettingsRoundTrip(t *testing.T) {
	t.Parallel()
	s, _, cs, _ := alertFixture(t)
	st, err := s.Settings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if r := st.TrafficReport; r.Enabled || r.Daily || r.Weekly || r.Monthly || r.Hour != 0 || len(r.Channels) != 0 {
		t.Fatalf("default traffic report = %+v, want everything off at hour 0", r)
	}
	want := TrafficReportSettings{Enabled: true, Weekly: true, Monthly: true, Hour: 23, Channels: []int64{cs[0].ID, cs[1].ID}}
	saved, err := s.SaveSettings(t.Context(), SettingsUpdate{TrafficReport: &TrafficReportUpdate{Enabled: true, Weekly: true, Monthly: true, Hour: 23, Channels: []int64{cs[1].ID, cs[0].ID, cs[1].ID}}})
	if err != nil {
		t.Fatal(err)
	}
	read, err := s.Settings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]TrafficReportSettings{"echo": saved.TrafficReport, "read": read.TrafficReport} {
		if got.Enabled != want.Enabled || got.Daily != want.Daily || got.Weekly != want.Weekly || got.Monthly != want.Monthly || got.Hour != want.Hour || !slices.Equal(got.Channels, want.Channels) {
			t.Errorf("%s = %+v, want %+v", name, got, want)
		}
	}
	// 只改别的组不动这一组。
	title := SiteAppearance{Title: "t", Theme: DefaultTheme}
	if _, err := s.SaveSettings(t.Context(), SettingsUpdate{Appearance: &title}); err != nil {
		t.Fatal(err)
	}
	if read, err = s.Settings(t.Context()); err != nil || !read.TrafficReport.Monthly || read.TrafficReport.Hour != 23 {
		t.Fatalf("saving appearance changed the traffic report: %+v, %v", read.TrafficReport, err)
	}
	// 关闭时保留周期与时刻；渠道按给出的整体替换。
	if _, err := s.SaveSettings(t.Context(), SettingsUpdate{TrafficReport: &TrafficReportUpdate{Daily: true, Hour: 7}}); err != nil {
		t.Fatal(err)
	}
	if read, err = s.Settings(t.Context()); err != nil {
		t.Fatal(err)
	}
	if r := read.TrafficReport; r.Enabled || !r.Daily || r.Weekly || r.Monthly || r.Hour != 7 || len(r.Channels) != 0 {
		t.Fatalf("after disabling: %+v", r)
	}
}

// 写侧两条约束各自点名字段，被拒时整次更新什么都不写。
func TestTrafficReportSettingsRejectsAndWritesNothing(t *testing.T) {
	t.Parallel()
	s, _, cs, _ := alertFixture(t)
	if _, err := s.SaveSettings(t.Context(), SettingsUpdate{TrafficReport: &TrafficReportUpdate{Enabled: true, Daily: true, Hour: 9, Channels: []int64{cs[0].ID}}}); err != nil {
		t.Fatal(err)
	}
	title := SiteAppearance{Title: "changed", Theme: DefaultTheme}
	for name, c := range map[string]struct {
		u     TrafficReportUpdate
		field string
	}{
		"no cadence": {TrafficReportUpdate{Enabled: true, Hour: 3}, "settings.traffic_report"},
		"hour 24":    {TrafficReportUpdate{Daily: true, Hour: 24}, "settings.traffic_report.hour"},
	} {
		_, err := s.SaveSettings(t.Context(), SettingsUpdate{Appearance: &title, TrafficReport: &c.u})
		var bad TrafficReportError
		if !errors.As(err, &bad) || bad.Field != c.field {
			t.Errorf("%s: err = %v, want TrafficReportError on %s", name, err, c.field)
		}
	}
	st, err := s.Settings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if r := st.TrafficReport; !r.Enabled || !r.Daily || r.Hour != 9 || !slices.Equal(r.Channels, []int64{cs[0].ID}) || st.Site.Title != "" {
		t.Fatalf("rejected update wrote something: %+v, title %q", r, st.Site.Title)
	}
}

// 库里读不懂的值报错，不按默认值猜：猜成关会静默停掉运维开启的报告，时刻越界无从定时。
func TestTrafficReportSettingsRejectStoredGarbage(t *testing.T) {
	t.Parallel()
	for key, value := range map[string]string{trafficReportEnabledKey: "yes", trafficReportMonthlyKey: "2", trafficReportHourKey: "24", string(TrafficReportNotifyList): "[1,"} {
		s, _ := open(t)
		execStmts(t, s, "INSERT INTO setting (key, value) VALUES ('"+key+"', '"+value+"')")
		if _, err := s.Settings(t.Context()); err == nil {
			t.Errorf("stored %s = %q was accepted", key, value)
		}
	}
}

// 删渠道在同一事务里把它从报告的渠道列表摘除，与其它选择列表同一路径（NotifyLists）。
func TestTrafficReportChannelRemovedWithChannel(t *testing.T) {
	t.Parallel()
	s, _, cs, _ := alertFixture(t)
	if _, err := s.SaveSettings(t.Context(), SettingsUpdate{TrafficReport: &TrafficReportUpdate{Enabled: true, Daily: true, Channels: []int64{cs[0].ID, cs[1].ID}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteNotifyChannel(t.Context(), cs[0].ID); err != nil {
		t.Fatal(err)
	}
	st, err := s.Settings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(st.TrafficReport.Channels, []int64{cs[1].ID}) {
		t.Fatalf("channels after deletion = %v, want [%d]", st.TrafficReport.Channels, cs[1].ID)
	}
}

// 事件、投递与周期标记同一事务提交；投递只发往报告的渠道，与登录通知的渠道互不相干，各自成批。
func TestRecordTrafficReportEvent(t *testing.T) {
	t.Parallel()
	s, _, cs, _ := alertFixture(t)
	login, report := []int64{cs[0].ID}, []int64{cs[1].ID}
	if _, err := s.SaveSettings(t.Context(), SettingsUpdate{LoginChannels: &login, TrafficReport: &TrafficReportUpdate{Enabled: true, Daily: true, Weekly: true, Channels: report}}); err != nil {
		t.Fatal(err)
	}
	periods := []ReportPeriod{{ReportDaily, day(2026, 10, 12)}, {ReportWeekly, day(2026, 10, 12)}}
	ev, err := s.RecordTrafficReportEvent(t.Context(), periods, "流量报告")
	if err != nil {
		t.Fatal(err)
	}
	if kind, ok := SystemEventKind(ev.Transition); !ok || kind != SystemKindTrafficReport || ev.RuleID != 0 || ev.NodeID != 0 || ev.Summary != "流量报告" {
		t.Fatalf("event = %+v (kind %q)", ev, kind)
	}
	if len(ev.Deliveries) != 1 || ev.Deliveries[0].ChannelID != cs[1].ID {
		t.Fatalf("deliveries = %+v, want one to the report channel %d", ev.Deliveries, cs[1].ID)
	}
	sent, err := s.TrafficReportSent(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(sent) != 2 || !sent[ReportDaily].Equal(day(2026, 10, 12)) || !sent[ReportWeekly].Equal(day(2026, 10, 12)) {
		t.Fatalf("markers = %v", sent)
	}
	assertAlertRows(t, s, "alert_delivery", "1=1", 1)
}

// 同一期至多一条事件：已记下的一期、更早的一期都被拒，什么都不写；标记读回的是最近一期。
func TestTrafficReportPeriodIsRecordedOnce(t *testing.T) {
	t.Parallel()
	s, _, cs, _ := alertFixture(t)
	if _, err := s.SaveSettings(t.Context(), SettingsUpdate{TrafficReport: &TrafficReportUpdate{Enabled: true, Daily: true, Monthly: true, Channels: []int64{cs[0].ID}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordTrafficReportEvent(t.Context(), []ReportPeriod{{ReportDaily, day(2026, 10, 12)}}, "a"); err != nil {
		t.Fatal(err)
	}
	for name, periods := range map[string][]ReportPeriod{
		"same day":            {{ReportDaily, day(2026, 10, 12)}},
		"earlier day":         {{ReportDaily, day(2026, 10, 11)}},
		"new month, same day": {{ReportMonthly, day(2026, 10, 1)}, {ReportDaily, day(2026, 10, 12)}},
	} {
		if _, err := s.RecordTrafficReportEvent(t.Context(), periods, name); !errors.Is(err, ErrTrafficReportSent) {
			t.Errorf("%s: err = %v, want ErrTrafficReportSent", name, err)
		}
	}
	assertAlertRows(t, s, "alert_event", "1=1", 1)
	assertAlertRows(t, s, "alert_delivery", "1=1", 1)
	sent, err := s.TrafficReportSent(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 || !sent[ReportDaily].Equal(day(2026, 10, 12)) {
		t.Fatalf("markers after rejected writes = %v", sent)
	}
	if _, err := s.RecordTrafficReportEvent(t.Context(), []ReportPeriod{{ReportDaily, day(2026, 10, 13)}}, "b"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordTrafficReportEvent(t.Context(), nil, "c"); err == nil {
		t.Fatal("report covering no period was accepted")
	}
	if _, err := s.RecordTrafficReportEvent(t.Context(), []ReportPeriod{{"traffic_report.hourly", day(2026, 10, 14)}}, "d"); err == nil {
		t.Fatal("unknown cadence was accepted")
	}
}

// 标记落在库里：重开之后读回的仍是已发的那一期。
func TestTrafficReportMarkersSurviveReopen(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "t.db")
	clk := clock.NewFake(time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC))
	s, err := Open(path, clk, slog.Default(), MigrateSchema)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordTrafficReportEvent(t.Context(), []ReportPeriod{{ReportMonthly, day(2026, 10, 1)}}, "m"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, clk, slog.Default(), MigrateSchema)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sent, err := s.TrafficReportSent(t.Context())
	if err != nil || !sent[ReportMonthly].Equal(day(2026, 10, 1)) {
		t.Fatalf("markers after reopen = %v, %v", sent, err)
	}
}

// 周期标记与维护任务同表，但不是时刻：存储健康面只取清理与上卷两项。
func TestTrafficReportMarkersStayOutOfStorageHealth(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	if _, err := s.RecordTrafficReportEvent(t.Context(), []ReportPeriod{{ReportDaily, day(2026, 1, 1)}}, "d"); err != nil {
		t.Fatal(err)
	}
	if err := s.write(t.Context(), func(tx *sql.Tx) error {
		last, err := lastMaintenance(t.Context(), tx)
		if err != nil {
			return err
		}
		if len(last) != 0 {
			t.Errorf("lastMaintenance = %v, want no entries besides prune and rollup", last)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
