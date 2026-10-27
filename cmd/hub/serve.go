package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"sync"
	"time"

	"github.com/xjetry/heron/internal/agentwire"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/alert"
	"github.com/xjetry/heron/internal/hub/api"
	"github.com/xjetry/heron/internal/hub/auth"
	"github.com/xjetry/heron/internal/hub/backup"
	"github.com/xjetry/heron/internal/hub/geo"
	"github.com/xjetry/heron/internal/hub/heartbeat"
	"github.com/xjetry/heron/internal/hub/ingest"
	"github.com/xjetry/heron/internal/hub/live"
	"github.com/xjetry/heron/internal/hub/nodeops"
	"github.com/xjetry/heron/internal/hub/outbound"
	"github.com/xjetry/heron/internal/hub/probe"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/hub/traffic"
	"github.com/xjetry/heron/internal/hub/updates"
	"github.com/xjetry/heron/internal/hub/web"
	"github.com/xjetry/heron/internal/releasesig"
	"github.com/xjetry/heron/internal/shutdown"
	"github.com/xjetry/heron/internal/tcpstall"
	"github.com/xjetry/heron/internal/update"
)

type mount struct {
	path string
	h    http.Handler
}

func mountOf(path string, h http.Handler) mount { return mount{path: path, h: h} }

// newMux 是面板所在 origin（主 origin）的挂载点；挂载点级测试（TestMuxRejectsAnonymousProcedures）从注册表枚举方法逐个
// 匿名调用，所以任何进了描述符的服务都必须在这里出现。除 PublicService 外，每个服务都带着它的鉴权拦截器；PublicService
// 不鉴权，它的全部过程构成那个测试的匿名白名单（publicProcedures），其余过程匿名调用必须得到 401。
// 往 PublicService 加方法等于把它公开给任何人，总闸只决定整站是否开放，不提供身份鉴权。
func newMux(mounts ...mount) *http.ServeMux {
	mux := http.NewServeMux()
	for _, m := range mounts {
		mux.Handle(m.path, m.h)
	}
	return mux
}

// routes 是生产服务与挂载点测试共用的 HTTP 装配。
type routes struct {
	agent, admin, public mount
	page                 http.Handler
	publicEnabled        func() bool
}

// 管理面板与 RPC 使用明确挂载点；其余路径统一经过公开总闸，包含主题资源与预览。
func newHandler(r routes) http.Handler {
	if r.publicEnabled == nil {
		panic("routes.publicEnabled is required")
	}
	return newMux(r.agent, r.admin,
		// /healthz 在打开库、加载索引、挂载全部服务之后放进同一个 mux，因此与 srv.Serve 同生命周期。
		mountOf("/healthz", http.HandlerFunc(healthzHandler)),
		mountOf(r.public.path, api.WithAdminPath(r.public.h, web.Prefix)),
		mountOf(web.Prefix, web.Handler()),
		mountOf("/", web.PublicGate(r.page, r.publicEnabled)))
}

func runServe(args []string) error {
	log := newServeLogger(os.Stderr)
	// 关停中 Ctrl-C 立即强退、重复 SIGTERM 仍排空到落盘、启动方对 SIGINT 的忽略沿用，三条都由 shutdown.Notify
	// 保证；stop 推迟到 runServeWith 返回，SIGTERM 的订阅因此覆盖整个排空。
	stopCtx, stop, interruptIgnored := shutdown.Notify()
	defer stop()
	if interruptIgnored {
		log.Info(shutdown.IgnoredInterruptNotice)
	}
	return runServeWith(stopCtx, args, clock.Real(), log)
}

// newServeLogger 是 serve 的日志装配。启动行的文本格式有外部读者（scripts/e2e.sh 按整秒读字段），
// 测试经同一个函数装配日志，才钉得住读者实际看到的格式。
func newServeLogger(w io.Writer) *slog.Logger { return slog.New(slog.NewTextHandler(w, nil)) }

// runServeWith 由调用方拥有停止信号；后台循环与请求排空完成后才能关闭它们共用的库。
func runServeWith(stopCtx context.Context, args []string, clk clock.Clock, log *slog.Logger) error {
	opts, err := parseServeOptions(args, os.LookupEnv)
	if err != nil {
		return err
	}
	h, err := newHub(opts, clk, log)
	if err != nil {
		return err
	}
	return h.run(stopCtx, log)
}

// serveOptions 是 serve 的全部配置，parseServeOptions 产出时每个字段都已解析校验成消费方要的类型；newHub 与 run
// 不再解析或校验用户输入，装配期构造函数据此把剩下的缺陷当作装配错误（见 api.New）。
type serveOptions struct {
	db     string
	listen string
	// adminOrigin 是 --admin-origin 经 auth.ParsePasskeyOrigin 规范化后的值，空串表示未给出。
	adminOrigin string
	retention   store.Retention
	loc         *time.Location
	// zoneFallback 表示 --timezone 缺席且本机时区不可判定，loc 回落为 UTC；newHub 据此告警。
	zoneFallback bool
	ttl          time.Duration
	trusted      []netip.Prefix
	// publicDir 是 --public-dir 的原值，空串表示用内置公开页；publicPage 是已核对过的对应 handler。
	publicDir  string
	publicPage http.Handler
	// geoMMDBSet 区分 --geo-mmdb 缺席（走 HTTP 后端）与给出（哪怕是空串，也必须打开并在失败时报错）。
	geoMMDB    string
	geoMMDBSet bool
	// offlineReload 是检查离线变更代数的周期（见 offlineReloadEvery）。不是用户输入：parseServeOptions 给生产值，
	// 测试在 newHub 之前改小。
	offlineReload time.Duration
}

// offlineReloadEvery 是运行中的 hub 检查离线变更代数的周期。它是离线子命令生效的延迟上界之一：rotate-token 换发的
// 安装凭据、node create 建的节点，要等下一次检查重载 token 映射之后 hub 才认得，在那之前拿新凭据注册会被拒绝；
// 运维从命令输出复制凭据、再到节点上跑安装命令，通常不止一秒。无变更的周期只在读连接池上做一次单行点查，每秒一次
// 对 hub 可以忽略；有变更的周期才重建 token 映射与探测任务缓存，离线子命令由人手动执行，频率很低。
const offlineReloadEvery = time.Second

// parseServeOptions 解析并校验 serve 的全部用户输入，不打开数据库、不监听：配置有误时 serve 不留下任何副作用就退出。
// 每个 flag 都可以由 HERON_<FLAG> 给出（applyFlagEnv），lookupEnv 是它的唯一来源（runServeWith 传 os.LookupEnv）；
// --timezone 连同 HERON_TIMEZONE 都缺席时 loadZone 读的 TZ 与 /etc/localtime 是时区自己的回落，不经它。
func parseServeOptions(args []string, lookupEnv func(string) (string, bool)) (serveOptions, error) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	db := fs.String("db", "heron.db", "SQLite database path")
	geoMMDB := fs.String("geo-mmdb", "", fmt.Sprintf("local MaxMind country database (at most %d MiB); overrides the HTTP lookup service and makes no network requests; read fully into memory and structurally verified at startup (the file carries no checksum, so a value replaced by another valid value is not detected, only structural damage such as invalid UTF-8 is) and not read again while running, so a replaced file takes effect on restart; geo.enabled still controls lookup", geo.MaxMMDBBytes>>20))
	tz := fs.String("timezone", "", "IANA time zone for traffic period boundaries and node expiry days (default: the host's zone, resolved from TZ or /etc/localtime; UTC if neither resolves); already-persisted period starts are interpreted in the new zone; usage of the current period may be reset at the next read, report or flush")
	listen := fs.String("listen", "127.0.0.1:8080", "listen address")
	adminOrigin := fs.String(adminOriginFlag, "", "legacy Passkey origin, used only to migrate existing credentials without a persisted binding; new registrations bind the current HTTPS origin automatically; an invalid value is rejected at startup even when a binding is persisted")
	proxies := fs.String("trusted-proxies", "", "comma-separated CIDRs whose X-Forwarded-For / X-Forwarded-Proto are trusted; empty trusts none. Behind a reverse proxy, list the proxy here: the public page and agent registration are rate-limited per source (one IPv4 address, or one IPv6 /64), and failed logins are locked out per source, so without it every visitor shares the proxy address's single bucket and lockout; a node's recorded source address is also the proxy address")
	publicDir := fs.String("public-dir", "", "serve this directory at / instead of the built-in public page; files are opened through os.Root, so paths cannot leave the directory and symbolic links are followed only if they are relative and never step outside it (absolute links are refused even when they point inside); a path that is not a file, or that has a segment starting with a dot (.git, .env, .well-known), gets the directory's index.html (404 under assets/); every response is no-cache. The directory shares the admin panel's origin: its scripts can read the panel and call the admin API with the session of any signed-in administrator who opens the page, so put only content you trust as much as the hub binary there")
	offlineAfter := fs.String(offlineAfterFlag, defaultTTL.String(), fmt.Sprintf("how long a node may go without reporting before it is shown offline, between %v and %v; the agent report interval, its retry backoff ceiling and the minimum node offline grace are derived from it; empty means the default", minTTL, agentwire.MaxTTL))
	retention := store.DefaultRetention
	fs.DurationVar(&retention.M1, "retention-1m", retention.M1, fmt.Sprintf("how long to keep 1-minute rows (minimum %s)", store.MinRetentionM1))
	fs.DurationVar(&retention.M5, "retention-5m", retention.M5, fmt.Sprintf("how long to keep 5-minute rows (minimum %s)", store.MinRetentionM5))
	fs.DurationVar(&retention.H1, "retention-1h", retention.H1, fmt.Sprintf("how long to keep hourly rows (minimum %s)", store.MinRetentionH1))
	fs.DurationVar(&retention.AlertEvents, "retention-alert-events", retention.AlertEvents, fmt.Sprintf("how long to keep alert events and their deliveries (minimum %s)", store.MinRetentionAlertEvents))
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage of serve:\nEvery flag can also be given as the environment variable HERON_<FLAG> (dashes become underscores, upper case: --%s is %s); an explicit flag takes precedence, and a variable set to the empty string is the same as the flag given empty.\n", offlineAfterFlag, flagEnvName(offlineAfterFlag))
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return serveOptions{}, err
	}
	if err := applyFlagEnv(fs, lookupEnv); err != nil {
		return serveOptions{}, err
	}
	opts := serveOptions{db: *db, listen: *listen, publicDir: *publicDir, geoMMDB: *geoMMDB, offlineReload: offlineReloadEvery}
	// 缺席才选择 HTTP；显式空路径也必须打开并报错，不能把部署配置错误变成意外出网。HERON_GEO_MMDB 经 applyFlagEnv
	// 回填后同样算作给出，设为空串也是显式空路径。
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "geo-mmdb" {
			opts.geoMMDBSet = true
		}
	})
	if err := retention.Validate(); err != nil {
		return serveOptions{}, err
	}
	opts.retention = retention
	var err error
	if opts.loc, opts.zoneFallback, err = loadZone(*tz); err != nil {
		return serveOptions{}, err
	}
	if opts.ttl, err = parseTTL(*offlineAfter); err != nil {
		return serveOptions{}, err
	}
	if opts.trusted, err = auth.ParsePrefixes(*proxies); err != nil {
		return serveOptions{}, err
	}
	// 旧 Passkey 来源只在库里没有持久绑定时才被 ConfigureWebAuthn 读到，但格式在这里不看绑定就校验：非法值是配置错误，
	// 不能因为库里已有绑定就被放过，等到换库或 security-reset 清掉绑定后的那次启动才暴露。
	if *adminOrigin != "" {
		if opts.adminOrigin, _, err = auth.ParsePasskeyOrigin(*adminOrigin); err != nil {
			return serveOptions{}, fmt.Errorf("%s: %w", flagSource(adminOriginFlag), err)
		}
	}
	// 替换目录在打开数据库之前核对：配置有误时 hub 不留下任何副作用就退出。
	opts.publicPage = web.PublicHandler()
	if *publicDir != "" {
		if opts.publicPage, err = web.DirHandler(*publicDir); err != nil {
			return serveOptions{}, err
		}
	}
	return opts, nil
}

// hub 是装配完成、尚未运行的服务：newHub 产出它，run 是它唯一的生命周期所有者。在 run 之前，持有的部件里只有库
// 占着资源（连接与它自己的协程，由 Close 回收）：其余部件的构造函数不起协程、不占文件或连接，协程要到它们的 Run 方法或请求处理里才起，
// 而这两者都只在 run 里发生。所以装配失败时 newHub 关库即可回收全部；给部件加构造期资源的改动必须同时改这里的回收。
type hub struct {
	opts serveOptions

	st       *store.Store
	live     *live.Live
	ingest   *ingest.Service
	updates  *updates.Manager
	relay    *updates.Relay
	book     *traffic.Book
	alerts   *alert.Engine
	notifier *alert.Queue
	reports  *alert.Reporter
	geo      *geo.Resolver
	backups  *backup.Manager
	hb       *heartbeat.Heartbeat
	reload   *nodeops.Reloader
	handler  http.Handler
	// geoLog 是选定的国家查询后端，追加在启动行末尾。
	geoLog []any
	// gate 是启动更新门（update.Client.Gate）：本机有待确认的更新时，向本机更新器报告这个版本已就绪。run 在监听建立
	// 之后、Serve 之前调用它，报告就绪时监听已经绑上；失败则不服务。生产取 hub 角色的更新器客户端，测试替换它。
	gate func(context.Context) error
}

// newHub 打开库、装配全部部件并从库加载内存状态。失败时自己关闭已打开的库；成功后关库的义务连同库一起交给 run。
func newHub(opts serveOptions, clk clock.Clock, log *slog.Logger) (_ *hub, result error) {
	if opts.zoneFallback {
		log.Warn("host time zone could not be resolved; using UTC; set --timezone explicitly")
	}
	if !isLoopback(opts.listen) {
		// 判定只看监听地址，不探测容器或网络模式，也不承诺"发布到回环就一定没有旁路"：容器桥接网络里是否对外，
		// 取决于发布端口绑定的宿主地址（如 -p 127.0.0.1:8080:8080 再经反代）；host 网络或裸机上取决于本监听地址与防火墙。
		log.Warn("listening on a non-loopback address: direct access bypasses the proxy, and forwarded headers are trusted only from configured peers; "+
			"exposure depends on deployment - in a container bridge network it follows the host address the published port binds to "+
			"(e.g. -p 127.0.0.1:8080:8080 behind a reverse proxy), with host networking or on bare metal it follows this listen address and the firewall", "listen", opts.listen)
	}
	// 通知渠道、国家查询与心跳外推共用一个出站客户端，三者不跟随重定向、带总时限的
	// 行为因此是同一份。时限取 alert.NotifyTimeout，推导在通知投递一侧（见其注释）；国家查询与心跳都是应答至多读一个小上界的
	// 消费方，同属 outbound.NewClient 所说的请求与应答都有小上界的消费方。
	client := outbound.NewClient(alert.NotifyTimeout)
	// 国家查询的后端只在这里选一次，同一个对象交给查询器与 api：面板回显的后端就是查询器实际用的那个。
	// 文件路径是部署配置，由启动参数指定，设置 API 没有可改它的字段。OpenMMDB 在这里把整个文件读进内存并做结构校验，运行期
	// 不再访问文件，所以换文件要重启才生效，原地覆盖或截断也不影响运行中的答案。
	// 显式选择本地库后不能静默退回 HTTP，否则运维以为不出网时会把节点地址送到外部。
	// geoLog 随选定的后端一起写出，追加在启动行末尾：运维据此确认国家查询会不会出网、加载的是哪一版本地库。
	// 本地库在打开数据库之前读：文件有误时 hub 不留下任何副作用就退出。
	var geoBackend geo.Backend = geo.NewHTTP(client)
	geoLog := []any{"geo_backend", "http"}
	if opts.geoMMDBSet {
		local, err := geo.OpenMMDB(opts.geoMMDB)
		if err != nil {
			return nil, err
		}
		geoBackend = local
		md := local.Metadata()
		geoLog = []any{"geo_backend", "mmdb", "geo_mmdb", local.MMDBPath(), "geo_mmdb_type", md.DatabaseType,
			"geo_mmdb_built", md.BuildTime().UTC().Format(time.RFC3339)}
	}

	st, err := store.Open(opts.db, clk, log, store.MigrateSchema)
	if err != nil {
		return nil, err
	}
	defer func() {
		if result != nil {
			result = errors.Join(result, st.Close())
		}
	}()
	ttl, loc := opts.ttl, opts.loc
	reg := probe.New(st, log)
	l := live.New(clk, ttl)
	book := traffic.New(st, clk, loc, log)
	alerts := alert.New(alert.Config{TTL: ttl, Location: loc}, st, st.Evaluation(), l, clk, log)
	alerts.SetTraffic(book)
	notifier := alert.NewQueue(alert.QueueConfig{}, alert.QueueDeps{Store: st, Channels: alerts.Channels, Client: client, Clock: clk, Log: log})
	alerts.SetSender(notifier)
	a := auth.New(st, reg, notifier, clk, loc, log)
	// opts.adminOrigin 已由 parseServeOptions 校验，这里剩下的失败只来自读、解析 admin_security 或写入导入的绑定：
	// 前缀点名这一步而不是 flag，损坏的库不被报成配置问题。
	if err := a.ConfigureWebAuthn(opts.adminOrigin); err != nil {
		return nil, fmt.Errorf("passkey binding: %w", err)
	}
	if opts.adminOrigin != "" {
		log.Warn("--admin-origin is only for legacy Passkey migration; persisted bindings take precedence, remove this flag after migration")
	}
	updateManager := updates.New(st, clk, log, agentVersion)
	official := update.NewOfficialSource()
	relay := updates.NewRelay(updateManager,
		func(ctx context.Context, version, arch string) (update.Artifacts, error) {
			return official.Fetch(ctx, update.Request{Version: version}, "agent", arch)
		},
		// 与节点更新器同一个接受函数：hub 处的预验签只为尽早报错，判定标准不能与节点不同。
		func(version, arch string, a update.Artifacts) error {
			_, err := update.Accept(releasesig.Trusted(), "agent", arch, version, a)
			return err
		}, clk)
	svc := ingest.New(ingest.Config{TTL: ttl, TrustedProxies: opts.trusted, Updates: updateManager, Releases: relay,
		// 证书观测的 not_after 变化即评估一次证书到期规则，续期不必等到日界；
		// ingest 已从写协程另起协程调用，这里直接取 writeMu 扫描。评估失败只记日志：
		// 观测已落库，下一次日界或变化会再评估。
		CertObserved: func() {
			if err := alerts.SweepExpiry(context.Background()); err != nil {
				log.Error("cert expiry sweep after a changed observation failed", "err", err)
			}
		}}, ingest.Deps{Live: l, Store: st, Auth: a, Traffic: book, Tasks: reg, Clock: clk, Log: log})
	ctx := context.Background()
	// 先读离线变更代数、后做各缓存的首次加载：此后的任何库外提交都让代数大于 offlineGen，由重载循环补上；反过来，
	// 夹在加载与读代数之间的库外提交会被计入起点却不在缓存里，永远不被重载（协议见 nodeops.Reloader）。
	offlineGen, err := st.OfflineGeneration(ctx)
	if err != nil {
		return nil, err
	}
	log.Debug("offline generation read", "generation", offlineGen)
	if err := errors.Join(a.Load(ctx), svc.Load(ctx), reg.Load(ctx), updateManager.Load(ctx)); err != nil {
		return nil, err
	}
	if err := loadTrafficAlerts(ctx, book, alerts); err != nil {
		return nil, err
	}
	// 续投读取已加载的渠道快照；所有 Load 成功后才入队，后台 worker 尚未启动。
	if err := notifier.Requeue(ctx); err != nil {
		return nil, err
	}
	backups := backup.New(st, notifier, clk, log)
	// 流量报告与告警共用投递队列，读的是 loadTrafficAlerts 已装入的同一本账的已提交观测。
	reports := alert.NewReporter(alert.ReportDeps{Store: st, Traffic: book, Sender: notifier, Location: loc, Clock: clk, Log: log})
	hb := heartbeat.New(heartbeatSource{st: st, live: l}, client, version, clk, log)
	nodes := nodeops.New(nodeops.Deps{Credentials: a, Nodes: reg, Alerts: alerts, Traffic: book, State: svc, Log: log})
	reload := nodeops.NewReloader(nodes, nodeops.ReloadDeps{Generation: st, Tokens: a, Tasks: reg, Log: log}, offlineGen, opts.offlineReload)
	admin := api.New(api.Config{Updates: updateManager, Backups: backups, Heartbeat: hb, TTL: ttl, ReportInterval: svc.Interval(), TrustedProxies: opts.trusted, HubVersion: version, Location: loc, Retention: opts.retention, PublicDir: opts.publicDir != "", Geo: geoBackend},
		api.Deps{Store: st, Auth: a, Live: l, Nodes: nodes,
			Traffic: book, Probes: reg, Alerts: alerts, Notifier: notifier, Clock: clk, Log: log})
	pub := api.NewPublic(api.PublicConfig{ReportInterval: svc.Interval(), TrustedProxies: opts.trusted, Location: loc},
		api.PublicDeps{Store: st, Live: l, Traffic: book, Probes: reg, Clock: clk, Log: log})

	public := opts.publicPage
	themes := web.ThemeHandler(st, public, admin.ThemePreviewAccess, log)
	if opts.publicDir == "" {
		public = themes
	} else {
		public = newMux(mountOf("/_heron/", themes), mountOf("/", public))
	}
	handler := newHandler(routes{
		agent: mountOf(svc.Handler()), admin: mountOf(admin.Handler()), public: mountOf(pub.Handler()), page: public,
		publicEnabled: st.PublicEnabled,
	})
	return &hub{opts: opts, st: st, live: l, ingest: svc, updates: updateManager, relay: relay, book: book, alerts: alerts,
		notifier: notifier, reports: reports, geo: geo.New(st, geoBackend, clk, log), backups: backups, hb: hb, reload: reload, handler: handler, geoLog: geoLog,
		gate: func(ctx context.Context) error { return update.NewClient("hub").Gate(ctx, version) }}, nil
}

// run 监听、过更新门、起后台循环并服务，直到 stopCtx 取消或 Serve 出错，然后排空并关停。它是 hub 唯一的生命周期
// 所有者：从进入起，任何返回路径都由它停止已起的循环并关库。
func (h *hub) run(stopCtx context.Context, log *slog.Logger) (result error) {
	defer func() { result = errors.Join(result, h.st.Close()) }()
	listener, err := net.Listen("tcp", h.opts.listen)
	if err != nil {
		return err
	}
	if err := h.gate(stopCtx); err != nil {
		_ = listener.Close()
		return fmt.Errorf("update startup gate: %w", err)
	}
	l := h.live
	drain := &drainingHandler{next: h.handler, stopReceiving: func() { l.SetReceiving(false) }}
	srv := &http.Server{
		Addr: h.opts.listen, Handler: drain, ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 30 * time.Second, // ReadMaxBytes 限量不限时。
		// 写侧没有超时：响应写阻塞在内核发送缓冲上，ReadTimeout 与 IdleTimeout（零值沿用 ReadTimeout）都管不到，
		// 对端停止接收的连接会一直占着 goroutine 与描述符。上界由内核在连接层兜住（tcpstall），每条接受的连接都设。
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if err := tcpstall.Bound(c, stallTimeout); err != nil {
				log.Warn("stall timeout not set on a connection", "peer", c.RemoteAddr(), "err", err)
			}
			return ctx
		},
	}

	retention := h.opts.retention
	defer startLoop(h.ingest.RunFlusher)()
	defer startLoop(l.RunObservation)()
	defer startLoop(h.updates.Run)()
	defer startLoop(h.relay.Run)()
	defer startLoop(func(ctx context.Context) { h.st.RunMaintenance(ctx, retention) })()
	defer startLoop(h.book.Run)()
	stopSweep := startLoop(h.alerts.RunOfflineSweep)
	defer startLoop(h.alerts.RunProbeEvaluation)()
	defer startLoop(h.alerts.RunExpirySweep)()
	defer startLoop(h.alerts.RunBaselineRecompute)()
	defer startLoop(h.notifier.Run)()
	defer startLoop(h.reports.Run)()
	defer startLoop(h.geo.Run)()
	defer startLoop(h.backups.Run)()
	defer startLoop(h.hb.Run)()
	defer startLoop(h.reload.Run)()

	// 监听在 net.Listen 返回时已建立，连接先进内核队列。runServe 装配的文本 handler 在 Info 返回前
	// 同步写完 stderr，所以先写启动行再开始 Serve，拿到任何响应的调用方都已能在日志里读到它。
	// 离线告警在 ttl 与节点宽限期中较大者之后至多再等一个 offline_sweep 才触发（NextOffline 两者都要满足），
	// 恢复在首个被接受的上报之后至多等一个 offline_sweep；delivery_retry_wait 只给出固定重试间隔的总和，不是投递
	// 等待上界：Retry-After、排队、not_before 的整秒取整与存储退避还会增加等待（见 alert.DeliveryRetryWait）。
	// scripts/e2e.sh 用不限节奏且总回 200 的 Webhook 接收器，从这一行读出其场景的等待预算。
	log.Info("hub listening", append([]any{"listen", listener.Addr().String(), "ttl", h.opts.ttl, "interval", h.ingest.Interval(), "offline_sweep", alert.OfflineSweepEvery, "delivery_retry_wait", alert.DeliveryRetryWait(), "retention_1m", retention.M1, "retention_5m", retention.M5, "retention_1h", retention.H1, "retention_alert_events", retention.AlertEvents, "timezone", h.opts.loc.String(), "public_dir", h.opts.publicDir, "version", version, "agent_version", agentVersion}, h.geoLog...)...)
	errCh := make(chan error, 1)
	go func() {
		l.SetReceiving(true)
		err := srv.Serve(listener)
		l.SetReceiving(false)
		errCh <- err
	}()

	select {
	case <-stopCtx.Done():
		log.Info("shutting down")
	case err = <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
	}
	// 离线推断以上报准入开放为前提；drainingHandler 关闭准入后新上报一律 503，
	// unseen 的增长不再代表节点沉默。先停止巡检，避免把健康节点判为 firing 并投递，
	// 随后重启又因上报补发 recovered。
	stopSweep()
	// HTTP 先停止准入并排空；超时则断开连接以中止慢请求体，仍等待已经进入的处理器。
	// 最后由 defer 依次停止后台循环、关库，避免晚到的上报落在最后一次刷出之后。
	return errors.Join(err, shutdownHTTP(srv, drain, drainTimeout))
}

// stallTimeout 是对端连续不确认已发出数据的上界（tcpstall.Bound）。hub 没有流式响应，最大的响应是更新中转的二进制
// 与 QueryMetrics 的历史，正常对端对每个分段的确认在毫秒到秒级；60 秒容得下跨洲链路上一次重传退避，又把一条坏连接
// 能占住的时间从"内核放弃零窗口探测的十几分钟"压到一分钟。它只管写侧的停滞，不限制一次下载的总时长。
const stallTimeout = 60 * time.Second

// drainTimeout 是关停时排空在途请求的上限。到点后断开连接以中止慢请求体，仍等待已经进入的处理器。
// 空闲关停和请求完成后的退出从发信号或 cancel 起算，必须早于它；否则是在等这个超时，而不是在等请求结束。
const drainTimeout = 10 * time.Second

// startLoop 返回的 stop 取消并等待循环退出；调用方用 defer 的后进先出表达停止顺序。
// 离线巡检是唯一在 HTTP 排空前显式停止的循环，原因见调用处；其余均在排空后、st.Close 前停止。
func startLoop(run func(context.Context)) func() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		run(ctx)
	}()
	return func() {
		cancel()
		<-done
	}
}

// heartbeatSource 把心跳循环的两个读侧接到现有入口：设置读同一份快照，在线计数规则见 live.Live，告警计数读 store 的状态表。
// online 用 live 的判定（不在此重算宽限期），maintenance 与 firing 都经 store 的查询函数读，心跳包不写 SQL。
type heartbeatSource struct {
	st   *store.Store
	live *live.Live
}

func (h heartbeatSource) HeartbeatSettings(ctx context.Context) (store.HeartbeatSettings, error) {
	return h.st.HeartbeatSettings(ctx)
}

func (h heartbeatSource) HeartbeatCounts(ctx context.Context) (heartbeat.Counts, error) {
	nodes, err := h.st.ListMonitoringNodes(ctx)
	if err != nil {
		return heartbeat.Counts{}, err
	}
	states, err := h.st.ListAlertStates(ctx)
	if err != nil {
		return heartbeat.Counts{}, err
	}
	counts := heartbeat.Counts{NodesTotal: len(nodes)}
	rentedOffline := 0
	for _, n := range nodes {
		online := h.live.Online(n.ID)
		if online {
			counts.Online++
		}
		if n.Maintenance {
			counts.Maintenance++
		}
		if n.Rented {
			counts.Rented++
			if !online {
				rentedOffline++
			}
		}
	}
	// offline 由同一份节点全集与在线判定推出，但不含已出租（它不上报是预期而不是故障）：
	// online + offline + 未在线的已出租 = nodes_total 恒成立。
	counts.Offline = counts.NodesTotal - counts.Online - rentedOffline
	for _, s := range states {
		if s.State == store.StateFiring {
			counts.Firing++
		}
	}
	return counts, nil
}

// drainingHandler 将请求准入与关闭裁决串行化，Wait 前封住 Add，
// 即使 http.Server 关闭连接后不再跟踪处理器，也不会提前刷出或关库。
type drainingHandler struct {
	next          http.Handler
	mu            sync.Mutex
	stopping      bool
	active        sync.WaitGroup
	stopReceiving func()
}

func (d *drainingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	if d.stopping {
		d.mu.Unlock()
		http.Error(w, "server shutting down", http.StatusServiceUnavailable)
		return
	}
	d.active.Add(1)
	d.mu.Unlock()
	defer d.active.Done()
	d.next.ServeHTTP(w, r)
}

func shutdownHTTP(srv *http.Server, drain *drainingHandler, timeout time.Duration) error {
	drain.mu.Lock()
	drain.stopping = true
	if drain.stopReceiving != nil {
		drain.stopReceiving()
	}
	drain.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	err := srv.Shutdown(ctx)
	if err != nil {
		err = errors.Join(err, srv.Close())
	}
	drain.active.Wait()
	return err
}
