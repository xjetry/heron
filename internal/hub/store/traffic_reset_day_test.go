package store

import (
	"errors"
	"strconv"
	"testing"
)

// 重置日的取值范围在写入口裁决：UpdateNodeTasks（UpdateNode 与 ExecuteChange 都经它）拒绝 [1, 31] 以外的值并且
// 什么都不写，边界值照常写入；建节点不写这一列，取列默认值，它也必须落在范围内。
func TestTrafficResetDayAdmission(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	id, _, err := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	day := func() int {
		t.Helper()
		var d int
		if err := s.r.QueryRow("SELECT traffic_reset_day FROM node WHERE id = ?", id).Scan(&d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	if got := day(); got != DefaultTrafficResetDay {
		t.Fatalf("new node traffic_reset_day = %d, want the default %d", got, DefaultTrafficResetDay)
	}
	for _, d := range []int{MinTrafficResetDay, MaxTrafficResetDay} {
		if _, err := s.UpdateNodeTasks(ctx, id, NodeEdit{Name: "n", TrafficResetDay: d}); err != nil || day() != d {
			t.Fatalf("UpdateNodeTasks(day %d) err = %v, stored %d", d, err, day())
		}
	}
	for _, d := range []int{0, -1, 32} {
		_, err := s.UpdateNodeTasks(ctx, id, NodeEdit{Name: "renamed", TrafficResetDay: d})
		var rangeErr TrafficResetDayError
		if !errors.As(err, &rangeErr) || rangeErr.Got != d || err.Error() != "traffic_reset_day must be between 1 and 31; got "+strconv.Itoa(d) {
			t.Fatalf("UpdateNodeTasks(day %d) err = %v, want TrafficResetDayError", d, err)
		}
		if n, err := s.GetNode(ctx, id); err != nil || n.Name != "n" || day() != MaxTrafficResetDay {
			t.Fatalf("a rejected update wrote: name=%q day=%d err=%v", n.Name, day(), err)
		}
	}
}
