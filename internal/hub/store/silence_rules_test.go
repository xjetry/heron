package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSilenceRuleValidation(t *testing.T) {
	s, _, _, _ := alertFixture(t)
	r := saveRule(t, s, AlertRule{Name: "offline", Kind: KindOffline})
	for _, tc := range []struct {
		name string
		all  bool
		ids  []int64
		want string
	}{
		{"missing", false, nil, "all_rules"},
		{"both", true, []int64{r.ID}, "rule_ids"},
		{"unknown", false, []int64{999}, "rule 999 does not exist"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.SaveSilence(t.Context(), Silence{Name: "test", AllNodes: true, AllRules: tc.all, RuleIDs: tc.ids, Kind: SilenceOnce, FromAt: 1, UntilAt: 2})
			var field KindFieldError
			if !errors.As(err, &field) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v, want field error containing %q", err, tc.want)
			}
		})
	}
	for _, kind := range []AlertKind{KindExpiry, KindCertExpiry} {
		t.Run(string(kind), func(t *testing.T) {
			// 只构造类型引用，任务关联的合法性由告警规则自己的保存测试覆盖。
			var id int64
			if err := s.w.QueryRow("INSERT INTO alert_rule (name, kind, created_at) VALUES (?, ?, ?) RETURNING id", string(kind), kind, s.clk.Now().Unix()).Scan(&id); err != nil {
				t.Fatal(err)
			}
			_, err := s.SaveSilence(t.Context(), Silence{Name: "test", AllNodes: true, RuleIDs: []int64{id}, Kind: SilenceOnce, FromAt: 1, UntilAt: 2})
			var field KindFieldError
			if !errors.As(err, &field) || field.Field != "rule_ids" || !strings.Contains(err.Error(), fmt.Sprintf("rule %d is a calendar rule (%s)", id, kind)) {
				t.Fatalf("calendar error=%v", err)
			}
		})
	}
}

func TestSilenceRulesRoundTripReplaceAndDelete(t *testing.T) {
	s, _, _, _ := alertFixture(t)
	a := saveRule(t, s, AlertRule{Name: "a", Kind: KindOffline})
	b := saveRule(t, s, AlertRule{Name: "b", Kind: KindOffline})
	si, err := s.SaveSilence(t.Context(), Silence{Name: "test", AllNodes: true, RuleIDs: []int64{b.ID, a.ID, b.ID}, Kind: SilenceOnce, FromAt: 1, UntilAt: 2})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListSilences(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if si.AllRules || !slices.Equal(si.RuleIDs, []int64{a.ID, b.ID}) || !reflect.DeepEqual(rows, []Silence{si}) {
		t.Fatalf("rule round trip: saved=%+v listed=%+v", si, rows)
	}
	si.RuleIDs = []int64{b.ID}
	si, err = s.SaveSilence(t.Context(), si)
	if err != nil {
		t.Fatal(err)
	}
	assertAlertRows(t, s, "silence_rule", fmt.Sprintf("silence_id = %d AND rule_id = %d", si.ID, a.ID), 0)
	if err := s.DeleteAlertRule(t.Context(), b.ID); err != nil {
		t.Fatal(err)
	}
	assertAlertRows(t, s, "silence_rule", fmt.Sprintf("rule_id = %d", b.ID), 0)
	rows, err = s.ListSilences(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].AllRules || len(rows[0].RuleIDs) != 0 {
		t.Fatalf("deleted rule must leave empty explicit scope: %+v", rows)
	}
	si.AllRules, si.RuleIDs = true, nil
	si, err = s.SaveSilence(t.Context(), si)
	if err != nil {
		t.Fatal(err)
	}
	rows, err = s.ListSilences(t.Context())
	if err != nil || !rows[0].AllRules || len(rows[0].RuleIDs) != 0 {
		t.Fatalf("all rules round trip: %+v %v", rows, err)
	}
}

func TestSilenceRuleCleanup(t *testing.T) {
	for _, prune := range []bool{false, true} {
		t.Run(fmt.Sprint(prune), func(t *testing.T) {
			s, _, _, _ := alertFixture(t)
			r := saveRule(t, s, AlertRule{Name: "a", Kind: KindOffline})
			si, err := s.SaveSilence(t.Context(), Silence{Name: "test", AllNodes: true, RuleIDs: []int64{r.ID}, Kind: SilenceOnce, FromAt: 1, UntilAt: 2})
			if err != nil {
				t.Fatal(err)
			}
			assertAlertRows(t, s, "silence_rule", fmt.Sprintf("silence_id = %d", si.ID), 1)
			if prune {
				_, err = s.PruneAlertEvents(t.Context(), time.Unix(3, 0))
			} else {
				err = s.DeleteSilence(t.Context(), si.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			assertAlertRows(t, s, "silence_rule", fmt.Sprintf("silence_id = %d", si.ID), 0)
		})
	}
}

func TestSilenceRulesSnapshotRestore(t *testing.T) {
	s, _, _, _ := alertFixture(t)
	r := saveRule(t, s, AlertRule{Name: "a", Kind: KindOffline})
	var want []Silence
	for _, si := range []Silence{
		{Name: "explicit", AllNodes: true, RuleIDs: []int64{r.ID}, Kind: SilenceOnce, FromAt: 1, UntilAt: 2},
		{Name: "all", AllNodes: true, AllRules: true, Kind: SilenceOnce, FromAt: 1, UntilAt: 2},
	} {
		saved, err := s.SaveSilence(t.Context(), si)
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, saved)
	}
	config := filepath.Join(t.TempDir(), "config.db")
	if err := s.SnapshotConfig(t.Context(), config); err != nil {
		t.Fatal(err)
	}
	target, _ := open(t)
	if err := target.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(t.Context(), target.path, config, "", "", s.clk.Now(), s.log); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(target.path, s.clk, s.log, RequireCurrentSchema)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	got, err := restored.ListSilences(t.Context())
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("restored=%+v want=%+v err=%v", got, want, err)
	}
}
