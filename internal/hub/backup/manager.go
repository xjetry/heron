// Package backup 负责分层快照、上传、按份保留及配置层故障通知。
package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/s3"
	"github.com/xjetry/heron/internal/hub/store"
)

type Sender interface{ Enqueue(store.AlertEvent) }

type objectStore interface {
	PutObject(context.Context, string, io.ReadSeeker) error
	ListObjectsV2(context.Context, string, int) ([]s3.Object, error)
	DeleteObject(context.Context, string) error
}

type LayerStatus struct {
	LastSuccess time.Time
	Failure     string
	Since       time.Time
	StatusCode  int
}

type Status struct {
	Enabled              bool
	Config, Metrics      LayerStatus
	ThemesWithoutPackage []string
}

type layerState struct {
	LayerStatus
	attempted   bool
	lastAttempt time.Duration
	// restored 与 notified 只属于配置层：restored 表示库里的未恢复标记已读回，notified 表示本段故障已发过触发事件。
	restored bool
	notified bool
	logged   bool
	lastLog  time.Duration
}

// Manager 将配置层和指标层的快照分别上传；各层的成功时间只在其完整上传后更新，失败不伪装成成功。
// 两层各自持有运行锁并从上次成功时间计算周期，慢上传不会累积并发轮次，也不会让一层故障阻塞另一层。
type Manager struct {
	st          *store.Store
	sender      Sender
	clk         clock.Clock
	log         *slog.Logger
	newClient   func(s3.Config) (objectStore, error)
	runMu       [2]sync.Mutex
	initMu      sync.Mutex
	initialized bool
	mu          sync.RWMutex
	layers      [2]layerState
}

func New(st *store.Store, sender Sender, clk clock.Clock, log *slog.Logger) *Manager {
	return &Manager{st: st, sender: sender, clk: clk, log: log,
		newClient: func(cfg s3.Config) (objectStore, error) { return s3.New(cfg, clk) }}
}

func (m *Manager) Status(ctx context.Context) (Status, error) {
	cfg, err := m.st.BackupSettings(ctx)
	// 设置或未恢复标记读不出时，配置层会把它记为当前故障（prepare 给出类别，finish 记下）；已观察到就照常展示，
	// 尚未观察到才返回读错误，不把读不出伪装成正常。状态读取本身不伪造首次失败时刻。
	settingsErr := err
	times, err := m.st.BackupSuccessTimes(ctx)
	if err != nil {
		return Status{}, err
	}
	m.mu.RLock()
	out := Status{Enabled: cfg.Target.Enabled(), Config: m.layers[0].LayerStatus, Metrics: m.layers[1].LayerStatus}
	m.mu.RUnlock()
	if out.Config.LastSuccess.IsZero() {
		out.Config.LastSuccess = times[store.MaintenanceBackupConfig]
	}
	if out.Metrics.LastSuccess.IsZero() {
		out.Metrics.LastSuccess = times[store.MaintenanceBackupMetrics]
	}
	if out.Config.Failure == "" {
		// 配置层读回标记之前（重启后首轮之前）以库里的标记为准，未恢复的故障重启后立即可见。
		since, err := m.st.BackupFailingSince(ctx)
		if err != nil {
			return Status{}, err
		}
		if !since.IsZero() {
			out.Config.Failure, out.Config.Since = "unrecovered", since
		}
	}
	if settingsErr != nil && out.Config.Failure == "" {
		return Status{}, settingsErr
	}
	out.ThemesWithoutPackage, err = m.st.ThemesWithoutPackage(ctx)
	return out, err
}

const (
	retentionBudget    = 5 * time.Minute
	maxListedObjects   = 10000
	failureLogInterval = 5 * time.Minute
	configUploadBase   = 5 * time.Minute
	bytesPerMiB        = 1 << 20
)

// 两层各自循环、各自互斥，每秒重新读设置；指标层在途不阻塞配置层。
// 首轮由持久化成功时刻折算等待，之后用单调钟计周期，慢上传不积累待执行次数。
// 墙钟用于快照、事件和成功时刻；对象名的墙钟顺序也是保留顺序，但刚上传的对象不参与删除。
func (m *Manager) Run(ctx context.Context) {
	ids, err := m.st.ThemesWithoutPackage(ctx)
	if err != nil {
		m.log.Error("reading themes without backup package failed", "err", err)
	} else {
		for _, id := range ids {
			m.log.Warn("theme not backed up; upload original package again", "theme", id)
		}
	}
	var wg sync.WaitGroup
	for i := range m.layers {
		wg.Add(1)
		go func() { defer wg.Done(); m.runLayer(ctx, i) }()
	}
	wg.Wait()
}

func (m *Manager) runLayer(ctx context.Context, i int) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var logged bool
	var lastLog time.Duration
	var wake <-chan struct{}
	if i == 0 {
		wake = m.st.ThemeChanges()
	}
	force := false
	for {
		if err := m.tickLayerWithWake(ctx, i, force); err != nil && ctx.Err() == nil && (!logged || m.clk.Mono()-lastLog >= failureLogInterval) {
			m.log.Error("backup scheduling failed", "layer", i, "err", err)
			logged, lastLog = true, m.clk.Mono()
		}
		force = false
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-wake:
			force = true
		}
	}
}

func (m *Manager) Tick(ctx context.Context) error {
	var result error
	for i := range m.layers {
		result = errors.Join(result, m.tickLayer(ctx, i))
	}
	return result
}

// initialize 是两层共用的启动准备：读回两层的成功时刻安排首轮，并清掉本库上次运行残留的暂存目录。
func (m *Manager) initialize(ctx context.Context) error {
	m.initMu.Lock()
	defer m.initMu.Unlock()
	if m.initialized {
		return nil
	}
	times, err := m.st.BackupSuccessTimes(ctx)
	if err != nil {
		return err
	}
	dir, prefix := m.st.BackupScratch()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	// serve 装配并运行一个 Manager；initMu 让清理先于该 Manager 两层创建目录完成。
	for _, entry := range entries {
		if entry.IsDir() && ownScratch(entry.Name(), prefix) {
			if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
				return err
			}
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, name := range []string{store.MaintenanceBackupConfig, store.MaintenanceBackupMetrics} {
		state := &m.layers[i]
		state.LastSuccess = times[name]
		if !state.LastSuccess.IsZero() {
			// 未来成功时刻最多等待一个周期；过旧时刻在 tickLayer 中立即到期。
			elapsed := max(time.Duration(0), m.clk.Now().Sub(state.LastSuccess))
			state.attempted, state.lastAttempt = true, m.clk.Mono()-elapsed
		}
	}
	m.initialized = true
	return nil
}

// ownScratch 判断目录名是否为本库建的暂存目录：prefix 之后恰为 os.MkdirTemp 换上的随机串。
// 前提是 MkdirTemp 把模式末尾的 * 换成十进制数（Go 1.27 的 os.nextRandom），而每个库的前缀都以"库文件名-"结尾：
// 另一个库的目录名若以本前缀开头，本前缀之后必然还含那个库名余下的部分和一个 '-'，不会全是数字。
// 随机串若不再是纯数字，这里只会少删（残留留在盘上），不会删到别的库；启动清理用例用真实 MkdirTemp 造残留，会因此变红。
func ownScratch(name, prefix string) bool {
	rest, ok := strings.CutPrefix(name, prefix)
	return ok && rest != "" && strings.Trim(rest, "0123456789") == ""
}

// restoreFailure 读回库里的配置层未恢复标记：读到即视为本段故障已通知，跨重启不重复触发。
// 只有 RecordBackupEvent 写这个标记，读不出说明库被手工改过或恢复了坏值，此时无从知道此前是否通知过。
func (m *Manager) restoreFailure(ctx context.Context) error {
	state := &m.layers[0]
	if state.restored {
		return nil
	}
	since, err := m.st.BackupFailingSince(ctx)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !since.IsZero() {
		state.notified, state.Since = true, since
		if state.Failure == "" {
			state.Failure = "unrecovered"
		}
	}
	state.restored = true
	return nil
}

// prepare 读出一轮判定所需的持久状态与设置，任一读不出即返回对应的故障类别。
// 配置层先读回未恢复标记：之后无论哪一步失败，判定是否已通知都须以它为准，否则重启后会重复触发。
// 标记读不出时 restored 保持为假，本轮以 marker 类别进入 finish；没有读回的通知状态，notified 为假，于是触发一次，
// RecordBackupEvent 的覆盖写同事务修复坏值。下一轮读回成功后照常执行，marker 故障随执行成功而恢复。
func (m *Manager) prepare(ctx context.Context, i int) (store.BackupSettings, string, error) {
	if i == 0 {
		if err := m.restoreFailure(ctx); err != nil {
			return store.BackupSettings{}, "marker", err
		}
	}
	if err := m.initialize(ctx); err != nil {
		return store.BackupSettings{}, "startup", err
	}
	cfg, err := m.st.BackupSettings(ctx)
	if err != nil {
		return store.BackupSettings{}, "settings", err
	}
	return cfg, "", nil
}

func (m *Manager) tickLayer(ctx context.Context, i int) error {
	return m.tickLayerWithWake(ctx, i, false)
}

func (m *Manager) tickLayerWithWake(ctx context.Context, i int, force bool) error {
	m.runMu[i].Lock()
	defer m.runMu[i].Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	cfg, category, err := m.prepare(ctx, i)
	if err == nil && i == 0 {
		select {
		case <-m.st.ThemeChanges():
			force = true
		default:
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// 前提读不出与执行失败走同一个 finish：配置层首次失败通知一次、日志限频，读得出之后按执行结果恢复。
		// 标记只属于配置层，指标层不读；启动准备与设置是两层共用的前提，由配置层报告，指标层跳过这一轮。
		if i == 0 {
			return m.finish(ctx, i, "config", category, 0, err.Error())
		}
		return nil
	}
	state := &m.layers[i]
	if !cfg.Target.Enabled() {
		state.attempted = false
		// 停用即结束两层的故障跟踪：不再执行，也就不会再有恢复。配置层已通知的故障以停用事件收尾。
		return m.resolve(ctx, i, store.TransitionBackupDisabled, "备份已停用，config 层先前的故障不再跟踪")
	}
	layer := "config"
	interval, keep := cfg.ConfigIntervalS, cfg.ConfigKeep
	if i == 1 {
		layer = "metrics"
		interval, keep = cfg.MetricsIntervalS, cfg.MetricsKeep
	}
	if !force && state.attempted && m.clk.Mono()-state.lastAttempt < time.Duration(interval)*time.Second {
		return nil
	}
	category, code, detail := m.perform(ctx, cfg, layer, keep)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	state.attempted, state.lastAttempt = true, m.clk.Mono()
	return m.finish(ctx, i, layer, category, code, detail)
}

func (m *Manager) perform(ctx context.Context, cfg store.BackupSettings, layer string, keep uint32) (string, int, string) {
	// 每轮刚上传的对象至少占一份；份数 0 无法满足保留不变式，必须在上传和删除前拒绝。
	if keep == 0 {
		return "retention_config", 0, ""
	}
	client, err := m.newClient(cfg.Target)
	if err != nil {
		return "client", 0, err.Error()
	}
	scratchDir, scratchPrefix := m.st.BackupScratch()
	// 末尾显式加 *：库文件名里若也有 *，MkdirTemp 只替换最后一个。
	dir, err := os.MkdirTemp(scratchDir, scratchPrefix+"*")
	if err != nil {
		return "snapshot", 0, err.Error()
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			m.log.Error("backup cleanup failed", "layer", layer, "err", err)
		}
	}()
	path := filepath.Join(dir, "snapshot.db")
	var packages []store.SnapshotThemePackage
	if layer == "metrics" {
		err = m.st.SnapshotMetrics(ctx, path)
	} else {
		packages, err = m.st.SnapshotConfigWithThemes(ctx, path, dir)
	}
	if err != nil {
		m.log.Error("backup snapshot failed", "layer", layer, "err", err)
		return "snapshot", 0, err.Error()
	}
	if layer == "config" {
		if category, code, detail := m.syncThemes(ctx, cfg, client, packages); category != "" {
			return category, code, detail
		}
		for _, pkg := range packages {
			if err := os.Remove(pkg.Path); err != nil {
				return "cleanup", 0, err.Error()
			}
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return "snapshot", 0, err.Error()
	}
	prefix := strings.TrimRight(cfg.Prefix, "/")
	if prefix != "" {
		prefix += "/"
	}
	prefix += layer + "/"
	key := prefix + m.clk.Now().UTC().Format("20060102T150405.000000000Z") + ".db"
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return "snapshot", 0, err.Error()
	}
	// 复用出站边界而非客户端总时限；每次请求由自己的 ctx 截止时间约束。
	uploadCtx, cancelUpload := context.WithTimeout(ctx, uploadBudget(cfg, layer, info.Size()))
	err = client.PutObject(uploadCtx, key, f)
	cancelUpload()
	closeErr := f.Close()
	if err != nil {
		return failure("upload", err)
	}
	if closeErr != nil {
		return "snapshot", 0, closeErr.Error()
	}
	if err := os.Remove(path); err != nil {
		return "cleanup", 0, err.Error()
	}
	// 列举与整轮删除共用五分钟预算；对象总量上限包含历史清理失败积累的额外快照。
	// 上限限制翻页与内存，并暴露清理积压；按最旧删除时截断列表只会少删，不会错删应保留对象。
	retentionCtx, cancelRetention := context.WithTimeout(ctx, retentionBudget)
	defer cancelRetention()
	objects, err := client.ListObjectsV2(retentionCtx, prefix, maxListedObjects)
	if err != nil {
		return failure("list", err)
	}
	// 仅删除本层生成的对象；同 bucket 里的主题、其他前缀及非备份文件不属于保留策略。
	objects = slices.DeleteFunc(objects, func(o s3.Object) bool {
		if o.Key == key {
			return true
		}
		if !strings.HasPrefix(o.Key, prefix) {
			return true
		}
		name := strings.TrimPrefix(o.Key, prefix)
		if !strings.HasSuffix(name, ".db") {
			return true
		}
		_, err := time.Parse("20060102T150405.000000000Z", strings.TrimSuffix(name, ".db"))
		return err != nil
	})
	slices.SortFunc(objects, func(a, b s3.Object) int { return strings.Compare(a.Key, b.Key) })
	// 本轮已上传对象占一份，即使列举暂未看到它也不再删除它。
	for excess := len(objects) - (int(keep) - 1); excess > 0; excess-- {
		if err := client.DeleteObject(retentionCtx, objects[0].Key); err != nil {
			return failure("delete", err)
		}
		objects = objects[1:]
	}
	if err := os.Remove(dir); err != nil {
		return "cleanup", 0, err.Error()
	}
	if err := m.st.RecordBackupSuccess(ctx, layer); err != nil {
		return "record", 0, err.Error()
	}
	return "", 0, ""
}

func uploadBudget(cfg store.BackupSettings, layer string, size int64) time.Duration {
	if layer == "metrics" {
		// 指标上传占自己的一个周期减去保留预算，不挤占配置层。默认 86400-300=86100 秒，
		// 2 GiB 要求有效载荷平均至少 2048/86100=0.02379 MiB/s（约 0.200 Mbit/s），线路还需额外开销。
		// 任意大小 S 的最低速率为 S/预算；低于该速率会超时失败，指标层按设计不告警。
		return time.Duration(cfg.MetricsIntervalS)*time.Second - retentionBudget
	}
	return configUploadBase + time.Duration((size+bytesPerMiB-1)/bytesPerMiB)*time.Second
}

func failure(stage string, err error) (string, int, string) {
	var remote *s3.Error
	if errors.As(err, &remote) {
		return stage + "/" + remote.Kind, remote.StatusCode, remote.Detail
	}
	return stage, 0, err.Error()
}

// finish 记下一轮的结果，category 为空即成功。
// 配置层的未恢复标记与事件由 RecordBackupEvent 同事务提交，prepare 在配置层首次判定通知之前读回。
// 因而持续故障跨重启也不重复通知；事务失败保留原通知状态，下次重试。
// 指标缺口由后续上报继续产生新数据，不触发通知，故障仍由日志与同一状态接口可见。
func (m *Manager) finish(ctx context.Context, i int, layer, category string, code int, detail string) error {
	state := &m.layers[i]
	if category == "" {
		m.mu.Lock()
		state.LastSuccess = time.Unix(m.clk.Now().Unix(), 0).UTC()
		m.mu.Unlock()
		return m.resolve(ctx, i, store.TransitionBackupRecovered, "config 层备份已恢复")
	}
	m.mu.Lock()
	previous := state.Failure
	if previous == "" {
		state.Since = m.clk.Now()
	}
	state.Failure, state.StatusCode = category, code
	notified, since := state.notified, state.Since
	logFailure := !state.logged || previous != category || m.clk.Mono()-state.lastLog >= failureLogInterval
	if logFailure {
		state.logged, state.lastLog = true, m.clk.Mono()
	}
	m.mu.Unlock()
	if logFailure {
		m.log.Warn("backup failed", "layer", layer, "category", category, "status_code", code, "detail", detail)
	}
	if i != 0 || notified {
		return nil
	}
	ev, err := m.st.RecordBackupEvent(ctx, store.TransitionBackupFailed, fmt.Sprintf("config 层备份失败（%s）", category), since)
	if err != nil {
		return err
	}
	if m.sender != nil {
		m.sender.Enqueue(ev)
	}
	m.mu.Lock()
	state.notified = true
	m.mu.Unlock()
	return nil
}

// resolve 结束本层当前的故障。恢复与停用走这同一条路，只差事件种类与文案：配置层已通知过的，
// 先由 RecordBackupEvent 把收尾事件与删除标记同事务提交，提交后才清内存状态；事务失败保留通知状态，下一轮重试。
// 未通知过（指标层，或触发事件从未提交）只清内存状态，不发事件。
func (m *Manager) resolve(ctx context.Context, i int, transition store.Transition, summary string) error {
	state := &m.layers[i]
	m.mu.RLock()
	notified, since := state.notified, state.Since
	m.mu.RUnlock()
	if notified {
		ev, err := m.st.RecordBackupEvent(ctx, transition, summary, since)
		if err != nil {
			return err
		}
		if m.sender != nil {
			m.sender.Enqueue(ev)
		}
	}
	m.mu.Lock()
	state.Failure, state.Since = "", time.Time{}
	state.StatusCode, state.logged, state.notified = 0, false, false
	m.mu.Unlock()
	return nil
}
