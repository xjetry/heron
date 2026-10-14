package main

import (
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/alert"
	"github.com/xjetry/heron/internal/hub/live"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/hub/traffic"
)

func TestTrafficStartupLoadsBeforeEvaluating(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC))
	s, err := store.Open(filepath.Join(t.TempDir(), "hub.db"), clk, slog.Default(), store.MigrateSchema)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id, _, err := s.CreateNode(t.Context(), store.NewNode{Name: "quota"}, []byte("quota"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateNode(t.Context(), id, store.NodeEdit{Name: "quota", TrafficResetDay: 1, TrafficQuotaBytes: 100, TrafficQuotaMode: "sum"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteTraffic(t.Context(), []store.TrafficRecord{{NodeID: id, PeriodRx: 90, TotalRx: 90, PeriodStart: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveAlertRule(t.Context(), store.AlertRule{Name: "quota", Kind: store.KindTraffic, Enabled: true, AllNodes: true, Threshold: 80}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		book := traffic.New(s, clk, time.UTC, slog.Default())
		engine := alert.New(alert.Config{TTL: 30 * time.Second, Location: time.UTC}, s, s.Evaluation(), live.New(clk, 30*time.Second), clk, slog.Default())
		engine.SetTraffic(book)
		if err := loadTrafficAlerts(t.Context(), book, engine); err != nil {
			t.Fatal(err)
		}
		events, err := s.ListAlertEvents(t.Context(), 0, 0, 10)
		if err != nil || len(events) != 1 || events[0].Transition != store.TransitionFiring || events[0].Value != 90 {
			t.Fatalf("startup %d: events=%+v err=%v", i, events, err)
		}
	}
}
