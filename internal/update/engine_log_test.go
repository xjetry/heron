package update

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func discardLog() *slog.Logger { return slog.New(slog.DiscardHandler) }

// syncBuffer 收 JSON 日志：引擎从 worker 协程写，用例在主协程读。
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) records(t *testing.T) []map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	dec := json.NewDecoder(bytes.NewReader(s.b.Bytes()))
	for {
		var r map[string]any
		if err := dec.Decode(&r); err == io.EOF {
			return out
		} else if err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
}

func loggedEngine(t *testing.T, m *fakeMachine, choice sourceChoice) (*Engine, *syncBuffer) {
	t.Helper()
	buf := &syncBuffer{}
	e, err := newEngine(context.Background(), slog.New(slog.NewJSONHandler(buf, nil)), filepath.Join(t.TempDir(), "state.json"), "hub", "amd64", choice, testKeys(), m)
	if err != nil {
		t.Fatal(err)
	}
	e.readyTimeout = time.Second
	return e, buf
}

// states 按顺序取出某任务的状态迁移；每条都要带任务 ID 与版本。
func states(t *testing.T, recs []map[string]any, r Request) []string {
	t.Helper()
	var out []string
	for _, rec := range recs {
		if rec["msg"] != "update state" {
			continue
		}
		if rec["task"] != r.ID || rec["version"] != r.Version {
			t.Fatalf("state record without the task ID and version: %v", rec)
		}
		out = append(out, rec["state"].(string))
	}
	return out
}

func record(recs []map[string]any, msg string) map[string]any {
	for _, rec := range recs {
		if rec["msg"] == msg {
			return rec
		}
	}
	return nil
}

func TestEngineLogsSuccessfulUpdate(t *testing.T) {
	m := &fakeMachine{version: "v0.2.0"}
	src := goodSource()
	e, buf := loggedEngine(t, m, githubChoice(src))
	r := request()
	if _, err := e.submit(r); err != nil {
		t.Fatal(err)
	}
	for e.status().Job.State != "verifying" {
		time.Sleep(time.Millisecond)
	}
	if err := e.ready(context.Background(), 42, r.Version); err != nil {
		t.Fatal(err)
	}
	waitEngine(t, e)
	recs := buf.records(t)
	if got, want := states(t, recs, r), []string{"queued", "downloading", "stopping", "installing", "verifying", "succeeded"}; !slices.Equal(got, want) {
		t.Fatalf("logged states %v, want %v", got, want)
	}
	done := record(recs, "update download complete")
	size := len(src.a.Sums) + len(src.a.Signature) + len(src.a.Archive)
	if done == nil || done["bytes"] != float64(size) || done["source"] != "github" || done["elapsed"] == nil || done["bytes_per_second"] == nil {
		t.Fatalf("download record = %v, want %d bytes with elapsed time and rate", done, size)
	}
	if rec := record(recs, "update failed"); rec != nil {
		t.Fatalf("successful update logged a failure: %v", rec)
	}
}

// 失败原因与 Job.Error 是同一段文字；回滚经过的每个状态与恢复步骤都有行。
func TestEngineLogsFailureReasonAndRollback(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fail   string
		source fakeSource
		states []string
		steps  []string
	}{
		{"download", "", fakeSource{err: errors.New("read official release: no data for 60s after 2.1 MiB of 9.8 MiB in 5m2s")},
			[]string{"queued", "downloading", "failed"}, nil},
		{"install", "install", goodSource(),
			[]string{"queued", "downloading", "stopping", "installing", "rolling_back", "rolled_back"},
			[]string{"update backup restored", "update original service restarted"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, buf := loggedEngine(t, &fakeMachine{version: "v0.2.0", fail: tc.fail}, githubChoice(tc.source))
			r := request()
			if _, err := e.submit(r); err != nil {
				t.Fatal(err)
			}
			j := waitEngine(t, e)
			recs := buf.records(t)
			if got := states(t, recs, r); !slices.Equal(got, tc.states) {
				t.Fatalf("logged states %v, want %v", got, tc.states)
			}
			failed := record(recs, "update failed")
			if failed == nil || failed["error"] != j.Error || failed["task"] != r.ID || failed["level"] != "ERROR" {
				t.Fatalf("failure record = %v, want error %q", failed, j.Error)
			}
			if tc.source.err != nil && !strings.Contains(j.Error, tc.source.err.Error()) {
				t.Fatalf("job error %q lost the source error", j.Error)
			}
			for _, step := range tc.steps {
				if record(recs, step) == nil {
					t.Errorf("missing %q in %v", step, recs)
				}
			}
		})
	}
}

// 启动时恢复被中断的任务也要留下行：journal 里看得到重启前停在哪个状态、恢复走到了哪。
func TestEngineLogsInterruptedRecovery(t *testing.T) {
	m := &fakeMachine{version: "v0.2.0"}
	e, _ := loggedEngine(t, m, githubChoice(fakeSource{}))
	r := request()
	e.mu.Lock()
	if err := e.save(Job{Request: r, State: "installing", Digest: "digest"}); err != nil {
		t.Fatal(err)
	}
	e.mu.Unlock()
	buf := &syncBuffer{}
	if _, err := newEngine(context.Background(), slog.New(slog.NewJSONHandler(buf, nil)), e.path, "hub", "amd64", githubChoice(fakeSource{}), testKeys(), m); err != nil {
		t.Fatal(err)
	}
	recs := buf.records(t)
	if rec := record(recs, "update interrupted; recovering"); rec == nil || rec["state"] != "installing" {
		t.Fatalf("recovery record = %v", rec)
	}
	if got, want := states(t, recs, r), []string{"rolling_back", "rolled_back"}; !slices.Equal(got, want) {
		t.Fatalf("logged states %v, want %v", got, want)
	}
}

func TestNewEngineRequiresLogger(t *testing.T) {
	if _, err := newEngine(context.Background(), nil, filepath.Join(t.TempDir(), "state.json"), "hub", "amd64", githubChoice(fakeSource{}), testKeys(), &fakeMachine{}); err == nil {
		t.Fatal("engine built without a logger")
	}
}
