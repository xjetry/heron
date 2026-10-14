package store

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSilenceRoundTripAndScope(t *testing.T) {
	t.Parallel()
	s, ids, _, _ := alertFixture(t)
	ctx := t.Context()
	daily := Silence{Name: "夜间维护", Enabled: true, AllNodes: true, Kind: SilenceDaily, StartHHMM: "22:00", EndHHMM: "06:00", Reason: "机房巡检"}
	daily, err := s.SaveSilence(ctx, daily)
	if err != nil {
		t.Fatal(err)
	}
	setTags(t, s, ids[0], "db")
	once := Silence{Name: "升级窗口", Kind: SilenceOnce, FromAt: 1000, UntilAt: 2000, SelectorTags: []string{"db"}}
	once, err = s.SaveSilence(ctx, once)
	if err != nil {
		t.Fatal(err)
	}
	explicit := Silence{Name: "单机维护", Kind: SilenceOnce, FromAt: 100, UntilAt: 200, NodeIDs: []int64{ids[1], ids[0], ids[1]}}
	explicit, err = s.SaveSilence(ctx, explicit)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.ListSilences(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("silences=%+v", got)
	}
	wantDaily := Silence{ID: daily.ID, Name: "夜间维护", Enabled: true, AllNodes: true, Kind: SilenceDaily, StartHHMM: "22:00", EndHHMM: "06:00", Reason: "机房巡检", CreatedAt: s.clk.Now()}
	if !reflect.DeepEqual(got[0], wantDaily) {
		t.Fatalf("daily=%+v, want %+v", got[0], wantDaily)
	}
	// AllNodes 不落 silence_node 行。
	assertAlertRows(t, s, "silence_node", fmt.Sprint("silence_id = ", daily.ID), 0)
	// 标签选择器读出的是当前交集的展开，标签名按先建的写法回显。
	if !reflect.DeepEqual(got[1], Silence{ID: once.ID, Name: "升级窗口", Kind: SilenceOnce, FromAt: 1000, UntilAt: 2000, NodeIDs: []int64{ids[0]}, SelectorTags: []string{"db"}, CreatedAt: s.clk.Now()}) {
		t.Fatalf("once=%+v", got[1])
	}
	// 显式集合升序去重。
	if got[2].ID != explicit.ID || !slices.Equal(got[2].NodeIDs, ids) {
		t.Fatalf("explicit=%+v, want id %d node ids %v", got[2], explicit.ID, ids)
	}
	// 更新整体替换作用域与字段，created_at 不动。
	daily.AllNodes, daily.Enabled, daily.NodeIDs = false, false, []int64{ids[1]}
	daily, err = s.SaveSilence(ctx, daily)
	if err != nil {
		t.Fatal(err)
	}
	got, _ = s.ListSilences(ctx)
	if got[0].Enabled || got[0].AllNodes || !slices.Equal(got[0].NodeIDs, []int64{ids[1]}) || !got[0].CreatedAt.Equal(s.clk.Now()) {
		t.Fatalf("updated=%+v", got[0])
	}
	assertAlertRows(t, s, "silence_node", fmt.Sprint("silence_id = ", daily.ID), 1)
}

func TestCheckSilenceFieldsRejectsBadCombinations(t *testing.T) {
	t.Parallel()
	s, ids, _, _ := alertFixture(t)
	ctx := t.Context()
	valid := Silence{Name: "n", Kind: SilenceDaily, StartHHMM: "22:00", EndHHMM: "06:00", NodeIDs: ids[:1]}
	for _, tc := range []struct {
		name   string
		mutate func(*Silence)
		want   string
	}{
		{"daily with once fields", func(s *Silence) { s.FromAt = 1 }, "from_at"},
		{"daily same start and end", func(s *Silence) { s.EndHHMM = "22:00" }, "end_hhmm"},
		{"daily bad start", func(s *Silence) { s.StartHHMM = "9:00" }, "start_hhmm"},
		{"daily hour overflow", func(s *Silence) { s.StartHHMM = "24:00" }, "start_hhmm"},
		{"daily minute overflow", func(s *Silence) { s.EndHHMM = "06:60" }, "end_hhmm"},
		{"daily missing end", func(s *Silence) { s.EndHHMM = "" }, "end_hhmm"},
		{"once with daily fields", func(s *Silence) { s.Kind = SilenceOnce; s.FromAt, s.UntilAt = 1, 2 }, "start_hhmm"},
		{"once inverted window", func(s *Silence) { s.Kind = SilenceOnce; s.StartHHMM, s.EndHHMM = "", ""; s.FromAt, s.UntilAt = 2, 1 }, "from_at"},
		{"once empty window", func(s *Silence) { s.Kind = SilenceOnce; s.StartHHMM, s.EndHHMM = "", ""; s.FromAt, s.UntilAt = 2, 2 }, "from_at"},
		{"unknown kind", func(s *Silence) { s.Kind = "weekly" }, "kind"},
		{"reason too long", func(s *Silence) { s.Reason = strings.Repeat("原", 257) }, "reason"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := valid
			tc.mutate(&bad)
			var field KindFieldError
			if _, err := s.SaveSilence(ctx, bad); !errors.As(err, &field) || !strings.HasPrefix(string(field.Field), tc.want) {
				t.Fatalf("err=%v, want a KindFieldError on %s", err, tc.want)
			}
		})
	}
	if _, err := s.SaveSilence(ctx, valid); err != nil {
		t.Fatalf("valid silence rejected: %v", err)
	}
}

func TestSaveSilenceValidatesReferencesAndScope(t *testing.T) {
	t.Parallel()
	s, ids, _, _ := alertFixture(t)
	ctx := t.Context()
	once := func() Silence { return Silence{Name: "n", Kind: SilenceOnce, FromAt: 1, UntilAt: 2, NodeIDs: ids[:1]} }
	bad := once()
	bad.NodeIDs = []int64{999}
	var missing NotFoundError
	if _, err := s.SaveSilence(ctx, bad); !errors.As(err, &missing) || missing.Kind != ObjectNode || missing.ID != 999 {
		t.Fatalf("missing node err=%v", err)
	}
	bad = once()
	bad.NodeIDs, bad.SelectorTags = nil, []string{"nope"}
	if _, err := s.SaveSilence(ctx, bad); err == nil || !strings.Contains(err.Error(), `tag "nope" does not exist`) {
		t.Fatalf("missing tag err=%v", err)
	}
	bad = once()
	bad.AllNodes, bad.NodeIDs = true, ids[:1]
	if _, err := s.SaveSilence(ctx, bad); err == nil {
		t.Fatal("all_nodes with explicit ids accepted")
	}
	bad = once()
	bad.ID = 999
	if _, err := s.SaveSilence(ctx, bad); !errors.As(err, &missing) || missing.Kind != ObjectSilence || missing.ID != 999 {
		t.Fatalf("missing silence err=%v", err)
	}
	if got, _ := s.ListSilences(ctx); len(got) != 0 {
		t.Fatalf("rejected saves persisted: %+v", got)
	}
}

func TestDeleteSilenceRemovesItsRows(t *testing.T) {
	t.Parallel()
	s, ids, _, _ := alertFixture(t)
	ctx := t.Context()
	setTags(t, s, ids[0], "db")
	si, err := s.SaveSilence(ctx, Silence{Name: "n", Kind: SilenceOnce, FromAt: 1, UntilAt: 2, NodeIDs: ids, SelectorTags: nil})
	if err != nil {
		t.Fatal(err)
	}
	si2, err := s.SaveSilence(ctx, Silence{Name: "m", Kind: SilenceOnce, FromAt: 1, UntilAt: 2, SelectorTags: []string{"db"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSilence(ctx, si.ID); err != nil {
		t.Fatal(err)
	}
	assertAlertRows(t, s, "silence", fmt.Sprint("id = ", si.ID), 0)
	for _, table := range []string{"silence_node", "silence_tag"} {
		assertAlertRows(t, s, table, fmt.Sprint("silence_id = ", si.ID), 0)
	}
	assertAlertRows(t, s, "silence_tag", fmt.Sprint("silence_id = ", si2.ID), 1)
	if err := s.DeleteSilence(ctx, si.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting a missing silence: %v", err)
	}
}

// 一次性静默到期后保留供审计，随告警事件的保留期清理：until_at 早于同一截止点才删，每日重复的永不清理。
func TestPruneAlertEventsRemovesOnlyExpiredOnceSilences(t *testing.T) {
	t.Parallel()
	s, ids, _, _ := alertFixture(t)
	ctx := t.Context()
	setTags(t, s, ids[0], "db")
	cutoff := s.clk.Now()
	save := func(si Silence) Silence {
		t.Helper()
		si, err := s.SaveSilence(ctx, si)
		if err != nil {
			t.Fatal(err)
		}
		return si
	}
	expired := save(Silence{Name: "expired", Kind: SilenceOnce, FromAt: 1, UntilAt: cutoff.Add(-time.Second).Unix(), NodeIDs: ids[:1]})
	expiredTag := save(Silence{Name: "expired-tag", Kind: SilenceOnce, FromAt: 1, UntilAt: cutoff.Add(-time.Second).Unix(), SelectorTags: []string{"db"}})
	onEdge := save(Silence{Name: "edge", Kind: SilenceOnce, FromAt: 1, UntilAt: cutoff.Unix(), NodeIDs: ids[:1]})
	future := save(Silence{Name: "future", Kind: SilenceOnce, FromAt: 1, UntilAt: cutoff.Add(time.Hour).Unix(), NodeIDs: ids[:1]})
	daily := save(Silence{Name: "daily", Kind: SilenceDaily, StartHHMM: "22:00", EndHHMM: "06:00", NodeIDs: ids[:1]})
	if _, err := s.PruneAlertEvents(ctx, cutoff); err != nil {
		t.Fatal(err)
	}
	assertAlertRows(t, s, "silence", fmt.Sprint("id = ", expired.ID), 0)
	assertAlertRows(t, s, "silence_node", fmt.Sprint("silence_id = ", expired.ID), 0)
	assertAlertRows(t, s, "silence_tag", fmt.Sprint("silence_id = ", expiredTag.ID), 0)
	got, err := s.ListSilences(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var kept []int64
	for _, si := range got {
		kept = append(kept, si.ID)
	}
	if !slices.Equal(kept, []int64{onEdge.ID, future.ID, daily.ID}) {
		t.Fatalf("kept=%v, want the edge/future once silences and the daily one", kept)
	}
}

// 被静默引用的标签不能删除：与探测任务、告警规则并列的第三种引用。
func TestDeleteTagRefusedWhileASilenceSelectsIt(t *testing.T) {
	t.Parallel()
	s, ids, _, _ := alertFixture(t)
	ctx := t.Context()
	setTags(t, s, ids[0], "db")
	si, err := s.SaveSilence(ctx, Silence{Name: "夜间维护", Kind: SilenceDaily, StartHHMM: "22:00", EndHHMM: "06:00", SelectorTags: []string{"db"}})
	if err != nil {
		t.Fatal(err)
	}
	err = s.DeleteTag(ctx, "db")
	if !errors.Is(err, ErrInUse) || !strings.Contains(err.Error(), `silence 1 (夜间维护)`) {
		t.Fatalf("referenced tag deletion=%v", err)
	}
	si.SelectorTags = nil
	si.NodeIDs = ids[:1]
	if _, err := s.SaveSilence(ctx, si); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTag(ctx, "db"); err != nil {
		t.Fatal(err)
	}
}

func TestParseHHMM(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		in   string
		want int
		ok   bool
	}{
		{"00:00", 0, true},
		{"09:05", 545, true},
		{"22:00", 1320, true},
		{"23:59", 1439, true},
		{"24:00", 0, false},
		{"22:60", 0, false},
		{"9:00", 0, false},
		{"2200", 0, false},
		{"22:0", 0, false},
		{"22:0a", 0, false},
		{"", 0, false},
	} {
		got, err := ParseHHMM(c.in)
		if (err == nil) != c.ok || (c.ok && got != c.want) {
			t.Errorf("ParseHHMM(%q) = %d, %v; want %d, ok=%v", c.in, got, err, c.want, c.ok)
		}
	}
}
