// Package store 是 hub 唯一的持久化层：单文件 SQLite，纯 Go 驱动。
//
// 不变式：所有写都经 runWriter 串行执行，w 只在那个协程里被使用。SQLite 同一
// 时刻只允许一个写者，应用内串行化从根上避免写者之间的 SQLITE_BUSY；读走
// 三个各自有上限的只读连接池（query_only）：请求驱动的大扫描走 hr，其余请求读走 r，
// hub 自己的告警评估读走 ev（见 readPoolFor），WAL 下读不阻塞写。
// 写请求返回错误意味着事务未应用，返回 nil 意味着已提交；这是 auth 只在
// 写成功后更新内存映射、保持映射与库一致的前提。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/xjetry/heron/internal/clock"
)

var (
	ErrClosed   = errors.New("store closed")
	ErrNotFound = errors.New("not found")
	ErrNoWindow = errors.New("register window closed")
	ErrBadKey   = errors.New("register key mismatch")
)

// Store 的所有写请求由 runWriter 串行提交；WriteMinuteBatch 在事务内核对节点、任务及上卷水位，
// 因此删除已提交的主体不会被迟到的分钟批次重新写入。Rollup 在写事务中推进水位，
// WriteMinuteBatch 用已提交的水位拒绝更早的行，查询不能把迟到行混入已冻结的历史。
type Store struct {
	path  string
	files fileStatter
	// backups 列举库旁的迁移前备份（StorageStats）；与 Open 做备份用的是同一组文件操作。
	backups backupFiles
	// siteWriteMu 让设置的提交与总闸发布对其它保存原子，维持的不变式写在 SaveSettings。
	siteWriteMu   sync.Mutex
	publicEnabled atomic.Bool
	closeMu       sync.RWMutex
	closed        bool
	w             *sql.DB
	// r 是轻读池，hr 是大扫描池，ev 是评估池；分工、上限与"不跨池等待"的约束见 readPoolFor。
	r      *sql.DB
	hr     *sql.DB
	ev     *sql.DB
	stats  storageStatsCache
	clk    clock.Clock
	log    *slog.Logger
	writes chan writeReq
	done   chan struct{}
	// themeGen 是主题版本与全站选择的代数；在线写者统一经 writeTheme 推进。
	themeGen     atomic.Uint64
	themeChanges chan struct{}
	// external 由 Open 的 ExternalWriter 选项给出，打开后不变：为真时 runWriter 在每个写事务提交前推进离线变更代数
	// （见 coordination.go）。
	external bool
}

type writeReq struct {
	ctx context.Context // 异步请求为 nil，不受取消影响。
	fn  func(*sql.Tx) error
	// runWriter 仅在请求因取消未执行或 inTx 返回最终结果后通知 res 或 done；
	// 已开始事务的提交或回滚先于通知，回调收到 nil 时读到的是已持久化的状态。
	res  chan error
	done func(error)
}

// 连接与检查点的实现笔记，只记结论，都不是已启用的策略：
//   - 驻留内存 = 连接数 × 每连接 page cache。连接数见 readPoolSize / historyPoolSize / evaluationPoolSize 加 statsDB 的一条，
//     每连接 cache 取驱动默认（cache_size -2000，约 2 MiB），没有按宿主内存分档；mmap 保持关闭（mmap_size 0），开着时
//     热页同时留在页缓存与映射里，RSS 随库文件增长而命中率不涨。两个默认值都是 modernc.org/sqlite v1.59.0 的实测。
//   - 不用 cache=shared：共享缓存是表级锁，busy_timeout 对它无效。写事务怎么开见 writeTxLock。
//   - 检查点若要做：写协程在事务之外按 -wal 文件大小阈值执行 wal_checkpoint(TRUNCATE)，平时短等待、超阈值才给长等待；
//     触发条件用文件大小，不用连接数——挡住重置的是持着快照的读事务（持得久的只有大扫描池里的长查询），空闲连接不
//     持事务。是否启用由实测决定，当前不做：modernc.org/sqlite v1.59.0（SQLite 3.53.4），Apple M4 Max、macOS 26.3.1，
//     100 个节点、3 天历史、每秒一批分钟行、三种条件各 15.5 分钟。只有写入时自动检查点反复重置 -wal；持续短查询读者或
//     持 60 秒只读事务的长读者挡住重置时 -wal 线性增长。生产 hub v0.6.1 在节点在线、每分钟写分钟行时，约十分钟内 -wal
//     停在 4,408,432 字节；生产 hub v0.12.0、158 个节点的 -wal 为 4,948,152 字节，逻辑库 118,439,936 字节，低于 16 MiB
//     上限，与 v0.6.1 同量级。journal_size_limit（下面的 walSizeLimit）只管重置之后留多大。

// dsn 不含 journal_mode(WAL)：这个 pragma 一旦生效就立即改写文件头（第 18—19 字节
// 标出日志模式），比 migrate 读版本号、判定"这是不是本项目的库"更早。所有连接都带它
// 会让身份判定本身成为一次写：外来库被拒绝时文件也已经被改成 WAL，判定分支之后没有
// 机会撤销。WAL 只在 migrate 判定通过（库已在当前版本、刚建成、或刚迁移完成）之后由它显式打开，
// 见 enableWAL；一旦打开，日志模式记在文件里，之后的连接（包括这里的写连接与读连接池）
// 不需要也不应重复声明它。
// walSizeLimit 是检查点重置 WAL 时保留的文件上限（journal_size_limit）。SQLite 默认不限：WAL 长到多大，检查点之后
// 文件就留多大，一次迁移或补数把它撑大后永远不回落。正常运行时自动检查点在 WAL 达到 1000 页时触发（页 4 KiB，约
// 4 MiB，即平时的高水位）；上限取它的 4 倍，平时的写入碰不到上限，不会反复截短又长回，突发写入之后截回上限以内。
// 截断发生在写连接下一次重启 WAL 时，所有连接都带这个 pragma，不必区分哪条连接会做重启。它只管重置之后留多大：
// 持续有读者挡住重置时 WAL 照样增长（modernc.org/sqlite v1.59.0 / SQLite 3.53.4，Apple M4 Max、macOS 26.3.1，
// 100 个节点、3 天历史、每秒一批分钟行并有持续读者，各 15.5 分钟：-wal 线性增长且不重置），那是检查点策略的事，不由这个上限兜住。
const walSizeLimit = 16 << 20

// busyTimeout 是每条连接等别的连接放锁的上限（busy_timeout），也是 enableWAL 自己重试的总时长上限。
const busyTimeout = 5 * time.Second

// writeTxLock 让连接上的显式事务以 BEGIN IMMEDIATE 开始，写锁在 begin 时就拿到。同一个库可能同时有不止一个进程在写：
// 运行中的 hub、它旁边的离线子命令（ExternalWriter）、恢复、另一个进程的建库或迁移。默认的 BEGIN DEFERRED 先只开
// 读事务，到第一次写才升级；升级前另一个写者已提交时以 SQLITE_BUSY_SNAPSHOT 失败，正持着锁时以 SQLITE_BUSY 失败，
// 两种都不经 busy_timeout 等待：已在读事务中的连接，SQLite 不为它调 busy handler（modernc.org/sqlite v1.59.0 实测，
// 前者见 TestWriterThatReadFirstIsNotRefusedByConcurrentWriter，后者见 TestRestoreWaitsForConcurrentTargetWriter）。
// begin 时取锁，等待就落在 begin 上，由 busy_timeout 兜住，事务读到的也一定是取锁之后的最新状态。所以凡在可能有
// 别的进程写的库上开写事务的连接都带它：写连接 w、migrate 的 schemaDB、Restore 的目标库连接。
const writeTxLock = "&_txlock=immediate"

func dsn(path string, extra string) string {
	// path 是文件名而非 URI；编码路径部分，避免 #、? 和 % 改变实际打开的数据库。
	u := url.URL{Path: path}
	return "file:" + u.EscapedPath() + "?_pragma=busy_timeout(" + strconv.FormatInt(busyTimeout.Milliseconds(), 10) + ")&_pragma=synchronous(NORMAL)&_pragma=journal_size_limit(" + strconv.Itoa(walSizeLimit) + ")" + extra
}

// SchemaPolicy 由打开库的入口显式选择。离线命令不得迁移库：否则用新二进制查看 stats
// 就会单向升级数据库，而 migrate 的版本检查会让仍按更旧 schema 运行的进程在下次启动时拒绝打开它。
// 零值非法，避免调用方遗漏选择时意外迁移。
type SchemaPolicy int

const (
	MigrateSchema SchemaPolicy = iota + 1
	RequireCurrentSchema
)

// OpenOption 调整 Open 打开的 Store 在库的写者中的角色。不给任何选项即运行中的 hub 自己。
type OpenOption func(*openConfig)

type openConfig struct {
	external bool
	// backups 是迁移前备份与备份列举用的文件操作；不给 withBackupFiles 时是真实文件系统（osBackupFiles）。
	backups backupFiles
}

// ExternalWriter 声明打开库的是 hub 进程之外的写者（离线子命令）：这样的 Store 在每个提交的写事务里推进离线
// 变更代数，运行中的 hub 据此发现库外写入并重载缓存。离线入口漏给这个选项，它的写入不会被运行中的 hub 看到。
func ExternalWriter() OpenOption { return func(c *openConfig) { c.external = true } }

// Open 按 policy 检查或迁移 schema。SQLite 打开失败的报错不带文件名（如 unable to open database file (14)）；
// serve 与离线子命令都经这里打开库，打开过程的每一种失败都在这一层补上路径，报错才指得出是哪个文件、
// 该查哪个目录的权限。
func Open(path string, clk clock.Clock, log *slog.Logger, policy SchemaPolicy, opts ...OpenOption) (*Store, error) {
	if policy != MigrateSchema && policy != RequireCurrentSchema {
		panic("store.Open requires a valid SchemaPolicy")
	}
	cfg := openConfig{backups: osBackupFiles{}}
	for _, o := range opts {
		o(&cfg)
	}
	s, err := openStore(path, clk, log, policy, cfg)
	if err != nil {
		return nil, fmt.Errorf("open database %s: %w", path, err)
	}
	return s, nil
}

func openStore(path string, clk clock.Clock, log *slog.Logger, policy SchemaPolicy, cfg openConfig) (*Store, error) {
	if err := migrate(path, policy, log, cfg.backups); err != nil {
		return nil, err
	}
	w, err := sql.Open("sqlite", dsn(path, writeTxLock))
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	r, err := sql.Open("sqlite", dsn(path, "&_pragma=query_only(1)"))
	if err != nil {
		w.Close()
		return nil, err
	}
	configureReadPool(r, readPoolSize())
	hr, err := sql.Open("sqlite", dsn(path, "&_pragma=query_only(1)"))
	if err != nil {
		r.Close()
		w.Close()
		return nil, err
	}
	configureReadPool(hr, historyPoolSize())
	ev, err := sql.Open("sqlite", dsn(path, "&_pragma=query_only(1)"))
	if err != nil {
		hr.Close()
		r.Close()
		w.Close()
		return nil, err
	}
	configureReadPool(ev, evaluationPoolSize())
	// 打开时读一次设置，同时满足两件事：总闸的内存副本从库加载（不变式见 SaveSettings）；设置里有必须合法才能解释的
	// 编码（两个开关只认 0 / 1，备份的数值有范围，渠道列表是 JSON 数组，见 readSettings），库里有非法值就拒绝打开。
	settings, err := readSettings(context.Background(), r)
	if err != nil {
		ev.Close()
		hr.Close()
		r.Close()
		w.Close()
		return nil, err
	}
	statsDB, err := sql.Open("sqlite", dsn(path, "&_pragma=query_only(1)"))
	if err != nil {
		ev.Close()
		hr.Close()
		r.Close()
		w.Close()
		return nil, err
	}
	statsDB.SetMaxOpenConns(1)
	statsCtx, cancelStats := context.WithCancel(context.Background())
	s := &Store{path: path, files: osFileStatter{}, backups: cfg.backups, w: w, r: r, hr: hr, ev: ev, clk: clk, log: log, writes: make(chan writeReq, 1024), done: make(chan struct{}), themeChanges: make(chan struct{}, 1),
		stats: storageStatsCache{db: statsDB, ctx: statsCtx, cancel: cancelStats}, external: cfg.external}
	s.publicEnabled.Store(settings.Site.PublicEnabled)
	go s.runWriter()
	return s, nil
}

// 读连接分成三个池（r、hr、ev），各自有上限，空闲保留等于上限。
//
// 为什么要有上限：WAL 下读者不阻塞写者，但每个连接是一个文件句柄加一份页缓存。database/sql 默认不限连接数，
// 开环过载时连接数随在飞读请求增长（比较查询负载台记录到约 310 个），内存与 fd 随之无界。
//
// 为什么分池：database/sql 的等待者随机出队（putConnDBLocked 的 TakeRandom），单池满时轻读与大扫描抽同一个签，
// 轻读要等一整条扫描结束。同时在飞的大扫描没有上界——api 的 historyGate 只限每来源，来源数不限——单池上限多大都能
// 被它占满。同机对照（GOMAXPROCS=16，32 个大扫描 + 16 个轻读者闭环，延迟只作同机比较）：单池上限 16 时轻读 p50
// 104ms，拆池后 4.8ms、轻池零排队；只有大扫描时，大扫描池的吞吐与单池不限相近（1118 vs 1182 次）。
//
// 哪个读走哪个池，按两条原则：
//   - 请求驱动的读按查询自身的扫描量分 r / hr，不按入口或调用方：queryFamily 按 scanEstimate 选首次池，预计扫描量
//     超过 lightScanRows 的走 hr，其余以及所有不经 queryFamily 的读走 r。同一个 QueryProbes，面板的一小时窗口留在 r，
//     公开页的长窗口进 hr。估计只定首次池，r 上的实测上界由封顶计数保证：queryFamily 在 r 上计数累计超过
//     lightScanRows 即放弃 r 上的事务、改在 hr 重跑（见 queryFamily）。新加的查询若读取与时间窗口成正比的行数，
//     要经 queryFamily，否则它就是轻池里没有上界的大扫描。
//   - hub 自己的告警评估读按调用方进 ev，只经 Evaluation 返回的读者：评估的时效是告警承诺的一部分，不该随请求负载
//     变化。r 与 hr 的排队长度都由外部请求决定，评估读排在哪个池里，告警判定就随那个池的负载变慢。
//
// 同一调用链不得持一个读连接再等另一个读连接，不论同池还是跨池：跨池时两个池都满，持甲池连接等乙池的与持乙池连接
// 等甲池的互相等到 ctx 取消；同池时上限个这样的调用就把池占死。现有路径满足它：历史请求的准入读（NodeExists、
// NodeIsPublic、ListComparisonNodes）在 r 上读完、归还之后才进 queryFamily；告警引擎的节点与任务读
// （ListMonitoringNodes、ProbeTaskNodeIDs）在 r 上读完、归还之后才经评估读者读历史；queryFamily 的 scan 与 summarize
// 回调只用它自己的事务。三个池都设为 1 个连接跑 store、api、cmd/hub 的全部测试可以复核这一点：违反它的路径会挂住。
//
// readPoolFor 给出请求驱动、预计扫描量为 estimate 的查询首次该用的池，以及在那个池上的计数封顶（0 为不封顶），
// 分界见 lightScanRows。
func (s *Store) readPoolFor(estimate int64) (*sql.DB, int64) {
	if estimate > lightScanRows {
		return s.hr, 0
	}
	return s.r, lightScanRows
}

// ReadPoolStats 是三个读池各自的连接池统计，取值时刻各池分别读取、彼此不是同一瞬间。每个字段的含义由
// database/sql 的 DBStats 保证：MaxOpenConnections 是池的上限，OpenConnections / InUse / Idle 是当下的连接数，
// WaitCount / WaitDuration 是自打开以来因池满排队的次数与累计时长，MaxIdleClosed / MaxIdleTimeClosed 是因空闲保留
// 上限与空闲时长被关掉的连接数。它是读池的可观测面：负载对照与存储统计据此看各池是否排队、连接是否被关掉重开。
type ReadPoolStats struct {
	Light, History, Evaluation sql.DBStats
}

// ReadPoolStats 返回 r、hr、ev 三个读池的统计，分工见 readPoolFor。
func (s *Store) ReadPoolStats() ReadPoolStats {
	return ReadPoolStats{Light: s.r.Stats(), History: s.hr.Stats(), Evaluation: s.ev.Stats()}
}

// readPoolSize 是轻池上限：2×GOMAXPROCS，至少 4。轻池里最重的是 lightScanRows 以内的扫描（同机一次约 16ms），
// 上限越大，面板轮询读排在它们后面的机会越小：单池混合负载下上限 2P 的轻读 p50 是上限 P 的约三分之一
// （31.9 vs 104ms）。至少 4：单核机器上 2P 只有 2 个，两条中等扫描就能占满；同机 1 核、两个一天分钟行扫描者与轻读者
// 闭环的单池对照里，上限 4 时轻读不排队（p50 0.19ms），上限 2 时 14.4ms。
func readPoolSize() int { return max(4, 2*runtime.GOMAXPROCS(0)) }

// historyPoolSize 是大扫描池上限：GOMAXPROCS，至少 2。大扫描是 CPU 密集的，并发超过可用 CPU 数不增加吞吐
// （只有大扫描的对照：16 核上限 P 1118 次、不限 1182 次；2 核 364 vs 358；1 核 177 vs 180，三次之间的波动比差值大）。
// 至少 2：单核上一条 365 天的长扫描不该让其它来源的大扫描全部串行等它，1 核上 2 与 1 的吞吐相同（177 vs 179）。
// 与 api 的 historyGate 的关系：historyGate 让一个来源至多 GOMAXPROCS/4（至少 1）个历史请求在飞，所以一个来源至多
// 占这个池的四分之一（2 核以下至多一半），挤不掉别的来源；来源数不限时，大扫描占用的连接仍以这个上限为界，
// 超出的在池里排队，不另开连接。
func historyPoolSize() int { return max(2, runtime.GOMAXPROCS(0)) }

// evaluationPoolSize 是评估池上限：2，不随核数。Evaluation 的调用方只有告警引擎（cmd/hub/serve.go 装配），经它的读
// 来自两个调用方，各自至多一条在飞：
//   - 一条评估：alert.Engine 的 EvaluateResources 与 EvaluateProbes 全程持 Engine.writeMu，同一时刻至多一个在读历史。
//   - 一条基线重算：alert.Engine.RunBaselineRecompute 是单个协程，逐对 (规则, 节点) 顺序调 ProbeBucketMeans，
//     不持 writeMu（只读历史，写经写协程），所以与评估并行，但自己同一时刻至多一条。
//
// 两者各占一个连接，互不排队：重算一次读至多 8640 个桶，若与评估共用一个连接，评估的时效就随重算的读量变化。
// 上限随核数增长不会让评估更快，只多留空闲连接与页缓存。Evaluation 有了第三个调用方、评估入口不再持 writeMu、
// 或重算不再是单协程，这个上限要重新论证。
func evaluationPoolSize() int { return 2 }

// readConnMaxIdle 是池里连接空闲多久才回收，三个池同一个值。面板每 5 秒、流量每 10 秒轮询（web/src/lib/poll.ts），
// database/sql 优先复用最近归还的连接，持续有人看面板时轮询用到的那几个连接空闲时长到不了它；只有突发时多开、之后
// 不再用到的连接才在 5 分钟后关掉，把页缓存还回去。轮询之间不关连接靠的是空闲保留等于上限（见 configureReadPool），
// 这个时长决定的只是突发过后多久缩回去。
const readConnMaxIdle = 5 * time.Minute

// lightScanRows 是轻读与大扫描的分界，单位是 scanEstimate 的源行数。分界只由请求驱动的读的几档窗口决定（告警评估读
// 不在 r / hr 上，与它无关）：面板与公开页的几档历史（web/src/components/History.tsx 的 RANGES 经 ChooseLevel 选级）
// 单序列 1h、6h、24h、7d、30d 分别是 60、360、288、2016、720 行，指标族全部在分界之下；探测族按节点的任务数、对比按
// 节点数计序列，一小时窗口满配 64 个任务（probelimit.MaxTasksPerNode）是 60×64=3840 行。
//
// 取 3840 之上的 4096，任务满配的一小时探测图也留在轻池。同机单次扫描（单序列指标、1m 级、无并发）4096 行 p50 16ms、p99 19ms，
// 轻池里的读至多排在这个量级的扫描后面；12000 行（单序列读量额度）约 40ms，多序列、天级以上的扫描到数百毫秒，
// 这些才值得隔离。
const lightScanRows = 4096

// scanEstimate 是 queryFamily 一次查询预计读取的源行数：请求级每桶一行、series 条序列（queryShape.estimatedSeries：
// 调用方给出的实际序列数，不知道时按序列上限）。按桶长而不按 step 算，因为 step 是桶长的整数倍、源行按桶长存，
// 输出点数会低估读量。估计只定首次池：偏大只多排队；偏小（任务更替留下的历史序列，或不计入的细级尾巴在维护停滞时
// 变长）由 queryFamily 在 r 上的封顶计数兜住，r 上完成的扫描不超过 lightScanRows。读量本身由额度（ReadQuotaError）
// 按实际行数裁决，与估计无关。
func scanEstimate(from, to int64, lv Level, series int64) int64 {
	return ceilDiv(to-from, lv.Bucket) * series
}

// configureReadPool 让空闲保留等于上限：连接的打开成本是重跑 DSN 里的 pragma、页缓存从冷开始，归还时关掉、下次再开
// 只省内存不省时间。同机 burst 对照（每 100ms 起 48 个轻读，GOMAXPROCS=16）：database/sql 默认的 2 个空闲时 5 秒内
// 关掉重开 2524 次、轻读 p50 55ms；空闲等于上限时 0 次、8.7ms；空闲取上限一半时 400 次、16ms。
func configureReadPool(db *sql.DB, size int) {
	db.SetMaxOpenConns(size)
	db.SetMaxIdleConns(size)
	db.SetConnMaxIdleTime(readConnMaxIdle)
}

// Close 阻止新统计入场，取消并等待在飞计算退出；队列里的写全部执行完后才关连接，最后一批刷出不丢。
func (s *Store) Close() error {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.stats.cancel()
	s.stats.mu.Lock()
	flight := s.stats.flight
	s.stats.mu.Unlock()
	if flight != nil {
		<-flight.done
	}
	close(s.writes)
	<-s.done
	return errors.Join(s.stats.db.Close(), s.ev.Close(), s.hr.Close(), s.r.Close(), s.w.Close())
}

func (s *Store) runWriter() {
	defer close(s.done)
	for req := range s.writes {
		var err error
		if req.ctx != nil && req.ctx.Err() != nil {
			err = req.ctx.Err()
		} else {
			fn := req.fn
			if s.external {
				fn = advancingOfflineGeneration(fn)
			}
			err = inTx(s.w, fn)
		}
		if req.res != nil {
			req.res <- err
		} else if req.done != nil {
			req.done(err)
		} else if err != nil {
			s.log.Error("async write failed", "err", err)
		}
	}
}

// inTx 在 db 上开一个事务执行 fn：fn 出错则回滚并返回该错误，否则返回提交的结果。runWriter（写连接）与 migrate
// 共用它；返回 nil 即已提交，是 write 契约的来源。
func inTx(db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// write 返回错误意味着事务未应用，返回 nil 意味着已提交；入队后必须等到
// runWriter 给出最终结果。中途放弃等待会让调用方在事务照常提交时误以为失败，
// 据此不更新内存映射就会造成映射与库分叉。取消只阻止尚未开始的事务。
// write 不看 ctx 里的类型化变更：它永不消费、永不审计，类型化变更的主写只经 writeChange（change.go）。
func (s *Store) write(ctx context.Context, fn func(*sql.Tx) error) error {
	s.closeMu.RLock()
	if s.closed {
		s.closeMu.RUnlock()
		return ErrClosed
	}
	// runWriter 在通道关闭前持续消费，满队列下的投递仍能完成并释放读锁；
	// Close 获得写锁后才关闭通道，因此不会因等待此读锁而阻止队列消费。
	req := writeReq{ctx: ctx, fn: fn, res: make(chan error, 1)}
	select {
	case s.writes <- req:
	case <-ctx.Done():
		s.closeMu.RUnlock()
		return ctx.Err()
	}
	s.closeMu.RUnlock()
	return <-req.res
}

// writeAsync 投递后立即返回；done 在写协程里被调用。队列满时丢弃并报告，
// 调用方据此保持自己的状态不变，让下一次上报重新触发。
func (s *Store) writeAsync(fn func(*sql.Tx) error, done func(error)) {
	s.closeMu.RLock()
	if s.closed {
		s.closeMu.RUnlock()
		if done != nil {
			done(ErrClosed)
		}
		return
	}
	req := writeReq{fn: fn, done: done}
	select {
	case s.writes <- req:
		s.closeMu.RUnlock()
	default:
		s.closeMu.RUnlock()
		s.log.Warn("write queue full, dropping async write")
		if done != nil {
			done(errors.New("write queue full"))
		}
	}
}

const schemaVersion = 47

type schemaAction int

const (
	schemaCurrent schemaAction = iota
	schemaCreate
	schemaMigrate
)

type schemaReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// schemaAdmission 只读取版本与对象数；Open 与 Restore 在任何建表或迁移之前共用这一判定。
func schemaAdmission(ctx context.Context, db schemaReader, policy SchemaPolicy) (schemaAction, int, error) {
	var v int
	if err := db.QueryRowContext(ctx, "PRAGMA main.user_version").Scan(&v); err != nil {
		return 0, v, err
	}
	switch {
	case v == schemaVersion:
		return schemaCurrent, v, nil
	case v > schemaVersion:
		return 0, v, fmt.Errorf("database schema version %d is newer than this binary (%d)", v, schemaVersion)
	case v < 0:
		// createSchema 写 schemaVersion，migrate 从已准入的正版本逐步写 next，两处都不产生负数。
		// 把负数当旧库交给迁移循环会查 migrations[v+1]（v=-1 时找不到 migrations[0]），
		// 报出与升级无关的内部错误；当空库建表则会把 schemaStatements 叠加到未知内容上。
		// 因而负版本既不能迁移，也不能按空库放行。
		return 0, v, fmt.Errorf("database schema version %d is invalid; not a Heron database", v)
	case v == 0:
		// PRAGMA user_version 未显式设置时读出的也是 0，任何 SQLite 文件都满足这一条；
		// 只有 sqlite_schema 里确实不存在任何对象才是空库。有对象却没有版本号
		// 说明这是别的程序建的库，在它上面叠加 schemaStatements 会把两套 schema 的对象混进
		// 同一个文件——stats --db 指错文件时就会把陌生库当空库建满全部表。
		var objects int
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM main.sqlite_schema").Scan(&objects); err != nil {
			return 0, v, err
		}
		if objects > 0 {
			return 0, v, errors.New("database has tables but no schema version; not a Heron database")
		}
		return schemaCreate, v, nil
	case policy == RequireCurrentSchema:
		return 0, v, fmt.Errorf("database schema version %d is older than this binary (%d); start the new heron-hub serve once to upgrade it (back up the database first)", v, schemaVersion)
	}
	return schemaMigrate, v, nil
}

// createSchema 使用调用方的事务，使恢复时建库与数据覆盖一并提交或回滚。
func createSchema(ctx context.Context, tx *sql.Tx) error {
	for _, stmt := range schemaStatements() {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("%w in %q", err, stmt)
		}
	}
	_, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA main.user_version = %d", schemaVersion))
	return err
}

func logSchemaCreated(log *slog.Logger) {
	log.Info("database schema created", "version", schemaVersion)
}

// migrate 用 user_version 保存当前版本，不在库里保存迁移历史或时间；运维从日志判断何时迁过、该还原哪份备份。
//
// 不变式：迁移要么整体生效、要么不改库。全部步骤在同一个事务里执行，提交之后才记一行 "database schema migrated"
// （from 为迁移前版本，to 为当前版本）；任何一步失败整体回滚，库仍停在迁移前的版本，旧二进制可以直接打开它。
// 迁移前备份依赖这一点：hub 被 Restart=always 反复拉起、迁移每次都失败时，每次备份的都是同一个原状，
// backupBeforeMigration 在迁移之前按 seq 只留两份，既不累积也不会把升级前的状态清掉。
// 代价是迁移期间日志文件（WAL 或回滚日志）要装下所有步骤改动的页，跨多个版本的大库在迁移时一次性占用更多磁盘。
//
// 不变式：据以建库或迁移的那次准入、迁移前备份、建库或迁移本身，都在同一段写锁之内，锁从 BEGIN 起持有。同一个库
// 可能同时被两个进程首次打开或迁移（serve 与 passwd、两个 serve、serve 与在线更新器起的候选 hub），版本号只在持写锁
// 时改变，所以持锁读到的版本就是备份与动手时的版本。保证它的是 schemaDB 的 writeTxLock：inTx 以 BEGIN
// IMMEDIATE 开始，begin 时取 RESERVED 锁，后到的进程在 begin 处按 busy_timeout 等先到的提交，拿到锁后读到当前版本，
// 既不备份也不迁移。两种别的写法都会让并发打开失败：默认的 DEFERRED 事务读版本时只持 SHARED 锁，两个进程都读到
// 旧版本，后写的那个从读锁升级写锁时不等 busy_timeout、立即得到 SQLITE_BUSY（modernc.org/sqlite v1.59.0 实测）；
// 准入若放在锁外，后到者等到锁之后不再看版本，在第一条 DDL 处撞上对方已建好的对象（table already exists、
// duplicate column），报错与并发无关。
//
// 备份放在锁内，是因为 backupBeforeMigration 的取 seq（扫描取最大值加一）与清理遗留临时文件都假定同一时刻只有它
// 一个在写这个库的备份：两个进程同时备份会取到同一个 seq、在发布时 EEXIST，或把对方正在写的临时文件当遗留删掉。
// VACUUM INTO 不能在持锁的这条连接上做（事务里执行报 cannot VACUUM from within a transaction），由 backupDB 这第二条
// 连接做：持锁事务此时只读过版本、还没写任何页，第二条连接读到的就是锁内看到的那个状态；RESERVED 锁不挡读者，
// 回滚日志与 WAL 两种模式下这样备份都能完成（同版本驱动实测）。VACUUM INTO 返回后第二条连接不再持读锁，提交取
// 排他锁时不会被它挡住；它用完即关。
//
// 锁外的那次准入只决定库已在当前版本时直接返回、不取写锁：运行中的 hub 旁边执行离线命令是常态，不该去等它的写。
func migrate(path string, policy SchemaPolicy, log *slog.Logger, backups backupFiles) error {
	schemaDB, err := sql.Open("sqlite", dsn(path, writeTxLock))
	if err != nil {
		return err
	}
	defer schemaDB.Close()
	// 持锁事务独占这一条连接；备份用的读连接另开（见上）。
	schemaDB.SetMaxOpenConns(1)
	if err := migrateLocked(schemaDB, path, policy, log, backups); err != nil {
		return nameConcurrentSchemaWriter(err)
	}
	// 准入接受了这个库（已在当前版本、刚建成、或刚迁移完成）才走到这里；拒绝分支在此之前已经返回，外来库、
	// 负版本、比二进制新的库不会被切成 WAL（dsn 的注释）。三条路径都要切：并发打开时，等锁后读到当前版本的那一方
	// 可能早于建库的一方执行到这里。
	return nameConcurrentSchemaWriter(enableWAL(schemaDB))
}

// enableWAL 把库的日志模式切到 WAL。改 journal_mode 时驱动不调用 busy handler：另一条连接持着 RESERVED 锁时它立刻
// 返回 SQLITE_BUSY，不等 busy_timeout（modernc.org/sqlite v1.59.0 实测；库已是 WAL 时它不取锁，立即成功）。新库刚建好、
// 还是回滚日志模式时，并发打开的另一方正好在 begin 处拿到写锁重新准入，就会撞上这一步，所以这里自己按 busyTimeout
// 重试；对方只是短暂持锁，等它提交即可。
func enableWAL(db *sql.DB) error {
	deadline := time.Now().Add(busyTimeout)
	for {
		_, err := db.Exec("PRAGMA journal_mode=WAL")
		var se *sqlite.Error
		if err == nil || !errors.As(err, &se) || se.Code()&0xff != sqlite3.SQLITE_BUSY || time.Now().After(deadline) {
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}
}

const (
	schemaCreateLockMsg  = "database is empty; taking the write lock to create the schema"
	schemaUpgradeLockMsg = "database schema is behind this binary; taking the write lock to upgrade"
)

// logSchemaLockTaking 在锁外准入决定建库或迁移之后、取写锁之前记一行。取锁可能等满 busy_timeout，迁移前备份
// （VACUUM INTO）在大库上要跑一阵，这一行让日志在这段静默开始前就写明进程打算做什么；拿到锁后若发现别人已经
// 做完，接下来记的是 already current，两行合起来就是并发打开的全过程。
//
// 不变式：这一行只在锁外准入之后、begin 之前记。它是"本进程已读到空库或旧版本、接下来去等锁"的唯一外部可见
// 信号，并发用例（schema_concurrent_test.go）等到它才让先到者提交；把它挪到锁内或 begin 之后，用例就等不到
// 后到者在锁内重新准入的那条路径。
func logSchemaLockTaking(log *slog.Logger, action schemaAction, from int) {
	if action == schemaCreate {
		log.Info(schemaCreateLockMsg, "version", schemaVersion)
		return
	}
	log.Info(schemaUpgradeLockMsg, "from", from, "to", schemaVersion)
}

func migrateLocked(db *sql.DB, path string, policy SchemaPolicy, log *slog.Logger, backups backupFiles) error {
	ctx := context.Background()
	action, seen, err := schemaAdmission(ctx, db, policy)
	if err != nil {
		return err
	}
	if action == schemaCurrent {
		return nil
	}
	logSchemaLockTaking(log, action, seen)
	var from int
	if err := inTx(db, func(tx *sql.Tx) error {
		var err error
		if action, from, err = schemaAdmission(ctx, tx, policy); err != nil {
			return err
		}
		switch action {
		case schemaCurrent:
			return nil
		case schemaCreate:
			return createSchema(ctx, tx)
		}
		if err := backupUnderLock(ctx, path, from, backups, log); err != nil {
			return fmt.Errorf("back up database before migrating schema %d: %w", from, err)
		}
		for next := from + 1; next <= schemaVersion; next++ {
			step, ok := migrations[next]
			if !ok {
				return fmt.Errorf("no migration to schema version %d", next)
			}
			if err := step(tx); err != nil {
				return fmt.Errorf("migrate to schema version %d: %w", next, err)
			}
		}
		_, err = tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion))
		return err
	}); err != nil {
		return err
	}
	switch action {
	case schemaCreate:
		logSchemaCreated(log)
	case schemaMigrate:
		log.Info("database schema migrated", "from", from, "to", schemaVersion)
	default:
		log.Info("database schema already current: another process created or migrated it while this one waited for the write lock", "version", schemaVersion)
	}
	return nil
}

// backupUnderLock 在 migrate 持着写锁、尚未写入时，经第二条连接做迁移前备份，返回前关掉这条连接。
func backupUnderLock(ctx context.Context, path string, version int, backups backupFiles, log *slog.Logger) (err error) {
	backupDB, err := sql.Open("sqlite", dsn(path, ""))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, backupDB.Close()) }()
	backupDB.SetMaxOpenConns(1)
	return backupBeforeMigration(ctx, backupDB, path, version, backups, log)
}

// nameConcurrentSchemaWriter 给建库与迁移途中的 SQLITE_BUSY 点名原因，驱动的原文 database is locked 不说是谁。
// 已在当前版本、已是 WAL 的库 migrate 不取锁，走到这里的库为空、版本落后、或刚建好还没切到 WAL：运行中的 hub 不会
// 持有这样的库，持锁的通常是另一个正在建库或迁移它的进程（手工用 sqlite3 打开着它也会这样）。等满 busyTimeout 才会走到这里：begin 处等写锁、回滚
// 日志模式下对方提交时持排他锁挡住了锁外的准入读，或 enableWAL 重试到期。
func nameConcurrentSchemaWriter(err error) error {
	var se *sqlite.Error
	if errors.As(err, &se) && se.Code()&0xff == sqlite3.SQLITE_BUSY {
		return fmt.Errorf("another process is creating or migrating the same database and held its lock past the busy timeout; retry after it finishes: %w", err)
	}
	return err
}
