// Package api 实现 AdminService。
//
// 鉴权在挂载点的拦截器里裁决：每个方法以 heron.v1.access 声明准入口径，流式调用一律拒绝，
// 方法体只在准入通过后执行。凭据有两条路径：Authorization 的 scheme 为 Bearer 时走 API token，
// 此后不看 cookie；否则走会话 cookie。两条路径互不回退——若互相回退，实际生效的是较弱的那条。
package api

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/netip"
	"net/textproto"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/gen/heron/v1/heronv1connect"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/alert"
	"github.com/xjetry/heron/internal/hub/auth"
	"github.com/xjetry/heron/internal/hub/backup"
	"github.com/xjetry/heron/internal/hub/geo"
	"github.com/xjetry/heron/internal/hub/heartbeat"
	"github.com/xjetry/heron/internal/hub/live"
	"github.com/xjetry/heron/internal/hub/nodeops"
	"github.com/xjetry/heron/internal/hub/probe"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/hub/theme"
	"github.com/xjetry/heron/internal/hub/traffic"
	"github.com/xjetry/heron/internal/hub/updates"
	"github.com/xjetry/heron/internal/update"
)

const (
	SessionCookie = "heron_session"

	// AdminService 的解码预算按过程分两类，由 Handler 分派：UploadTheme 用 maxThemeBody，其余过程用 maxSettingsBody。
	// connect 先解码再进拦截器，未鉴权的请求也会被读到所在过程的预算，所以两类都必须有界，而主题包那么大的预算只给
	// 需要它的那一个过程——全部过程共用取大者，匿名请求在每个过程上都会被读到约 10.7 MiB。每个字段的合法取值都有
	// 字节上限是两条推导成立的前提。多余的 JSON 空白、对无需转义的字符的转义不在预算内：这样的请求超出预算时得到
	// resource_exhausted。

	// Settings 的预算由 settings_budget.go 的 settingsBudget 按字段登记、由 descriptor 汇总成 maxSettingsBody，
	// 不留隐含语法余量。encoding/json 默认写法下，每个码点的编码不超过其 UTF-8 字节数的六倍，逐码点用例核对这条上界。
	// U+0000–U+001F 中除 \b、\f、\n、\r、\t 外的码点，以及 <、>、&，会写成六字节转义；这五个控制字符只占两字节，
	// U+2028/2029 从三字节变成六字节。只限制清洗后的字符数不足以限制解码前的请求，字符串须有原始字节上限。

	// maxChannelIDJSONBytes 是渠道列表里一条合法 ID 在 JSON 里的最大份额：proto3 JSON 把 int64 写成带引号的十进制串，
	// 合法 ID 为正（渠道 id 从 1 起）、至多 19 位，连引号与分隔的逗号共 22 字节。负数与冗余写法（protojson 也收 1.000
	// 与 1e000）不是合法请求的最坏情况，不在预算内。方括号及末项没有逗号的差额由 budgetIDList 的类型规则计算。
	maxChannelIDJSONBytes = 22

	// maxThemeBody 是 UploadTheme 的解码预算，装下满额主题包的 JSON：bytes 在 JSON 里是带填充的标准 base64，8 MiB
	// 编码成 4 × ⌈8388608 / 3⌉ = 11184812 字节（base64 字母表无需转义；二进制编码是原样 8 MiB，更小）；另留 4 KiB 给
	// expect_id（合法值至多 32 个 ASCII 字符，每个最坏转义成 6 字节）、字段名与 JSON 语法。合计 11188908 字节，约
	// 10.7 MiB：这也是匿名请求在 UploadTheme 上被读到的上限。包本身的 8 MiB 由 theme.Parse 另行核对——二进制编码的
	// 请求在这个预算内能带更大的包。
	maxThemeBody = 4*((theme.MaxPackageBytes+2)/3) + 4<<10
)

type Config struct {
	// Updates 为 nil 时没有节点在线更新：绑定版本下发空串（boundAgent），更新目标列表里的节点只带"未报告支持"的
	// 占位状态，启动与取消节点更新返回 FailedPrecondition。缺省是不提供，不是放宽。
	Updates *updates.Manager
	// Backups 是分层备份管理器；New 将其视为装配错误并 panic 当它为 nil。
	Backups *backup.Manager
	// Heartbeat 是心跳循环，GetHeartbeatStatus 从它读进程内状态；New 将其视为装配错误并 panic 当它为 nil。
	// 接口字段的 typed nil 约束见 Deps。
	Heartbeat HeartbeatStatusProvider
	// TTL 必须为正；零值会放宽宽限期下限，New 将其视为装配错误并 panic。
	TTL time.Duration
	// ReportInterval 是 agent 的正常上报间隔，客户端据此选择轮询节奏。
	ReportInterval time.Duration
	TrustedProxies []netip.Prefix
	// HubVersion 原样经 GetSnapshotResponse.hub_version 下发。空串（装配时没传）与 dev 一样
	// 被面板当作非正式版本：给出 latest 安装命令、不标落后节点。
	HubVersion string
	// Location 是 hub 的 --timezone，days_left 按它的日历日算；New 要求非 nil。
	Location *time.Location
	// Retention 是 serve 交给维护循环的同一份保留期，存储健康按它判定最老桶是否超期。零值会把最老桶早于
	// 一个桶长加一个维护间隔之前的表都标成超期，New 用 Retention.Validate 把它当作装配错误拒绝。
	Retention store.Retention
	// PublicDir 为真时可信目录接管公开页，禁止启用托管主题。
	PublicDir bool
	// Geo 是 serve 选定并交给国家查询器的同一个后端对象，New 要求非 nil。面板回显的后端与本地库路径取自它
	// （Settings.geo_backend、geo_mmdb_path），不另由启动参数推导，回显因此不会与查询器实际用的后端分叉；仅回显，
	// 不落入运行设置。接口字段的 typed nil 约束见 Deps。
	Geo geo.Backend
}

// HeartbeatStatusProvider 是心跳循环持有的进程内状态。api 只读它：目标地址与开关都在设置里，状态不含凭据。
type HeartbeatStatusProvider interface {
	Status() heartbeat.Status
}

type Service struct {
	cfg   Config
	store *store.Store
	auth  *auth.Auth
	live  *live.Live
	// nodes 编排节点的建、改、删、轮换 token 与批量改标签；跨协作者的串行与提交后的清理都在它里面，节点 handler 只做
	// 校验、调用与错误映射。
	nodes    *nodeops.Service
	traffic  *traffic.Book
	probes   *probe.Registry
	alerts   *alert.Engine
	notifier *alert.Queue
	clk      clock.Clock
	log      *slog.Logger
	history  history
	// heartbeat 是心跳循环的进程内状态，GetHeartbeatStatus 读它；循环与设置都不在这里。
	heartbeat HeartbeatStatusProvider

	// access 是 AdminService 每个过程的准入口径，New 时从描述符读出，之后只读。
	access map[string]heronv1.Access

	// uploading 是容量 1 的信号量，UploadTheme 从校验到入库一直持有它：同一时刻至多一个请求在展开与入库，被引用着的
	// 展开内容至多一份（≤ theme.MaxTotalBytes），Parse 的解压也至多一路。占用时直接拒绝而不排队：到了方法体的请求
	// 已各自持有解码后的包，排队只会把它们攒在内存里。请求体的解码在方法体之前，不归它管，由 maxThemeBody 按请求设界。
	uploading chan struct{}
	// nodeListEpoch 是本进程的随机纪元，拼进 ListNodesResponse.revision：库里的计数在恢复备份后可能回到旧值，换进程
	// 即换纪元，旧标签页带来的版本一定对不上，不依赖计数单调。
	nodeListEpoch string
	github        *theme.GitHubClient
	previewMu     sync.Mutex
	previews      map[string]themePreviewGrant
	updateLocal   localUpdateClient
	updateSource  releaseSource
}

// boundAgent 是下发给面板与 API 的 hub 绑定 agent 版本（规则见 Makefile 的 AGENT_VERSION 注释），只读 updates.Manager：绑定只有它一个持有者。
// 没有 Manager 的装配下没有节点在线更新，下发空串，与“没有绑定”同义。
func (s *Service) boundAgent() string {
	if s.cfg.Updates == nil {
		return ""
	}
	return s.cfg.Updates.BoundAgent()
}

// Deps 是 Service 的协作者，全部必需：New 逐字段核对非 nil。New 对接口类型的字段（Config.Heartbeat、Config.Geo）
// 只能识别"没有给出"：装进接口的 nil 指针（typed nil）不等于 nil，会通过核对、到第一次调用才空指针，装配方不得这样传。
type Deps struct {
	Store    *store.Store
	Auth     *auth.Auth
	Live     *live.Live
	Nodes    *nodeops.Service
	Traffic  *traffic.Book
	Probes   *probe.Registry
	Alerts   *alert.Engine
	Notifier *alert.Queue
	Clock    clock.Clock
	Log      *slog.Logger
}

// New 对配置与依赖的一切缺陷 panic，信息带包名与字段名；hub 其余组件的构造函数（NewPublic、ingest.New、
// alert.NewQueue）沿用同一口径并指向这里。理由：装配只发生在 serve 与测试夹具里，用户输入（flag、环境变量）已由
// serve 的 options 解析校验过，读库、监听、Load 这些运行期可能失败的步骤不在构造函数里；到这里还不合法的配置或
// 缺失的依赖只能是装配代码写错了，不是运行期可以恢复的状态。返回 error 只会让每个调用点多一段永远走不到的分支，
// 而 panic 让写错的装配在第一次启动或第一个测试里就停下。
//
// 先核对 Config 再核对 Deps，各自按字段声明顺序，报出第一处缺陷。
func New(cfg Config, deps Deps) *Service {
	if cfg.TTL <= 0 {
		panic("api.Config.TTL must be positive")
	}
	if cfg.Location == nil {
		panic("api.Config.Location must be set")
	}
	if cfg.Backups == nil {
		panic("api.Config.Backups must be set")
	}
	if cfg.Heartbeat == nil {
		panic("api.Config.Heartbeat must be set")
	}
	if err := cfg.Retention.Validate(); err != nil {
		panic("api.Config.Retention: " + err.Error())
	}
	if cfg.Geo == nil {
		panic("api.Config.Geo must be set")
	}
	if deps.Store == nil {
		panic("api.Deps.Store must be set")
	}
	if deps.Auth == nil {
		panic("api.Deps.Auth must be set")
	}
	if deps.Live == nil {
		panic("api.Deps.Live must be set")
	}
	if deps.Nodes == nil {
		panic("api.Deps.Nodes must be set")
	}
	if deps.Traffic == nil {
		panic("api.Deps.Traffic must be set")
	}
	if deps.Probes == nil {
		panic("api.Deps.Probes must be set")
	}
	if deps.Alerts == nil {
		panic("api.Deps.Alerts must be set")
	}
	if deps.Notifier == nil {
		panic("api.Deps.Notifier must be set")
	}
	if deps.Clock == nil {
		panic("api.Deps.Clock must be set")
	}
	if deps.Log == nil {
		panic("api.Deps.Log must be set")
	}
	return &Service{
		cfg: cfg, store: deps.Store, auth: deps.Auth, live: deps.Live, nodes: deps.Nodes, traffic: deps.Traffic, probes: deps.Probes,
		alerts: deps.Alerts, notifier: deps.Notifier, clk: deps.Clock, log: deps.Log,
		history:   history{store: deps.Store, probes: deps.Probes, log: deps.Log, gate: newHistoryGate()},
		heartbeat: cfg.Heartbeat,
		access:    accessTable(heronv1.File_heron_v1_admin_proto.Services().ByName("AdminService")),
		uploading: make(chan struct{}, 1), nodeListEpoch: rand.Text(),
		github: theme.NewGitHubClient(), previews: make(map[string]themePreviewGrant),
		updateLocal: update.NewClient("hub"), updateSource: update.NewOfficialSource(),
	}
}

// today 是 hub 时区（--timezone）的今天，days_left 以它为基准。
func (s *Service) today() time.Time { return alert.Today(s.clk.Now(), s.cfg.Location) }

// Handler 是 AdminService 的挂载点。三个 connect 处理器挂同一个 Service 与同一个鉴权拦截器，只差解码预算与响应压缩；
// 请求按路径分给它们：UploadTheme 走大预算，compressed 里的过程走可压缩的那个，其余走小预算、不压缩的那个。生成的
// 处理器分派过程用的也是 r.URL.Path 的精确相等，所以另外两个处理器只会执行各自分到的过程。
func (s *Service) Handler() (string, http.Handler) {
	// 管理响应默认不压缩：节点表等响应把私有字段（备注、来源地址）与 agent 自报的任意字符串（Facts 里的主机名等）放在
	// 一起，控制一台 agent 又能看到响应长度的人，可以借压缩后的长度逐字试探私有字段。只抬高响应压缩门槛，不移除 gzip
	// 支持，保证压缩请求仍可解码。可压缩的过程只有 compressed 里的两个，各自的理由如下。
	//
	// GetSnapshot：面板按 POLL_MS（web/src/lib/poll.ts）轮询它，响应随节点数线性增长，不压缩时持续流量是压缩后的数倍。
	// 它的字符串只有节点名（含非公开节点，属于要保护的
	// 内容）、hub 构建时注入的两个版本号，以及 agent 写入、但写法受准入约束的 boot_id（UUID，ingest.validateBootID）与
	// net_counter_epoch（小写十六进制摘要）；其余是数值，以及 hub 判定的 online 与管理员设定的 quota_mode。所以 agent
	// 能放进这条响应的只有十六进制字符、连字符与数值写法里的字符：节点名里由这些字符组成的片段仍可被试探，其余字符不能。
	// TestSnapshotStringFieldsAreAudited 枚举这条响应可达的全部字符串字段，新增字段必须先过这条判断再放进去。
	//
	// ListNodes：有意放弃上面那条防线。响应的主体是 agent 自报的内容（facts、地址显示值、来源与国家查询，160 个节点的
	// 生产库实测约占四分之三），把管理员资料单独拆出去压缩几乎没有收益；不压缩时一次整份读取在慢线路上要几十秒。残余
	// 风险：同时控制一台 agent（可改自己的 facts 字符串与上报地址）并能观察管理员浏览器流量密文长度的人，可以逐次改
	// facts、看整份响应长度的变化，试探同一响应里的备注、地址与其它节点的 facts。条件读取（ListNodesRequest.known_revision）
	// 让未变的轮询只回版本号，但每次 facts 变化都会推进版本、换来一次整份响应，所以它不削弱这一点。
	compressed := map[string]bool{
		heronv1connect.AdminServiceGetSnapshotProcedure: true,
		heronv1connect.AdminServiceListNodesProcedure:   true,
	}
	access := connect.WithInterceptors(s.accessInterceptor())
	closed := connect.WithCompressMinBytes(math.MaxInt)
	path, rest := heronv1connect.NewAdminServiceHandler(s, access, closed, connect.WithReadMaxBytes(maxSettingsBody))
	_, upload := heronv1connect.NewAdminServiceHandler(s, access, closed, connect.WithReadMaxBytes(maxThemeBody))
	_, compressible := heronv1connect.NewAdminServiceHandler(s, access, connect.WithReadMaxBytes(maxSettingsBody))
	return path, auth.WebAuthnContext(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// no-transform 要求遵守该指令的反代/CDN 不改写响应，包括重新压缩。
		// 来源检查会直接写出 403，缓存头必须在它之前设置，才能覆盖这条提前返回路径。
		w.Header().Set("Cache-Control", "no-store, no-transform")
		// 不透明沙箱和跨源浏览器请求在解码与鉴权之前拒绝，不能依赖 CORS 阻止副作用。
		if !auth.SameOriginRequest(r, s.cfg.TrustedProxies) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"code":"permission_denied","message":"管理请求来源不匹配；使用 HTTPS 反代时请保留 Host、转发协议并配置 --trusted-proxies"}`))
			return
		}
		scheme, err := auth.TrustedRequestScheme(r, s.cfg.TrustedProxies)
		if err != nil {
			scheme = "http"
		}
		r = r.WithContext(context.WithValue(r.Context(), schemeKey{}, scheme))
		switch {
		case r.URL.Path == heronv1connect.AdminServiceUploadThemeProcedure:
			upload.ServeHTTP(w, r)
		case compressed[r.URL.Path]:
			compressible.ServeHTTP(w, r)
		default:
			rest.ServeHTTP(w, r)
		}
	}), s.cfg.TrustedProxies)
}

type sessionKey struct{}
type peerKey struct{}
type schemeKey struct{}

// peerInfo 由拦截器统一计算；转发头只有来自可信代理时才参与来源地址与协议判定。
type peerInfo struct {
	from   netip.Addr
	scheme string
}

func unauthenticated(msg string) error {
	return connect.NewError(connect.CodeUnauthenticated, errors.New(msg))
}

func internalError(msg string) error {
	return connect.NewError(connect.CodeInternal, errors.New(msg))
}

func invalid(format string, args ...any) error {
	return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(format, args...))
}

func notFound(id int64) error {
	return connect.NewError(connect.CodeNotFound, fmt.Errorf("node %d does not exist", id))
}

func permissionDenied(format string, args ...any) error {
	return connect.NewError(connect.CodePermissionDenied, fmt.Errorf(format, args...))
}

// bearerCredential 按 scheme 选凭据路径。scheme 为 Bearer（大小写不敏感）即走 token 路径，
// 哪怕 token 为空，此后不再看 cookie。其他 scheme 不是 hub 的凭据，按不存在处理：反代做 Basic
// 认证时浏览器对每个请求自动附带 Authorization: Basic，若因此选了 token 路径，面板的每个请求都会被拒。
// 同一请求带多个 Bearer 无从判定该用哪个，按无效凭据处理。
func bearerCredential(h http.Header) (string, bool, error) {
	var found []string
	for _, v := range h.Values("Authorization") {
		scheme, rest, _ := strings.Cut(strings.TrimSpace(v), " ")
		if strings.EqualFold(scheme, "Bearer") {
			found = append(found, strings.TrimSpace(rest))
		}
	}
	switch {
	case len(found) == 0:
		return "", false, nil
	case len(found) > 1:
		return "", true, errors.New("multiple bearer credentials")
	case found[0] == "":
		return "", true, errors.New("empty bearer token")
	}
	return found[0], true, nil
}

type accessInterceptor struct{ s *Service }

func (s *Service) accessInterceptor() connect.Interceptor { return accessInterceptor{s: s} }

func (i accessInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	s := i.s
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		level, ok := s.access[req.Spec().Procedure]
		if !ok {
			return nil, unauthenticated("unauthenticated")
		}
		scheme, _ := ctx.Value(schemeKey{}).(string)
		peer := peerInfo{
			from:   auth.ClientIP(req.Peer().Addr, req.Header().Values("X-Forwarded-For"), s.cfg.TrustedProxies),
			scheme: scheme,
		}
		ctx = context.WithValue(ctx, peerKey{}, peer)
		// 先鉴别身份再裁决权限：无效 token 调任何方法都是 401；有效 token 只读 READ，
		// 写入只能经 CHANGE 再检查具体权限，不能借已有写权限直接调用会话方法。
		// Login 带 Bearer 也落在 403，但文案不能写成"去开一个面板会话"：token 代替不了密码。
		if tok, isBearer, err := bearerCredential(req.Header()); isBearer {
			if err != nil {
				return nil, unauthenticated(err.Error() + "; send exactly one Authorization: Bearer <API token>")
			}
			principal, err := s.auth.AuthenticateAPIToken(ctx, tok)
			if err != nil {
				s.log.Error("API token lookup failed", "err", err)
				return nil, internalError("API token lookup failed")
			}
			if principal == nil {
				return nil, unauthenticated("API token unknown or revoked")
			}
			if level != heronv1.Access_ACCESS_READ && !(level == heronv1.Access_ACCESS_CHANGE && len(principal.Permissions) > 0) {
				if level == heronv1.Access_ACCESS_LOGIN {
					return nil, permissionDenied("%s: API tokens cannot log in; send the admin password without an Authorization: Bearer header", req.Spec().Procedure)
				}
				return nil, permissionDenied("%s: use ExecuteChange with a preauthorized write permission; other write methods require a panel session", req.Spec().Procedure)
			}
			if !principal.AllNodes && !scopedReadAllowed(req.Spec().Procedure) && level != heronv1.Access_ACCESS_CHANGE {
				return nil, permissionDenied("this method requires a site-wide API token")
			}
			return next(store.WithPrincipal(ctx, *principal), req)
		}
		if level == heronv1.Access_ACCESS_LOGIN {
			// 凭据是请求体里的密码，由 Login 裁决；按来源的锁定也在那里。
			return next(ctx, req)
		}
		candidates := sessionCandidates(req.Header())
		if len(candidates) == 0 {
			return nil, unauthenticated("no session cookie; call Login first")
		}
		tok, alive, err := s.auth.AuthenticateSession(ctx, candidates)
		if err != nil {
			s.log.Error("session lookup failed", "err", err)
			return nil, internalError("session lookup failed")
		}
		if !alive {
			return nil, unauthenticated("session expired or unknown; call Login again")
		}
		// ctx 里放通过校验的那个 token，而不是第一个候选：Logout、撤销当前会话与会话列表的"当前"都读它。
		// 放第一个候选时，排在前面的无效值会让 Logout 吊销一个不存在的会话，真正在用的会话照旧有效。
		return next(context.WithValue(ctx, sessionKey{}, tok), req)
	}
}

func (i accessInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i accessInterceptor) WrapStreamingHandler(connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(context.Context, connect.StreamingHandlerConn) error {
		return unauthenticated("unauthenticated")
	}
}

// sessionCandidates 按出现顺序返回请求里全部非空的 heron_session 值，跨所有 Cookie 字段行。
//
// 请求里的 cookie 不全由 hub 签发：与面板同属一个父域的主机能写 Domain 为父域的 cookie，浏览器把它们与管理员的
// host-only 会话放进同一个 Cookie 行，同名时路径更长的排在前面（RFC 6265 第 5.4 节）。会话读取的不变式是多出来的
// cookie 不能让有效会话失效，否则任何能写父域 cookie 的主机都能把管理员锁在面板外。由此：
//   - 返回全部同名值，由 auth.AuthenticateSession 逐个校验、任一有效即通过。
//   - 逐对切分、只认名字，不校验其他 cookie。http.ParseCookie 在同一行里任一 cookie 不合它的语法时整行报错：
//     没有 "="、名字不是 token、去掉两端成对的双引号之后值里仍有双引号、值里有非 ASCII 字节都算，而这些 cookie
//     由别的主机写，形状不归 hub 管。
//   - 不设个数上限，也不用 Request.Cookies：上限让写 cookie 的一方能用更多的值把有效值挤出去；
//     Request.Cookies 遇到超过 3000 个 cookie 时整体返回空，http.ParseCookie 则报错。
//     候选数与校验成本由请求头的大小上限约束，推导见 auth.AuthenticateSession。
//
// 每一对的切分与 Request.Cookies 相同：去掉两端空白，按第一个 "=" 分成名字与值，名字去空白后比较，值两端成对的
// 双引号去掉。值的字节不在这里校验：不是 token 形状的值由 auth.AuthenticateSession 在哈希前丢弃。
func sessionCandidates(h http.Header) []string {
	var out []string
	for _, line := range h.Values("Cookie") {
		for part := range strings.SplitSeq(line, ";") {
			name, value, ok := strings.Cut(textproto.TrimString(part), "=")
			if !ok || textproto.TrimString(name) != SessionCookie {
				continue
			}
			if len(value) > 1 && value[0] == '"' && value[len(value)-1] == '"' {
				value = value[1 : len(value)-1]
			}
			if value != "" {
				out = append(out, value)
			}
		}
	}
	return out
}

// sessionCookie 不设 Domain，避免会话被发送到其他子域；它不隔离同主机不同端口。
// 跨站发送由 SameSite 限制，跨源读取由不下发 CORS 允许头限制。
// Secure 只由可信代理转发的协议决定。
func sessionCookie(value string, secure bool, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name: SessionCookie, Value: value, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: secure, MaxAge: maxAge,
	}
}

func clearSessionCookie(ctx context.Context, header http.Header) {
	peer := ctx.Value(peerKey{}).(peerInfo)
	header.Add("Set-Cookie", sessionCookie("", peer.scheme == "https", -1).String())
}

func (s *Service) Login(ctx context.Context, req *connect.Request[heronv1.LoginRequest]) (*connect.Response[heronv1.LoginResponse], error) {
	peer := ctx.Value(peerKey{}).(peerInfo)
	tok, err := s.auth.LoginFactors(ctx, req.Msg.GetPassword(), req.Msg.GetOtp(), req.Msg.GetRecoveryCode(), peer.from)
	switch {
	case errors.Is(err, auth.ErrLoginBusy):
		return nil, connect.NewError(connect.CodeResourceExhausted, errors.New("password verification is busy; please try again later"))
	case errors.Is(err, auth.ErrLocked):
		return nil, unauthenticated("too many failed logins from this source (one IPv4 address, or one IPv6 /64); retry in 15 minutes")
	case errors.Is(err, auth.ErrNoAdmin), errors.Is(err, auth.ErrBadPassword), errors.Is(err, auth.ErrSecurity), errors.Is(err, store.ErrAdminChanged):
		// 对外同一响应，避免匿名调用方由错误内容判断管理员是否已配置。
		return nil, unauthenticated("密码或第二认证因素无效")
	case err != nil:
		s.log.Error("login failed", "err", err)
		return nil, internalError("login failed")
	}
	resp := connect.NewResponse(&heronv1.LoginResponse{})
	resp.Header().Add("Set-Cookie", sessionCookie(tok, peer.scheme == "https", int(auth.SessionAbsolute/time.Second)).String())
	return resp, nil
}

func (s *Service) Logout(ctx context.Context, _ *connect.Request[heronv1.LogoutRequest]) (*connect.Response[heronv1.LogoutResponse], error) {
	tok := ctx.Value(sessionKey{}).(string)
	if err := s.auth.Logout(ctx, tok); err != nil {
		s.log.Error("logout failed", "err", err)
		return nil, internalError("logout failed")
	}
	resp := connect.NewResponse(&heronv1.LogoutResponse{})
	clearSessionCookie(ctx, resp.Header())
	return resp, nil
}
