package main

import (
	"context"
	"testing"
	"time"
)

// time 在小库上跑通全部操作与两种条件，并核对被测操作确实干了活：Rollup 的写入行数由 checkRollup 在每次重复后核对
// （不符即报错），这里核对 Prune 删掉的行数、挂钩子时记到的删除块数，以及并发写入没有被拒、没有出错。
func TestTimeRunsEveryOperation(t *testing.T) {
	path, m := generated(t)
	cfg := timeConfig{db: path, work: t.TempDir(), reps: 1, conditions: []string{"idle", "writes"}, ops: []string{"rollup", "query", "prune"},
		writePeriod: 50 * time.Millisecond, pruneAdvance: 24 * time.Hour}
	rep, err := timeOps(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	nodes, tasks := int64(len(m.NodeIDs)), int64(len(m.TaskIDs))
	// 拨快一天后每节点每级过期的行：1m 1440、5m 288、1h 24；探测表另乘任务数。每节点每表一个删除块（一天落在一片里）。
	wantDeleted := nodes * (1440 + 288 + 24) * (1 + tasks)
	wantChunks := int(nodes) * 6
	seen := map[string]int{}
	for _, r := range rep.Results {
		seen[r.Condition+"/"+r.Op]++
		if len(r.Samples) != cfg.reps {
			t.Errorf("%s %s %s %s: %d samples, want %d", r.Condition, r.Op, r.Family, r.Window, len(r.Samples), cfg.reps)
		}
		switch r.Op {
		case "rollup":
			if r.Samples[0].Detail["metric_5m_rows_written"] != nodes || r.Samples[0].Detail["probe_1h_rows_written"] != nodes*tasks {
				t.Errorf("%s rollup detail %v", r.Condition, r.Samples[0].Detail)
			}
		case "prune":
			if got := r.Samples[0].Detail["deleted_rows"]; got != wantDeleted {
				t.Errorf("%s prune (hooks %v) deleted %v rows, want %d", r.Condition, r.Hooks, got, wantDeleted)
			}
			if r.Hooks {
				if got := r.Samples[0].Detail["chunks"]; got != wantChunks {
					t.Errorf("%s prune recorded %v delete chunks, want %d", r.Condition, got, wantChunks)
				}
				if r.HoldMs == nil || r.HoldMs.N != wantChunks || len(r.HoldByTable) != 6 {
					t.Errorf("%s prune holds %+v by table %v", r.Condition, r.HoldMs, r.HoldByTable)
				}
				if (r.CommitGapMs != nil) != (r.Condition == "idle") {
					t.Errorf("%s prune commit gaps %+v: given only when Prune is the only writer", r.Condition, r.CommitGapMs)
				}
			}
		case "query":
			if p, _ := r.Samples[0].Detail["points"].(int); p == 0 {
				t.Errorf("%s query %s %s returned no points", r.Condition, r.Family, r.Window)
			}
		}
		if r.Condition == "writes" && r.Samples[0].Overlapped == nil {
			t.Errorf("writes %s %s: overlap not reported", r.Op, r.Family)
		}
	}
	// 每种条件：1 个上卷、4 个窗口 × 3 族查询、挂与不挂钩子的 2 组清理。
	for _, cond := range cfg.conditions {
		if seen[cond+"/rollup"] != 1 || seen[cond+"/query"] != 12 || seen[cond+"/prune"] != 2 {
			t.Errorf("%s results: %v", cond, seen)
		}
	}
	if len(rep.Writes) != 3 {
		t.Fatalf("%d write summaries, want one per op in the writes condition", len(rep.Writes))
	}
	for _, w := range rep.Writes {
		if w.Batches == 0 || w.Rejected != 0 || w.Errors != 0 {
			t.Errorf("writer during %s: %+v", w.Op, w)
		}
	}
	if rep.WriteRate.Multiplier != 1200 {
		t.Errorf("write rate multiplier %v, want 1200 for a 50ms period", rep.WriteRate.Multiplier)
	}
}
