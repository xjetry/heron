package ingest

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/updates"
	"github.com/xjetry/heron/internal/update"
)

type fakeReleases struct {
	node  atomic.Int64
	block chan struct{}
	err   error
}

func (f *fakeReleases) Get(ctx context.Context, node int64, taskID, arch string) (update.Artifacts, error) {
	f.node.Store(node)
	if f.block != nil {
		<-f.block
	}
	if f.err != nil {
		return update.Artifacts{}, f.err
	}
	return update.Artifacts{Sums: []byte(taskID), Signature: []byte(arch), Archive: []byte("archive")}, nil
}

func getRelease(tok string) *connect.Request[heronv1.GetReleaseRequest] {
	req := connect.NewRequest(&heronv1.GetReleaseRequest{TaskId: "aaaaaaaaaaaaaaaa", Arch: "amd64"})
	req.Header().Set("Authorization", "Bearer "+tok)
	return req
}

func TestGetReleaseUsesTheAuthenticatedNode(t *testing.T) {
	f := &fakeReleases{}
	h := newHubWith(t, filepath.Join(t.TempDir(), "t.db"), Config{TTL: 30 * time.Second, Releases: f})
	id, tok := h.node(t)
	resp, err := h.client.GetRelease(context.Background(), getRelease(tok))
	if err != nil {
		t.Fatal(err)
	}
	if f.node.Load() != id || string(resp.Msg.Sums) != "aaaaaaaaaaaaaaaa" || string(resp.Msg.Signature) != "amd64" || string(resp.Msg.Archive) != "archive" {
		t.Fatalf("node=%d resp=%v", f.node.Load(), resp.Msg)
	}
	if _, err := h.client.GetRelease(context.Background(), getRelease("bogus")); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("bad token: %v", err)
	}
}

func TestGetReleaseErrorCodes(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code connect.Code
	}{
		{updates.ErrArch, connect.CodeInvalidArgument},
		{updates.ErrNoTask, connect.CodeFailedPrecondition},
		{updates.ErrBusy, connect.CodeResourceExhausted},
		{updates.ErrAttempts, connect.CodeResourceExhausted},
		{updates.ErrCacheFull, connect.CodeResourceExhausted},
		{errors.New("github unreachable"), connect.CodeUnavailable},
	} {
		h := newHubWith(t, filepath.Join(t.TempDir(), "t.db"), Config{TTL: 30 * time.Second, Releases: &fakeReleases{err: tc.err}})
		_, tok := h.node(t)
		_, err := h.client.GetRelease(context.Background(), getRelease(tok))
		if connect.CodeOf(err) != tc.code {
			t.Errorf("%v: code %v, want %v", tc.err, connect.CodeOf(err), tc.code)
		}
	}
}

func TestGetReleaseUnconfiguredIsUnavailable(t *testing.T) {
	h := newHub(t)
	_, tok := h.node(t)
	if _, err := h.client.GetRelease(context.Background(), getRelease(tok)); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("err = %v", err)
	}
}

// 一次下载可能持续数分钟；它若持有 stateMu 的读锁，Forget 会等它，而排队的写锁又会挡住此后全部 Report。
func TestGetReleaseDoesNotBlockForgetOrReport(t *testing.T) {
	f := &fakeReleases{block: make(chan struct{})}
	h := newHubWith(t, filepath.Join(t.TempDir(), "t.db"), Config{TTL: 30 * time.Second, Releases: f})
	slowID, slowTok := h.node(t)
	_, otherTok := h.node(t)
	done := make(chan error, 1)
	go func() { _, err := h.client.GetRelease(context.Background(), getRelease(slowTok)); done <- err }()
	deadline := time.Now().Add(2 * time.Second)
	for f.node.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	forgot := make(chan struct{})
	go func() { h.svc.Forget(slowID); close(forgot) }()
	select {
	case <-forgot:
	case <-time.After(2 * time.Second):
		t.Fatal("Forget waited for an in-flight GetRelease")
	}
	if _, err := h.client.Report(context.Background(), report(otherTok, &heronv1.Metrics{CpuPct: proto.Float64(1)})); err != nil {
		t.Fatalf("Report blocked or failed while a download was in flight: %v", err)
	}
	close(f.block)
	<-done
}
