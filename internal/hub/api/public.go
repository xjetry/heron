package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/gen/heron/v1/heronv1connect"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/alert"
	"github.com/xjetry/heron/internal/hub/live"
	"github.com/xjetry/heron/internal/hub/probe"
	"github.com/xjetry/heron/internal/hub/ratelimit"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/hub/traffic"
)

// publicMaxBody 是公开请求的解码预算。公开请求的字段只有历史查询的几个数值与节点 id，正常客户端发出的
// JSON 远小于 4 KiB；connect 的 JSON 解码接受任意空白与未知字段，所以没有"最大合法请求"这样的上界，
// 这个数只是对匿名请求体的有界约束。
const publicMaxBody = 4 << 10

const (
	// 每个来源的桶（IPv4 一个地址、IPv6 一个 /64，见 ratelimit.BySource）：容量 publicBurst，每 publicRefill 补充一个。公开页每 5 秒轮询一次快照，一个访客开几个标签页仍远在其下。
	publicBurst  = 60
	publicRefill = time.Second / 10
)

type PublicConfig struct {
	// ReportInterval 原样经 PublicSnapshot.report_interval_ms 下发。
	ReportInterval time.Duration
	// TrustedProxies 决定限流按哪个来源地址计：只有来自这些对端的 X-Forwarded-For 才被采信；空表示一个都不信。
	TrustedProxies []netip.Prefix
	// Location 是 hub 的 --timezone，days_left 按它的日历日算；NewPublic 要求非 nil。
	Location *time.Location
}

// Public 实现 PublicService。它不经会话或 token：挂载点只绑定按来源键（IPv4 一个地址、IPv6 一个 /64，
// 见 ratelimit.BySource）的限流、缓存头与公开总闸（Handler）。
// 可见范围由 store 的 ListPublicNodes 与 NodeIsPublic 承载，节点是否公开在每个请求里现读库；
// 进程里只有 GetSnapshot 的响应字节有 snapshotTTL 的缓存窗口。
type Public struct {
	cfg     PublicConfig
	store   *store.Store
	live    *live.Live
	traffic *traffic.Book
	probes  *probe.Registry
	history history
	clk     clock.Clock
	log     *slog.Logger

	facts   projection
	metrics projection
	billing projection

	// limit 按来源计数，覆盖挂载点收到的每个请求。
	limit *ratelimit.Buckets[netip.Addr]
	// maxAge 是每个接受 GET 的过程的成功响应 max-age，构造时从描述符读出（cachePolicy），之后只读。
	maxAge map[string]uint32
}

// publicProjections 是公开端的全部投影。public.proto 里带 reserved 的消息都必须是这里某个投影（含逐层的子投影）的目标：
// buf.yaml 据此对 public.proto 豁免 RESERVED_MESSAGE_NO_DELETE，由 TestPublicReservedOnlyMarksUnpublishedSourceFields 核对。
type publicProjectionSet struct{ facts, metrics, billing projection }

func (s publicProjectionSet) all() []projection { return []projection{s.facts, s.metrics, s.billing} }

func publicProjections() publicProjectionSet {
	return publicProjectionSet{
		facts:   newProjection((&heronv1.PublicFacts{}).ProtoReflect().Type(), (&heronv1.Facts{}).ProtoReflect().Descriptor()),
		metrics: newProjection((&heronv1.PublicMetrics{}).ProtoReflect().Type(), (&heronv1.Metrics{}).ProtoReflect().Descriptor()),
		billing: newProjection((&heronv1.PublicBilling{}).ProtoReflect().Type(), (&heronv1.Billing{}).ProtoReflect().Descriptor()),
	}
}

// PublicDeps 是 Public 的协作者，全部必需：NewPublic 逐字段核对非 nil。
type PublicDeps struct {
	Store   *store.Store
	Live    *live.Live
	Traffic *traffic.Book
	Probes  *probe.Registry
	Clock   clock.Clock
	Log     *slog.Logger
}

// NewPublic 对配置与依赖的缺陷 panic，口径与理由见 New。
func NewPublic(cfg PublicConfig, deps PublicDeps) *Public {
	if cfg.Location == nil {
		panic("api.PublicConfig.Location must be set")
	}
	if deps.Store == nil {
		panic("api.PublicDeps.Store must be set")
	}
	if deps.Live == nil {
		panic("api.PublicDeps.Live must be set")
	}
	if deps.Traffic == nil {
		panic("api.PublicDeps.Traffic must be set")
	}
	if deps.Probes == nil {
		panic("api.PublicDeps.Probes must be set")
	}
	if deps.Clock == nil {
		panic("api.PublicDeps.Clock must be set")
	}
	if deps.Log == nil {
		panic("api.PublicDeps.Log must be set")
	}
	projections := publicProjections()
	return &Public{
		cfg: cfg, store: deps.Store, live: deps.Live, traffic: deps.Traffic, probes: deps.Probes, clk: deps.Clock, log: deps.Log,
		history: history{store: deps.Store, probes: deps.Probes, log: deps.Log, gate: newHistoryGate()},
		facts:   projections.facts, metrics: projections.metrics, billing: projections.billing,
		limit:  ratelimit.New[netip.Addr](publicBurst, publicRefill),
		maxAge: cachePolicy(probeServices()),
	}
}

// connectHandler 是不带挂载点中间件的处理器。
func (p *Public) connectHandler() (string, http.Handler) {
	return heronv1connect.NewPublicServiceHandler(p, connect.WithReadMaxBytes(publicMaxBody), connect.WithInterceptors(connect.UnaryInterceptorFunc(p.requireEnabled)))
}

// 总闸与节点 public 取交集，不改逐节点标记，重新打开即可恢复原范围。
// GetSite 也在同一入口拒绝，否则关闸仍会泄漏标题与 logo；检查在快照字节缓存内侧，保留其 1 秒窗口。
func (p *Public) requireEnabled(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if !p.store.PublicEnabled() {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("public page is disabled"))
		}
		return next(ctx, req)
	}
}

// Handler 是公开服务唯一的挂载点：cacheControl(BySource(snapshotCache(connect)))。限流包在缓存与 connect 之外，
// 缓存命中与解码失败的请求同样计数；缓存头包在最外面，限流的 429 也带 no-store。
func (p *Public) Handler() (string, http.Handler) {
	path, h := p.connectHandler()
	return path, p.cacheControl(ratelimit.BySource(p.limit, p.cfg.TrustedProxies, p.clk, newSnapshotCache(h, p.clk)))
}

// cacheControl 只作用于 GET，POST 响应不带缓存头。成功响应按方法声明的 cache_max_age_s；失败响应（含限流的 429
// 与 NotFound）一律 no-store——节点改为公开后，之前缓存的 NotFound 不能继续挡住访客。
//
// 浏览器与共享缓存按 URL 与 Accept-Encoding 复用 GET 响应：正文随协商的压缩在 gzip 与 identity 之间变化，
// connect 在 GET 响应上声明 Vary: Accept-Encoding（mergeResponseHeader），由 TestPublicCacheControlPerMethod 钉住。
// 响应若再随别的请求头变化，就要把它加进 Vary。
//
// 每个 GET 响应都经 cacheHeaderWriter.WriteHeader 定下缓存头：next 显式写头或写正文时在那一刻定；next 什么都没写
// 就返回时（proto 编码的全默认值消息是 0 字节，connect 不调用 Write），状态码本会由 net/http 隐式补成 200，
// 包装器看不到，所以返回后在这里补一次 WriteHeader(200)。
func (p *Public) cacheControl(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			next.ServeHTTP(w, r)
			return
		}
		cw := &cacheHeaderWriter{ResponseWriter: w, maxAge: p.maxAge[r.URL.Path]}
		next.ServeHTTP(cw, r)
		if !cw.wrote {
			cw.WriteHeader(http.StatusOK)
		}
	})
}

// cacheHeaderWriter 在状态码确定的那一刻写 Cache-Control：之后头已发出，改不了。
// GET 得到 200 只能是一个接受 GET 的过程在应答；cachePolicy 保证每个这样的过程都在 maxAge 表里且值为正。
// 表外的路径（未知过程 404、不接受 GET 的过程 405）都不是 200，所以 maxAge 为 0 时一律 no-store。
type cacheHeaderWriter struct {
	http.ResponseWriter
	maxAge uint32
	wrote  bool
}

func (c *cacheHeaderWriter) WriteHeader(code int) {
	if !c.wrote {
		c.wrote = true
		v := "no-store"
		if code == http.StatusOK && c.maxAge > 0 {
			v = "max-age=" + strconv.FormatUint(uint64(c.maxAge), 10)
		}
		c.Header().Set("Cache-Control", v)
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *cacheHeaderWriter) Write(b []byte) (int, error) {
	if !c.wrote {
		c.WriteHeader(http.StatusOK)
	}
	return c.ResponseWriter.Write(b)
}

// Unwrap 让 http.ResponseController 找到底层 ResponseWriter 的能力。
func (c *cacheHeaderWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// cachePolicy 读出接受 GET 的方法的缓存上界，键为 Connect 过程路径，并在装配时核对不变式：一个方法接受 GET
// （idempotency_level = NO_SIDE_EFFECTS，protoc-gen-connect-go 据它生成 GET 支持）当且仅当它声明了正的
// cache_max_age_s。前者缺后者，GET 响应没有上界可写；后者缺前者，声明的上界永远用不上，多半是标错了方法。
// 任一方向不符即 panic，与准入表同一口径：不合的 proto 无法随 hub 启动（NewPublic 在 serve 装配时调用它）。
func cachePolicy(services []protoreflect.ServiceDescriptor) map[string]uint32 {
	table := map[string]uint32{}
	for _, svc := range services {
		methods := svc.Methods()
		for i := 0; i < methods.Len(); i++ {
			m := methods.Get(i)
			opts, _ := m.Options().(*descriptorpb.MethodOptions)
			get := opts.GetIdempotencyLevel() == descriptorpb.MethodOptions_NO_SIDE_EFFECTS
			v, _ := proto.GetExtension(m.Options(), heronv1.E_CacheMaxAgeS).(uint32)
			switch {
			case get && v == 0:
				panic(fmt.Sprintf("%s accepts GET (idempotency_level = NO_SIDE_EFFECTS) but does not declare a positive heron.v1.cache_max_age_s", m.FullName()))
			case !get && v != 0:
				panic(fmt.Sprintf("%s declares heron.v1.cache_max_age_s but does not accept GET (idempotency_level is not NO_SIDE_EFFECTS)", m.FullName()))
			case get:
				table["/"+string(svc.FullName())+"/"+string(m.Name())] = v
			}
		}
	}
	return table
}

// probeServices 是 heron.v1 包里的全部服务。包里每个 proto 文件的生成代码都在 gen/heron/v1 这一个 Go 包里，
// 本包 import 了它，所以它们都已登记进 protoregistry.GlobalFiles。
func probeServices() []protoreflect.ServiceDescriptor {
	var out []protoreflect.ServiceDescriptor
	protoregistry.GlobalFiles.RangeFilesByPackage("heron.v1", func(fd protoreflect.FileDescriptor) bool {
		for i := 0; i < fd.Services().Len(); i++ {
			out = append(out, fd.Services().Get(i))
		}
		return true
	})
	return out
}

// noPublicNode 是公开端对"没有这个公开节点"的唯一回答。匿名调用方区分不出"存在但未公开"与"不存在"，
// 由两处承载：store 的 NodeIsPublic 对不存在的 id 返回 false 而不是错误；requirePublic 对 false 只有这一条分支。
// 在这条分支里按"是否存在"分叉，哪怕文案不变、只多一个错误 metadata（它走响应头），私有节点就可区分了。
// 文案不带 id 只是让所有非公开 id 的响应逐字节相同，测试因此能拿两个不同的 id 比较整个响应。
func noPublicNode() error {
	return connect.NewError(connect.CodeNotFound, errors.New("node_id: no public node has this id"))
}

// requirePublic 是公开端历史查询的节点准入。
func (p *Public) requirePublic(ctx context.Context, id int64) error {
	public, err := p.store.NodeIsPublic(ctx, id)
	if err != nil {
		p.log.Error("looking up node failed", "err", err)
		return internalError("looking up node failed")
	}
	if !public {
		return noPublicNode()
	}
	return nil
}

func (p *Public) GetSite(ctx context.Context, _ *connect.Request[heronv1.GetSiteRequest]) (*connect.Response[heronv1.PublicSite], error) {
	st, err := p.store.SiteSettings(ctx)
	if err != nil {
		p.log.Error("reading settings failed", "err", err)
		return nil, internalError("reading settings failed")
	}
	adminPath, _ := ctx.Value(adminPathKey{}).(string)
	return connect.NewResponse(&heronv1.PublicSite{Title: st.Title, Theme: st.Theme, AccentColor: st.AccentColor, Logo: st.Logo, CustomCss: st.CustomCSS, AdminPath: adminPath}), nil
}

type adminPathKey struct{}

// WithAdminPath 包在挂着管理面板的那个 origin 的公开服务挂载点外面，path 是面板在该 origin 上的挂载路径，经
// GetSite 的 admin_path 由生产挂载点下发给内置公开页作登录入口；主题 SDK 桥接不转交此字段。
// 没包的挂载点 GetSite 回空串，
// 缺席即"这里没有面板"：漏包只会少一个入口，不会让公开页链到一个 404。
func WithAdminPath(h http.Handler, path string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), adminPathKey{}, path)))
	})
}

func (p *Public) GetSnapshot(ctx context.Context, _ *connect.Request[heronv1.PublicServiceGetSnapshotRequest]) (*connect.Response[heronv1.PublicSnapshot], error) {
	nodes, err := p.store.ListPublicNodes(ctx)
	if err != nil {
		p.log.Error("listing public nodes failed", "err", err)
		return nil, internalError("listing nodes failed")
	}
	// now 只读一次：快照的 now 与每个节点的 days_left 出自同一时刻，调用方拿 now 核对 days_left 不会差一天。
	now := p.clk.Now()
	today := alert.Today(now, p.cfg.Location)
	out := &heronv1.PublicSnapshot{Now: now.Unix(), ReportIntervalMs: uint32(p.cfg.ReportInterval / time.Millisecond)}
	for _, n := range nodes {
		online, seen, m := liveState(p.live, n)
		pn := &heronv1.PublicNode{Id: n.ID, Name: n.Name, Online: online, LastSeenAt: seen, SortOrder: n.SortOrder, Traffic: trafficProto(p.traffic.View(n.ID), n), Maintenance: n.Maintenance, Rented: n.Rented}
		// 公开备注随公开节点下发：n 来自 ListPublicNodes，只含 public = 1 的节点，私有节点的备注不会走到这里。
		pn.PublicRemark = n.PublicRemark
		// 国家只放行显示值：查得于哪个地址与来源不公开，公开页表达"在哪个区域"，不定位机器。
		pn.Country, _ = n.DisplayCountry()
		// 标签随公开节点公开：n 来自 ListPublicNodes，只含 public = 1 的节点，私有节点的标签不会走到这里。
		pn.Tags = n.Tags
		// 计费经投影公开：PublicBilling 没有 auto_renew（reserved），它与 Billing 的对齐由 NewPublic 构造投影时核对。
		if b := billingProto(n.Billing, today); b != nil {
			pn.Billing = p.billing.apply(b).(*heronv1.PublicBilling)
		}
		// 地址族状态取显示值（手填与 hub 来源的族为 AVAILABLE），与管理端 Node.network 出自同一个判定。从未上报 facts 的
		// 节点只要有手填或 hub 来源，也公开那一族的状态：这时 PublicFacts 只有 network，其余字段为零值——标记跟着显示值
		// 走，不跟着 agent 有没有上报过。
		network := publicNetworkInput(n.DisplayNetwork())
		if n.Facts != nil || network != nil {
			facts := &heronv1.Facts{}
			if n.Facts != nil {
				facts = proto.Clone(n.Facts).(*heronv1.Facts)
			}
			facts.Network = network
			pn.Facts = p.facts.apply(facts).(*heronv1.PublicFacts)
		}
		if m != nil {
			pn.Metrics = p.metrics.apply(m).(*heronv1.PublicMetrics)
		}
		out.Nodes = append(out.Nodes, pn)
	}
	out.Tags = unionTags(nodes)
	return connect.NewResponse(out), nil
}

// publicNetworkInput 把显示值写成 NetworkInfo 的形状，作公开投影的输入：投影按字段号从 Facts.network 取出
// PublicNetworkInfo，输入换成显示值，公开端就与管理端出自 store.Node.DisplayNetwork 这同一个判定。只填 state：地址与
// 探测时间在 PublicAddressDetection 里是保留号，投影本就不放行它们；这里也不带进来，地址不公开就不只靠投影这一道。
// HUB_SEEN 的族 state 为 AVAILABLE，公开页照常画该族标记：它依赖 hub 的来源解析正确（--trusted-proxies，见
// store.Node.DisplayNetwork 的观测局限），地址照旧不公开。两族都没有显示值时为 nil，与 agent 不报双栈出口时相同。
func publicNetworkInput(display [2]store.DisplayAddress) *heronv1.NetworkInfo {
	var families [2]*heronv1.AddressDetection
	for i, d := range display {
		if d.Source != store.AddressNone {
			families[i] = &heronv1.AddressDetection{State: d.State}
		}
	}
	if families[0] == nil && families[1] == nil {
		return nil
	}
	return &heronv1.NetworkInfo{Ipv4: families[0], Ipv6: families[1]}
}

// unionTags 是 nodes 各自标签的并集，按 store.TagFold 排序，与 ListTags 的 ORDER BY name_fold 同序：name_fold 就是
// TagFold 的结果，SQLite 的 BINARY 比较与 Go 的字符串比较都是逐字节比较 UTF-8。同一个标签在 tag 表里只有一行
// （name_fold 唯一），各节点的 Tags 里写法相同，按折叠键去重不会丢掉另一种写法。
func unionTags(nodes []store.Node) []string {
	var out []string
	for _, n := range nodes {
		for _, t := range n.Tags {
			if !slices.ContainsFunc(out, func(o string) bool { return store.TagFold(o) == store.TagFold(t) }) {
				out = append(out, t)
			}
		}
	}
	slices.SortFunc(out, func(a, b string) int { return strings.Compare(store.TagFold(a), store.TagFold(b)) })
	return out
}

func (p *Public) QueryMetrics(ctx context.Context, req *connect.Request[heronv1.QueryMetricsRequest]) (*connect.Response[heronv1.QueryMetricsResponse], error) {
	m := req.Msg
	maxPoints, err := checkWindow(m.GetFrom(), m.GetTo(), m.GetMaxPoints())
	if err != nil {
		return nil, err
	}
	resp, err := p.history.metrics(ctx, m, maxPoints, func(ctx context.Context) error { return p.requirePublic(ctx, m.GetNodeId()) })
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

func (p *Public) QueryProbes(ctx context.Context, req *connect.Request[heronv1.QueryProbesRequest]) (*connect.Response[heronv1.QueryProbesResponse], error) {
	m := req.Msg
	maxPoints, err := checkWindow(m.GetFrom(), m.GetTo(), m.GetMaxPoints())
	if err != nil {
		return nil, err
	}
	node := m.GetNodeId()
	resp, err := p.history.probeSeries(ctx, m, maxPoints, func(id uint64) (heronv1.ProbeKind, string, bool) {
		return p.probes.TargetFor(node, id)
	}, p.probes.OrderedIDs(), func(ctx context.Context) error { return p.requirePublic(ctx, node) })
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}
