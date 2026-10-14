package metric

import (
	"slices"
	"testing"
)

func TestBatchWithoutNode(t *testing.T) {
	r1, r2, r3 := Row{NodeID: 1, TS: 60, Bucket: NewBucket()}, Row{NodeID: 2, TS: 120, Bucket: NewBucket()}, Row{NodeID: 3, TS: 180, Bucket: NewBucket()}
	p1, p2, p3 := ProbeRow{NodeID: 1, TS: 60, TaskID: 7, Bucket: &ProbeBucket{Sent: 1}}, ProbeRow{NodeID: 2, TS: 120, TaskID: 8, Bucket: &ProbeBucket{Sent: 2}}, ProbeRow{NodeID: 3, TS: 180, TaskID: 9, Bucket: &ProbeBucket{Sent: 3}}
	for _, tc := range []struct {
		name     string
		id       int64
		in, want Batch
	}{
		{"empty", 1, Batch{}, Batch{}},
		{"mixed", 1, Batch{Rows: []Row{r1, r2, r1, r3}, Probes: []ProbeRow{p1, p3, p1, p2}}, Batch{Rows: []Row{r2, r3}, Probes: []ProbeRow{p3, p2}}},
		{"metrics_only", 1, Batch{Rows: []Row{r1, r2}}, Batch{Rows: []Row{r2}}},
		{"probes_only", 1, Batch{Probes: []ProbeRow{p1, p2}}, Batch{Probes: []ProbeRow{p2}}},
		{"remove_all", 1, Batch{Rows: []Row{r1}, Probes: []ProbeRow{p1}}, Batch{}},
		{"absent", 1, Batch{Rows: []Row{r2, r3}, Probes: []ProbeRow{p3, p2}}, Batch{Rows: []Row{r2, r3}, Probes: []ProbeRow{p3, p2}}},
		{"other_node", 2, Batch{Rows: []Row{r1, r2, r3}, Probes: []ProbeRow{p2, p3, p1}}, Batch{Rows: []Row{r1, r3}, Probes: []ProbeRow{p3, p1}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := Batch{Rows: slices.Clone(tc.in.Rows), Probes: slices.Clone(tc.in.Probes)}
			got := tc.in.WithoutNode(tc.id)
			if !slices.Equal(got.Rows, tc.want.Rows) || !slices.Equal(got.Probes, tc.want.Probes) {
				t.Errorf("filtered batch=%+v want=%+v", got, tc.want)
			}
			if len(got.Rows) > 0 {
				got.Rows[0].NodeID = -1
			}
			if len(got.Probes) > 0 {
				got.Probes[0].NodeID = -1
			}
			if !slices.Equal(tc.in.Rows, before.Rows) || !slices.Equal(tc.in.Probes, before.Probes) {
				t.Errorf("source batch changed: got=%+v before=%+v", tc.in, before)
			}
		})
	}
}
