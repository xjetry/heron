package api

import (
	"strings"
	"testing"

	"connectrpc.com/connect"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
)

// 没有翻页游标或按"不满一页即到底"翻页的列表，超过上限时静默截断会让调用方以为已经拿全；
// 每个带 limit 的列表都必须拒绝超限并点名 limit 与上限，上限本身照常可用。
func TestListLimitsAboveMaximumAreRejectedNotTruncated(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	cases := []struct {
		name string
		call func(limit uint32) error
		max  uint32
	}{
		{"ListAlertEvents", func(limit uint32) error {
			_, err := h.admin.ListAlertEvents(t.Context(), connect.NewRequest(&heronv1.ListAlertEventsRequest{Limit: limit}))
			return err
		}, 500},
		{"ListOperations", func(limit uint32) error {
			_, err := h.admin.ListOperations(t.Context(), connect.NewRequest(&heronv1.ListOperationsRequest{Limit: limit}))
			return err
		}, 500},
		{"ListRestoreRecords", func(limit uint32) error {
			_, err := h.admin.ListRestoreRecords(t.Context(), connect.NewRequest(&heronv1.ListRestoreRecordsRequest{Limit: limit}))
			return err
		}, 100},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, ok := range []uint32{0, 1, c.max} {
				if err := c.call(ok); err != nil {
					t.Fatalf("limit %d: %v", ok, err)
				}
			}
			err := c.call(c.max + 1)
			if connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Fatalf("limit %d: code %v, err %v", c.max+1, connect.CodeOf(err), err)
			}
			for _, want := range []string{"limit", "got"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q lacks %q", err, want)
				}
			}
		})
	}
}
