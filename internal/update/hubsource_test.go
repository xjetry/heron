package update

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/gen/heron/v1/heronv1connect"
)

type relayHub struct {
	heronv1connect.UnimplementedAgentServiceHandler
	calls atomic.Int32
	auth  string
	req   *heronv1.GetReleaseRequest
	resp  *heronv1.GetReleaseResponse
	err   error
}

func (h *relayHub) GetRelease(_ context.Context, req *connect.Request[heronv1.GetReleaseRequest]) (*connect.Response[heronv1.GetReleaseResponse], error) {
	h.calls.Add(1)
	h.auth, h.req = req.Header().Get("Authorization"), req.Msg
	if h.err != nil {
		return nil, h.err
	}
	return connect.NewResponse(h.resp), nil
}

func serveRelay(t *testing.T, h *relayHub) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(heronv1connect.NewAgentServiceHandler(h))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func configFor(url string) func() ([]byte, error) {
	return func() ([]byte, error) {
		return []byte(`{"hub":"` + url + `","token":"node-secret-token","name":"n"}`), nil
	}
}

func TestHubSourceFetch(t *testing.T) {
	want := signedArtifacts("agent", "amd64", "v1.2.3", []byte("archive-bytes"))
	h := &relayHub{resp: &heronv1.GetReleaseResponse{Sums: want.Sums, Signature: want.Signature, Archive: want.Archive}}
	s := NewHubSource(configFor(serveRelay(t, h)))
	got, err := s.Fetch(boundedCtx(t), Request{ID: "aaaaaaaaaaaaaaaa", Version: "v1.2.3"}, "agent", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Sums) != string(want.Sums) || string(got.Signature) != string(want.Signature) || string(got.Archive) != "archive-bytes" {
		t.Fatalf("got %+v", got)
	}
	if h.auth != "Bearer node-secret-token" || h.req.GetTaskId() != "aaaaaaaaaaaaaaaa" || h.req.GetArch() != "amd64" {
		t.Fatalf("auth=%q req=%v", h.auth, h.req)
	}
}

func TestHubSourceRefusesBeforeNetwork(t *testing.T) {
	h := &relayHub{}
	url := serveRelay(t, h)
	for name, tc := range map[string]struct {
		role string
		read func() ([]byte, error)
		want string
	}{
		"hub_role":          {"hub", configFor(url), "only agent"},
		"plain_http_remote": {"agent", func() ([]byte, error) { return []byte(`{"hub":"http://10.0.0.1:8080","token":"t"}`), nil }, "plain http"},
		"read_error":        {"agent", func() ([]byte, error) { return nil, errors.New("managed file must be regular") }, "read agent config"},
		"unknown_field":     {"agent", func() ([]byte, error) { return []byte(`{"hub":"` + url + `","token":"t","x":1}`), nil }, "unknown field"},
	} {
		_, err := NewHubSource(tc.read).Fetch(boundedCtx(t), Request{ID: "aaaaaaaaaaaaaaaa", Version: "v1.2.3"}, tc.role, "amd64")
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err %v, want %q", name, err, tc.want)
		}
	}
	if h.calls.Load() != 0 {
		t.Fatalf("refused fetches still reached the hub %d times", h.calls.Load())
	}
}

func TestHubSourceErrorDoesNotLeakToken(t *testing.T) {
	h := &relayHub{err: connect.NewError(connect.CodeFailedPrecondition, errors.New("no matching update task"))}
	_, err := NewHubSource(configFor(serveRelay(t, h))).Fetch(boundedCtx(t), Request{ID: "aaaaaaaaaaaaaaaa", Version: "v1.2.3"}, "agent", "amd64")
	if err == nil || !strings.Contains(err.Error(), "no matching update task") || strings.Contains(err.Error(), "node-secret-token") {
		t.Fatalf("err = %v", err)
	}
}

// hub 来源的连接同样不设总时限（hub 首次取回要先从 GitHub 取完才应答），期限只来自调用方的 ctx；
// 没有期限的取回在读配置、连 hub 之前被拒。
func TestHubSourceRequiresTimeLimit(t *testing.T) {
	h := &relayHub{}
	read := 0
	cfg := configFor(serveRelay(t, h))
	_, err := NewHubSource(func() ([]byte, error) { read++; return cfg() }).Fetch(context.Background(), Request{ID: "aaaaaaaaaaaaaaaa", Version: "v1.2.3"}, "agent", "amd64")
	if !errors.Is(err, errUnbounded) || read != 0 || h.calls.Load() != 0 {
		t.Fatalf("fetch without a time limit: err=%v reads=%d calls=%d", err, read, h.calls.Load())
	}
}
