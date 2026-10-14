package update

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/githubtransport"
	"github.com/xjetry/heron/internal/releasesig/sigtest"
)

// 测试里的停滞期限：生产值 stallTimeout 是 60 秒，缩短 200 倍。"旧的 5 分钟总时限"按同一比例折算为 1.5 秒。
const (
	testStall      = 300 * time.Millisecond
	oldLimitScaled = 5 * time.Minute / (stallTimeout / testStall)
)

// slowRelease 是一个逐块写归档的 httptest 服务：清单与签名一次写完，归档每隔 interval 写一块 chunk 字节并 flush；
// stallAfter ≥ 0 时写完这么多块后不再写，直到请求结束。三份文件都按 testKeys 签名，取回成功时 Accept 能过。
type slowRelease struct {
	archive    []byte
	chunk      int
	interval   time.Duration
	stallAfter int
	a          Artifacts
}

func newSlowRelease(chunks, chunk int, interval time.Duration, stallAfter int) *slowRelease {
	// 二进制用不可压缩的伪随机字节，归档才接近 chunks×chunk 的大小，分块与停滞点才落在预期的字节数上。
	bin := make([]byte, chunks*chunk)
	rand.NewChaCha8([32]byte{1}).Read(bin)
	archive := sigtest.Archive("hub", bin)
	return &slowRelease{archive: archive, chunk: chunk, interval: interval, stallAfter: stallAfter,
		a: signedArtifacts("hub", "amd64", "v0.3.0", archive)}
}

func (s *slowRelease) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasSuffix(r.URL.Path, "/SHA256SUMS"):
		w.Write(s.a.Sums)
		return
	case strings.HasSuffix(r.URL.Path, "/SHA256SUMS.sig"):
		w.Write(s.a.Signature)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(s.a.Archive)))
	w.WriteHeader(http.StatusOK)
	flusher := w.(http.Flusher)
	for i, off := 0, 0; off < len(s.a.Archive); i++ {
		if s.stallAfter >= 0 && i == s.stallAfter {
			<-r.Context().Done()
			return
		}
		if i > 0 {
			select {
			case <-time.After(s.interval):
			case <-r.Context().Done():
				return
			}
		}
		end := min(off+s.chunk, len(s.a.Archive))
		w.Write(s.a.Archive[off:end])
		flusher.Flush()
		off = end
	}
}

// slowSource 让 OfficialSource 的官方地址落到 httptest 服务上：githubtransport 照常校验原地址，内层传输改写目标。
func slowSource(t *testing.T, h http.Handler, stall time.Duration) *OfficialSource {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	inner := srv.Client().Transport
	return &OfficialSource{stall: stall, http: githubtransport.WithTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		out := r.Clone(r.Context())
		out.URL.Scheme, out.URL.Host, out.Host = "http", strings.TrimPrefix(srv.URL, "http://"), ""
		return inner.RoundTrip(out)
	}), 0)}
}

func fetchWithin(t *testing.T, s *OfficialSource, limit time.Duration) (Artifacts, time.Duration, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), limit)
	defer cancel()
	start := time.Now()
	a, err := s.Fetch(ctx, Request{Version: "v0.3.0"}, "hub", "amd64")
	return a, time.Since(start), err
}

// 慢而不停：每块间隔远小于停滞期限，总用时超过旧总时限的折算值；停滞判定不能把它当成停了，总上限也不该挡它。
func TestOfficialSlowDownloadWithoutStallSucceeds(t *testing.T) {
	rel := newSlowRelease(20, 4<<10, testStall/3, -1)
	s := slowSource(t, rel, testStall)
	a, took, err := fetchWithin(t, s, 30*time.Second)
	if err != nil {
		t.Fatalf("slow but steady download failed after %v: %v", took, err)
	}
	if took <= oldLimitScaled {
		t.Fatalf("download took %v; the fixture must run longer than the scaled old limit %v to prove anything", took, oldLimitScaled)
	}
	if _, err := Accept(testKeys(), "hub", "amd64", "v0.3.0", a); err != nil {
		t.Fatalf("slowly fetched artifacts do not verify: %v", err)
	}
}

// 中途停滞：连续 stall 收不到字节即失败，不等调用方的总上限；文案写停了多久、已收字节、总长与用时。
func TestOfficialStalledDownloadFailsWithProgress(t *testing.T) {
	rel := newSlowRelease(20, 4<<10, time.Millisecond, 3)
	s := slowSource(t, rel, testStall)
	_, took, err := fetchWithin(t, s, 10*time.Second)
	if !errors.Is(err, errStalled) {
		t.Fatalf("stalled download ended with %v after %v, want a stall", err, took)
	}
	total := formatBytes(int64(len(rel.a.Archive)))
	want := regexp.MustCompile(`^read official release: no data for 0\.3s after 12\.0 KiB of ` + regexp.QuoteMeta(total) + ` in \d+ms$`)
	if !want.MatchString(err.Error()) {
		t.Fatalf("stall error = %q, want progress matching %s", err, want)
	}
	if took > 5*time.Second {
		t.Fatalf("stall took %v to detect; it must not wait for the caller's limit", took)
	}
}

// 超总上限：一直有字节但取不完，到调用方期限即失败，文案写上限与已收进度，仍可按 context.DeadlineExceeded 判别。
func TestOfficialDownloadExceedingLimitFailsWithProgress(t *testing.T) {
	rel := newSlowRelease(20, 4<<10, testStall/3, -1)
	s := slowSource(t, rel, testStall)
	_, took, err := fetchWithin(t, s, 700*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errStalled) {
		t.Fatalf("over-limit download ended with %v after %v, want the caller's deadline", err, took)
	}
	total := formatBytes(int64(len(rel.a.Archive)))
	want := regexp.MustCompile(`^read official release: download exceeded (69\d|700)ms after \d+\.\d KiB of ` + regexp.QuoteMeta(total) + `$`)
	if !want.MatchString(err.Error()) {
		t.Fatalf("over-limit error = %q, want %s", err, want)
	}
}

// 常量从推导重算：总上限要容得下最大的取回以最低速率传完，向上取整不超过一分钟；最低速率不高于 64 KiB/s。
// 节点经 hub 的期限必须长于 hub 侧的总上限，hub 先到期节点才收得到 hub 的错误文案。
func TestDownloadLimitDerivation(t *testing.T) {
	need := time.Duration(maxFetchBytes) * time.Second / minDownloadRate
	if DownloadLimit < need || DownloadLimit-need >= time.Minute {
		t.Fatalf("DownloadLimit %v does not round up %v (max fetch %d bytes at %d B/s)", DownloadLimit, need, maxFetchBytes, minDownloadRate)
	}
	if minDownloadRate > 64<<10 {
		t.Fatalf("minimum acceptable rate %d B/s is above 64 KiB/s", minDownloadRate)
	}
	if hubFetchLimit < DownloadLimit+need {
		t.Fatalf("hubFetchLimit %v does not cover the hub's fetch %v plus relaying the same bytes %v", hubFetchLimit, DownloadLimit, need)
	}
	if stallTimeout >= DownloadLimit || NewOfficialSource().stall != stallTimeout {
		t.Fatalf("stall timeout %v (official source %v) must be the production constant and shorter than %v", stallTimeout, NewOfficialSource().stall, DownloadLimit)
	}
}

func TestReadErrorWording(t *testing.T) {
	other := errors.New("unexpected EOF")
	for _, tc := range []struct {
		cause error
		total int64
		want  string
	}{
		{errStalled, 10276044, "read official release: no data for 60s after 2.1 MiB of 9.8 MiB in 5m2s"},
		{context.DeadlineExceeded, 10276044, "read official release: download exceeded 35m0s after 7.4 MiB of 9.8 MiB"},
		{context.Canceled, -1, "read official release: unexpected EOF after 512 B in 5m2s"},
	} {
		got := map[error]int64{errStalled: 2202010, context.DeadlineExceeded: 7759462, context.Canceled: 512}[tc.cause]
		err := readError(other, tc.cause, stallTimeout, DownloadLimit, got, tc.total, 5*time.Minute+2*time.Second+300*time.Millisecond)
		if err.Error() != tc.want {
			t.Errorf("cause %v: %q, want %q", tc.cause, err, tc.want)
		}
	}
}

// 零值的停滞期限只可能来自绕开构造函数的字面量；它必须被拒，而不是让计时器立刻到点、把任何正文判成停滞。
func TestOfficialSourceRequiresStallTimeout(t *testing.T) {
	s := slowSource(t, newSlowRelease(1, 1, 0, -1), 0)
	if _, _, err := fetchWithin(t, s, 5*time.Second); err == nil || !strings.Contains(err.Error(), "stall timeout") {
		t.Fatalf("zero stall timeout: %v", err)
	}
}
