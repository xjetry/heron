package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestMaintenanceRejectsZeroRetentionBeforeDeletingEvents(t *testing.T) {
	t.Parallel()
	s, ids, _, _ := alertFixture(t)
	r := saveRule(t, s, AlertRule{Name: "offline", Kind: KindOffline, AllNodes: true})
	ev := recordEvent(t, s, r.ID, ids[0], nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var caught any
	func() {
		defer func() { caught = recover() }()
		s.RunMaintenance(ctx, Retention{})
	}()
	if caught == nil || !strings.Contains(fmt.Sprint(caught), "alert event retention") {
		t.Errorf("invalid retention must panic before maintenance: %v", caught)
	}
	rows, err := s.ListAlertEvents(t.Context(), 0, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != ev.ID {
		t.Fatalf("invalid retention deleted events: %+v", rows)
	}
}
