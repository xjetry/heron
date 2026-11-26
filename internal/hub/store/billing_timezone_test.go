package store

import (
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

// 重置时刻的范围 [0, 1439]：两端可写，越界被拒且一行都不改；新节点取列默认值 0（00:00）。
func TestTrafficResetMinuteAdmission(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	id, _, err := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	minute := func() int {
		t.Helper()
		n, err := s.GetNode(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return n.TrafficResetMinute
	}
	if got := minute(); got != 0 {
		t.Fatalf("new node traffic_reset_minute = %d, want 0", got)
	}
	for _, m := range []int{0, MaxTrafficResetMinute} {
		if _, err := s.UpdateNodeTasks(ctx, id, NodeEdit{Name: "n", TrafficResetDay: 1, TrafficResetMinute: m}); err != nil || minute() != m {
			t.Fatalf("UpdateNodeTasks(minute %d) err = %v, stored %d", m, err, minute())
		}
	}
	for _, m := range []int{-1, MaxTrafficResetMinute + 1} {
		_, err := s.UpdateNodeTasks(ctx, id, NodeEdit{Name: "renamed", TrafficResetDay: 1, TrafficResetMinute: m})
		var rangeErr TrafficResetMinuteError
		want := fmt.Sprintf("traffic_reset_minute must be between 0 and 1439 (minutes after local midnight in billing_timezone, 00:00–23:59, e.g. 510 for 08:30); got %d", m)
		if !errors.As(err, &rangeErr) || rangeErr.Got != m || err.Error() != want {
			t.Fatalf("UpdateNodeTasks(minute %d) err = %v, want %q", m, err, want)
		}
		if n, err := s.GetNode(ctx, id); err != nil || n.Name != "n" || n.TrafficResetMinute != MaxTrafficResetMinute {
			t.Fatalf("a rejected update wrote: %+v %v", n, err)
		}
	}
}

// 计费时区：空串（沿用 hub 时区）与 hub 能加载的 IANA 名可写；Local、编码目录前缀、加载不了的名字被拒，错误点名字段与
// 期望取值，一行都不改。两个写入口（UpdateNodeTasks、insertNode）同一判定。
func TestBillingTimezoneAdmission(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	id, _, err := s.CreateNode(ctx, NewNode{Name: "n", BillingTimezone: "America/New_York"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	zone := func() string {
		t.Helper()
		n, err := s.GetNode(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return n.BillingTimezone
	}
	if got := zone(); got != "America/New_York" {
		t.Fatalf("created billing_timezone = %q, want America/New_York", got)
	}
	for _, name := range []string{"", "UTC", "Asia/Kolkata"} {
		if _, err := s.UpdateNodeTasks(ctx, id, NodeEdit{Name: "n", TrafficResetDay: 1, BillingTimezone: name}); err != nil || zone() != name {
			t.Fatalf("UpdateNodeTasks(zone %q) err = %v, stored %q", name, err, zone())
		}
	}
	for _, name := range []string{"Local", "Mars/Olympus", "posix/Asia/Shanghai", "right/UTC", "../etc/passwd"} {
		want := fmt.Sprintf("billing_timezone must be empty (use the hub's --timezone) or an IANA time zone name such as Asia/Shanghai or UTC; got %q", name)
		_, err := s.UpdateNodeTasks(ctx, id, NodeEdit{Name: "renamed", TrafficResetDay: 1, BillingTimezone: name})
		var zoneErr BillingTimezoneError
		if !errors.As(err, &zoneErr) || zoneErr.Got != name || err.Error() != want {
			t.Fatalf("UpdateNodeTasks(zone %q) err = %v, want %q", name, err, want)
		}
		if n, err := s.GetNode(ctx, id); err != nil || n.Name != "n" || n.BillingTimezone != "Asia/Kolkata" {
			t.Fatalf("a rejected update wrote: %+v %v", n, err)
		}
		before := nodeCount(t, s)
		_, _, err = s.CreateNode(ctx, NewNode{Name: "other", BillingTimezone: name}, hash(2))
		if !errors.As(err, &zoneErr) || err.Error() != want {
			t.Fatalf("CreateNode(zone %q) err = %v, want %q", name, err, want)
		}
		if after := nodeCount(t, s); after != before {
			t.Fatalf("a rejected create left %d nodes, want %d", after, before)
		}
	}
	// 空串不是时区名：它的含义（沿用 hub 时区）由调用方先行处理，LoadBillingZone 不把它当成 UTC。
	if _, err := LoadBillingZone(""); err == nil {
		t.Fatal(`LoadBillingZone("") loaded a zone; an empty billing time zone means the hub's zone, not UTC`)
	}
}

// 计费时区变了算计费变化（到期日界跟着变，nodeops 据它立刻重扫到期），也算流量变化（周期规则变了，提交并重评流量告警）；
// 只改重置时刻只算流量变化。
func TestUpdateNodeReportsScheduleChanges(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	id, _, err := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name                    string
		edit                    NodeEdit
		billing, trafficChanged bool
	}{
		{"unchanged", NodeEdit{Name: "n", TrafficResetDay: 1}, false, false},
		{"reset minute", NodeEdit{Name: "n", TrafficResetDay: 1, TrafficResetMinute: 30}, false, true},
		{"billing time zone", NodeEdit{Name: "n", TrafficResetDay: 1, TrafficResetMinute: 30, BillingTimezone: "UTC"}, true, true},
		{"same again", NodeEdit{Name: "n", TrafficResetDay: 1, TrafficResetMinute: 30, BillingTimezone: "UTC"}, false, false},
	} {
		res, err := s.UpdateNodeTasks(ctx, id, c.edit)
		if err != nil {
			t.Fatal(err)
		}
		if res.BillingChanged != c.billing || res.TrafficChanged != c.trafficChanged {
			t.Errorf("%s: BillingChanged=%v TrafficChanged=%v, want %v %v", c.name, res.BillingChanged, res.TrafficChanged, c.billing, c.trafficChanged)
		}
	}
}

// 当前版本快照里绕过写入口改出的非法重置时刻与计费时区在恢复预检被拒：快照不经写入口，预检是它进库前唯一的检查。
func TestRestoreRejectsIllegalSchedule(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, set, want string
	}{
		{"reset minute", "traffic_reset_minute = 1440", "traffic_reset_minute must be between 0 and 1439"},
		{"billing time zone", "billing_timezone = 'Local'", `billing_timezone must be empty (use the hub's --timezone) or an IANA time zone name such as Asia/Shanghai or UTC; got "Local"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			source, _ := open(t)
			id, _, err := source.CreateNode(t.Context(), NewNode{Name: "ok"}, hash(1))
			if err != nil {
				t.Fatal(err)
			}
			mustExec(t, source, "UPDATE node SET "+c.set+" WHERE id = ?", id)
			config := filepath.Join(t.TempDir(), "config.db")
			if err := source.SnapshotConfig(t.Context(), config); err != nil {
				t.Fatal(err)
			}
			source.Close()
			target := filepath.Join(t.TempDir(), "target.db")
			_, err = Restore(t.Context(), target, config, "", "", time.Now(), slog.Default())
			prefix := fmt.Sprintf("config node %d (\"ok\"): ", id)
			if err == nil || err.Error()[:min(len(err.Error()), len(prefix)+len(c.want))] != prefix+c.want {
				t.Fatalf("err = %v, want it to start with %q", err, prefix+c.want)
			}
		})
	}
}
