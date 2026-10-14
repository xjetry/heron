package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// 清理作业的两种主体，取值即 cleanup_job.kind。
const (
	cleanupKindNode = "node"
	cleanupKindTask = "task"
)

// 每轮维护里清理作业的预算：合计至多这么多片或这么长时间，先到者止。一片是一个写事务（经写协程），删除
// （级别 × 节点 × 任务 × 时间片）一格，行数上界由 pruneSlice 决定：1m 一天 1440 行、5m 七天 2016 行、1h 三十天 720 行，
// 与 prune 的分块同形，片间其它写请求照常排进写协程。满配节点（64 个任务、默认保留期）一个作业约 1625 片
// （每条序列 1m 7 片 + 5m 5 片 + 1h 13 片，指标 1 条加探测 64 条），约 9 轮（每轮一个 MaintenanceInterval）排干。
// 时间预算按单调钟计，只在每一步开始前检查：越过预算的至多是检查之后开始的那一步（一片，加上它前面可能有的
// 展开读，见 cleanupCursor.step）。
const (
	cleanupSlicesPerRound = 200
	cleanupTimePerRound   = 10 * time.Second
)

// enqueueCleanup 在调用方的写事务里登记一个清理作业；同一主体已有作业时不变（attempts 与 last_error 保留）。
// 调用方必须在同一事务里删掉主体本身（node 行或 probe_task 行）：读侧与写入口都以配置层判定主体是否存在，
// 主体消失与作业登记一起提交：历史行从这一刻起对任何读者不可见（family.live），写入口也不再接受该主体的新行
// （WriteMinuteBatch）。行数此后只可能因上卷增加——上卷不按任务过滤，已删任务未删到的细级行可被聚合成粗级行，
// 由游标的级别顺序与完成时的复扫处理（cleanupCursor）。
func enqueueCleanup(tx *sql.Tx, kind string, nodeID, taskID, now int64) error {
	_, err := tx.Exec("INSERT OR IGNORE INTO cleanup_job (kind, node_id, task_id, created_at) VALUES (?, ?, ?, ?)", kind, nodeID, taskID, now)
	return err
}

// orphanTaskJobSQL 给 table 里出现、但 probe_task 里已没有的任务各登记一个 kind=task 作业。任务 id 沿
// (task_id, node_id, ts) 索引逐个跳读（每步一次索引定位），不扫描全部行。迁移 38 有自己冻结的同一条语句。
func orphanTaskJobSQL(table string) string {
	return `INSERT OR IGNORE INTO cleanup_job (kind, node_id, task_id, created_at)
WITH RECURSIVE seen(id) AS (
  SELECT min(task_id) FROM ` + table + `
  UNION ALL
  SELECT (SELECT min(task_id) FROM ` + table + ` WHERE task_id > seen.id) FROM seen WHERE seen.id IS NOT NULL
)
SELECT '` + cleanupKindTask + `', 0, id, ? FROM seen WHERE id IS NOT NULL AND id NOT IN (SELECT id FROM probe_task)`
}

// enqueueOrphanTaskCleanup 给探测表里所有已不存在的任务登记清理作业。Restore 在整表替换之后调用：配置层决定哪些任务
// 存在，指标层可能带着配置快照里没有的任务的行（任务在两次快照之间建或删），这些行没有作业就只能等保留期。
func enqueueOrphanTaskCleanup(ctx context.Context, tx *sql.Tx, now int64) error {
	for _, table := range probeTables {
		if _, err := tx.ExecContext(ctx, orphanTaskJobSQL(table), now); err != nil {
			return fmt.Errorf("enqueue orphan task cleanup from %s: %w", table, err)
		}
	}
	return nil
}

type cleanupJob struct {
	kind           string
	nodeID, taskID int64
}

// cleanupSeries 是一条要删光的序列：一张表里由 where 的等值键确定的全部行。where 的占位符与 args 同序。
type cleanupSeries struct {
	table string
	level int
	where string
	args  []any
}

// cleanupCursor 按级别从细到粗遍历一个作业的全部序列，每次 step 删一片。每轮维护重新建游标、从最细一级重新展开，
// 不跨轮保存进度：上一轮之后上卷可能把尚未删到的细级行重新聚合成粗级行，重新展开就会看到它们。
//
// 级别从细到粗：上卷只从细一级生成粗一级（rollupSQL），且它的源行按 family.live 过滤——已删主体的细级行不会被聚合成
// 粗级行，上卷本身不再重生孤儿行。顺序仍钉为细→粗：它让"一遍走完即为空"不依赖上卷过滤这一处（过滤若被改掉，先删粗级
// 的作业会在片间被重生、复扫不为零，只能等下一轮），完成判据也不靠它（见 finish）。RunMaintenance 里上卷与清理在同一个
// 协程里先后执行，上卷只落在两轮之间；一轮没走完的作业下一轮从最细一级重新展开。
type cleanupCursor struct {
	s       *Store
	job     cleanupJob
	next    int // 下一个待展开的级别下标
	pending []cleanupSeries
}

func (s *Store) newCleanupCursor(job cleanupJob) *cleanupCursor {
	return &cleanupCursor{s: s, job: job}
}

// expand 列出级别 i 上属于本作业的序列。node 作业：指标族一条（node_id 等值走主键），探测族逐任务一条（任务清单经
// 主键前缀 node_id 取 DISTINCT）；task 作业只有探测族，逐节点一条（节点清单经 (task_id, node_id, ts) 索引前缀取）。
// 探测序列一律以 (task_id, node_id) 等值定位，走同一个索引：一片只读、只删这一条序列的行。
func (c *cleanupCursor) expand(ctx context.Context, i int) error {
	probe := probeFamily.tables[i]
	switch c.job.kind {
	case cleanupKindNode:
		c.pending = append(c.pending, cleanupSeries{table: metricFamily.tables[i], level: i, where: "node_id = ?", args: []any{c.job.nodeID}})
		tasks, err := scanIDs(c.s.r.QueryContext(ctx, nodeTasksSQL(probe), c.job.nodeID))
		if err != nil {
			return err
		}
		for _, task := range tasks {
			c.pending = append(c.pending, cleanupSeries{table: probe, level: i, where: "task_id = ? AND node_id = ?", args: []any{task, c.job.nodeID}})
		}
	case cleanupKindTask:
		nodes, err := scanIDs(c.s.r.QueryContext(ctx, taskNodesSQL(probe), c.job.taskID))
		if err != nil {
			return err
		}
		for _, node := range nodes {
			c.pending = append(c.pending, cleanupSeries{table: probe, level: i, where: "task_id = ? AND node_id = ?", args: []any{c.job.taskID, node}})
		}
	default:
		return fmt.Errorf("cleanup job has unknown kind %q", c.job.kind)
	}
	return nil
}

// nodeTasksSQL 列出节点在探测表里出现过的任务：NOT INDEXED 让 WITHOUT ROWID 表只能走主键本身，按前缀 node_id
// 定位，不会被规划器换成以 task_id 打头、要扫遍全部节点的 by_task 索引。读量是该节点在这一级的全部行，每轮每级一次。
func nodeTasksSQL(table string) string {
	return "SELECT DISTINCT task_id FROM " + table + " NOT INDEXED WHERE node_id = ? ORDER BY task_id"
}

// taskNodesSQL 列出任务在探测表里出现过的节点，经 (task_id, node_id, ts) 索引的前缀 task_id 定位。
func taskNodesSQL(table string) string {
	return "SELECT DISTINCT node_id FROM " + table + " INDEXED BY " + table + "_by_task WHERE task_id = ? ORDER BY node_id"
}

// hint 给探测序列钉住 (task_id, node_id, ts) 索引：store 不跑 ANALYZE，没有统计时规划器也可能选主键
// (node_id, ts, task_id)，那会读出该节点全部任务在时间片里的行再逐行过滤，一片的读量不再以一条序列为界。
func (sr cleanupSeries) hint() string {
	if sr.table == probeFamily.tables[sr.level] {
		return " INDEXED BY " + sr.table + "_by_task"
	}
	return ""
}

func (sr cleanupSeries) oldestSQL() string {
	return "SELECT min(ts) FROM " + sr.table + sr.hint() + " WHERE " + sr.where
}

func (sr cleanupSeries) deleteSQL() string {
	return "DELETE FROM " + sr.table + sr.hint() + " WHERE " + sr.where + " AND ts >= ? AND ts < ?"
}

// step 删掉当前序列最老的一片；返回 deleted 为 false 表示本作业的全部级别都已走完，没有片可删。
func (c *cleanupCursor) step(ctx context.Context) (deleted bool, err error) {
	for {
		if len(c.pending) == 0 {
			if c.next == len(levels) {
				return false, nil
			}
			if err := c.expand(ctx, c.next); err != nil {
				return false, err
			}
			c.next++
			continue
		}
		sr := c.pending[0]
		var oldest sql.NullInt64
		if err := c.s.r.QueryRowContext(ctx, sr.oldestSQL(), sr.args...).Scan(&oldest); err != nil {
			return false, err
		}
		if !oldest.Valid {
			c.pending = c.pending[1:]
			continue
		}
		end := oldest.Int64 + pruneSlice[levels[sr.level].Name]
		err := c.s.write(ctx, func(tx *sql.Tx) error {
			_, err := tx.Exec(sr.deleteSQL(), append(append([]any{}, sr.args...), oldest.Int64, end)...)
			return err
		})
		return err == nil, err
	}
}

// finish 是作业的完成判据：在一个写事务里复扫该主体在全部时序表里是否还有行，没有才删作业。判据是"没有剩余行"，
// 不是"片都跑过了"：写入口拒绝已删主体的新行（WriteMinuteBatch）、上卷的源行按 family.live 过滤（rollupSQL），两处都是
// 显式检查，完成仍以库里实际为空为准，不把它们当作完成的依据。复扫与删作业在同一事务里，写协程串行，两者之间插不进写入。
// done 为 false 时作业留着，下一轮重新展开。
func (c *cleanupCursor) finish(ctx context.Context) (done bool, err error) {
	err = c.s.write(ctx, func(tx *sql.Tx) error {
		for _, f := range families {
			for _, table := range f.tables {
				var where string
				var arg int64
				switch {
				case c.job.kind == cleanupKindNode:
					where, arg = "node_id = ?", c.job.nodeID
				case f == probeFamily:
					where, arg = "task_id = ?", c.job.taskID
				default:
					continue // 指标表没有 task_id，task 作业不涉及
				}
				var left bool
				if err := tx.QueryRow("SELECT EXISTS (SELECT 1 FROM "+table+" WHERE "+where+")", arg).Scan(&left); err != nil {
					return err
				}
				if left {
					return nil
				}
			}
		}
		_, err := tx.Exec("DELETE FROM cleanup_job WHERE kind = ? AND node_id = ? AND task_id = ?", c.job.kind, c.job.nodeID, c.job.taskID)
		done = err == nil
		return err
	})
	return done, err
}

// CleanupRound 是一轮清理的结果，供日志使用。
type CleanupRound struct {
	Slices    int
	Completed int
	Failed    int
}

// CleanupDeleted 跑一轮清理：作业按登记先后轮转，每个作业每次删一片，合计至多 cleanupSlicesPerRound 片或
// cleanupTimePerRound（先到者止）。一个作业走完全部级别就复扫并在为空时删除作业。某一步出错就记下 attempts 与
// last_error、本轮不再碰它，下一轮从头重试；作业从不因失败而删除。返回的错误只来自读作业清单本身。
func (s *Store) CleanupDeleted(ctx context.Context) (CleanupRound, error) {
	return s.cleanupRound(ctx, cleanupSlicesPerRound, cleanupTimePerRound)
}

func (s *Store) cleanupRound(ctx context.Context, maxSlices int, maxTime time.Duration) (CleanupRound, error) {
	var round CleanupRound
	jobs, err := s.cleanupJobs(ctx)
	if err != nil {
		return round, err
	}
	cursors := make([]*cleanupCursor, len(jobs))
	for i, job := range jobs {
		cursors[i] = s.newCleanupCursor(job)
	}
	// 预算只有这一个判定：每取一片之前查一次，用尽就停下整轮，不在别处另设条件。
	start := s.clk.Mono()
	spent := func() bool { return round.Slices >= maxSlices || s.clk.Mono()-start >= maxTime }
	for len(cursors) > 0 && !spent() {
		live := cursors[:0]
		for _, c := range cursors {
			if spent() {
				live = append(live, c)
				continue
			}
			deleted, err := c.step(ctx)
			if err == nil && !deleted {
				var done bool
				if done, err = c.finish(ctx); err == nil {
					if done {
						round.Completed++
					}
					continue // 复扫不为零的作业留到下一轮重新展开
				}
			}
			if err != nil {
				round.Failed++
				s.recordCleanupFailure(ctx, c.job, err)
				continue
			}
			round.Slices++
			live = append(live, c)
		}
		cursors = live
	}
	return round, nil
}

// cleanupJobs 按登记先后列出全部作业；同一时刻登记的按主键排，顺序确定。
func (s *Store) cleanupJobs(ctx context.Context) ([]cleanupJob, error) {
	rows, err := s.r.QueryContext(ctx, "SELECT kind, node_id, task_id FROM cleanup_job ORDER BY created_at, kind, node_id, task_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []cleanupJob
	for rows.Next() {
		var j cleanupJob
		if err := rows.Scan(&j.kind, &j.nodeID, &j.taskID); err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// recordCleanupFailure 只累加计数、覆盖最近一次错误，不删作业：作业在就是待办，删掉等于放弃这些行。
// 记录本身失败只打日志，作业原样留着，下一轮照常重试。
func (s *Store) recordCleanupFailure(ctx context.Context, job cleanupJob, cause error) {
	s.log.Error("cleanup of deleted history failed", "kind", job.kind, "node", job.nodeID, "task", job.taskID, "err", cause)
	// 错误文本可能很长（SQL 错误带语句），只留开头；截断落在字符边界上，列里始终是有效 UTF-8。
	message := strings.ToValidUTF8(cause.Error(), "\uFFFD")
	if len(message) > 512 {
		end := 512
		for !utf8.RuneStart(message[end]) {
			end--
		}
		message = message[:end]
	}
	err := s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE cleanup_job SET attempts = attempts + 1, last_error = ? WHERE kind = ? AND node_id = ? AND task_id = ?",
			message, job.kind, job.nodeID, job.taskID)
		return err
	})
	if err != nil {
		s.log.Error("recording cleanup failure failed", "kind", job.kind, "node", job.nodeID, "task", job.taskID, "err", err)
	}
}
