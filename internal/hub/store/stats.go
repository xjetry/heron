package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type TableRows struct {
	Name string
	Rows int64
}

// StorageStats 是库的规模与健康读数，GetStorageStats 与 heron-hub stats 都从它取：DBBytes 为
// page_count × page_size，即数据库的逻辑大小，等于 WAL 检查点之后主文件的大小（检查点之前主文件可能远小于它）；
// 不含 -wal 与 -shm 文件。Tables 按表名升序。Series 按 metric_1m、5m、1h、probe_1m、5m、1h 的固定顺序。
// LastPrune、LastRollup 是 maintenance_state 里的完成时刻（Unix 秒），nil 即从未整轮成功过。CleanupPending 是尚未完成的
// 清理作业数（cleanup.go）：删除节点或探测任务后登记、复扫为零才删，非零表示还有已删主体的时序行在表里。
type StorageStats struct {
	SQLObservedAt  int64
	DBBytes        int64
	Tables         []TableRows
	Series         []SeriesHealth
	LastPrune      *int64
	LastRollup     *int64
	CleanupPending int64
	WAL            WALObservation
	// SchemaBackups 是库旁的迁移前备份，按 seq 从新到旧；每次调用独立列目录，不随 SQL 统计复用。
	// SchemaBackupsError 非空表示列举失败，此时 SchemaBackups 为空且不表示没有备份。
	SchemaBackups      []SchemaBackup
	SchemaBackupsError string
	// WALSizeLimit 是 journal_size_limit（walSizeLimit），读者据此判断 -wal 文件长度相对上限的倍数。
	WALSizeLimit int64
}

// 存储页与 API 读者只需分钟级新鲜度。成功计算之间至少空出此窗口，让耗时 S 的整库
// 扫描持有长读快照的占比不超过 S/(S+60秒)，避免连续请求一直阻止 WAL 重置。
const storageStatsReuse = 60 * time.Second

type storageStatsFlight struct {
	done   chan struct{}
	result StorageStats
	err    error
}

type storageStatsCache struct {
	db        *sql.DB
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	flight    *storageStatsFlight
	cached    *StorageStats
	completed time.Duration
	// 测试在事务已开始时控制计算的进度；只在首次调用前设置。
	afterBegin func(context.Context) error
}

// 返回值归调用方所有，不能让切片或指针的修改污染后续读者复用的快照。
func (s StorageStats) clone() StorageStats {
	s.Tables = slices.Clone(s.Tables)
	s.Series = slices.Clone(s.Series)
	cloneInt := func(p *int64) *int64 {
		if p == nil {
			return nil
		}
		v := *p
		return &v
	}
	for i := range s.Series {
		s.Series[i].Oldest = cloneInt(s.Series[i].Oldest)
		s.Series[i].Watermark = cloneInt(s.Series[i].Watermark)
	}
	s.LastPrune = cloneInt(s.LastPrune)
	s.LastRollup = cloneInt(s.LastRollup)
	return s
}

// WALObservation 是库旁 -wal 的一次 stat 结果，不含 -shm。Bytes 非 nil 表示存在（包括零字节），
// Absent 表示无文件，Error 表示未知；三者只取一个。ObservedAt 为 stat 完成时的 hub 墙钟 Unix 秒。
type WALObservation struct {
	ObservedAt int64
	Bytes      *int64
	Absent     bool
	Error      string
}

// fileStatter 是本包观测库的三个文件的途径，只有 Stat：stat 不打开文件，而打开主文件或 -shm 再关掉会放掉本进程的 SQLite
// 锁（见包注释）。
type fileStatter interface {
	Stat(string) (fs.FileInfo, error)
}

type osFileStatter struct{}

func (osFileStatter) Stat(path string) (fs.FileInfo, error) { return os.Stat(path) }

func (s *Store) observeWAL() WALObservation {
	info, err := s.files.Stat(s.path + "-wal")
	out := WALObservation{ObservedAt: s.clk.Now().Unix()}
	switch {
	case err == nil:
		size := info.Size()
		out.Bytes = &size
	case errors.Is(err, fs.ErrNotExist):
		out.Absent = true
	default:
		out.Error = observationError(err, "stat failed")
	}
	return out
}

// observationError 把文件系统观测的错误变成可放进响应的文本：文件名可含非法 UTF-8，proto string 必须有效，且错误
// 不能使整个统计响应无界增长，所以替换非法字节并截到 512 字节以内；空文本换成 fallback，非空才能表达"失败"。
func observationError(err error, fallback string) string {
	message := strings.ToValidUTF8(err.Error(), "\uFFFD")
	const maxBytes = 512
	if len(message) > maxBytes {
		end := maxBytes
		for !utf8.RuneStart(message[end]) {
			end--
		}
		message = message[:end]
	}
	if message == "" {
		message = fallback
	}
	return message
}

// observeFiles 填入每次调用独立读取的文件系统观测：-wal 文件与迁移前备份。它们不随 SQL 统计复用，与 SQL 快照不是同一时刻。
func (s *Store) observeFiles(out *StorageStats) {
	out.WAL = s.observeWAL()
	out.WALSizeLimit = walSizeLimit
	backups, err := listSchemaBackups(s.backups, s.path)
	if err != nil {
		out.SchemaBackups, out.SchemaBackupsError = nil, observationError(err, "listing backups failed")
		return
	}
	out.SchemaBackups, out.SchemaBackupsError = backups, ""
}

// StorageStats 共用 Store 自有的单份计算；请求取消只结束等待，不取消计算。成功结果从计算完成起
// 复用 storageStatsReuse，失败不缓存。SQLObservedAt 是只读事务开始时的墙钟 Unix 秒；
// WAL 与迁移前备份每次调用独立读取（observeFiles），不承诺与 SQL 同一时刻。
func (s *Store) StorageStats(ctx context.Context) (StorageStats, error) {
	if err := ctx.Err(); err != nil {
		return StorageStats{}, err
	}
	// Close 持写锁阻止新计算入场；计算协程不获取 closeMu，Close 可持锁等待它退出。
	s.closeMu.RLock()
	if s.closed {
		s.closeMu.RUnlock()
		return StorageStats{}, ErrClosed
	}
	s.stats.mu.Lock()
	if cached := s.stats.cached; cached != nil && s.clk.Mono()-s.stats.completed < storageStatsReuse {
		s.stats.mu.Unlock()
		s.closeMu.RUnlock()
		out := cached.clone()
		s.observeFiles(&out)
		return out, nil
	}
	flight := s.stats.flight
	if flight == nil {
		flight = &storageStatsFlight{done: make(chan struct{})}
		s.stats.flight = flight
		go s.runStorageStats(flight)
	}
	s.stats.mu.Unlock()
	s.closeMu.RUnlock()
	select {
	case <-ctx.Done():
		return StorageStats{}, ctx.Err()
	case <-flight.done:
		if err := ctx.Err(); err != nil {
			return StorageStats{}, err
		}
		if flight.err != nil {
			return StorageStats{}, flight.err
		}
		out := flight.result.clone()
		s.observeFiles(&out)
		return out, nil
	}
}

func (s *Store) runStorageStats(flight *storageStatsFlight) {
	flight.result, flight.err = s.computeStorageStats(s.stats.ctx)
	s.stats.mu.Lock()
	defer s.stats.mu.Unlock()
	if flight.err == nil {
		s.stats.cached = &flight.result
		s.stats.completed = s.clk.Mono()
	}
	s.stats.flight = nil
	close(flight.done)
}

// 行数、逻辑大小与维护健康在独立连接的同一只读事务里读出。表名取自 sqlite_master 而不是手写清单：
// 新增的表自动计入。名字以 sqlite_ 开头的是 SQLite 内部表（如 AUTOINCREMENT 的 sqlite_sequence），不计；
// 前缀按字面比较，不用 LIKE（它的 _ 是通配符，且对 ASCII 不分大小写）。
func (s *Store) computeStorageStats(ctx context.Context) (StorageStats, error) {
	tx, err := s.stats.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return StorageStats{}, err
	}
	defer tx.Rollback()
	out := StorageStats{SQLObservedAt: s.clk.Now().Unix()}
	if s.stats.afterBegin != nil {
		if err := s.stats.afterBegin(ctx); err != nil {
			return StorageStats{}, err
		}
	}
	var names []string
	rows, err := tx.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type = 'table' AND substr(name, 1, 7) <> 'sqlite_' ORDER BY name")
	if err != nil {
		return StorageStats{}, err
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return StorageStats{}, err
		}
		names = append(names, n)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return StorageStats{}, err
	}
	oldest := map[string]*int64{}
	for _, f := range families {
		for _, table := range f.tables {
			// nil 只表示空表的最老桶；必需表缺失是 schema 损坏，不能伪装成空表健康读数。
			if !slices.Contains(names, table) {
				return StorageStats{}, fmt.Errorf("storage stats: missing time-series table %s", table)
			}
			oldest[table] = nil
		}
	}
	for _, n := range names {
		var c int64
		// 表名来自 sqlite_master，按 SQL 标识符规则加双引号并转义内部的双引号。
		table := `"` + strings.ReplaceAll(n, `"`, `""`) + `"`
		var err error
		if _, series := oldest[n]; series {
			var ts sql.NullInt64
			err = tx.QueryRowContext(ctx, `SELECT COUNT(*), min(ts) FROM `+table).Scan(&c, &ts)
			if ts.Valid {
				oldest[n] = &ts.Int64
			}
		} else {
			err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&c)
		}
		if err != nil {
			return StorageStats{}, err
		}
		out.Tables = append(out.Tables, TableRows{Name: n, Rows: c})
	}
	var pages, pageSize int64
	if err := tx.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages); err != nil {
		return StorageStats{}, err
	}
	if err := tx.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		return StorageStats{}, err
	}
	out.DBBytes = pages * pageSize
	if out.Series, err = seriesHealth(ctx, tx, oldest); err != nil {
		return StorageStats{}, err
	}
	last, err := lastMaintenance(ctx, tx)
	if err != nil {
		return StorageStats{}, err
	}
	if at, ok := last[MaintenancePrune]; ok {
		out.LastPrune = &at
	}
	if at, ok := last[MaintenanceRollup]; ok {
		out.LastRollup = &at
	}
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM cleanup_job").Scan(&out.CleanupPending); err != nil {
		return StorageStats{}, err
	}
	if err := tx.Commit(); err != nil {
		return StorageStats{}, err
	}
	return out, nil
}
