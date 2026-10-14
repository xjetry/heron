package api

import (
	"bytes"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/metric"
)

// pubResult 是一次原样 HTTP 调用的结果：公开服务的断言常要比较整段响应字节与响应头。
type pubResult struct {
	status int
	header http.Header
	body   []byte
}

// rawClient 不带 cookie，也不替调用方协商压缩：默认 Transport 会自动加 Accept-Encoding: gzip 并解压、
// 删掉 Content-Encoding，测试就看不到服务端实际发了什么。
var rawClient = &http.Client{Transport: &http.Transport{DisableCompression: true}}

func jsonQuery(msg string) string { return "connect=v1&encoding=json&message=" + url.QueryEscape(msg) }

func pubDo(t *testing.T, req *http.Request, header map[string]string) pubResult {
	t.Helper()
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := rawClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return pubResult{status: resp.StatusCode, header: resp.Header, body: body}
}

// pubGet 用 curl 同款的 Connect GET 形态调公开服务，不带任何凭据；query 是已编码的查询串。
func pubGet(t *testing.T, h *harness, method, query string, header map[string]string) pubResult {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.srv.URL+"/heron.v1.PublicService/"+method+"?"+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	return pubDo(t, req, header)
}

// pubPost 以 JSON POST 调公开服务；header 里的 Content-Type 覆盖默认值。
func pubPost(t *testing.T, h *harness, method, body string, header map[string]string) pubResult {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+"/heron.v1.PublicService/"+method, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	return pubDo(t, req, header)
}

// 未公开与不存在的节点得到同一个响应：状态码、正文与响应头（除 Date）都相同，错误里没有 id。
// 错误 metadata 走响应头，只比正文看不到按分支加的头。
func TestPublicHistoryTreatsPrivateAndMissingNodesAlike(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	pub, _ := h.createNode(t, "pub")
	priv, _ := h.createNode(t, "priv")
	h.setPublic(t, pub, "pub", true)
	h.setPublic(t, priv, "priv", false)
	window := func(id int64) string { return fmt.Sprintf(`{"nodeId":"%d","from":"0","to":"3600"}`, id) }
	for _, method := range []string{"QueryMetrics", "QueryProbes"} {
		if got := pubGet(t, h, method, jsonQuery(window(pub)), nil); got.status != http.StatusOK {
			t.Fatalf("%s on a public node: %d %s", method, got.status, got.body)
		}
		private := pubGet(t, h, method, jsonQuery(window(priv)), nil)
		missing := pubGet(t, h, method, jsonQuery(window(999999)), nil)
		if private.status != http.StatusNotFound || missing.status != private.status || !bytes.Equal(private.body, missing.body) {
			t.Fatalf("%s: private %d %s, missing %d %s", method, private.status, private.body, missing.status, missing.body)
		}
		ph, mh := private.header.Clone(), missing.header.Clone()
		ph.Del("Date")
		mh.Del("Date")
		if !reflect.DeepEqual(ph, mh) {
			t.Fatalf("%s: private headers %v, missing headers %v", method, ph, mh)
		}
		if !bytes.Contains(private.body, []byte(`"node_id: no public node has this id"`)) {
			t.Fatalf("%s: body %s", method, private.body)
		}
	}
}

// 匿名请求的解码上限是 publicMaxBody：GET 的 message 与 POST 正文都按它计，恰好满额照常处理，多一个字节即
// resource_exhausted。填充用 JSON 空白，请求本身合法，拒绝只能来自大小。
func TestPublicDecodeBudget(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	padded := func(n int) string { return "{" + strings.Repeat(" ", n-2) + "}" }
	for _, c := range []struct {
		size int
		want int
	}{{publicMaxBody, http.StatusOK}, {publicMaxBody + 1, http.StatusTooManyRequests}} {
		for name, got := range map[string]pubResult{
			"GET":  pubGet(t, h, "GetSite", jsonQuery(padded(c.size)), nil),
			"POST": pubPost(t, h, "GetSite", padded(c.size), nil),
		} {
			if got.status != c.want || (c.want != http.StatusOK && !bytes.Contains(got.body, []byte(`"code":"resource_exhausted"`))) {
				t.Fatalf("%s with a %d-byte message: %d %s, want %d", name, c.size, got.status, got.body, c.want)
			}
		}
	}
}

func TestPublicHistorySharesWindowValidation(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	pub, _ := h.createNode(t, "pub")
	priv, _ := h.createNode(t, "priv")
	h.setPublic(t, pub, "pub", true)
	h.setPublic(t, priv, "priv", false)
	client := h.publicClient()
	for _, tc := range []struct {
		name           string
		node, from, to int64
		max            uint32
		code           connect.Code
		text           string
	}{
		{"negative", pub, -1, 60, 0, connect.CodeInvalidArgument, "from must be a nonnegative Unix timestamp; got -1"},
		{"order", pub, 60, 60, 0, connect.CodeInvalidArgument, "from (60) must be earlier than to (60)"},
		{"span", pub, 0, 401 * 86400, 0, connect.CodeInvalidArgument, "window spans 34646400 seconds; the maximum is 34560000 (400 days)"},
		{"points", pub, 0, 3600, 2001, connect.CodeInvalidArgument, "max_points must be at most 2000; got 2001"},
		{"private", priv, 0, 3600, 0, connect.CodeNotFound, "node_id: no public node has this id"},
		{"missing", 999, 0, 3600, 0, connect.CodeNotFound, "node_id: no public node has this id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, pe := client.QueryProbes(t.Context(), connect.NewRequest(&heronv1.QueryProbesRequest{NodeId: tc.node, From: tc.from, To: tc.to, MaxPoints: tc.max}))
			_, me := client.QueryMetrics(t.Context(), connect.NewRequest(&heronv1.QueryMetricsRequest{NodeId: tc.node, From: tc.from, To: tc.to, MaxPoints: tc.max}))
			for _, err := range []error{pe, me} {
				if codeOf(err) != tc.code || !strings.Contains(err.Error(), tc.text) {
					t.Errorf("error=%v want=%s %q", err, tc.code, tc.text)
				}
			}
		})
	}
}

func TestPublicSnapshotListsOnlyPublicNodesWithPublicFields(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	ctx := t.Context()
	a, tokA := h.createNode(t, "a")
	b, _ := h.createNode(t, "b")
	c, _ := h.createNode(t, "c")
	h.setPublic(t, a, "a", true)
	h.setPublic(t, b, "b", false)
	h.setPublic(t, c, "c", true)
	if _, err := h.admin.ReorderNodes(ctx, connect.NewRequest(&heronv1.ReorderNodesRequest{Ids: []int64{c, b, a}})); err != nil {
		t.Fatal(err)
	}
	facts := &heronv1.Facts{Hostname: "secret-host", Os: "Debian 12", Kernel: "6.1.0-secret", Arch: "amd64", Virtualization: "kvm",
		CpuModel: "EPYC", CpuCores: 4, AgentVersion: "v9.9.9-secret", IcmpAvailable: true}
	facts.Network = &heronv1.NetworkInfo{Ipv4: &heronv1.AddressDetection{State: heronv1.AddressDetectionState_ADDRESS_DETECTION_STATE_AVAILABLE, Address: "8.8.4.4", CheckedAt: 123}}
	if err := h.store.UpsertFacts(ctx, a, 1, facts); err != nil {
		t.Fatal(err)
	}
	adminNodes, err := h.admin.ListNodes(ctx, connect.NewRequest(&heronv1.ListNodesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var adminNetwork *heronv1.NetworkInfo
	for _, node := range adminNodes.Msg.Nodes {
		if node.Id == a {
			adminNetwork = node.GetFacts().GetNetwork()
		}
	}
	if !proto.Equal(adminNetwork, facts.Network) {
		t.Fatalf("admin network missing: %v", adminNetwork)
	}
	if err := h.report(t, tokA, &heronv1.Metrics{BootId: testBootID, CpuPct: proto.Float64(12.5), MemUsed: proto.Uint64(0), MemTotal: proto.Uint64(1 << 30)}); err != nil {
		t.Fatal(err)
	}
	resp, err := h.publicClient().GetSnapshot(ctx, connect.NewRequest(&heronv1.PublicServiceGetSnapshotRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	snap := resp.Msg
	if snap.GetNow() != h.clk.Now().Unix() || snap.GetReportIntervalMs() != 10000 || len(snap.GetNodes()) != 2 {
		t.Fatalf("snapshot = %v", snap)
	}
	first, second := snap.GetNodes()[0], snap.GetNodes()[1]
	if first.GetId() != c || second.GetId() != a || first.GetSortOrder() >= second.GetSortOrder() {
		t.Fatalf("nodes not in panel order: %v", snap.GetNodes())
	}
	if first.GetOnline() || first.LastSeenAt != nil || first.Facts != nil || first.Metrics != nil || first.Traffic == nil {
		t.Fatalf("never-reported node = %v", first)
	}
	wantFacts := &heronv1.PublicFacts{Os: "Debian 12", Arch: "amd64", Virtualization: "kvm", CpuModel: "EPYC", CpuCores: 4,
		Network: &heronv1.PublicNetworkInfo{Ipv4: &heronv1.PublicAddressDetection{State: heronv1.AddressDetectionState_ADDRESS_DETECTION_STATE_AVAILABLE}}}
	wantMetrics := &heronv1.PublicMetrics{CpuPct: proto.Float64(12.5), MemUsed: proto.Uint64(0), MemTotal: proto.Uint64(1 << 30)}
	if !second.GetOnline() || second.GetLastSeenAt() != h.clk.Now().Unix() || !proto.Equal(second.GetFacts(), wantFacts) ||
		!proto.Equal(second.GetMetrics(), wantMetrics) || second.Traffic == nil {
		t.Fatalf("reported node = %v", second)
	}
	// 正文层面再核一次：不公开的字段与私有节点的名字都不在 JSON 里；双栈出口只有状态，地址与探测时间不在。
	raw := pubGet(t, h, "GetSnapshot", jsonQuery("{}"), nil)
	if !bytes.Contains(raw.body, []byte(`"network":{"ipv4":{"state":"ADDRESS_DETECTION_STATE_AVAILABLE"}}`)) {
		t.Errorf("snapshot JSON lacks the address family state: %s", raw.body)
	}
	for _, leak := range []string{"secret", testBootID, "hostname", "kernel", "agentVersion", "icmpAvailable", "bootId", `"b"`, "8.8.4.4", "checkedAt", "address"} {
		if bytes.Contains(raw.body, []byte(leak)) {
			t.Errorf("snapshot JSON contains %s: %s", leak, raw.body)
		}
	}
}

func TestPublicSiteServesSavedSettings(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	client := h.publicClient()
	got, err := client.GetSite(t.Context(), connect.NewRequest(&heronv1.GetSiteRequest{}))
	if err != nil || !proto.Equal(got.Msg, &heronv1.PublicSite{Theme: "auto"}) {
		t.Fatalf("never saved: %v %v", got, err)
	}
	saveSettings(t, h, validSettings())
	got, err = client.GetSite(t.Context(), connect.NewRequest(&heronv1.GetSiteRequest{}))
	want := &heronv1.PublicSite{Title: "状态", Theme: "dark", AccentColor: "#112233", Logo: "data:image/png;base64,iVBORw0KGgo=", CustomCss: "body { color: red }"}
	if err != nil || !proto.Equal(got.Msg, want) {
		t.Fatalf("site = %v %v, want %v", got, err, want)
	}
}

// publicFields 是 PublicService 的响应能到达的每个消息的字段全集。往这些消息加字段就是公开给匿名访客，
// 必须同时改这份清单——与 access_test 的 readMethods 同一口径。共用的 Traffic 与历史查询类型同样在列：
// 给它们加字段也会出现在公开页。
var publicFields = map[protoreflect.FullName][]protoreflect.Name{
	"heron.v1.PublicSite":             {"title", "theme", "accent_color", "logo", "custom_css", "admin_path"},
	"heron.v1.PublicSnapshot":         {"now", "report_interval_ms", "nodes", "tags"},
	"heron.v1.PublicNode":             {"id", "name", "online", "last_seen_at", "sort_order", "facts", "metrics", "traffic", "billing", "country", "tags", "maintenance", "public_remark", "rented"},
	"heron.v1.PublicFacts":            {"os", "arch", "virtualization", "cpu_model", "cpu_cores", "network"},
	"heron.v1.PublicNetworkInfo":      {"ipv4", "ipv6"},
	"heron.v1.PublicAddressDetection": {"state"},
	"heron.v1.PublicBilling":          {"price", "currency", "billing_cycle", "expires_on", "days_left"},
	"heron.v1.PublicMetrics": {"cpu_pct", "cpu_steal_pct", "cpu_iowait_pct", "load1", "load5", "load15", "load1_per_core", "mem_total", "mem_used", "swap_total", "swap_used",
		"disk_total", "disk_used", "net_rx_total", "net_tx_total", "net_rx_bps", "net_tx_bps", "disk_read_bps", "disk_write_bps", "tcp_conns", "udp_conns", "procs", "uptime_s", "tcp_active_opens_ps", "tcp_passive_opens_ps", "tcp_out_segs_ps", "tcp_retrans_segs_ps"},
	"heron.v1.Traffic":              {"total_rx", "total_tx", "period_rx", "period_tx", "period_start", "next_reset_at", "reset_day", "quota_bytes", "quota_mode", "quota_used_bytes", "quota_used_pct"},
	"heron.v1.QueryMetricsResponse": {"level", "step_s", "ts", "series", "coverage", "coverage_summary"},
	"heron.v1.PointCoverage":        {"minutes", "observed", "observed_reported"},
	"heron.v1.CoverageSummary":      {"eligible_minutes", "observed_minutes", "observed_reported_minutes", "coverage_start"},
	"heron.v1.MetricSeries":         {"name", "unit", "samples"},
	"heron.v1.MetricSample":         {"n", "mean", "max", "sum"},
	"heron.v1.QueryProbesResponse":  {"level", "step_s", "series"},
	"heron.v1.ProbeSeries":          {"task_id", "samples", "kind", "target"},
	"heron.v1.ProbeSample":          {"ts", "sent", "lost", "errors", "rtt_mean_us", "rtt_min_us", "rtt_max_us"},

	"heron.v1.ListProbeComparisonNodesResponse": {"kind", "target", "node_ids", "max_nodes_per_query"},
	"heron.v1.QueryProbeComparisonResponse":     {"level", "step_s", "series", "unavailable_node_ids"},
	"heron.v1.NodeProbeSamples":                 {"node_id", "samples"},
}

func TestPublicResponsesExposeOnlyAllowlistedFields(t *testing.T) {
	t.Parallel()
	svc := heronv1.File_heron_v1_public_proto.Services().ByName("PublicService")
	seen := map[protoreflect.FullName]bool{}
	var walk func(md protoreflect.MessageDescriptor)
	walk = func(md protoreflect.MessageDescriptor) {
		if seen[md.FullName()] {
			return
		}
		seen[md.FullName()] = true
		var got []protoreflect.Name
		for i := 0; i < md.Fields().Len(); i++ {
			f := md.Fields().Get(i)
			got = append(got, f.Name())
			if f.Message() != nil {
				walk(f.Message())
			}
		}
		want, ok := publicFields[md.FullName()]
		if !ok {
			t.Errorf("%s is reachable from PublicService but not in publicFields; its fields are %v", md.FullName(), got)
			return
		}
		slices.Sort(got)
		if want = slices.Sorted(slices.Values(want)); !slices.Equal(got, want) {
			t.Errorf("fields of %s = %v, allowlist has %v", md.FullName(), got, want)
		}
	}
	if svc.Methods().Len() == 0 {
		t.Fatal("PublicService has no methods")
	}
	for i := 0; i < svc.Methods().Len(); i++ {
		walk(svc.Methods().Get(i).Output())
	}
	for name := range publicFields {
		if !seen[name] {
			t.Errorf("publicFields lists %s, which no PublicService response reaches", name)
		}
	}
}

// 公开节点的历史与管理端同一来源：对同一个请求两端的应答逐字段相同，公开端只多了"节点必须公开"这一道门。
// 任务仍分配给该节点时探测序列的标签两端也相同；撤下之后的分歧由 TestPublicProbeLabelsOnlyTasksAssignedToTheNode 钉住。
func TestPublicHistoryMatchesAdminForAPublicNode(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, _ := h.createNode(t, "pub")
	h.setPublic(t, id, "pub", true)
	saved, err := h.admin.SaveProbeTask(t.Context(), connect.NewRequest(&heronv1.SaveProbeTaskRequest{
		Task:    &heronv1.ProbeTask{Kind: heronv1.ProbeKind_PROBE_KIND_ICMP, Target: "192.0.2.1", IntervalS: 30, TimeoutMs: 1000},
		NodeIds: []int64{id},
	}))
	if err != nil {
		t.Fatal(err)
	}
	base := h.clk.Now().Truncate(time.Hour).Unix()
	b := metric.NewBucket()
	b.Add(&heronv1.Metrics{CpuPct: proto.Float64(7)})
	batch := metric.Batch{
		Rows:   []metric.Row{{NodeID: id, TS: base, CoverageStart: base, Bucket: b}},
		Probes: []metric.ProbeRow{{NodeID: id, TS: base, TaskID: saved.Msg.GetTask().GetTask().GetId(), Bucket: &metric.ProbeBucket{Sent: 2, Lost: 1}}},
	}
	if _, err := h.store.WriteMinuteBatch(t.Context(), batch); err != nil {
		t.Fatal(err)
	}
	pub := h.publicClient()
	metrics := &heronv1.QueryMetricsRequest{NodeId: id, From: base, To: base + 3600}
	pm, err := pub.QueryMetrics(t.Context(), connect.NewRequest(metrics))
	if err != nil {
		t.Fatal(err)
	}
	am, err := h.admin.QueryMetrics(t.Context(), connect.NewRequest(metrics))
	if err != nil {
		t.Fatal(err)
	}
	if len(am.Msg.GetTs()) != 1 || !proto.Equal(pm.Msg, am.Msg) {
		t.Errorf("public QueryMetrics = %v\nadmin QueryMetrics = %v", pm.Msg, am.Msg)
	}
	probes := &heronv1.QueryProbesRequest{NodeId: id, From: base, To: base + 3600}
	pp, err := pub.QueryProbes(t.Context(), connect.NewRequest(probes))
	if err != nil {
		t.Fatal(err)
	}
	ap, err := h.admin.QueryProbes(t.Context(), connect.NewRequest(probes))
	if err != nil {
		t.Fatal(err)
	}
	if len(ap.Msg.GetSeries()) != 1 || !proto.Equal(pp.Msg, ap.Msg) {
		t.Errorf("public QueryProbes = %v\nadmin QueryProbes = %v", pp.Msg, ap.Msg)
	}
}

// 公开端只标注当前分配给被查节点的任务：任务从公开节点撤下、目标改成只分配给私有节点的内网地址之后，
// 公开节点历史里的这条序列不带种类与目标；管理端照旧按任务当前的配置标注。
func TestPublicProbeLabelsOnlyTasksAssignedToTheNode(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	pub, _ := h.createNode(t, "pub")
	priv, _ := h.createNode(t, "priv")
	h.setPublic(t, pub, "pub", true)
	save := func(id uint64, target string, node int64) uint64 {
		t.Helper()
		resp, err := h.admin.SaveProbeTask(t.Context(), connect.NewRequest(&heronv1.SaveProbeTaskRequest{
			Task:    &heronv1.ProbeTask{Id: id, Kind: heronv1.ProbeKind_PROBE_KIND_ICMP, Target: target, IntervalS: 30, TimeoutMs: 1000},
			NodeIds: []int64{node},
		}))
		if err != nil {
			t.Fatal(err)
		}
		return resp.Msg.GetTask().GetTask().GetId()
	}
	kept := save(0, "192.0.2.1", pub)
	moved := save(0, "192.0.2.2", pub)
	base := h.clk.Now().Truncate(time.Hour).Unix()
	rows := []metric.ProbeRow{
		{NodeID: pub, TS: base, TaskID: kept, Bucket: &metric.ProbeBucket{Sent: 1, Lost: 1}},
		{NodeID: pub, TS: base, TaskID: moved, Bucket: &metric.ProbeBucket{Sent: 1, Lost: 1}},
	}
	if _, err := h.store.WriteMinuteBatch(t.Context(), metric.Batch{Probes: rows}); err != nil {
		t.Fatal(err)
	}
	save(moved, "10.0.0.5", priv)
	req := &heronv1.QueryProbesRequest{NodeId: pub, From: base, To: base + 3600}
	labels := func(series []*heronv1.ProbeSeries) map[uint64]string {
		out := map[uint64]string{}
		for _, s := range series {
			out[s.GetTaskId()] = fmt.Sprintf("%v %q", s.GetKind(), s.GetTarget())
		}
		return out
	}
	public, err := h.publicClient().QueryProbes(t.Context(), connect.NewRequest(req))
	if err != nil {
		t.Fatal(err)
	}
	admin, err := h.admin.QueryProbes(t.Context(), connect.NewRequest(req))
	if err != nil {
		t.Fatal(err)
	}
	wantPublic := map[uint64]string{kept: `PROBE_KIND_ICMP "192.0.2.1"`, moved: `PROBE_KIND_UNSPECIFIED ""`}
	wantAdmin := map[uint64]string{kept: `PROBE_KIND_ICMP "192.0.2.1"`, moved: `PROBE_KIND_ICMP "10.0.0.5"`}
	if got := labels(public.Msg.GetSeries()); !maps.Equal(got, wantPublic) {
		t.Errorf("public labels = %v, want %v", got, wantPublic)
	}
	if got := labels(admin.Msg.GetSeries()); !maps.Equal(got, wantAdmin) {
		t.Errorf("admin labels = %v, want %v", got, wantAdmin)
	}
}

func TestPublicRateLimitPerSourceAddress(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "127.0.0.0/8")
	site := jsonQuery("{}")
	from := func(ip string) map[string]string { return map[string]string{"X-Forwarded-For": ip} }
	// 60 与 100ms 是公开限流的字面值（桶容量 60、每秒补充 10），不引用常量：常量改了，这里要红。
	for i := range 60 {
		if got := pubGet(t, h, "GetSite", site, from("198.51.100.7")); got.status != http.StatusOK {
			t.Fatalf("request %d within the burst: %d %s", i+1, got.status, got.body)
		}
	}
	over := pubGet(t, h, "GetSite", site, from("198.51.100.7"))
	if over.status != http.StatusTooManyRequests || !bytes.Contains(over.body, []byte(`"code":"resource_exhausted"`)) || over.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("request past the burst: %d %v %s", over.status, over.header, over.body)
	}
	if got := pubGet(t, h, "GetSite", site, from("198.51.100.8")); got.status != http.StatusOK {
		t.Fatalf("another source shares the bucket: %d", got.status)
	}
	h.clk.Advance(100 * time.Millisecond)
	if got := pubGet(t, h, "GetSite", site, from("198.51.100.7")); got.status != http.StatusOK {
		t.Fatalf("after one refill period: %d", got.status)
	}
	if got := pubGet(t, h, "GetSite", site, from("198.51.100.7")); got.status != http.StatusTooManyRequests {
		t.Fatalf("second request after one refill period: %d", got.status)
	}
	// 解码失败的请求同样计数：限流在 connect 之外裁决。先确认这种请求确实在解码阶段失败。
	for i := range 60 {
		got := pubGet(t, h, "GetSite", "connect=v1&encoding=json&message=%7B", from("198.51.100.9"))
		if i == 0 && (got.status != http.StatusBadRequest || !bytes.Contains(got.body, []byte(`"code":"invalid_argument"`))) {
			t.Fatalf("malformed request: %d %s, want 400 invalid_argument from the decoder", got.status, got.body)
		}
	}
	if got := pubGet(t, h, "GetSite", site, from("198.51.100.9")); got.status != http.StatusTooManyRequests {
		t.Fatalf("malformed requests were not counted: %d", got.status)
	}
}

// 对端不是可信代理时 X-Forwarded-For 不被采信：改这个头换不了桶。
func TestPublicRateLimitIgnoresForwardedForFromUntrustedPeers(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	for i := range 60 {
		pubGet(t, h, "GetSite", jsonQuery("{}"), map[string]string{"X-Forwarded-For": fmt.Sprintf("198.51.100.%d", i+1)})
	}
	if got := pubGet(t, h, "GetSite", jsonQuery("{}"), map[string]string{"X-Forwarded-For": "203.0.113.1"}); got.status != http.StatusTooManyRequests {
		t.Fatalf("spoofed X-Forwarded-For got a fresh bucket: %d", got.status)
	}
}

func TestPublicCacheControlPerMethod(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	pub, _ := h.createNode(t, "pub")
	h.setPublic(t, pub, "pub", true)
	window := fmt.Sprintf(`{"nodeId":"%d","from":"0","to":"3600"}`, pub)
	for _, c := range []struct{ method, body, want string }{
		{"GetSite", "{}", "max-age=300"},
		{"GetSnapshot", "{}", "max-age=1"},
		{"QueryMetrics", window, "max-age=60"},
		{"QueryProbes", window, "max-age=60"},
	} {
		got := pubGet(t, h, c.method, jsonQuery(c.body), nil)
		if got.status != http.StatusOK || got.header.Get("Cache-Control") != c.want {
			t.Errorf("GET %s: %d Cache-Control %q, want %q", c.method, got.status, got.header.Get("Cache-Control"), c.want)
		}
		// 正文随 Accept-Encoding 变化，共享缓存要靠 Vary 才能按它区分；这个头由 connect 给出，中间件不补。
		if !slices.Contains(got.header.Values("Vary"), "Accept-Encoding") {
			t.Errorf("GET %s: Vary %q, want Accept-Encoding", c.method, got.header.Values("Vary"))
		}
		if got := pubPost(t, h, c.method, c.body, nil); got.status != http.StatusOK || got.header.Values("Cache-Control") != nil {
			t.Errorf("POST %s: %d Cache-Control %q, want none", c.method, got.status, got.header.Values("Cache-Control"))
		}
	}
	// 失败的 GET 一律 no-store：节点改回公开之后，浏览器不能继续用缓存里的 NotFound 挡住访客。
	h.setPublic(t, pub, "pub", false)
	if got := pubGet(t, h, "QueryMetrics", jsonQuery(window), nil); got.status != http.StatusNotFound || got.header.Get("Cache-Control") != "no-store" {
		t.Errorf("NotFound: %d Cache-Control %q", got.status, got.header.Get("Cache-Control"))
	}
	if got := pubGet(t, h, "QueryMetrics", jsonQuery(`{"from":"-1"}`), nil); got.status != http.StatusBadRequest || got.header.Get("Cache-Control") != "no-store" {
		t.Errorf("InvalidArgument: %d Cache-Control %q", got.status, got.header.Get("Cache-Control"))
	}
}

// next 什么都不写就返回时，net/http 会隐式补 200；缓存头仍须按表写出，而不是缺席。
func TestCacheControlCoversImplicitOK(t *testing.T) {
	t.Parallel()
	p := &Public{maxAge: map[string]uint32{"/heron.v1.PublicService/GetSite": 60}}
	rec := httptest.NewRecorder()
	p.cacheControl(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/heron.v1.PublicService/GetSite", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "max-age=60" {
		t.Fatalf("empty handler: %d Cache-Control %q, want 200 max-age=60", rec.Code, rec.Header().Get("Cache-Control"))
	}
}

func TestCachePolicyRequiresGETAndMaxAgeTogether(t *testing.T) {
	t.Parallel()
	opts := func(get bool, maxAge uint32, set bool) *descriptorpb.MethodOptions {
		o := &descriptorpb.MethodOptions{}
		if get {
			o.IdempotencyLevel = descriptorpb.MethodOptions_NO_SIDE_EFFECTS.Enum()
		}
		if set {
			proto.SetExtension(o, heronv1.E_CacheMaxAgeS, maxAge)
		}
		return o
	}
	one := func(o *descriptorpb.MethodOptions) []protoreflect.ServiceDescriptor {
		return []protoreflect.ServiceDescriptor{syntheticService(t, o)}
	}
	expectPanic(t, "synthetic.S.Bare accepts GET", func() { cachePolicy(one(opts(true, 0, false))) })
	expectPanic(t, "synthetic.S.Bare accepts GET", func() { cachePolicy(one(opts(true, 0, true))) })
	expectPanic(t, "synthetic.S.Bare declares heron.v1.cache_max_age_s but does not accept GET", func() { cachePolicy(one(opts(false, 30, true))) })
	if got := cachePolicy(one(opts(false, 0, false))); len(got) != 0 {
		t.Fatalf("a POST-only method without a max-age: table = %v, want empty", got)
	}
	// 装配时核对的是 heron.v1 的全部服务，不只是 PublicService：枚举本身不能是空的。
	services := probeServices()
	var names []string
	for _, s := range services {
		names = append(names, string(s.FullName()))
	}
	slices.Sort(names)
	if want := []string{"heron.v1.AdminService", "heron.v1.AgentService", "heron.v1.PublicService"}; !slices.Equal(names, want) {
		t.Fatalf("heron.v1 services = %v, want %v", names, want)
	}
	got := cachePolicy(services)
	want := map[string]uint32{
		"/heron.v1.PublicService/GetSite": 300, "/heron.v1.PublicService/GetSnapshot": 1,
		"/heron.v1.PublicService/QueryMetrics": 60, "/heron.v1.PublicService/QueryProbes": 60,
		"/heron.v1.PublicService/ListProbeComparisonNodes": 60, "/heron.v1.PublicService/QueryProbeComparison": 60,
	}
	if !maps.Equal(got, want) {
		t.Fatalf("table = %v, want %v", got, want)
	}
}
