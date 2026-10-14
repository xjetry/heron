package geo

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/alert"
	"github.com/xjetry/heron/internal/hub/metric"
	"github.com/xjetry/heron/internal/hub/outbound"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/netaddr"
)

// 每一段都钉住内外两侧：段内的首末地址（not）不是公网，紧邻段外的地址（public）是公网；紧邻的若是另一类非公网段，
// 由那一类的行覆盖，这里不重复。地址写字面值，不从 special 推：用例要独立于被测的表。段外的邻居有的落在未分配的
// 空间（如 fe00::、ff:ffff::），本函数不按分配状态判定，它们照样是公网，这里只用它们钉住段的宽度。
func TestIsPublic(t *testing.T) {
	const max = ":ffff:ffff:ffff:ffff:ffff:ffff"
	for _, c := range []struct {
		class       string
		not, public []string
	}{
		{"RFC 1918 10/8", []string{"10.0.0.0", "10.255.255.255"}, []string{"9.255.255.255", "11.0.0.0"}},
		{"RFC 1918 172.16/12", []string{"172.16.0.0", "172.31.255.255"}, []string{"172.15.255.255", "172.32.0.0"}},
		{"RFC 1918 192.168/16", []string{"192.168.0.0", "192.168.255.255"}, []string{"192.167.255.255", "192.169.0.0"}},
		{"CGNAT 100.64/10", []string{"100.64.0.0", "100.127.255.255"}, []string{"100.63.255.255", "100.128.0.0"}},
		{"回环 127/8", []string{"127.0.0.0", "127.255.255.255"}, []string{"126.255.255.255", "128.0.0.0"}},
		{"链路本地 169.254/16", []string{"169.254.0.0", "169.254.255.255"}, []string{"169.253.255.255", "169.255.0.0"}},
		{"本网络 0/8，含未指定", []string{"0.0.0.0", "0.255.255.255"}, []string{"1.0.0.0"}},
		{"IETF 协议分配 192.0.0/24", []string{"192.0.0.0", "192.0.0.8", "192.0.0.11", "192.0.0.255"}, []string{"191.255.255.255", "192.0.1.0"}},
		{"192.0.0/24 里全球可达的 PCP 与 TURN 任播", nil, []string{"192.0.0.9", "192.0.0.10"}},
		{"文档 192.0.2/24", []string{"192.0.2.0", "192.0.2.255"}, []string{"192.0.1.255", "192.0.3.0"}},
		{"6a44 中继任播 192.88.99.2；所在的已废弃 192.88.99.0/24 按公网", []string{"192.88.99.2"}, []string{"192.88.99.1", "192.88.99.3"}},
		{"基准测试 198.18/15", []string{"198.18.0.0", "198.19.255.255"}, []string{"198.17.255.255", "198.20.0.0"}},
		{"文档 198.51.100/24", []string{"198.51.100.0", "198.51.100.255"}, []string{"198.51.99.255", "198.51.101.0"}},
		{"文档 203.0.113/24", []string{"203.0.113.0", "203.0.113.255"}, []string{"203.0.112.255", "203.0.114.0"}},
		{"组播 224/4", []string{"224.0.0.0", "224.0.0.251", "239.255.255.255"}, []string{"223.255.255.255"}},
		{"保留 240/4，含受限广播", []string{"240.0.0.0", "255.255.255.254", "255.255.255.255"}, nil},
		{"IPv4 映射的 IPv6 按 IPv4 判定", []string{"::ffff:10.0.0.1", "::ffff:100.64.0.1"}, []string{"::ffff:1.1.1.1"}},
		{"回环 ::1", []string{"::1"}, nil},
		{"未指定 ::", []string{"::"}, nil},
		{"ULA fc00::/7", []string{"fc00::", "fdff:ffff" + max}, []string{"fbff:ffff" + max, "fe00::"}},
		{"链路本地 fe80::/10", []string{"fe80::", "febf:ffff" + max}, []string{"fe7f:ffff" + max}},
		{"废弃的站点本地 fec0::/10", []string{"fec0::", "feff:ffff" + max}, nil},
		{"组播 ff00::/8", []string{"ff00::", "ff02::1", "ffff:ffff" + max}, nil},
		{"本地 NAT64 64:ff9b:1::/48", []string{"64:ff9b:1::", "64:ff9b:1:ffff:ffff:ffff:ffff:ffff"}, []string{"64:ff9b:0:ffff:ffff:ffff:ffff:ffff", "64:ff9b:2::"}},
		{"NAT64 知名前缀 64:ff9b::/96 按公网，内嵌私网 IPv4 也照查", nil, []string{"64:ff9b::808:808", "64:ff9b::a00:1"}},
		{"丢弃 100::/64", []string{"100::", "100::ffff:ffff:ffff:ffff"}, []string{"ff:ffff" + max}},
		{"Dummy IPv6 前缀 100:0:0:1::/64", []string{"100:0:0:1::", "100:0:0:1:ffff:ffff:ffff:ffff"}, []string{"100:0:0:2::"}},
		{"IETF 协议分配 2001::/23，含基准测试 2001:2::/48", []string{"2001:1::", "2001:1::4", "2001:2::", "2001:2:0:ffff:ffff:ffff:ffff:ffff", "2001:2:1::", "2001:10::", "2001:1ff" + max},
			[]string{"2000:ffff" + max, "2001:200::"}},
		{"Teredo 2001::/32 按公网", nil, []string{"2001::", "2001:0" + max}},
		{"2001::/23 里登记表标为全球可达的分配；两侧紧邻的仍属 2001::/23",
			[]string{"2001:2" + max, "2001:4::", "2001:4:111:ffff:ffff:ffff:ffff:ffff", "2001:4:113::", "2001:1f" + max, "2001:40::"},
			[]string{"2001:1::1", "2001:1::2", "2001:1::3", "2001:3::", "2001:3" + max, "2001:4:112::", "2001:4:112:ffff:ffff:ffff:ffff:ffff",
				"2001:20::", "2001:2f" + max, "2001:30::", "2001:3f" + max}},
		{"文档 2001:db8::/32", []string{"2001:db8::", "2001:db8" + max}, []string{"2001:db7" + max, "2001:db9::"}},
		{"6to4 2002::/16 按公网，内嵌私网 IPv4 也照查", nil, []string{"2002:808:808::1", "2002:a00:1::1"}},
		{"文档 3fff::/20", []string{"3fff::", "3fff:fff" + max}, []string{"3ffe:ffff" + max, "3fff:1000::"}},
		{"SRv6 SID 5f00::/16", []string{"5f00::", "5f00:ffff" + max}, []string{"5eff:ffff" + max, "5f01::"}},
		{"公网", nil, []string{"8.8.8.8", "2606:4700::1111"}},
	} {
		for want, addrs := range map[bool][]string{false: c.not, true: c.public} {
			for _, a := range addrs {
				if got := netaddr.IsPublic(netip.MustParseAddr(a)); got != want {
					t.Errorf("%s: IsPublic(%s) = %v, want %v", c.class, a, got, want)
				}
			}
		}
	}
	if netaddr.IsPublic(netip.Addr{}) {
		t.Error("the zero Addr is public")
	}
}

// fakeService 是 geo.url 指向的假服务：记下每个请求，按 reply 应答。
type fakeService struct {
	srv   *httptest.Server
	mu    sync.Mutex
	reqs  []*http.Request
	reply func(w http.ResponseWriter, r *http.Request)
}

func (f *fakeService) paths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.reqs {
		out = append(out, r.URL.Path)
	}
	return out
}

type fixture struct {
	t     *testing.T
	clk   *clock.Fake
	st    *store.Store
	svc   *fakeService
	r     *Resolver
	logs  *syncBuffer
	ts    int64
	nodes map[string]int64
}

// syncBuffer 收集查询器的日志：假服务的处理函数与 Sweep 在不同协程里，写入要加锁。
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, clk: clock.NewFake(time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)), logs: &syncBuffer{}, nodes: map[string]int64{}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var err error
	f.st, err = store.Open(filepath.Join(t.TempDir(), "hub.db"), f.clk, log, store.MigrateSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.st.Close() })
	f.svc = &fakeService{reply: func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "US\n") }}
	f.svc.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.svc.mu.Lock()
		f.svc.reqs = append(f.svc.reqs, r)
		reply := f.svc.reply
		f.svc.mu.Unlock()
		reply(w, r)
	}))
	t.Cleanup(f.svc.srv.Close)
	// 服务地址一开始就指向假服务，开关保持从未保存过（即开）：任何出网都落在假服务上、被记下，不会打到默认的
	// 真实服务。要测"关"的用例显式保存为关。
	url := f.svc.srv.URL + "/{ip}/country"
	if err := f.saveGeo(t.Context(), store.GeoUpdate{URL: &url}); err != nil {
		t.Fatal(err)
	}
	f.restart()
	return f
}

// restart 换一个新的查询器，与 hub 重启后一样没有任何内存状态；库与假服务不变。
func (f *fixture) restart() {
	f.r = New(f.st, NewHTTP(outbound.NewClient(alert.NotifyTimeout)), f.clk, slog.New(slog.NewTextHandler(f.logs, nil)))
}

func (f *fixture) enable(on bool) {
	f.t.Helper()
	if err := f.saveGeo(f.t.Context(), store.GeoUpdate{Enabled: &on}); err != nil {
		f.t.Fatal(err)
	}
}

// saveGeo 改国家查询设置。假服务的处理函数里也用它来模拟"查询进行中运维改了设置"：那不是测试协程，不能 Fatal，
// 错误交给调用方报告。
func (f *fixture) saveGeo(ctx context.Context, u store.GeoUpdate) error {
	_, err := f.st.SaveSettings(ctx, store.SettingsUpdate{Geo: u})
	return err
}

// report 让节点以 addr 为来源上报一次并刷出：与 ingest 的分钟刷出走同一个写入口。
func (f *fixture) report(name, addr string) int64 {
	f.t.Helper()
	id, ok := f.nodes[name]
	if !ok {
		var err error
		id, _, err = f.st.CreateNode(f.t.Context(), store.NewNode{Name: name}, []byte(name))
		if err != nil {
			f.t.Fatal(err)
		}
		f.nodes[name] = id
	}
	f.ts += 60
	row := metric.Row{NodeID: id, TS: f.ts, CoverageStart: f.ts, Bucket: metric.NewBucket(), LastSeen: f.clk.Now(), Source: addr}
	if _, err := f.st.WriteMinuteBatch(f.t.Context(), metric.Batch{Rows: []metric.Row{row}}); err != nil {
		f.t.Fatal(err)
	}
	return id
}

func (f *fixture) sweep() {
	f.t.Helper()
	if err := f.r.Sweep(f.t.Context()); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) country(id int64) (country, ip string) {
	f.t.Helper()
	n, err := f.st.GetNode(f.t.Context(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	return n.Country, n.CountryIP
}

func (f *fixture) wantRequests(want ...string) {
	f.t.Helper()
	if got := f.svc.paths(); !slices.Equal(got, want) {
		f.t.Fatalf("requests = %q, want %q", got, want)
	}
}

func (f *fixture) wantCountry(id int64, country, ip string) {
	f.t.Helper()
	if c, i := f.country(id); c != country || i != ip {
		f.t.Fatalf("node %d country = %q for %q, want %q for %q", id, c, i, country, ip)
	}
}

// 开启后公网地址被查且只查一次：应答去掉换行后写入，与所查地址成对；有了答案之后不再查，退避期过了也不查。
// 请求只带地址：GET、无请求体、没有凭据头。
func TestLookupQueriesAPublicAddressOnce(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	id := f.report("a", "8.8.8.8")
	f.sweep()
	f.wantRequests("/8.8.8.8/country")
	f.wantCountry(id, "US", "8.8.8.8")
	r := f.svc.reqs[0]
	if r.Method != http.MethodGet || r.ContentLength != 0 || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
		t.Fatalf("request carried more than the address: %s %v length %d", r.Method, r.Header, r.ContentLength)
	}
	f.sweep()
	f.clk.Advance(2 * time.Hour)
	f.report("a", "8.8.8.8")
	f.sweep()
	f.wantRequests("/8.8.8.8/country")
	f.wantCountry(id, "US", "8.8.8.8")
}

// 只接受 200 且去掉首尾空白后恰为两个大写字母的应答，其余按失败：不写国家，记一小时退避。一小时写字面值，
// 不引用常量：常量改了而 spec 没改，这里要红。302 不跟随：跳转目标即使应答 US 也不被请求。203 是 2xx，正文也合法，
// 只有"只认 200"让它失败。"超长"是 US 后接 100 个空白：读完再去空白就是合法的 US，只有限读让它失败。
func TestLookupFailuresBackOffForAnHour(t *testing.T) {
	for _, c := range []struct {
		name  string
		reply func(w http.ResponseWriter, r *http.Request)
	}{
		{"三个字母", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "usa") }},
		{"小写", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "us") }},
		{"HTML", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "<html><body>US</body></html>") }},
		{"302", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/moved" {
				io.WriteString(w, "US")
				return
			}
			http.Redirect(w, r, "/moved", http.StatusFound)
		}},
		{"500 带合法正文", func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "US", http.StatusInternalServerError) }},
		{"203 带合法正文", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNonAuthoritativeInfo)
			io.WriteString(w, "US")
		}},
		{"超长", func(w http.ResponseWriter, _ *http.Request) {
			io.WriteString(w, "US"+strings.Repeat(" ", 50)+strings.Repeat("\n", 50))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			f.svc.reply = c.reply
			f.enable(true)
			id := f.report("a", "8.8.8.8")
			f.sweep()
			f.wantRequests("/8.8.8.8/country")
			f.wantCountry(id, "", "")
			f.clk.Advance(time.Hour - time.Second)
			f.sweep()
			f.wantRequests("/8.8.8.8/country")
			f.clk.Advance(time.Second)
			f.sweep()
			f.wantRequests("/8.8.8.8/country", "/8.8.8.8/country")
		})
	}
}

// 地址变了：刷出的同一条语句清空查得的国家（不等查询器），下一轮按新地址重查。同一地址再次刷出、或那次取不到
// 对端（空串）不清空。
func TestAddressChangeClearsAndRequeries(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	id := f.report("a", "8.8.8.8")
	f.sweep()
	f.wantCountry(id, "US", "8.8.8.8")
	f.report("a", "8.8.8.8")
	f.report("a", "")
	f.wantCountry(id, "US", "8.8.8.8")
	f.report("a", "1.1.1.1")
	f.wantCountry(id, "", "")
	f.svc.reply = func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, " AU \r\n") }
	f.sweep()
	f.wantRequests("/8.8.8.8/country", "/1.1.1.1/country")
	f.wantCountry(id, "AU", "1.1.1.1")
}

// 迟到的应答：查询发出之后、应答到达之前节点换了地址，对旧地址的答案不写；下一轮按新地址查得并写入。
func TestLateAnswerForTheOldAddressIsNotWritten(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	id := f.report("a", "8.8.8.8")
	f.svc.reply = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/8.8.8.8/country" {
			f.report("a", "1.1.1.1")
			io.WriteString(w, "US")
			return
		}
		io.WriteString(w, "AU")
	}
	f.sweep()
	f.wantRequests("/8.8.8.8/country")
	f.wantCountry(id, "", "")
	if logs := f.logs.String(); !strings.Contains(logs, "node address changed") || strings.Contains(logs, "node deleted") {
		t.Fatalf("dropped answer logged as %q, want the address change", logs)
	}
	f.sweep()
	f.wantRequests("/8.8.8.8/country", "/1.1.1.1/country")
	f.wantCountry(id, "AU", "1.1.1.1")
}

// 查询发出之后节点被删除：答案丢掉、日志写明是节点已删而不是换了地址，这一轮照常处理后面的节点。
func TestNodeDeletedWhileQueryingIsSkipped(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	gone := f.report("a", "8.8.8.8")
	kept := f.report("b", "1.1.1.1")
	f.svc.reply = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/8.8.8.8/country" {
			if err := f.st.DeleteNode(r.Context(), gone); err != nil {
				t.Error(err)
			}
		}
		io.WriteString(w, "US")
	}
	f.sweep()
	f.wantRequests("/8.8.8.8/country", "/1.1.1.1/country")
	f.wantCountry(kept, "US", "1.1.1.1")
	if logs := f.logs.String(); !strings.Contains(logs, "node deleted") || strings.Contains(logs, "node address changed") {
		t.Fatalf("dropped answer logged as %q, want the deletion", logs)
	}
}

// 查询关闭（显式保存为关、开启后又关闭）时不出网，假服务一个请求都收不到。开启的那一轮先证明这个节点确实会被查。
func TestDisabledLookupMakesNoRequests(t *testing.T) {
	f := newFixture(t)
	f.enable(false)
	f.report("a", "8.8.8.8")
	f.sweep()
	f.wantRequests()
	f.enable(true)
	f.report("b", "1.1.1.1")
	f.sweep()
	f.wantRequests("/8.8.8.8/country", "/1.1.1.1/country")
	f.enable(false)
	f.report("a", "9.9.9.9")
	f.report("b", "2606:4700::1111")
	f.clk.Advance(2 * time.Hour)
	f.sweep()
	f.wantRequests("/8.8.8.8/country", "/1.1.1.1/country")
}

// 从未保存过开关时查询是开的：不经任何设置，公网地址的节点就被查到国家。与上一个用例的"保存为关即不查"成对。
func TestNeverSavedSettingsLookUp(t *testing.T) {
	f := newFixture(t)
	n := f.report("a", "8.8.8.8")
	f.sweep()
	f.wantRequests("/8.8.8.8/country")
	f.wantCountry(n, "US", "8.8.8.8")
}

// 非公网地址不发出查询。同一轮里的公网节点被查，证明这一轮确实跑了、零请求不是因为查询器根本没运行。
func TestNonPublicAddressesMakeNoRequests(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	var private []int64
	for _, addr := range []string{"10.0.0.1", "172.16.0.1", "192.168.1.1", "100.64.0.1", "127.0.0.1", "::1", "169.254.0.1", "fe80::1", "fd00::1", "203.0.113.7", "2001:db8::1"} {
		private = append(private, f.report(addr, addr))
	}
	f.report("public", "8.8.8.8")
	f.sweep()
	f.wantRequests("/8.8.8.8/country")
	for _, id := range private {
		f.wantCountry(id, "", "")
	}
}

// hub 重启后尚无答案的地址重查一次（退避只在内存），已有答案的不查；重查仍失败则重新退避。
func TestRestartRequeriesUnansweredAddresses(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.svc.reply = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/1.1.1.1/country" {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, "US")
	}
	answered := f.report("a", "8.8.8.8")
	f.report("b", "1.1.1.1")
	f.sweep()
	f.wantRequests("/8.8.8.8/country", "/1.1.1.1/country")
	f.sweep()
	f.wantRequests("/8.8.8.8/country", "/1.1.1.1/country")
	f.restart()
	f.sweep()
	f.wantRequests("/8.8.8.8/country", "/1.1.1.1/country", "/1.1.1.1/country")
	f.sweep()
	f.wantRequests("/8.8.8.8/country", "/1.1.1.1/country", "/1.1.1.1/country")
	f.wantCountry(answered, "US", "8.8.8.8")
}

// 手动指定的国家与查得值互不覆盖：查询只写查得的两列，pin 原样保留；UpdateNode 设或清 pin 不动查得的两列。
func TestLookupAndPinDoNotOverwriteEachOther(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	id := f.report("a", "8.8.8.8")
	edit := func(pin string) {
		t.Helper()
		if _, err := f.st.UpdateNode(t.Context(), id, store.NodeEdit{Name: "a", TrafficResetDay: 1, CountryPin: pin}); err != nil {
			t.Fatal(err)
		}
	}
	edit("JP")
	f.sweep()
	n, err := f.st.GetNode(t.Context(), id)
	if err != nil || n.CountryPin != "JP" || n.Country != "US" || n.CountryIP != "8.8.8.8" {
		t.Fatalf("after lookup with a pin: %+v %v", n, err)
	}
	edit("")
	f.wantCountry(id, "US", "8.8.8.8")
}

// 一轮之中关掉开关：这一轮不再发出任何请求（开关在每次外呼前重读），关闭前在途的那一个照常完成并写入。
func TestDisablingMidSweepStopsFurtherRequests(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	var ids []int64
	for _, addr := range []string{"8.8.8.8", "8.8.4.4", "1.1.1.1", "1.0.0.1", "9.9.9.9"} {
		ids = append(ids, f.report(addr, addr))
	}
	off := false
	f.svc.reply = func(w http.ResponseWriter, r *http.Request) {
		if err := f.saveGeo(r.Context(), store.GeoUpdate{Enabled: &off}); err != nil {
			t.Error(err)
		}
		io.WriteString(w, "US")
	}
	f.sweep()
	f.wantRequests("/8.8.8.8/country")
	f.wantCountry(ids[0], "US", "8.8.8.8")
	for _, id := range ids[1:] {
		f.wantCountry(id, "", "")
	}
}

// 一轮之中改服务地址：这一轮之后的请求走新地址（服务地址在每次外呼前重读），不再发往旧地址。
func TestURLChangeMidSweepTakesEffectForTheNextRequest(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	for _, addr := range []string{"8.8.8.8", "1.1.1.1", "9.9.9.9"} {
		f.report(addr, addr)
	}
	other := f.svc.srv.URL + "/other/{ip}"
	f.svc.reply = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/8.8.8.8/country" {
			if err := f.saveGeo(r.Context(), store.GeoUpdate{URL: &other}); err != nil {
				t.Error(err)
			}
		}
		io.WriteString(w, "US")
	}
	f.sweep()
	f.wantRequests("/8.8.8.8/country", "/other/1.1.1.1", "/other/9.9.9.9")
}

// 一轮之中，同一次保存里关掉开关并把服务地址换成 B：这之后一个请求都不发往 B，原服务也只收到保存之前发出的那一个。
// B 从未与"开启"一起保存过，发往 B 就是把节点地址交给运维没有授权过的服务：一次外呼用的开关与服务地址必须出自同一次
// 读取。两条断言分开写：发往 B 说明开关与地址来自两次读取，原服务多收说明开关没在每次外呼前重读。
func TestDisablingAndSwitchingServiceInOneSaveSendsNothingToTheNewService(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	for _, addr := range []string{"8.8.8.8", "1.1.1.1"} {
		f.report(addr, addr)
	}
	off, other := false, f.svc.srv.URL+"/other/{ip}"
	f.svc.reply = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/8.8.8.8/country" {
			if err := f.saveGeo(r.Context(), store.GeoUpdate{Enabled: &off, URL: &other}); err != nil {
				t.Error(err)
			}
		}
		io.WriteString(w, "US")
	}
	f.sweep()
	var original, switched []string
	for _, p := range f.svc.paths() {
		if strings.HasPrefix(p, "/other/") {
			switched = append(switched, p)
		} else {
			original = append(original, p)
		}
	}
	if len(switched) != 0 {
		t.Errorf("requests to the service saved together with the switch off = %q, want none", switched)
	}
	if !slices.Equal(original, []string{"/8.8.8.8/country"}) {
		t.Errorf("requests to the original service = %q, want only the one sent before the save", original)
	}
}

// saveFirst 在把查询转交给真实后端之前先保存一次设置：保存因此落在查询器读完设置之后、后端发出之前，不依赖 Sweep
// 内部的调用顺序。
type saveFirst struct {
	Backend
	t    *testing.T
	save func(context.Context) error
}

func (b saveFirst) Lookup(ctx context.Context, s store.GeoSettings, addr netip.Addr) (string, error) {
	if err := b.save(ctx); err != nil {
		b.t.Error(err)
	}
	return b.Backend.Lookup(ctx, s, addr)
}

// 运维恰在查询器读完设置、HTTP 后端发出之前保存"关闭 + 换成 B"：这一个请求仍发往与"开启"一起读到的原服务（不变式
// 允许的"刚读完设置、正要发出的那一个"），不发往 B。后端若另读设置，就会拿着这次保存的 B 发出。
func TestHTTPBackendSendsToTheServiceReadWithTheSwitch(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	id := f.report("a", "8.8.8.8")
	off, other := false, f.svc.srv.URL+"/other/{ip}"
	f.r.backend = saveFirst{Backend: f.r.backend, t: t, save: func(ctx context.Context) error {
		return f.saveGeo(ctx, store.GeoUpdate{Enabled: &off, URL: &other})
	}}
	f.sweep()
	f.wantRequests("/8.8.8.8/country")
	f.wantCountry(id, "US", "8.8.8.8")
}

// saveOnFirstService 在 Sweep 第一次取退避键的服务一项时保存一次设置。Sweep 重读设置之后、调用 Lookup 之前先调用
// Service，保存因此落在"Sweep 读完设置"与"交给后端"之间；Lookup 里核对这个先后，先后变了就不再是这个窗口。
type saveOnFirstService struct {
	Backend
	t     *testing.T
	save  func() error
	saved bool
}

func (b *saveOnFirstService) Service(s store.GeoSettings) string {
	if !b.saved {
		b.saved = true
		if err := b.save(); err != nil {
			b.t.Error(err)
		}
	}
	return b.Backend.Service(s)
}

func (b *saveOnFirstService) Lookup(ctx context.Context, s store.GeoSettings, addr netip.Addr) (string, error) {
	if !b.saved {
		b.t.Error("fixture: Lookup ran before Service, the save does not fall between reading and handing over")
	}
	return b.Backend.Lookup(ctx, s, addr)
}

// 运维恰在 Sweep 重读设置之后、把这一份交给后端之前保存"关闭 + 换成 B"：B 从未与"开启"一起被读到，一个请求都不能
// 发往它；至多还有发往原服务的那一个。Sweep 若在交给后端之前再读一次设置、按旧的那份判定准入却把新读的那份交给
// 后端，就会拿着这次保存的 B 发出。按新读的那份重新判定准入（读到关闭就不发）不违反这条，所以不断言原服务一定收到。
func TestSweepHandsTheBackendTheSettingsItAdmittedWith(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.report("a", "8.8.8.8")
	off, other := false, f.svc.srv.URL+"/other/{ip}"
	b := &saveOnFirstService{Backend: f.r.backend, t: t, save: func() error {
		return f.saveGeo(t.Context(), store.GeoUpdate{Enabled: &off, URL: &other})
	}}
	f.r.backend = b
	f.sweep()
	if !b.saved {
		t.Fatal("fixture: Sweep never asked for the service, the save did not happen")
	}
	for _, p := range f.svc.paths() {
		if p != "/8.8.8.8/country" {
			t.Errorf("request %q after the save, want at most the one to the service read together with the switch on", p)
		}
	}
}

// 退避的键含服务地址：旧服务失败留下的退避不挡住对新服务的查询，改了服务地址的下一轮即重查；旧服务的那一条随之丢掉。
func TestURLChangeLiftsTheOldServicesBackoff(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.svc.reply = func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/other/") {
			io.WriteString(w, "US")
			return
		}
		http.Error(w, "down", http.StatusServiceUnavailable)
	}
	id := f.report("a", "8.8.8.8")
	f.sweep()
	f.clk.Advance(time.Minute)
	f.sweep()
	f.wantRequests("/8.8.8.8/country")
	other := f.svc.srv.URL + "/other/{ip}"
	if err := f.saveGeo(t.Context(), store.GeoUpdate{URL: &other}); err != nil {
		t.Fatal(err)
	}
	f.sweep()
	f.wantRequests("/8.8.8.8/country", "/other/8.8.8.8")
	f.wantCountry(id, "US", "8.8.8.8")
	if len(f.r.retryAt) != 0 {
		t.Fatalf("backoff after the new service answered: %v, want empty", f.r.retryAt)
	}
}

// 退避表只留仍待查的条目：节点在不同地址上接连失败，表里始终只有当前地址的那一条，不随地址变化增长。
func TestBackoffTableKeepsOnlyTheCurrentAddress(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.svc.reply = func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "down", http.StatusServiceUnavailable) }
	for i, addr := range []string{"8.8.8.8", "8.8.4.4", "1.1.1.1", "2606:4700::1111"} {
		id := f.report("a", addr)
		f.sweep()
		if len(f.r.retryAt) != 1 || !f.r.backingOff(id, addr) {
			t.Fatalf("after failing on address %d (%s): backoff %v, want only that address", i+1, addr, f.r.retryAt)
		}
	}
}

// 节点删除后，它记住的答案与退避都在下一轮丢掉。
func TestDeletedNodeLeavesNoRememberedState(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.svc.reply = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/1.1.1.1/country" {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, "US")
	}
	answered := f.report("a", "8.8.8.8")
	failed := f.report("b", "1.1.1.1")
	f.sweep()
	if len(f.r.answers[answered]) != 1 || !f.r.backingOff(failed, "1.1.1.1") {
		t.Fatalf("before deletion: answers %v, backoff %v", f.r.answers, f.r.retryAt)
	}
	for _, id := range []int64{answered, failed} {
		if err := f.st.DeleteNode(t.Context(), id); err != nil {
			t.Fatal(err)
		}
	}
	f.sweep()
	if len(f.r.answers) != 0 || len(f.r.retryAt) != 0 {
		t.Fatalf("after deletion: answers %v, backoff %v, want both empty", f.r.answers, f.r.retryAt)
	}
}

// 最近几个地址之内不重复外呼：v4 与 v6 交替上报时两个地址各查一次，之后的切换由记住的答案直接写回、不外呼。hub 重启后
// 记住的答案清空，两个地址各重查一次：库里那一对只属于当前地址，节点一换走就清空了。
func TestAlternatingAddressesQueryEachAddressOnce(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.svc.reply = func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, ":") {
			io.WriteString(w, "CA")
			return
		}
		io.WriteString(w, "US")
	}
	want := map[string]string{"8.8.8.8": "US", "2606:4700::1111": "CA"}
	alternate := func(times int) {
		t.Helper()
		for i := range times {
			addr := []string{"8.8.8.8", "2606:4700::1111"}[i%2]
			id := f.report("a", addr)
			f.sweep()
			f.wantCountry(id, want[addr], addr)
		}
	}
	alternate(6)
	f.wantRequests("/8.8.8.8/country", "/2606:4700::1111/country")
	f.restart()
	alternate(6)
	f.wantRequests("/8.8.8.8/country", "/2606:4700::1111/country", "/8.8.8.8/country", "/2606:4700::1111/country")
}

// 记住的答案每节点至多 4 个地址，按最近用到保留：命中的地址留下，最久未用的被挤掉、再来时重查。4 写字面值，
// 不引用常量：常量改了，这里要红。
func TestRememberedAnswersAreBoundedPerNode(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	visit := func(addr string) {
		t.Helper()
		id := f.report("a", addr)
		f.sweep()
		f.wantCountry(id, "US", addr)
	}
	for _, addr := range []string{"1.0.0.1", "1.0.0.2", "1.0.0.3", "1.0.0.4", "1.0.0.5"} {
		visit(addr)
	}
	visit("1.0.0.2") // 命中，移到最前；1.0.0.1 已被挤掉
	visit("1.0.0.1") // 未命中，重查；挤掉此刻最久未用的 1.0.0.3，不是刚命中的 1.0.0.2
	visit("1.0.0.2")
	visit("1.0.0.3")
	f.wantRequests("/1.0.0.1/country", "/1.0.0.2/country", "/1.0.0.3/country", "/1.0.0.4/country", "/1.0.0.5/country",
		"/1.0.0.1/country", "/1.0.0.3/country")
	if n := len(f.r.answers[f.nodes["a"]]); n != 4 {
		t.Fatalf("remembered %d addresses, want 4", n)
	}
}

// 退避按单调钟计：墙钟向前拨过一小时不提前重查，向后拨也不把退避拖长。
func TestBackoffFollowsTheMonotonicClock(t *testing.T) {
	f := newFixture(t)
	f.enable(true)
	f.svc.reply = func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "down", http.StatusServiceUnavailable) }
	f.report("a", "8.8.8.8")
	f.sweep()
	f.clk.SetWall(f.clk.Now().Add(2 * time.Hour))
	f.sweep()
	f.wantRequests("/8.8.8.8/country")
	f.clk.SetWall(f.clk.Now().Add(-24 * time.Hour))
	f.clk.Advance(time.Hour)
	f.sweep()
	f.wantRequests("/8.8.8.8/country", "/8.8.8.8/country")
}

// backingOff 报告（节点, 地址）在任一服务地址下是否有退避条目，供用例检查退避表。
func (r *Resolver) backingOff(node int64, addr string) bool {
	for k := range r.retryAt {
		if k.node == node && k.addr == addr {
			return true
		}
	}
	return false
}

// 对外文字写出答案表的上界：agent 读的 proto 注释（Settings.geo_enabled）要写明"最近用过的 N 个地址"与"超过 N 个
// 地址轮换会再查"，N 与 answersPerNode 一致。常量改了而注释没改，这里红；面板那一侧由 web 的 countryLimits.test.ts 对照。
func TestExternalTextsStateTheAnswerBound(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "..", "proto", "heron", "v1", "admin.proto"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(src), "\n")
	i := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, "optional bool geo_enabled") })
	if i < 0 {
		t.Fatal("admin.proto has no geo_enabled field")
	}
	var comment []string
	for j := i - 1; j >= 0 && strings.HasPrefix(strings.TrimSpace(lines[j]), "//"); j-- {
		comment = append([]string{strings.TrimPrefix(strings.TrimSpace(lines[j]), "// ")}, comment...)
	}
	text := strings.Join(comment, "")
	for _, want := range []string{fmt.Sprintf("最近用过的 %d 个地址", answersPerNode), fmt.Sprintf("超过 %d 个地址轮换时被挤出的地址会再查", answersPerNode)} {
		if !strings.Contains(text, want) {
			t.Errorf("Settings.geo_enabled comment lacks %q:\n%s", want, text)
		}
	}
}
