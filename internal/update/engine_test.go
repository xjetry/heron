package update

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/releasesig/sigtest"
)

type fakeMachine struct {
	mu      sync.Mutex
	calls   []string
	fail    string
	version string
}

func (m *fakeMachine) action(s string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, s)
	if m.fail == s {
		return errors.New(s + " failed")
	}
	return nil
}
func (m *fakeMachine) Current(context.Context) (string, error) { return m.version, nil }
func (m *fakeMachine) Stage([]byte) (string, error)            { return "digest", m.action("stage") }
func (m *fakeMachine) Stop(context.Context) error              { return m.action("stop") }
func (m *fakeMachine) Backup() error                           { return m.action("backup") }
func (m *fakeMachine) Install() error                          { return m.action("install") }
func (m *fakeMachine) Start(context.Context) error             { return m.action("start") }
func (m *fakeMachine) Verify(_ context.Context, pid int, digest string) error {
	if pid != 42 || digest != "digest" {
		return errors.New("wrong process")
	}
	return m.action("verify")
}
func (m *fakeMachine) Restore() error { return m.action("restore") }
func (m *fakeMachine) Gate(v bool) error {
	if v {
		return m.action("close")
	}
	return m.action("open")
}

type fakeSource struct {
	err error
	a   Artifacts
}

func (s fakeSource) Fetch(context.Context, Request, string, string) (Artifacts, error) {
	return s.a, s.err
}

// goodSource 给出 request() 版本的 hub amd64 合法签名产物。
func goodSource() fakeSource {
	return fakeSource{a: signedArtifacts("hub", "amd64", "v0.3.0", sigtest.Archive("hub", []byte("binary")))}
}

func testEngine(t *testing.T, m *fakeMachine, s fakeSource) *Engine {
	t.Helper()
	return testEngineWith(t, m, githubChoice(s))
}

func testEngineWith(t *testing.T, m *fakeMachine, choice sourceChoice) *Engine {
	t.Helper()
	e, err := newEngine(context.Background(), discardLog(), filepath.Join(t.TempDir(), "state.json"), "hub", "amd64", choice, testKeys(), m)
	if err != nil {
		t.Fatal(err)
	}
	e.readyTimeout = 30 * time.Millisecond
	return e
}
func request() Request {
	return Request{ID: "0123456789abcdef", Version: "v0.3.0", ExpiresAt: time.Now().Add(time.Hour).Unix()}
}
func waitEngine(t *testing.T, e *Engine) *Job {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		e.mu.Lock()
		busy := e.working
		e.mu.Unlock()
		if !busy {
			return e.status().Job
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("worker did not finish")
	return nil
}

func TestEngineFailureRecovery(t *testing.T) {
	for _, tc := range []struct {
		name, fail, state string
		download          bool
		restore           bool
	}{
		{"download", "", "failed", true, false},
		{"stage", "stage", "failed", false, false},
		{"backup", "backup", "failed", false, false},
		{"install", "install", "rolled_back", false, true},
		{"readiness timeout", "", "rolled_back", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &fakeMachine{version: "v0.2.0", fail: tc.fail}
			s := goodSource()
			if tc.download {
				s.err = errors.New("download failed")
			}
			e := testEngine(t, m, s)
			if _, err := e.submit(request()); err != nil {
				t.Fatal(err)
			}
			j := waitEngine(t, e)
			if j.State != tc.state || j.Error == "" {
				t.Fatalf("job=%+v", j)
			}
			m.mu.Lock()
			defer m.mu.Unlock()
			restored := false
			stopped := false
			for _, c := range m.calls {
				restored = restored || c == "restore"
				stopped = stopped || c == "stop"
			}
			if restored != tc.restore {
				t.Fatalf("restore=%v calls=%v", restored, m.calls)
			}
			if tc.download && stopped {
				t.Fatal("download failure stopped original service")
			}
		})
	}
}

func TestEngineCommitRequiresActualProcess(t *testing.T) {
	m := &fakeMachine{version: "v0.2.0"}
	e := testEngine(t, m, goodSource())
	e.reason = "service was previously stopped"
	e.readyTimeout = time.Second
	r := request()
	if _, err := e.submit(r); err != nil {
		t.Fatal(err)
	}
	for e.status().Job.State != "verifying" {
		time.Sleep(time.Millisecond)
	}
	if !e.status().Supported {
		t.Fatal("successful submit retained stale unavailable reason")
	}
	if err := e.ready(context.Background(), 41, r.Version); err == nil {
		t.Fatal("accepted wrong PID")
	}
	if err := e.ready(context.Background(), 42, "v0.9.0"); err == nil {
		t.Fatal("accepted wrong version")
	}
	if err := e.ready(context.Background(), 42, r.Version); err != nil {
		t.Fatal(err)
	}
	if j := waitEngine(t, e); j.State != "succeeded" {
		t.Fatalf("job=%+v", j)
	}
	if _, err := e.submit(r); err != nil {
		t.Fatal("idempotent task rejected", err)
	}
	b, err := os.ReadFile(e.path)
	if err != nil || len(b) == 0 {
		t.Fatalf("journal missing: %v", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.calls {
		if c == "restore" {
			t.Fatal("committed update rolled back")
		}
	}
}

func TestEngineInterruptedRecovery(t *testing.T) {
	for _, state := range []string{"queued", "downloading", "stopping", "installing", "verifying", "rolling_back", "succeeded"} {
		t.Run(state, func(t *testing.T) {
			m := &fakeMachine{version: "v0.2.0"}
			e := testEngine(t, m, fakeSource{})
			if err := e.save(Job{Request: request(), State: state}); err != nil {
				t.Fatal(err)
			}
			r, err := newEngine(context.Background(), discardLog(), e.path, "hub", "amd64", githubChoice(fakeSource{}), testKeys(), m)
			if err != nil {
				t.Fatal(err)
			}
			j := r.status().Job
			if j.Active() {
				t.Fatalf("interrupted task remains active: %+v", j)
			}
			if (state == "installing" || state == "verifying" || state == "rolling_back") && j.State != "rolled_back" {
				t.Fatalf("expected rollback: %+v", j)
			}
			if state == "succeeded" && j.State != "succeeded" {
				t.Fatal("committed journal was rolled back")
			}
		})
	}
}

func TestFailedRollbackCannotConfirmCandidate(t *testing.T) {
	m := &fakeMachine{version: "v0.2.0", fail: "restore"}
	e := testEngine(t, m, goodSource())
	if _, err := e.submit(request()); err != nil {
		t.Fatal(err)
	}
	waitEngine(t, e)
	if err := e.ready(context.Background(), 42, request().Version); err == nil {
		t.Fatal("candidate committed after failed database restoration")
	}
	if e.status().Job.State != "rolling_back" {
		t.Fatal("rollback intent was not durable")
	}
	m.fail = ""
	restarted, err := newEngine(context.Background(), discardLog(), e.path, "hub", "amd64", githubChoice(fakeSource{}), testKeys(), m)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.status().Job.State != "rolled_back" {
		t.Fatal("restart did not resume failed rollback")
	}
}

func TestRequestValidation(t *testing.T) {
	now := time.Now()
	r := request()
	if err := r.Validate(now); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []Request{{ID: "x", Version: r.Version, ExpiresAt: r.ExpiresAt}, {ID: r.ID, Version: "v0.3.0-rc1", ExpiresAt: r.ExpiresAt}, {ID: r.ID, Version: r.Version, ExpiresAt: now.Unix()}, {ID: r.ID, Version: r.Version, ExpiresAt: now.Add(25 * time.Hour).Unix()}} {
		if bad.Validate(now) == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
}

func TestMaintenanceSerializesWithSubmit(t *testing.T) {
	e := testEngine(t, &fakeMachine{version: "v0.2.0"}, fakeSource{})
	if err := e.enterMaintenance(); err != nil {
		t.Fatal(err)
	}
	if err := e.enterMaintenance(); err == nil {
		t.Fatal("two installers reserved the updater")
	}
	if _, err := e.submit(request()); err == nil {
		t.Fatal("accepted task during maintenance")
	}
	e.maintenance = false
	e.job = &Job{Request: request(), State: "verifying"}
	if err := e.enterMaintenance(); err == nil {
		t.Fatal("maintenance interrupted active task")
	}
}

func TestConsumedTaskCannotReplayAfterAnotherFailure(t *testing.T) {
	m := &fakeMachine{version: "v0.2.0"}
	e := testEngine(t, m, fakeSource{err: errors.New("unavailable")})
	r := request()
	if _, err := e.submit(r); err != nil {
		t.Fatal(err)
	}
	waitEngine(t, e)
	second := r
	second.ID = "another_task_1234"
	if _, err := e.submit(second); err != nil {
		t.Fatal(err)
	}
	waitEngine(t, e)
	reloaded, err := newEngine(context.Background(), discardLog(), e.path, "hub", "amd64", githubChoice(fakeSource{}), testKeys(), m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reloaded.submit(r); err == nil {
		waitEngine(t, reloaded)
		t.Fatal("consumed task replayed after restart")
	}
}

func TestRecoveryStartFailureDoesNotRestoreTwice(t *testing.T) {
	m := &fakeMachine{version: "v0.2.0", fail: "start"}
	e := testEngine(t, m, fakeSource{})
	if err := e.save(Job{Request: request(), State: "verifying"}); err != nil {
		t.Fatal(err)
	}
	if err := e.recover(); err == nil {
		t.Fatal("expected restart failure")
	}
	m.fail = ""
	if _, err := newEngine(context.Background(), discardLog(), e.path, "hub", "amd64", githubChoice(fakeSource{}), testKeys(), m); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, c := range m.calls {
		if c == "restore" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("restored %d times; a committed recovery must only retry starting", count)
	}
}

func TestIncompleteRecoveryRejectsNewWork(t *testing.T) {
	for _, fail := range []string{"start", "open"} {
		t.Run(fail, func(t *testing.T) {
			m := &fakeMachine{version: "v0.2.0", fail: fail}
			e := testEngine(t, m, goodSource())
			if _, err := e.submit(request()); err != nil {
				t.Fatal(err)
			}
			waitEngine(t, e)
			if e.status().Supported {
				t.Error("incomplete recovery advertised as ready")
			}
			if err := e.enterMaintenance(); err == nil {
				t.Error("installer accepted before recovery completed")
				e.maintenance = false
			}
			next := request()
			next.ID = "next_request_1234"
			if _, err := e.submit(next); err == nil {
				waitEngine(t, e)
				t.Error("new task replaced incomplete recovery")
			}
		})
	}
}

func TestEngineRejectsArtifactsSignedForAnotherVersion(t *testing.T) {
	m := &fakeMachine{version: "v0.2.0"}
	s := fakeSource{a: signedArtifacts("hub", "amd64", "v0.2.9", sigtest.Archive("hub", []byte("binary")))}
	e := testEngine(t, m, s)
	if _, err := e.submit(request()); err != nil {
		t.Fatal(err)
	}
	j := waitEngine(t, e)
	if j.State != "failed" || !strings.Contains(j.Error, "does not verify") {
		t.Fatalf("job = %+v", j)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if slices.Contains(m.calls, "stage") || slices.Contains(m.calls, "stop") {
		t.Fatalf("rejected artifacts reached the machine: %v", m.calls)
	}
}

func TestEngineReportsSource(t *testing.T) {
	e := testEngine(t, &fakeMachine{version: "v0.2.0"}, goodSource())
	if s := e.status(); s.Source != "github" || !s.Supported {
		t.Fatalf("status = %+v", s)
	}
}

func TestEngineSourceConfigErrorDisablesUpdates(t *testing.T) {
	m := &fakeMachine{version: "v0.2.0"}
	e, err := newEngine(context.Background(), discardLog(), filepath.Join(t.TempDir(), "state.json"), "agent", "amd64",
		sourceChoice{err: "update source config " + sourceConfigPath + " is unusable: bad"}, testKeys(), m)
	if err != nil {
		t.Fatal(err)
	}
	s := e.status()
	if s.Supported || !strings.Contains(s.Reason, sourceConfigPath) {
		t.Fatalf("status = %+v", s)
	}
	if _, err := e.submit(request()); err == nil || !strings.Contains(err.Error(), sourceConfigPath) {
		t.Fatalf("submit accepted with an unusable source config: %v", err)
	}
}

type deadlineSource struct{ got chan time.Duration }

func (s deadlineSource) Fetch(ctx context.Context, _ Request, _, _ string) (Artifacts, error) {
	d, _ := ctx.Deadline()
	s.got <- time.Until(d)
	return Artifacts{}, errors.New("stop after recording the deadline")
}

// 引擎给来源的期限跟着来源走：GitHub 来源是 DownloadLimit，hub 来源是更长的 hubFetchLimit。
func TestEngineFetchDeadlineFollowsSource(t *testing.T) {
	for _, tc := range []struct {
		choice func(source) sourceChoice
		want   time.Duration
	}{{githubChoice, DownloadLimit}, {hubChoice, hubFetchLimit}} {
		src := deadlineSource{got: make(chan time.Duration, 1)}
		e := testEngineWith(t, &fakeMachine{version: "v0.2.0"}, tc.choice(src))
		if _, err := e.submit(request()); err != nil {
			t.Fatal(err)
		}
		got := <-src.got
		waitEngine(t, e)
		if got > tc.want || got < tc.want-5*time.Second {
			t.Errorf("%s source got a %v deadline, want %v", e.choice.name, got, tc.want)
		}
	}
}
