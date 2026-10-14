package netinfo

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/agent/client"
	"github.com/xjetry/heron/internal/agentwire"
	"google.golang.org/protobuf/proto"
)

func testAddresses() ([]netip.Addr, error) {
	return []netip.Addr{netip.MustParseAddr("10.0.0.2"), netip.MustParseAddr("fd00::2")}, nil
}

// 每族的回显主机只发布该族的 DNS 记录，拨号族与主机配错时线上必然拨不出去，所以按族核对。
var familyHosts = map[string]string{"tcp4": "api-ipv4.ip.sb", "tcp6": "api-ipv6.ip.sb"}

// 请求仍指向正式主机名，拨号仅重定向到受控 TLS 服务，不依赖本机公网或 IPv6 路由。
func localDetector(t *testing.T, handler http.HandlerFunc) *Detector {
	t.Helper()
	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)
	d := newDetector(testAddresses, func(ctx context.Context, network, address string) (net.Conn, error) {
		host, ok := familyHosts[network]
		if !ok {
			t.Errorf("dial family=%q", network)
		}
		if address != host+":443" {
			t.Errorf("dial family=%q address=%q, want %q", network, address, host+":443")
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp4", srv.Listener.Addr().String())
	})
	for _, client := range d.clients {
		client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs, ServerName: "example.com"}
	}
	d.now = func() time.Time { return time.Unix(123, 0) }
	return d
}

func TestDetectionResponseAndFailureStates(t *testing.T) {
	for _, tc := range []struct {
		name, body   string
		family, code int
		state        heronv1.AddressDetectionState
	}{
		{"v4", "8.8.8.8\n", 0, 200, 1}, {"v6", "2606:4700::1111", 1, 200, 1},
		{"wrong_family", "8.8.8.8", 1, 200, 3}, {"mapped_v6", "::ffff:8.8.8.8", 1, 200, 3},
		{"private", "10.0.0.1", 0, 200, 3}, {"documentation", "2001:db8::1", 1, 200, 3},
		{"bad_body", "not an IP", 0, 200, 3}, {"oversized", "8.8.8.8" + strings.Repeat(" ", 65), 0, 200, 3},
		{"redirect", "8.8.8.8", 0, 302, 3}, {"server_error", "8.8.8.8", 0, 500, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			d := localDetector(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if want := familyHosts[[]string{"tcp4", "tcp6"}[tc.family]]; r.Host != want || r.URL.Path != "/ip" {
					t.Errorf("request host=%q path=%q, want %q /ip", r.Host, r.URL.Path, want)
				}
				w.Header().Set("Location", "/redirected")
				w.WriteHeader(tc.code)
				io.WriteString(w, tc.body)
			})
			addresses, _ := testAddresses()
			result := d.detect(t.Context(), tc.family, addresses, nil)
			if result.State != tc.state || (tc.state != 1 && result.Address != "") {
				t.Fatalf("result=%v", result)
			}
			if tc.state == 1 && result.Address != strings.TrimSpace(tc.body) {
				t.Fatalf("address=%q", result.Address)
			}
			if calls != 1 {
				t.Fatalf("requests=%d, redirects must not be followed", calls)
			}
		})
	}
}

func TestInterfacesAndDialFailures(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		addresses             []netip.Addr
		interfaceErr, dialErr error
		state                 heronv1.AddressDetectionState
		calls                 int
	}{
		{"absent", nil, nil, nil, 2, 0},
		{"loopback_linklocal", []netip.Addr{netip.MustParseAddr("::1"), netip.MustParseAddr("fe80::1")}, nil, nil, 2, 0},
		{"enumeration_error", nil, errors.New("interfaces"), nil, 3, 0},
		{"no_route", []netip.Addr{netip.MustParseAddr("fd00::1")}, nil, syscall.ENETUNREACH, 2, 1},
		{"no_family", []netip.Addr{netip.MustParseAddr("fd00::1")}, nil, syscall.EAFNOSUPPORT, 2, 1},
		{"timeout", []netip.Addr{netip.MustParseAddr("fd00::1")}, nil, context.DeadlineExceeded, 3, 1},
		{"dns", []netip.Addr{netip.MustParseAddr("fd00::1")}, nil, &net.DNSError{Err: "not found", IsNotFound: true}, 3, 1},
		{"dns_no_route", []netip.Addr{netip.MustParseAddr("fd00::1")}, nil, &net.DNSError{Err: "network unreachable", UnwrapErr: syscall.ENETUNREACH}, 3, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			d := newDetector(testAddresses, func(context.Context, string, string) (net.Conn, error) { calls++; return nil, tc.dialErr })
			result := d.detect(t.Context(), 1, tc.addresses, tc.interfaceErr)
			if result.State != tc.state || result.Address != "" || calls != tc.calls {
				t.Fatalf("result=%v calls=%d", result, calls)
			}
		})
	}
}

func TestRefreshClearsOldAddressAndSnapshotsAreIndependent(t *testing.T) {
	var mu sync.Mutex
	var fail bool
	d := localDetector(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			w.WriteHeader(500)
		} else {
			io.WriteString(w, "8.8.8.8")
		}
	})
	d.refresh(t.Context(), [2]bool{true, true})
	first := d.Snapshot()
	if first.GetIpv4().GetAddress() != "8.8.8.8" || first.GetIpv6().GetState() != 3 {
		t.Fatalf("first=%v", first)
	}
	first.Ipv4.Address = "changed"
	if d.Snapshot().GetIpv4().Address != "8.8.8.8" {
		t.Fatal("snapshot aliases detector")
	}
	mu.Lock()
	fail = true
	mu.Unlock()
	d.refresh(t.Context(), [2]bool{true, true})
	if got := d.Snapshot().Ipv4; got.State != 3 || got.Address != "" {
		t.Fatalf("stale address after failure=%v", got)
	}
}

func TestTransportPolicyAndCancellation(t *testing.T) {
	d := New()
	for _, client := range d.clients {
		if client.Timeout != 10*time.Second || client.Transport.(*http.Transport).Proxy != nil {
			t.Fatalf("unsafe transport=%+v", client)
		}
	}
	d = localDetector(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	addresses, _ := testAddresses()
	if got := d.detect(ctx, 0, addresses, nil); got.State != 3 {
		t.Fatalf("timeout=%v", got)
	}
	ctx, cancel = context.WithCancel(t.Context())
	cancel()
	d.Run(ctx)
	if d.Snapshot().Ipv4 != nil {
		t.Fatal("canceled detector must not probe")
	}
}

// 一轮快照由两个地址族的完整结果构成：refresh 在两族都返回后才整体替换 current。
// 因此两族状态都不再是 UNSPECIFIED，就代表这一轮已经写完（首次运行前两份都是 nil）。
func waitForRound(t *testing.T, d *Detector) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		got := d.Snapshot()
		if got.GetIpv4().GetState() != heronv1.AddressDetectionState_ADDRESS_DETECTION_STATE_UNSPECIFIED &&
			got.GetIpv6().GetState() != heronv1.AddressDetectionState_ADDRESS_DETECTION_STATE_UNSPECIFIED {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("round did not complete: %v", got)
		}
		runtime.Gosched()
	}
}

func TestRunProbesBothFamiliesImmediately(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := make(chan string, 2)
	d := newDetector(testAddresses, func(ctx context.Context, network, address string) (net.Conn, error) {
		calls <- network
		return nil, syscall.ENETUNREACH
	})
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	seen := map[string]int{}
	for range 2 {
		select {
		case network := <-calls:
			seen[network]++
		case <-time.After(time.Second):
			t.Fatal("initial probe did not run")
		}
	}
	// 拨号被调用只说明探测开始，分类发生在请求返回之后；必须先等两个地址族的结果都写入
	// 快照再取消，否则取消可能截断本轮，留下尚未分类的错误。
	waitForRound(t, d)
	cancel()
	<-done
	if seen["tcp4"] != 1 || seen["tcp6"] != 1 {
		t.Fatalf("families=%v", seen)
	}
	if got := d.Snapshot(); got.GetIpv4().GetState() != 2 || got.GetIpv6().GetState() != 2 {
		t.Fatalf("snapshot=%v", got)
	}
}

// 取消只中止本轮探测，不是检测结果：上一轮的完整快照必须原样保留，
// 否则 shutdown/reload 触发的一次取消会把已知的能力结论覆盖成失败并上报。
func TestCanceledRoundKeepsPreviousSnapshot(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	dialed := make(chan struct{}, 2)
	d := newDetector(testAddresses, func(ctx context.Context, network, address string) (net.Conn, error) {
		dialed <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	d.now = func() time.Time { return time.Unix(123, 0) }
	d.mu.Lock()
	d.current = &heronv1.NetworkInfo{
		Ipv4: &heronv1.AddressDetection{State: heronv1.AddressDetectionState_ADDRESS_DETECTION_STATE_AVAILABLE, Address: "8.8.8.8", CheckedAt: 123},
		Ipv6: &heronv1.AddressDetection{State: heronv1.AddressDetectionState_ADDRESS_DETECTION_STATE_UNSUPPORTED, CheckedAt: 123},
	}
	d.mu.Unlock()
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	for range 2 {
		select {
		case <-dialed:
		case <-time.After(time.Second):
			t.Fatal("round did not start")
		}
	}
	cancel()
	<-done
	got := d.Snapshot()
	if got.GetIpv4().GetState() != heronv1.AddressDetectionState_ADDRESS_DETECTION_STATE_AVAILABLE ||
		got.GetIpv4().GetAddress() != "8.8.8.8" ||
		got.GetIpv6().GetState() != heronv1.AddressDetectionState_ADDRESS_DETECTION_STATE_UNSUPPORTED ||
		got.GetIpv6().GetCheckedAt() != 123 {
		t.Fatalf("canceled round replaced snapshot: %v", got)
	}
}

func TestTLSVerificationFailureIsFailed(t *testing.T) {
	d := localDetector(t, func(w http.ResponseWriter, r *http.Request) { t.Error("untrusted TLS request reached HTTP handler") })
	d.clients[0].Transport.(*http.Transport).TLSClientConfig = &tls.Config{ServerName: "invalid.example"}
	addresses, _ := testAddresses()
	if got := d.detect(t.Context(), 0, addresses, nil); got.State != 3 || got.Address != "" {
		t.Fatalf("TLS error=%v", got)
	}
}

// countingDial 按拨号族计数，立即以网络不可达返回（该族判为 UNSUPPORTED），不发出任何真实连接。
type countingDial struct {
	mu    sync.Mutex
	calls map[string]int
	seen  chan string
}

func newCountingDial() *countingDial {
	return &countingDial{calls: map[string]int{}, seen: make(chan string, 64)}
}

func (c *countingDial) dial(_ context.Context, network, _ string) (net.Conn, error) {
	c.mu.Lock()
	c.calls[network]++
	c.mu.Unlock()
	select {
	case c.seen <- network:
	default:
	}
	return nil, syscall.ENETUNREACH
}

func (c *countingDial) count(network string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[network]
}

func disabled(at int64) *heronv1.AddressDetection {
	return &heronv1.AddressDetection{State: heronv1.AddressDetectionState_ADDRESS_DETECTION_STATE_DISABLED, CheckedAt: at}
}

// 被停用的族一次都不拨，另一族照常按周期探测；停用的族报 DISABLED，没有地址，checked_at 是停用生效的时刻。
func TestSkippedFamilyIsNeverDialed(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	dial := newCountingDial()
	d := newDetector(testAddresses, dial.dial)
	d.now = func() time.Time { return time.Unix(123, 0) }
	d.interval = time.Millisecond
	d.Skip(&heronv1.NetworkDetection{SkipIpv4: true})
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for dial.count("tcp6") < 5 {
		if time.Now().After(deadline) {
			t.Fatalf("IPv6 probed %d times, want at least 5 rounds", dial.count("tcp6"))
		}
		runtime.Gosched()
	}
	cancel()
	<-done
	if n := dial.count("tcp4"); n != 0 {
		t.Fatalf("skipped IPv4 dialed %d times", n)
	}
	got := d.Snapshot()
	if !proto.Equal(got.GetIpv4(), disabled(123)) || got.GetIpv6().GetState() != heronv1.AddressDetectionState_ADDRESS_DETECTION_STATE_UNSUPPORTED {
		t.Fatalf("snapshot = %v, want IPv4 disabled at 123 and IPv6 probed", got)
	}
}

// 停用立即生效：不等下一轮，快照里该族马上换成 DISABLED，另一族不动；重复同一开关不改 checked_at。
func TestSkipReplacesFamilyAtOnce(t *testing.T) {
	d := newDetector(testAddresses, newCountingDial().dial)
	now := int64(100)
	d.now = func() time.Time { return time.Unix(now, 0) }
	v6 := &heronv1.AddressDetection{State: heronv1.AddressDetectionState_ADDRESS_DETECTION_STATE_AVAILABLE, Address: "2606:4700::1111", CheckedAt: 50}
	d.current = &heronv1.NetworkInfo{Ipv4: &heronv1.AddressDetection{State: heronv1.AddressDetectionState_ADDRESS_DETECTION_STATE_AVAILABLE, Address: "8.8.8.8", CheckedAt: 50}, Ipv6: v6}
	d.Skip(&heronv1.NetworkDetection{SkipIpv4: true})
	if got := d.Snapshot(); !proto.Equal(got.GetIpv4(), disabled(100)) || !proto.Equal(got.GetIpv6(), v6) {
		t.Fatalf("after skip: %v", got)
	}
	now = 200
	d.Skip(&heronv1.NetworkDetection{SkipIpv4: true})
	if got := d.Snapshot().GetIpv4(); !proto.Equal(got, disabled(100)) {
		t.Fatalf("repeated skip rewrote the family: %v", got)
	}
}

// 恢复不等周期：hub 不再要求停用时，该族立即退回缺失（已恢复探测，但尚无本轮结果），并马上探测一次——周期设成一小时，
// 拨号只可能来自恢复；另一族不跟着重探。
func TestResumedFamilyIsProbedImmediately(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	dial := newCountingDial()
	// 恢复的那一族拨号后停在 gate 上，直到断言完"探测完成前没有结果"。refresh 等本轮所有族的 detect 返回才写回快照，
	// 所以 gate 未开时快照里不可能有它的新结果；不拦住时，Skip 唤醒的探测可能在断言之前就已写回，断言看到的就不再是
	// 探测前的快照。
	gate := make(chan struct{})
	d := newDetector(testAddresses, func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial.dial(ctx, network, address)
		if network == "tcp4" {
			select {
			case <-gate:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return conn, err
	})
	d.now = func() time.Time { return time.Unix(123, 0) }
	d.interval = time.Hour
	d.Skip(&heronv1.NetworkDetection{SkipIpv4: true})
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	select {
	case network := <-dial.seen:
		if network != "tcp6" {
			t.Fatalf("first round dialed %s", network)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first round did not run")
	}
	waitFor(t, func() bool {
		return d.Snapshot().GetIpv6().GetState() == heronv1.AddressDetectionState_ADDRESS_DETECTION_STATE_UNSUPPORTED
	})
	d.Skip(nil)
	select {
	case network := <-dial.seen:
		if network != "tcp4" {
			t.Fatalf("resume dialed %s, want only the resumed family", network)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("resumed family was not probed before the next period")
	}
	if got := d.Snapshot().GetIpv4(); got != nil {
		t.Fatalf("resumed family still reported %v before its probe", got)
	}
	close(gate)
	waitFor(t, func() bool {
		return d.Snapshot().GetIpv4().GetState() == heronv1.AddressDetectionState_ADDRESS_DETECTION_STATE_UNSUPPORTED
	})
	cancel()
	<-done
	if dial.count("tcp6") != 1 || dial.count("tcp4") != 1 {
		t.Fatalf("dials tcp4=%d tcp6=%d, want one each", dial.count("tcp4"), dial.count("tcp6"))
	}
}

// 探测在途时被停用的族：结果迟到也不覆盖 Skip 写下的 DISABLED。
func TestLateResultDoesNotOverwriteSkip(t *testing.T) {
	release := make(chan struct{})
	dialed := make(chan struct{}, 2)
	d := newDetector(testAddresses, func(ctx context.Context, network, address string) (net.Conn, error) {
		dialed <- struct{}{}
		<-release
		return nil, syscall.ENETUNREACH
	})
	d.now = func() time.Time { return time.Unix(123, 0) }
	finished := make(chan struct{})
	go func() { d.refresh(t.Context(), [2]bool{true, true}); close(finished) }()
	for range 2 {
		<-dialed
	}
	d.Skip(&heronv1.NetworkDetection{SkipIpv4: true})
	close(release)
	<-finished
	got := d.Snapshot()
	if !proto.Equal(got.GetIpv4(), disabled(123)) || got.GetIpv6().GetState() != heronv1.AddressDetectionState_ADDRESS_DETECTION_STATE_UNSUPPORTED {
		t.Fatalf("late result overwrote the skip: %v", got)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		runtime.Gosched()
	}
}

// checked_at 是当前结果首次得到的时刻：同一结果连续探测，checked_at 与 Facts 摘要都不动，hub 不必重新索取 facts；
// state 或 address 任一变化（含停用与恢复），该族的 checked_at 取此刻、摘要随之变化，另一族不动。每一步的快照都要过
// hub 的上报准入 agentwire.ValidateNetwork：沿用的时刻仍在 1..MaxDetectionTime 之内，照收。
func TestCheckedAtMarksWhenResultFirstSeen(t *testing.T) {
	var mu sync.Mutex
	v4Body, v4Code := "8.8.8.8", http.StatusOK
	d := localDetector(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Host == familyHosts["tcp6"] {
			io.WriteString(w, "2606:4700::1111")
			return
		}
		mu.Lock()
		defer mu.Unlock()
		w.WriteHeader(v4Code)
		io.WriteString(w, v4Body)
	})
	serve := func(body string, code int) {
		mu.Lock()
		defer mu.Unlock()
		v4Body, v4Code = body, code
	}
	now := int64(1000)
	d.now = func() time.Time { return time.Unix(now, 0) }
	var hash uint64
	check := func(step string, v4State heronv1.AddressDetectionState, v4At, v6At int64, changed bool) {
		t.Helper()
		got := d.Snapshot()
		if err := agentwire.ValidateNetwork(got); err != nil {
			t.Fatalf("%s: snapshot rejected by hub admission: %v", step, err)
		}
		if got.GetIpv4().GetState() != v4State || got.GetIpv4().GetCheckedAt() != v4At || got.GetIpv6().GetCheckedAt() != v6At {
			t.Fatalf("%s: snapshot = %v, want IPv4 %v at %d, IPv6 at %d", step, got, v4State, v4At, v6At)
		}
		next := client.FactsHash(&heronv1.Facts{Network: got})
		if (next != hash) != changed {
			t.Fatalf("%s: facts hash changed = %v, want %v", step, next != hash, changed)
		}
		hash = next
	}
	round := func() {
		t.Helper()
		now += 300
		d.refresh(t.Context(), [2]bool{true, true})
	}
	available := heronv1.AddressDetectionState_ADDRESS_DETECTION_STATE_AVAILABLE
	failed := heronv1.AddressDetectionState_ADDRESS_DETECTION_STATE_FAILED
	unsupported := heronv1.AddressDetectionState_ADDRESS_DETECTION_STATE_UNSUPPORTED

	round()
	check("first", available, 1300, 1300, true)
	for range 3 {
		round()
		check("unchanged", available, 1300, 1300, false)
	}
	serve("1.1.1.1", http.StatusOK)
	round()
	check("address changed", available, 2500, 1300, true)
	round()
	check("new address unchanged", available, 2500, 1300, false)
	serve("", http.StatusInternalServerError)
	round()
	check("available to failed", failed, 3100, 1300, true)
	round()
	check("failed unchanged", failed, 3100, 1300, false)
	serve("1.1.1.1", http.StatusOK)
	round()
	check("failed to available", available, 3700, 1300, true)
	d.interfaces = func() ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr("fd00::2")}, nil }
	round()
	check("available to unsupported", unsupported, 4000, 1300, true)
	d.interfaces = testAddresses
	round()
	check("unsupported to available", available, 4300, 1300, true)

	now += 300
	d.Skip(&heronv1.NetworkDetection{SkipIpv4: true})
	check("disabled", heronv1.AddressDetectionState_ADDRESS_DETECTION_STATE_DISABLED, 4600, 1300, true)
	round()
	check("disabled unchanged", heronv1.AddressDetectionState_ADDRESS_DETECTION_STATE_DISABLED, 4600, 1300, false)
	d.Skip(nil)
	check("resumed before probe", heronv1.AddressDetectionState_ADDRESS_DETECTION_STATE_UNSPECIFIED, 0, 1300, true)
	round()
	check("resumed with the pre-disable result", available, 5200, 1300, true)
}
