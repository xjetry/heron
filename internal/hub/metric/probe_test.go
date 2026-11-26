package metric

import "testing"

// 桶的丢包率是告警与面板共用的口径：分母是有效测量数（Sent − Errors），探测错误不稀释丢包；
// 全是错误（含 Sent 为 0）时没有读数。
func TestProbeBucketLossPct(t *testing.T) {
	for _, tc := range []struct {
		name string
		b    ProbeBucket
		want float64
		ok   bool
	}{
		{"empty", ProbeBucket{}, 0, false},
		{"all_errors", ProbeBucket{Sent: 4, Errors: 4}, 0, false},
		{"all_lost", ProbeBucket{Sent: 4, Lost: 4}, 100, true},
		{"mixed", ProbeBucket{Sent: 4, Lost: 1, Errors: 2, RttN: 1}, 50, true},
		{"errors_do_not_dilute", ProbeBucket{Sent: 10, Lost: 2, Errors: 8}, 100, true},
		{"clean", ProbeBucket{Sent: 4, RttN: 4}, 0, true},
	} {
		got, ok := tc.b.LossPct()
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: LossPct = %v, %v; want %v, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}
