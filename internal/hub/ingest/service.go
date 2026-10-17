// Package ingest 实现 AgentService：校验 → live 与流量入账、facts 落盘、分钟刷出。
//
// 上报路径只碰内存：鉴权查 auth 的映射，状态写进 live 与 traffic，facts 投递给写协程
// 即返回。分钟桶由 RunFlusher 落盘，流量由 traffic.Book.Run 定时刷出。
package ingest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/gen/heron/v1/heronv1connect"
	"github.com/xjetry/heron/internal/agentwire"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/auth"
	"github.com/xjetry/heron/internal/hub/live"
	"github.com/xjetry/heron/internal/hub/metric"
	"github.com/xjetry/heron/internal/hub/ratelimit"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/hub/traffic"
	"github.com/xjetry/heron/internal/hub/updates"
	"github.com/xjetry/heron/internal/probelimit"
	"github.com/xjetry/heron/internal/update"
)

// Report 的 protobuf 请求由 Metrics 数值标量、boot_id、Facts、有界版本号/摘要和探测结果组成。
// 数值由 proto 类型定界；validateMetrics/validateFacts 将主机字符串各限在 maxHostString 字节，
// 诊断的规则、接口与失败类别由 agentwire.ValidateDiagnostics 限定条数和长度，逐网卡计数的条数与名称由
// agentwire.ValidateNetInterfaces 限定；执行环境的容量与说明条数由
// agentwire.ValidateExecutionScope 定界，清单摘要长度由 agentwire.TasksDigestLen 定界。
// validateResults 限条数与错误长度；合法 agent 由 Runner 限批、ToProto 截断错误来遵守这些约束。
// connect 在拦截器前整条读取，超出 maxBody 返回 ResourceExhausted；Runner 会回队，
// 因而合法 agent 的编码上界必须从常量推出，不能因读上限不足而永久重发同一超限批次。
// 这些预算针对已知字段的 protobuf 编码；非法输入仍可能先撞读上限，再也到不了字段校验。
const (
	// Metrics/Facts 与逐网卡列表的字段类型、字符串校验及条数上限共同定界；TestHostPayloadFitsMetricsBudget 覆盖满值编码
	// （逐网卡列表的最坏 5760 字节由 TestMaxNetInterfacesWire 钉住）。
	// 结构排除列表至多 128 项，每项至多 64 字节名称，连同 protobuf 标签、长度与原因共 8960 字节，约 9 KiB。
	// 读上限 256 KiB 减去主机预算与满批探测结果仍有余量，由下面的编译期断言钉住。
	metricsBudget = 48 << 10
	// maxResultWire 含结果及外层 repeated 字段开销，上界由 TestMaxProbeResultWire 钉住：最大的形状是满额错误文本
	// 加回显的配置身份（172 字节）。
	maxResultWire = 176
	maxBody       = 256 << 10
)

// 非探测部分加上整批最坏编码不能超过读上限；单结果由 TestMaxProbeResultWire 钉住。
const _ = uint(maxBody - metricsBudget - probelimit.MaxResultsPerReport*maxResultWire)

const (
	// burst 是上报的令牌桶容量：允许上报间隔的抖动与一次立即重试，再多就是异常。
	burst = 3
	// registerBurst 与 registerRefillPer 是 Register 按来源的限速（来源的口径见 ratelimit.BySource）：
	// 桶容量与每补充一个令牌的周期。
	registerBurst     = 30
	registerRefillPer = time.Second
)

type Config struct {
	// TTL 必须在 [agentwire.MinTTL, agentwire.MaxTTL] 内。用户给出的值由 serve 的 --offline-after 解析校验，那里是
	// 唯一的用户输入校验处；New 只核对范围并对越界 panic。
	TTL            time.Duration
	TrustedProxies []netip.Prefix
	// Updates 为 nil 时不处理节点在线更新：上报不观测更新状态、不下发更新任务，删除节点也无更新状态可清。
	// 字段是接口：装进接口的 nil 指针（typed nil）不等于 nil，会被当作已给出、到第一次调用才空指针，装配方要么给出
	// 可用的对象，要么不给。
	Updates interface {
		// Observe 收下请求的 UpdateStatus 全文与 update_hash（nil 即缺席），返回要下发的任务与是否索要全文。
		Observe(int64, *heronv1.UpdateStatus, *uint64) (*heronv1.UpdateTask, bool)
		Forget(int64)
	}
	// Releases 为 hub 来源的节点中转官方产物。nil 时 GetRelease 返回 Unavailable：
	// 缺省是不提供中转，不是放宽。typed nil 的约束同 Updates。
	Releases interface {
		Get(ctx context.Context, node int64, taskID, arch string) (update.Artifacts, error)
	}
	// CertObserved 在一份证书观测改变了 probe_cert 的 not_after（含首次写入）后被调用；
	// 装配方用它触发一次证书到期评估，续期不必等到日界才恢复。回调从写协程另起的
	// 协程里调用，允许它写库；同值重复上报不触发。可为 nil（不评估）。
	CertObserved func()
}

// Deps 是 Service 的协作者，全部必需：New 逐字段核对非 nil。Tasks 是接口，typed nil 的约束同 Config.Updates。
type Deps struct {
	Live    *live.Live
	Store   *store.Store
	Auth    *auth.Auth
	Traffic *traffic.Book
	Tasks   TaskSource
	Clock   clock.Clock
	Log     *slog.Logger
}

type storeWriter interface {
	WriteMinuteBatch(ctx context.Context, batch metric.Batch) (int, error)
	UpsertFactsAsync(nodeID int64, hash uint64, f *heronv1.Facts, done func(error))
	UpsertProbeCertAsync(nodeID int64, taskID uint64, notAfter, observedAt int64, configID []byte, done func(bool, error))
	UpsertPresentedAsync(nodeID int64, taskID uint64, configID, spki []byte, notAfter, observedAt int64, reason int32, done func(error))
}

type TaskSource interface {
	Version() uint64
	TasksFor(nodeID int64) *heronv1.ProbeTasks
	CertPolicy(id uint64) (https, pinned bool, configID []byte, ok bool)
	Assigned(nodeID int64, taskID uint64) bool
	Forget(nodeID int64)
}

type Service struct {
	cfg           Config
	live          *live.Live
	traffic       *traffic.Book
	tasks         TaskSource
	store         *store.Store
	writer        storeWriter
	auth          *auth.Auth
	clk           clock.Clock
	log           *slog.Logger
	limit         *ratelimit.Buckets[int64]
	registerLimit *ratelimit.Buckets[netip.Addr]

	// stateMu 将 Report 的鉴权及内存写入与 Forget 排他，防止已放行的在途请求重建状态。
	// 同持时锁序为 pendingMu → stateMu → mu；上报不取 pendingMu，不等待刷盘。持这些锁时会去取的别包的锁，以及持 mu
	// 时不得同步等待 store 写的理由，见 internal/hub 的包注释（doc.go）。
	stateMu sync.RWMutex
	mu      sync.Mutex
	// factsHash 是 hub 已持久化的各节点 facts 摘要；只在写库成功后更新，
	// 写失败则保持旧值，下一次上报会因不一致再次要求 facts。
	factsHash map[int64]uint64
	// taskDigest 每个节点只留当前一份：键是算出它的那份清单自带的版本，注册表版本变化即失效。
	taskDigest map[int64]taskDigestCache
	// detection 是各节点的逐族探测开关（store.NetworkDetection），Report 每次应答带上，上报路径不查库。Load 从库建立，
	// 此后只由 SetAddressPins 在节点编辑提交之后改写（nodeops.Service.Update，在 nodeops.mu 下与删除串行），Forget
	// 清掉。手填只经 UpdateNode 改：离线子命令不写这两列，恢复要求 hub 停机，启动时 Load 读到的就是恢复后的值。
	// 没有条目即两族都探测。存的消息只读不改，多个应答共用同一个。
	detection map[int64]*heronv1.NetworkDetection

	pendingMu sync.Mutex
	pending   []metric.Batch
}

// New 对配置与依赖的缺陷 panic，口径与理由见 api.New。
func New(cfg Config, deps Deps) *Service {
	if cfg.TTL < agentwire.MinTTL || cfg.TTL > agentwire.MaxTTL {
		panic(fmt.Sprintf("ingest.Config.TTL %v is outside [%v, %v]", cfg.TTL, agentwire.MinTTL, agentwire.MaxTTL))
	}
	if deps.Live == nil {
		panic("ingest.Deps.Live must be set")
	}
	if deps.Store == nil {
		panic("ingest.Deps.Store must be set")
	}
	if deps.Auth == nil {
		panic("ingest.Deps.Auth must be set")
	}
	if deps.Traffic == nil {
		panic("ingest.Deps.Traffic must be set")
	}
	if deps.Tasks == nil {
		panic("ingest.Deps.Tasks must be set")
	}
	if deps.Clock == nil {
		panic("ingest.Deps.Clock must be set")
	}
	if deps.Log == nil {
		panic("ingest.Deps.Log must be set")
	}
	// 上报的补充周期是下发间隔的一半：允许正常间隔内的一次重试。间隔由 TTL 决定，服务存续期间不变；
	// 限速与下发都经 interval 算，下发间隔改了，限速跟着改。
	return &Service{cfg: cfg, live: deps.Live, traffic: deps.Traffic, tasks: deps.Tasks, store: deps.Store, writer: deps.Store, auth: deps.Auth,
		clk: deps.Clock, log: deps.Log,
		limit: ratelimit.New[int64](burst, agentwire.ReportInterval(cfg.TTL)/2), registerLimit: ratelimit.New[netip.Addr](registerBurst, registerRefillPer),
		factsHash: map[int64]uint64{}}
}

func (s *Service) Load(ctx context.Context) error {
	starts, err := s.store.CoverageStarts(ctx)
	if err != nil {
		return err
	}
	s.live.LoadCoverage(starts)
	m, err := s.store.FactsHashes(ctx)
	if err != nil {
		return err
	}
	detection, err := s.store.NetworkDetections(ctx)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.factsHash = m
	s.detection = detection
	s.mu.Unlock()
	return nil
}

// SetAddressPins 发布节点编辑提交后的手填出口地址，该节点的下一次 Report 应答即按它给出逐族探测开关。调用方必须在
// 库提交成功之后调用，且与该节点的删除串行（nodeops.Service.Update 持 nodeops.mu 调用），否则 Forget 之后会重建
// 已删节点的条目。
func (s *Service) SetAddressPins(nodeID int64, ipv4Pin, ipv6Pin string) {
	d := store.NetworkDetection(ipv4Pin, ipv6Pin)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.detection == nil {
		s.detection = map[int64]*heronv1.NetworkDetection{}
	}
	if !d.GetSkipIpv4() && !d.GetSkipIpv6() {
		delete(s.detection, nodeID)
		return
	}
	s.detection[nodeID] = d
}

// detectionFor 是该节点这次应答要带的逐族开关；没有条目时给两族都探测的零值。每次都带上，agent 才能在手填清空后
// 收到"恢复探测"。
func (s *Service) detectionFor(nodeID int64) *heronv1.NetworkDetection {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d, ok := s.detection[nodeID]; ok {
		return d
	}
	return &heronv1.NetworkDetection{}
}

// Interval 是下发给 agent 的上报间隔。
func (s *Service) Interval() time.Duration { return agentwire.ReportInterval(s.cfg.TTL) }

// New 将 TTL 限在 MaxTTL 内；间隔为 ReportInterval(TTL) = TTL/ReportsPerTTL，满速产出
// MaxTasksPerNode×(TTL/ReportsPerTTL)/MinIntervalS 条，单批上限必须容纳它。
const _ = uint(probelimit.MaxResultsPerReport*probelimit.MinIntervalS*agentwire.ReportsPerTTL - probelimit.MaxTasksPerNode*int(agentwire.MaxTTL/time.Second))

// Handler 挂载 AgentService。Register 是唯一的匿名方法，按来源键限速（IPv4 一个地址一桶、IPv6 一个 /64 一桶，见 ratelimit.BySource），限流中间件包在 connect 外面，
// 解码失败的请求同样计数（ratelimit.BySource 的注释写了理由）。Report 不进这个桶：同一出口地址后面可以有很多
// agent，上报按节点限速（Report 方法体里的 s.limit）。路径判定与 connect 分派用同一个 r.URL.Path 全等比较，
// 所以到达 Register 方法体的请求都先经过了限流。
func (s *Service) Handler() (string, http.Handler) {
	path, h := heronv1connect.NewAgentServiceHandler(s,
		connect.WithInterceptors(s.authInterceptor()),
		connect.WithReadMaxBytes(maxBody))
	register := ratelimit.BySource(s.registerLimit, s.cfg.TrustedProxies, s.clk, h)
	return path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == heronv1connect.AgentServiceRegisterProcedure {
			register.ServeHTTP(w, r)
			return
		}
		h.ServeHTTP(w, r)
	})
}

type nodeKey struct{}
type nodeTokenKey struct{}

func unauthenticated() error {
	return connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
}

// authInterceptor 在挂载点上裁决每个方法的凭据来源。没有在这里显式列出的
// 方法一律拒绝：新增方法不可能因为忘了加检查而被放行。
// Register 的来源限速不在这里，在更外层的 Handler：超限请求连解码都不进，更到不了方法体里会触碰写协程的语句。
type authInterceptor struct{ service *Service }

func (s *Service) authInterceptor() connect.Interceptor { return authInterceptor{service: s} }

func (i authInterceptor) WrapStreamingHandler(connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(context.Context, connect.StreamingHandlerConn) error { return unauthenticated() }
}

func (i authInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

// bearer 从请求头取节点 token 并裁决。返回的 token 供需要重新核对身份的路径（facts 对账）复用。
func (s *Service) bearer(req connect.AnyRequest) (id int64, tok string, ok bool) {
	tok, ok = strings.CutPrefix(req.Header().Get("Authorization"), "Bearer ")
	if !ok {
		return 0, "", false
	}
	id, ok = s.auth.Authenticate(tok)
	return id, tok, ok
}

func (i authInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	s := i.service
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		switch req.Spec().Procedure {
		case heronv1connect.AgentServiceRegisterProcedure:
			// 凭据是请求体里的窗口 key，由 Register 裁决。
			return next(ctx, req)
		case heronv1connect.AgentServiceReportProcedure:
			s.stateMu.RLock()
			defer s.stateMu.RUnlock()
			id, tok, ok := s.bearer(req)
			if !ok {
				return nil, unauthenticated()
			}
			ctx = context.WithValue(ctx, nodeTokenKey{}, tok)
			return next(context.WithValue(ctx, nodeKey{}, id), req)
		case heronv1connect.AgentServiceGetReleaseProcedure:
			// 不取 stateMu：一次下载可能持续数分钟，持读锁会让 Forget 等待，而排队的写锁又会挡住此后全部
			// Report 的读锁（sync.RWMutex 有写者排队时新读者阻塞）。GetRelease 不写 Report 维护的内存状态；
			// 节点删除后它的任务随 Manager.Forget 消失，Relay 的任务检查即拒绝。
			id, _, ok := s.bearer(req)
			if !ok {
				return nil, unauthenticated()
			}
			return next(context.WithValue(ctx, nodeKey{}, id), req)
		}
		return nil, unauthenticated()
	}
}

func (s *Service) Register(ctx context.Context, req *connect.Request[heronv1.RegisterRequest]) (*connect.Response[heronv1.RegisterResponse], error) {
	// 窗口裁决的失败计数与限速按同一个来源键（IPv4 按地址、IPv6 按 /64）：由 Handler 里的 ratelimit.BySource 算出并放进 ctx。
	// 取不到只可能是挂载绕过了 Handler，属于装配错误。
	from, ok := ratelimit.SourceOf(ctx)
	if !ok {
		panic("ingest: Register reached without the source-address rate limit; mount Service.Handler")
	}
	// 注册名走共享的 agent 口径（AgentNodeName）：清洗后截到 64 个字符、截断露出的尾空白再裁掉、空了用缺省名；
	// 超长不拒绝注册。入库即规范形，store 的 insertNode 准入（checkShortName）不会拒绝它。
	name := store.AgentNodeName(req.Msg.GetName())
	id, tok, err := s.auth.Register(ctx, req.Msg.GetKey(), name, from)
	if errors.Is(err, auth.ErrDenied) {
		return nil, unauthenticated()
	}
	// 继承的 all_nodes 任务超限只在窗口与 key 都通过之后才会发生（store.RegisterNode 先判窗口与 key，再 insertNode），
	// 调用方已持有效 key，说明原文可以给它：修复要由管理员减少 all_nodes 任务，heron-agent register 把原文打到 stderr。
	if errors.Is(err, store.ErrNodeLimit) {
		return nil, connect.NewError(connect.CodeResourceExhausted, err)
	}
	if err != nil {
		s.log.Error("register failed", "err", err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("registration failed"))
	}
	s.log.Info("node registered", "node", id, "name", name, "source", auth.DescribeSource(from))
	return connect.NewResponse(&heronv1.RegisterResponse{NodeId: id, Token: tok}), nil
}

func (s *Service) Report(ctx context.Context, req *connect.Request[heronv1.ReportRequest]) (*connect.Response[heronv1.ReportResponse], error) {
	id := ctx.Value(nodeKey{}).(int64)
	if !s.limit.Allow(id, s.clk.Mono()) {
		return nil, connect.NewError(connect.CodeResourceExhausted, errors.New("reporting faster than twice the assigned interval"))
	}
	m := req.Msg.GetMetrics()
	if err := validateMetrics(m); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if err := validateNetInterfaces(req.Msg.GetNetInterfaces()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if err := validateTasksDigest(req.Msg.GetTasksDigest()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if err := s.validateResults(req.Msg.GetProbeResults()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if err := validateFacts(req.Msg.GetFacts()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if err := update.ValidateStatus(req.Msg.GetUpdate()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	// 来源地址只取 hub 在这次请求上看到的对端，经 auth.ClientIP 按 --trusted-proxies 解析，与限流、登录锁定同一口径：
	//   - 不读 CF-Connecting-IP 之类的旁路头：任何客户端都能自己带上它们，读了就等于采信请求方自述；
	//   - 不采信 agent 自报的地址：那是 agent 的自述，与"hub 看到什么"是两个事实，混在一列里无法区分。
	source := auth.SourceText(auth.ClientIP(req.Peer().Addr, req.Header().Values("X-Forwarded-For"), s.cfg.TrustedProxies))
	// 准入全部通过之后才归一化：被拒的上报不留下任何改动。逐网卡列表只交给账本，不进 live。
	normalizeNetTotals(m, req.Msg.GetNetInterfaces())
	ts, gap, first := s.live.Observe(id, source, m)
	// 每 token 由单个 agent 串行发出 unary 上报、等到响应才发下一次；这保证同节点的
	// Observe、Account、AddBytes 不被另一次上报交错。多端共用 token 不提供此保证。
	// 间隔达到 TTL 意味着按在线判定节点在这段时间里离线过，这段增量跨过一次离线，
	// 不能当作当前这一分钟的速率；hub 重启后的首次上报（live 无条目）同理。
	// 流量入账只碰内存；first 与 gap 都来自 Observe 之前的状态。
	if d, ok := s.traffic.Account(id, m, req.Msg.GetNetInterfaces()); ok && !first && gap < s.cfg.TTL {
		s.live.AddBytes(id, ts, d.Rx, d.Tx)
	}
	want := s.reconcileFacts(id, ctx.Value(nodeTokenKey{}).(string), req.Msg.GetFactsHash(), req.Msg.GetFacts())
	resp := &heronv1.ReportResponse{
		ReportIntervalMs: agentwire.ReportIntervalMs(s.cfg.TTL),
		WantFacts:        want,
		Detection:        s.detectionFor(id),
	}
	if s.cfg.Updates != nil {
		resp.Update, resp.WantUpdate = s.cfg.Updates.Observe(id, req.Msg.GetUpdate(), req.Msg.UpdateHash)
	}
	s.foldResults(id, req.Msg.GetProbeResults())
	// 摘要缺席表示尚未持有清单，必须下发；给出摘要时只比较内容。
	if digest := req.Msg.GetTasksDigest(); len(digest) == agentwire.TasksDigestLen {
		resp.Tasks = s.tasksIfDigestDiffers(id, digest)
	} else {
		resp.Tasks = s.tasks.TasksFor(id)
	}
	return connect.NewResponse(resp), nil
}

type taskDigestCache struct {
	version uint64
	sum     []byte
}

// tasksIfDigestDiffers 返回应下发给该节点的清单；agent 报的摘要与之相等时返回 nil。
//
// 每次上报都会走到这里，稳态是"摘要相等"：命中缓存只读一次版本、查一次 map，不取清单、不克隆任务、不重算摘要。
// 缓存项的版本取自算出摘要的那份清单（TasksFor 在同一把读锁下给出版本与任务），而不是另一次调用读到的版本：
// 两次读之间注册表可能已经重载，拿错的版本当键会把新清单的摘要记在旧版本名下。命中判断用当前版本，
// 读到旧版本而注册表随后重载时，下一次上报读到新版本、不再命中，与只比计数时的时序相同。
// 摘要算不出来时按下发处理并记日志，上报本身不失败。
func (s *Service) tasksIfDigestDiffers(node int64, digest []byte) *heronv1.ProbeTasks {
	version := s.tasks.Version()
	s.mu.Lock()
	e, ok := s.taskDigest[node]
	s.mu.Unlock()
	if ok && e.version == version && bytes.Equal(e.sum, digest) {
		return nil
	}
	tasks := s.tasks.TasksFor(node)
	sum, err := agentwire.TasksDigest(tasks.GetTasks())
	if err != nil {
		s.log.Error("tasks digest failed; sending the task list", "node", node, "err", err)
		return tasks
	}
	s.mu.Lock()
	if s.taskDigest == nil {
		s.taskDigest = map[int64]taskDigestCache{}
	}
	s.taskDigest[node] = taskDigestCache{version: tasks.GetVersion(), sum: sum}
	s.mu.Unlock()
	if bytes.Equal(sum, digest) {
		return nil
	}
	return tasks
}

func (s *Service) GetRelease(ctx context.Context, req *connect.Request[heronv1.GetReleaseRequest]) (*connect.Response[heronv1.GetReleaseResponse], error) {
	if s.cfg.Releases == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("release relay is not configured"))
	}
	id := ctx.Value(nodeKey{}).(int64)
	a, err := s.cfg.Releases.Get(ctx, id, req.Msg.GetTaskId(), req.Msg.GetArch())
	if err != nil {
		return nil, releaseError(err)
	}
	return connect.NewResponse(&heronv1.GetReleaseResponse{Sums: a.Sums, Signature: a.Signature, Archive: a.Archive}), nil
}

// releaseError 把中转的失败映射为 Connect 错误码。取回失败的原文回给更新器、进入任务的 error，
// 管理员据此判断是 hub 连不上 GitHub 还是产物验签不过；原文里只有地址与原因，没有凭据。
func releaseError(err error) error {
	switch {
	case errors.Is(err, updates.ErrArch):
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, updates.ErrNoTask):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, updates.ErrBusy), errors.Is(err, updates.ErrAttempts), errors.Is(err, updates.ErrCacheFull):
		return connect.NewError(connect.CodeResourceExhausted, err)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeCanceled, err)
	}
	return connect.NewError(connect.CodeUnavailable, fmt.Errorf("fetch official release: %w", err))
}

// foldResults 按归属与迟到预算逐条准入。task_id 未分配给本节点的结果不得写进本节点的历史：
// token 被挪用时它是伪造的，分配撤销后仍在途时它属于已不承担的任务。
// 超龄结果可能落在已冻结的分钟里；测量时刻由收到时刻减 age_ms 得到。
func (s *Service) foldResults(id int64, rs []*heronv1.ProbeResult) {
	if len(rs) == 0 {
		return
	}
	now := s.clk.Now()
	var foreign, late int
	for _, r := range rs {
		if !s.tasks.Assigned(id, r.GetTaskId()) {
			foreign++
			continue
		}
		age := time.Duration(r.GetAgeMs()) * time.Millisecond
		if age > MaxProbeAge {
			late++
			continue
		}
		https, _, configID, ok := s.tasks.CertPolicy(r.GetTaskId())
		s.live.AddProbe(id, now.Add(-age), r.GetTaskId(), r)
		taskID := r.GetTaskId()
		observed := append([]byte(nil), r.GetTaskConfigId()...)
		if r.CertNotAfterS != nil && ok && https && len(observed) > 0 && bytes.Equal(observed, configID) {
			notAfter := r.GetCertNotAfterS()
			// 证书观测是"最新值"不是时间序列：覆盖写 probe_cert，与分钟桶的折叠路径分开。
			// not_after 变化时经 CertObserved 触发一次证书到期评估；done 在写协程里执行，
			// 评估会写库，必须另起协程——写协程等自己就是死锁。
			s.writer.UpsertProbeCertAsync(id, taskID, notAfter, now.Unix(), observed, func(changed bool, err error) {
				if err != nil {
					s.log.Error("probe cert write failed", "node", id, "task", taskID, "err", err)
					return
				}
				if changed && s.cfg.CertObserved != nil {
					go s.cfg.CertObserved()
				}
			})
		}
		if p := r.GetPresented(); p != nil && ok && https && len(observed) > 0 && bytes.Equal(observed, configID) {
			spki := append([]byte(nil), p.GetSpkiSha256()...)
			reason := int32(p.GetReason())
			notAfter := p.GetNotAfterS()
			s.writer.UpsertPresentedAsync(id, taskID, observed, spki, notAfter, now.Unix(), reason, func(err error) {
				if err != nil {
					s.log.Error("probe cert candidate write failed", "node", id, "task", taskID, "err", err)
				}
			})
		}
	}
	if foreign > 0 || late > 0 {
		s.log.Warn("probe results dropped", "node", id, "unassigned", foreign, "too_old", late)
	}
}

// reconcileFacts 是电平触发的对账：agent 每次带摘要，hub 只在不一致时索要。
func (s *Service) reconcileFacts(id int64, token string, hash uint64, f *heronv1.Facts) bool {
	if f != nil {
		sanitizeFacts(f)
		s.writer.UpsertFactsAsync(id, hash, f, func(err error) {
			if err != nil {
				s.log.Error("facts write failed", "node", id, "err", err)
				return
			}
			s.mu.Lock()
			// 回调可晚于删除；在 mu 下复查 token 并发布摘要，与 Forget 的摘要清理互斥。
			if current, ok := s.auth.Authenticate(token); ok && current == id {
				s.factsHash[id] = hash
			}
			s.mu.Unlock()
		})
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	known, ok := s.factsHash[id]
	return !ok || known != hash
}

// Forget 在 auth 移除 token 后清理节点状态。stateMu 等待已鉴权上报退出，
// pendingMu 将 live 桶移交与重试队列清理串行化，因此返回后两处都不再持有该节点。
func (s *Service) Forget(nodeID int64) {
	// 唯一的调用方 nodeops.Service.Forget 的前提是库删除已提交、token 已撤销（在线删除由 nodeops.Service.Delete 在
	// auth.DeleteNode 返回 nil 之后调用它保证）。
	// Registry.Forget 会等在途管理写入完成整个 store 往返；写协程 facts 回调会取 mu，
	// 持 mu 等待可能形成等待环，持 stateMu 或 mu 会挡住其他节点的 Report，
	// 持 pendingMu 会挡住全体节点的分钟刷出，因此必须在所有 ingest 锁之外调用。
	s.tasks.Forget(nodeID)
	if s.cfg.Updates != nil {
		s.cfg.Updates.Forget(nodeID)
	}
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.live.Forget(nodeID)
	s.traffic.Forget(nodeID)
	s.limit.Forget(nodeID)
	s.mu.Lock()
	delete(s.factsHash, nodeID)
	delete(s.taskDigest, nodeID)
	delete(s.detection, nodeID)
	s.mu.Unlock()
	var pending []metric.Batch
	for _, batch := range s.pending {
		kept := batch.WithoutNode(nodeID)
		if !kept.Empty() {
			pending = append(pending, kept)
		}
	}
	s.pending = pending
}
