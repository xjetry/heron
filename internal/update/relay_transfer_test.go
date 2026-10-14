package update_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/updates"
	"github.com/xjetry/heron/internal/update"
)

type relayTask struct{ t *heronv1.UpdateTask }

func (r relayTask) Snapshot(int64) *heronv1.UpdateStatus { return &heronv1.UpdateStatus{Task: r.t} }
func (r relayTask) ActiveTasks() map[string]string       { return map[string]string{r.t.Id: r.t.Version} }

// hub 中转从 GitHub 取与更新器直连 GitHub 读的是同一个 OfficialSource.get：归档读到一半停住时，节点从 hub 拿到的
// 是同一句带进度的停滞文案，而不是等到中转的总上限（DownloadLimit，35 分钟）或节点自己的期限才失败。取回函数与
// cmd/hub 的装配同形：Relay 只经 update.OfficialSource.Fetch 取 GitHub 的字节。
func TestRelayReadsGitHubThroughTheStallReader(t *testing.T) {
	const declared = 64 << 10
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ".tar.gz") {
			w.Write([]byte("small file"))
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(declared))
		w.WriteHeader(http.StatusOK)
		w.Write(make([]byte, 4<<10))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	// 中转的取回不随节点请求结束（Relay.load 用自己的上下文），读者失效时它会一直挂在停住的正文上；先断开客户端连接，
	// srv.Close 才不会等它到 DownloadLimit。
	t.Cleanup(srv.CloseClientConnections)
	inner := srv.Client().Transport
	official := update.NewTestOfficialSource(roundTrip(func(r *http.Request) (*http.Response, error) {
		out := r.Clone(r.Context())
		out.URL.Scheme, out.URL.Host, out.Host = "http", strings.TrimPrefix(srv.URL, "http://"), ""
		return inner.RoundTrip(out)
	}), 300*time.Millisecond)
	task := &heronv1.UpdateTask{Id: "aaaaaaaaaaaaaaaa", Version: "v9.0.0", State: "dispatched", ExpiresAt: 20_000}
	relay := updates.NewRelay(relayTask{task},
		func(ctx context.Context, version, arch string) (update.Artifacts, error) {
			return official.Fetch(ctx, update.Request{Version: version}, "agent", arch)
		},
		func(string, string, update.Artifacts) error { return nil }, clock.NewFake(time.Unix(10_000, 0)))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, err := relay.Get(ctx, 1, task.Id, "amd64")
	if err == nil || !strings.HasPrefix(err.Error(), "read official release: no data for 0.3s after 4.0 KiB of 64.0 KiB in ") {
		t.Fatalf("relay error = %v, want the stall reader's wording", err)
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
