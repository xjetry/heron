package calendar

import (
	"testing"
	"time"
)

func zone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// 存在的读数原样；空档取跳变时刻，time.Date 往回（New York、跨午夜的 Santiago）与往前（Lord Howe、Beirut、Cairo）两种
// 归一都落到它；重复时段取第一次，time.Date 给第一次（New York）与给第二次（London）都一样。半点与非整点偏移的时区
// （Asia/Kolkata、Asia/Kathmandu）和半点的目标读数同样适用。
func TestMomentResolvesGapsAndRepeatsToOneInstant(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		zone      string
		y         int
		m         time.Month
		d, minute int
		want      time.Time
	}{
		{"Asia/Tokyo", 2026, 10, 12, 9 * 60, time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)},
		{"Asia/Kolkata", 2026, 11, 1, 0, time.Date(2026, 10, 31, 18, 30, 0, 0, time.UTC)},
		{"Asia/Kathmandu", 2026, 11, 1, 8*60 + 30, time.Date(2026, 11, 1, 2, 45, 0, 0, time.UTC)},
		{"America/New_York", 2026, 3, 8, 2 * 60, time.Date(2026, 3, 8, 7, 0, 0, 0, time.UTC)},
		{"America/New_York", 2026, 3, 8, 2*60 + 30, time.Date(2026, 3, 8, 7, 0, 0, 0, time.UTC)},
		{"America/New_York", 2026, 11, 1, 1 * 60, time.Date(2026, 11, 1, 5, 0, 0, 0, time.UTC)},
		{"America/New_York", 2026, 11, 1, 1*60 + 30, time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC)},
		{"Australia/Lord_Howe", 2026, 10, 4, 2 * 60, time.Date(2026, 10, 3, 15, 30, 0, 0, time.UTC)},
		{"Australia/Lord_Howe", 2026, 10, 4, 2*60 + 15, time.Date(2026, 10, 3, 15, 30, 0, 0, time.UTC)},
		{"America/Santiago", 2026, 9, 6, 0, time.Date(2026, 9, 6, 4, 0, 0, 0, time.UTC)},
		{"America/Santiago", 2026, 9, 6, 30, time.Date(2026, 9, 6, 4, 0, 0, 0, time.UTC)},
		{"Asia/Beirut", 2026, 3, 29, 0, time.Date(2026, 3, 28, 22, 0, 0, 0, time.UTC)},
		{"Africa/Cairo", 2026, 4, 24, 0, time.Date(2026, 4, 23, 22, 0, 0, 0, time.UTC)},
		{"Europe/London", 2026, 10, 25, 1 * 60, time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)},
		{"Europe/London", 2026, 10, 25, 1*60 + 30, time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC)},
		// 月与日越界按 time.Date 归一：2026-12-32 是 2027-01-01。
		{"UTC", 2026, 12, 32, 0, time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)},
	} {
		loc := zone(t, c.zone)
		got := Moment(c.y, c.m, c.d, c.minute, loc)
		if !got.Equal(c.want) {
			t.Errorf("%s %04d-%02d-%02d %02d:%02d: got %s, want %s", c.zone, c.y, c.m, c.d, c.minute/60, c.minute%60, got.UTC(), c.want)
		}
	}
}

// 与逐分钟扫描对照：从这一日在任何时区开始之前（UTC 前一日 10:00，偏移至多 +14 小时）起按分钟往后走，本地读数第一次
// 不早于目标的那一刻就是答案。覆盖两个方向的夏令时日与普通日的每一个分钟；重复时段里读数先越过目标、再回落、再越过，
// 扫描取的是第一次。
func TestMomentMatchesAMinuteByMinuteScan(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		zone string
		y    int
		m    time.Month
		d    int
	}{
		{"America/New_York", 2026, 3, 8},
		{"America/New_York", 2026, 11, 1},
		{"Europe/London", 2026, 10, 25},
		{"Australia/Lord_Howe", 2026, 10, 4},
		{"Australia/Lord_Howe", 2026, 4, 5},
		{"America/Santiago", 2026, 9, 6},
		{"Asia/Kolkata", 2026, 6, 1},
	} {
		loc := zone(t, c.zone)
		day := time.Date(c.y, c.m, c.d, 0, 0, 0, 0, time.UTC)
		type reading struct{ at, wall time.Time }
		var scan []reading
		for at := day.Add(-14 * time.Hour); at.Before(day.Add(38 * time.Hour)); at = at.Add(time.Minute) {
			scan = append(scan, reading{at, wallClock(at.In(loc))})
		}
		// 第一次到达的时刻随目标单调不减（到达较晚目标的那一刻也已到达较早的目标），扫描从上一个答案接着往后找。
		next := 0
		for minute := 0; minute < MinutesPerDay; minute++ {
			target := day.Add(time.Duration(minute) * time.Minute)
			for next < len(scan) && scan[next].wall.Before(target) {
				next++
			}
			want := scan[next].at
			if got := Moment(c.y, c.m, c.d, minute, loc); !got.Equal(want) {
				t.Fatalf("%s %s: got %s, the scan reaches it at %s", c.zone, target.Format("2006-01-02 15:04"), got.UTC(), want.UTC())
			}
		}
	}
}
