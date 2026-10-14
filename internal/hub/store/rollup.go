package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/xjetry/heron/internal/hub/metric"
	"github.com/xjetry/heron/internal/probelimit"
)

// RollupLag 是上卷不越过的滞后：只上卷结束时刻不晚于 now − RollupLag 的桶。
// 它必须覆盖一条数据最晚成为 1m 行的时间（迟到上限 + 一个刷出周期 + 排队余量），
// 关系由 ingest 包的编译期断言钉住。上下级一致性本身不依赖这个取值，只依赖
// 写协程拒绝水位之前的写入；滞后的作用是让那条拒绝在正常运行时不触发。
const RollupLag = 300 * time.Second

type Level struct {
	Name string
	// Bucket 是桶长，秒。
	Bucket int64
}

// levels 从细到粗；上卷按此顺序进行，粗一级只能用细一级已冻结的行。
var levels = []Level{
	{Name: "1m", Bucket: 60},
	{Name: "5m", Bucket: 300},
	{Name: "1h", Bucket: 3600},
}

func LevelByName(name string) (Level, bool) {
	for _, lv := range levels {
		if lv.Name == name {
			return lv, true
		}
	}
	return Level{}, false
}

func alignDown(ts, bucket int64) int64 { return ts - ts%bucket }

// aggregates 是"把若干行折叠成一行"的 SELECT 列表：和、样本数求和，最大值取最大。
// 上卷与查询时的二次分桶是同一种运算，所以共用。
func aggregates() []string {
	var out []string
	for _, c := range metric.Columns {
		out = append(out, "sum("+c.Name+"_sum)", "sum("+c.Name+"_n)")
		if c.Kind == metric.MeanMax {
			out = append(out, "max("+c.Name+"_max)")
		}
	}
	return out
}

// family 描述一个时间序列表族：三级表、各级水位在 rollup_state 里的键、键列与值列的
// SQL 片段。两族共用上卷、清理与按水位拼接查询的控制流程；
// 表名、水位键和聚合列来自同一个描述，避免各操作把两族混用。
type family struct {
	name string
	// tables / states 与 levels 同序；states[0] 为空——最细一级没有水位。
	tables []string
	states []string
	// extraKey 是 node_id、ts 之外的键列（探测族为 task_id），为空则没有。
	extraKey string
	values   func() []string
	aggs     func() []string
	// live 把源行限定在配置层里仍存在的主体上：节点在 node 表里，探测行的任务在 probe_task 表里。删除节点或任务的
	// 事务只删配置层并登记清理作业（cleanup.go），时序行在作业完成前仍在表里；所有历史读都经 rangeSQL 带上它
	// （不经 queryFamily 的 EvaluationReader.ProbeBucketMeans 在自己的 SQL 里拼上 probeFamily.live），
	// 与主体存在性判定在同一个读快照里，这些孤儿行因此不出现在任何查询结果里，与准入检查和读之间是否插进了删除无关。
	// 它只过滤、不参与定位，没有占位符：每条源查询仍按 keyWhere 的前导键 SEARCH，读到的行（额度计数的对象）含孤儿行。
	live string
}

var metricFamily = &family{name: "metric", tables: metricTables, states: []string{"", "5m", "1h"},
	values: func() []string { return append(metricColumnNames(), "minutes", "observed", "both") },
	aggs:   func() []string { return append(aggregates(), coverageAggregates()...) },
	live:   "node_id IN (SELECT id FROM node)"}

var probeFamily = &family{name: "probe", tables: probeTables, states: []string{"", "probe_5m", "probe_1h"},
	extraKey: "task_id", values: probeValueColumns, aggs: probeAggregates,
	live: "node_id IN (SELECT id FROM node) AND task_id IN (SELECT id FROM probe_task)"}

// 水位各自独立，成功完成全部族时结果与顺序无关；Rollup 在首个错误处返回，
// 前一族失败时后一族本轮不上卷。
var families = []*family{metricFamily, probeFamily}

func (f *family) keys() []string {
	k := []string{"node_id", "ts"}
	if f.extraKey != "" {
		k = append(k, f.extraKey)
	}
	return k
}

// groupBy 是"整桶重算"的分组键：ts 对齐到桶长，其余键原样。
func (f *family) groupBy(bucket string) string {
	g := "node_id, ts - ts % " + bucket
	if f.extraKey != "" {
		g += ", " + f.extraKey
	}
	return g
}

// rollupSQL 的源行同样按 family.live 过滤：删除节点或任务之后、清理作业完成之前仍在细级表里的孤儿行不会被聚合成粗级行，
// 上卷因此不是孤儿行的重生来源。cleanup.go 的细→粗顺序与完成复扫仍保留——完成判据是"库里实际为空"这一显式检查，不靠
// 这里的过滤兜底。
func (f *family) rollupSQL(i int) string {
	b := fmt.Sprint(levels[i].Bucket)
	source := f.tables[i-1]
	if f == metricFamily {
		source = "(" + metricSourceSQL(source, i-1) + ")"
	}
	return "INSERT OR REPLACE INTO " + f.tables[i] + " (" + strings.Join(append(f.keys(), f.values()...), ", ") + ") " +
		"SELECT " + f.groupBy(b) + ", " + strings.Join(f.aggs(), ", ") +
		" FROM " + source + " WHERE " + f.live + " AND ts >= ? AND ts < ? GROUP BY " + f.groupBy(b)
}

// Rollup 对每一粗级：取水位之后、滞后期已过的下级桶，整桶重算写入本级，并在
// 同一事务推进水位。两族全部级别都成功后才记下 maintenance_state 的 rollup 完成时刻。levels 必须从细到粗：lowerUpto 取自刚完成的下级，
// 按本级桶长对齐后限制上界，只消费下级已冻结的整桶。当前正常推进路径中，
// 此上界与 ceiling 的对齐上界相同；min 显式保留两项约束，不替代处理顺序。
func (s *Store) Rollup(ctx context.Context) error {
	ceiling := s.clk.Now().Add(-RollupLag).Unix()
	for _, f := range families {
		lowerUpto := int64(math.MaxInt64)
		for i := 1; i < len(levels); i++ {
			lv := levels[i]
			limit := min(alignDown(ceiling, lv.Bucket), alignDown(lowerUpto, lv.Bucket))
			upto, err := s.rollupLevel(ctx, f, i, limit)
			if err != nil {
				return fmt.Errorf("rollup %s %s: %w", f.name, lv.Name, err)
			}
			lowerUpto = upto
		}
	}
	if err := s.recordMaintenance(ctx, MaintenanceRollup); err != nil {
		return fmt.Errorf("record rollup completion: %w", err)
	}
	return nil
}

// rollupSlice 限制每次占用写协程的历史跨度，让其他写请求能在追赶的片间提交。
var rollupSlice = map[string]int64{"5m": 86400, "1h": 7 * 86400}

// rollupLevel 每片通过同一次 write 事务插入桶并推进水位，二者一起提交或回滚。
// 每片重读水位，允许其他维护调用在片间推进；空白历史在同一事务中确认后跳过，
// 避免新库从零水位逐日提交空事务。非空片的起止均对齐目标桶，不拆开整桶。
func (s *Store) rollupLevel(ctx context.Context, f *family, i int, limit int64) (int64, error) {
	lv := levels[i]
	var upto int64
	for {
		err := s.write(ctx, func(tx *sql.Tx) error {
			if err := tx.QueryRow("SELECT upto_ts FROM rollup_state WHERE level = ?", f.states[i]).Scan(&upto); err != nil {
				return err
			}
			if limit <= upto {
				return nil
			}
			var first sql.NullInt64
			// 每个节点从复合主键的时间范围取首行，再求全局最小值，不扫描全部历史。
			if err := tx.QueryRow("SELECT min((SELECT ts FROM "+f.tables[i-1]+
				" WHERE node_id = node.id AND ts >= ? AND ts < ? ORDER BY ts LIMIT 1)) FROM node", upto, limit).Scan(&first); err != nil {
				return err
			}
			end := limit
			if first.Valid {
				start := max(upto, alignDown(first.Int64, lv.Bucket))
				end = start + min(rollupSlice[lv.Name], limit-start)
				if _, err := tx.Exec(f.rollupSQL(i), start, end); err != nil {
					return err
				}
			}
			res, err := tx.Exec("UPDATE rollup_state SET upto_ts = ? WHERE level = ?", end, f.states[i])
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return fmt.Errorf("rollup_state has no row for level %s", f.states[i])
			}
			upto = end
			return nil
		})
		if err != nil || upto >= limit {
			return upto, err
		}
	}
}

type Retention struct {
	M1, M5, H1 time.Duration
	// 零值会把截止点放在现在，删除此前全部事件；RunMaintenance 自行 Validate，拒绝过短或非正保留期。
	AlertEvents time.Duration
}

var DefaultRetention = Retention{M1: 7 * 24 * time.Hour, M5: 30 * 24 * time.Hour, H1: 365 * 24 * time.Hour, AlertEvents: 90 * 24 * time.Hour}

// 保留期准入与命令帮助共用下限，避免显示可用的配置在实际启动时被拒绝。
const (
	MinRetentionM1 = 6 * time.Hour
	MinRetentionM5 = 7 * 24 * time.Hour
	MinRetentionH1 = 7 * 24 * time.Hour
	// 满队列的最坏投递时长必须短于事件保留期，否则 worker 回读已清理事件会丢失通知。
	// alert 包用例按队列容量、尝试次数、客户端超时和退避的真实常量钉住关系；按天取整留足余量。
	MinRetentionAlertEvents = 24 * time.Hour
)

// Validate 的下限与 ChooseLevel 的选级阈值同向：1m 覆盖六小时、5m 覆盖七天，
// 且粗级不短于细级，避免刚跨选级边界就因保留期更短而失去历史。
// 事件下限覆盖满队列的最坏投递时长，不能在正常重试完成前清掉事件。
func (r Retention) Validate() error {
	if r.AlertEvents < MinRetentionAlertEvents {
		return fmt.Errorf("alert event retention is %v, minimum is %v", r.AlertEvents, MinRetentionAlertEvents)
	}
	if r.M1 < MinRetentionM1 {
		return fmt.Errorf("retention for 1m level is %v, minimum is %v", r.M1, MinRetentionM1)
	}
	if r.M5 < MinRetentionM5 {
		return fmt.Errorf("retention for 5m level is %v, minimum is %v", r.M5, MinRetentionM5)
	}
	if r.H1 < MinRetentionH1 {
		return fmt.Errorf("retention for 1h level is %v, minimum is %v", r.H1, MinRetentionH1)
	}
	if r.M5 < r.M1 || r.H1 < r.M5 {
		return errors.New("retention must not shrink as the level gets coarser (1m <= 5m <= 1h)")
	}
	return nil
}

// ForLevel 是 name 这一级的保留期；Prune 的截止点与存储健康的超期判定都按它取。
func (r Retention) ForLevel(name string) time.Duration {
	switch name {
	case "5m":
		return r.M5
	case "1h":
		return r.H1
	}
	return r.M1
}

// pruneSlice 限制每块删除覆盖的时间片（秒）；deleteRange 每次只处理一个节点，
// 不把该节点的全部过期历史合并成一个写事务。
var pruneSlice = map[string]int64{"1m": 86400, "5m": 7 * 86400, "1h": 30 * 86400}

// Prune 删除各级保留期之外的行。每块一个短事务，按节点、按时间片：
// WHERE node_id = ? AND ts >= ? AND ts < ? 走主键。节点集合取自表本身而不是
// node 表：已删节点若留有孤儿行，也要随保留期消失。全部表都清理完才记下 maintenance_state 的
// prune 完成时刻；事件表的清理（PruneAlertEvents）不在其内。
func (s *Store) Prune(ctx context.Context, r Retention) (int64, error) {
	now := s.clk.Now().Unix()
	var total int64
	for _, f := range families {
		for i, lv := range levels {
			cutoff := alignDown(now-int64(r.ForLevel(lv.Name)/time.Second), lv.Bucket)
			// 初始化后只有 rollupLevel 写水位，每片 end > upto，且与聚合原子提交。
			// 因此消费水位只前进，读到旧值至多延迟清理，不会提前删掉未聚合的行。
			if i+1 < len(levels) {
				var consumed int64
				if err := s.r.QueryRowContext(ctx, "SELECT upto_ts FROM rollup_state WHERE level = ?", f.states[i+1]).Scan(&consumed); err != nil {
					return total, err
				}
				cutoff = min(cutoff, consumed)
			}
			ids, err := s.distinctNodes(ctx, f.tables[i])
			if err != nil {
				return total, err
			}
			for _, id := range ids {
				for {
					var oldest sql.NullInt64
					if err := s.r.QueryRowContext(ctx, "SELECT min(ts) FROM "+f.tables[i]+" WHERE node_id = ?", id).Scan(&oldest); err != nil {
						return total, err
					}
					if !oldest.Valid || oldest.Int64 >= cutoff {
						break
					}
					end := min(oldest.Int64+pruneSlice[lv.Name], cutoff)
					n, err := s.deleteRange(ctx, f.tables[i], id, oldest.Int64, end)
					if err != nil {
						return total, err
					}
					total += n
				}
			}
		}
	}
	if err := s.recordMaintenance(ctx, MaintenancePrune); err != nil {
		return total, fmt.Errorf("record prune completion: %w", err)
	}
	return total, nil
}

func (s *Store) distinctNodes(ctx context.Context, table string) ([]int64, error) {
	rows, err := s.r.QueryContext(ctx, "SELECT DISTINCT node_id FROM "+table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) deleteRange(ctx context.Context, table string, nodeID, from, to int64) (int64, error) {
	var n int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec("DELETE FROM "+table+" WHERE node_id = ? AND ts >= ? AND ts < ?", nodeID, from, to)
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		return nil
	})
	return n, err
}

func ceilDiv(n, d int64) int64 {
	q := n / d
	if n%d > 0 {
		q++
	}
	return q
}

// ChooseLevel 按窗口跨度选级，步长保持为桶长的整数倍。点数预算按
// QueryMetrics 对齐后的窗口计算，不能只限制原始跨度，以免首尾扩展后超限。
func ChooseLevel(from, to int64, maxPoints int) (Level, int64) {
	span := to - from
	lv := levels[0]
	switch {
	case span <= 6*3600:
	case span <= 7*86400:
		lv = levels[1]
	default:
		lv = levels[2]
	}
	step := lv.Bucket
	if maxPoints > 0 {
		budget := int64(maxPoints)
		step = max(step, ceilDiv(ceilDiv(span, budget), lv.Bucket)*lv.Bucket)
		for ceilDiv(to, step)-from/step > budget {
			// k 是当前首桶编号。增大步长后首桶编号只会减小，所以任何可行的
			// 下一步长至少是 ceil(to/(k+budget))；跳过更小值不会丢掉可行解。
			need := ceilDiv(to, from/step+budget)
			step = ceilDiv(need, lv.Bucket) * lv.Bucket
		}
	}
	return lv, step
}

// aggregateSQL 是查询时的二次分桶：与上卷同一种运算，步长作为绑定参数。
// groupKey 非空时按它拆序列并按 (groupKey, ts) 排序：单节点探测按 task_id（调用方据此切成
// 每任务一条序列），跨节点对比按 node_id（每节点一条序列）。
func (f *family) aggregateSQL(table, groupKey string) string {
	sel, group, order := "ts - ts % ?", "1", "1"
	if groupKey != "" {
		sel += ", " + groupKey
		group, order = "1, 2", "2, 1"
	}
	return "SELECT " + sel + ", " + strings.Join(f.aggs(), ", ") + " FROM " + table +
		" GROUP BY " + group + " ORDER BY " + order
}

func (f *family) rangeSQL(i int, shape queryShape) string {
	columns := strings.Join(append(f.keys(), f.values()...), ", ")
	if f == metricFamily {
		columns = "node_id, ts, " + strings.Join(append(metricColumnNames(), coverageSource(i)...), ", ") + fmt.Sprintf(", %d AS source_width", levels[i].Bucket)
	}
	return "SELECT " + columns + " FROM " + f.tables[i] + byTaskHint(f, i, shape) + " WHERE " + shape.keyWhere + " AND " + f.live + " AND ts >= ? AND ts <= ?"
}

// byTaskHint 给对比形状的每条源查询钉 INDEXED BY <表>_by_task。store 从不跑 ANALYZE，无统计时
// 规划器可能给（task_id 等值 + node_id IN + ts 范围）选中主键探测——按节点扫窗口内全部任务的行，
// 读量与对比分块的任务无关，额度"计数 = 聚合工作量"的前提就破了。索引在 v33 起恒存在（迁移 33
// 补建），钉死计划也让计数与聚合走同一条路。
func byTaskHint(f *family, i int, shape queryShape) string {
	if !shape.byTaskIndex {
		return ""
	}
	return " INDEXED BY " + f.tables[i] + "_by_task"
}

// queryShape 描述一次查询的来源形状：等值键约束决定每条源查询经哪个主键或索引定位，
// 分组键决定二次分桶在 ts 之外还按哪列拆序列。两个取值都由查询的语义决定，不由表结构决定：
// 同一张探测表，单节点查询按 (node_id) 等值加 ts 范围走主键、按 task_id 拆序列；跨节点对比按
// (task_id, node_id) 等值加 ts 范围走 (task_id, node_id, ts) 索引、按 node_id 拆序列。
// seriesLimit 是本次请求的序列额度权重（见 quotaRowsPerSeries）：指标与覆盖率为 1（每节点每时刻
// 一行）、单节点探测为每节点任务分配上限、对比为分块节点上限。seriesEstimate 是调用方所知的实际序列数，
// 只决定首次选池（见 scanEstimate），与额度无关。
type queryShape struct {
	// keyWhere 的占位符与 keyArgs 同序，必须是某主键或索引的前导等值键——每条源查询因此是
	// SEARCH 且扫描行 = 输出行，这是读量额度"计数 = 工作量"的前提，由查询计划的断言测试钉住。
	keyWhere string
	keyArgs  []any
	// groupKey 为空表示只按 ts 分桶（指标族）；非空时聚合输出多一列并按 (groupKey, ts) 排序。
	groupKey string
	// seriesLimit 恒为正；额度 R = quotaRowsPerSeries × seriesLimit。
	seriesLimit int64
	// seriesEstimate 为 0 表示调用方不知道实际序列数，估计退回 seriesLimit（见 estimatedSeries）。
	seriesEstimate int64
	// byTaskIndex 钉住每条源查询的读取计划（见 byTaskHint）：对比形状必须置位，其余形状留空。
	byTaskIndex bool
}

// estimatedSeries 是预计扫描量按多少条序列算：调用方给了实际序列数就用它，否则按额度权重（序列上限）估。
func (q queryShape) estimatedSeries() int64 {
	if q.seriesEstimate > 0 {
		return q.seriesEstimate
	}
	return q.seriesLimit
}

// queryFamily 的水位与源行属于同一个读快照：rollupLevel 原子提交桶与水位，
// Prune 随后可删除已消费的细级行，分开读会把旧水位与清理后的源行混用。
// 各源区间互斥；水位不必对齐 step，所以合并源行后才用上卷表达式统一分桶。
//
// 读量额度在聚合之前、同一事务里对各级源查询按同一条件计数：计数读到的行就是聚合要读的行。
// 每级计数的 LIMIT 是剩余额度 + 1，命中即超额返回，不必数完全表；未超额时每级计数恰为该级
// 实际行数。计数只约束实际要读的源行，不从窗口跨度或水位落后时长推算：未来时刻没有数据，
// 空库与新库不会被拒，维护长期停滞或细级尾巴过长则按实际行数被拒（ReadQuotaError）。
//
// 请求驱动的读在轻池上执行时，计数另按 lightScanRows 封顶（见 readFamily 的 scanCap）：预计扫描量只决定首次选池，
// 估计偏小（任务更替留下的历史序列、过长的细级尾巴）时，计数在读到第 lightScanRows+1 行时停下，readFamily 回滚
// r 上的事务、归还连接后返回 errScanCapped，这里再到 hr 上按原额度重跑。所以在 r 上完成的查询，实测源行数不超过
// lightScanRows。重跑的额外代价有界：r 上的计数在累计读到 lightScanRows+1 行时停下，这一次尝试至多多读这么多行，
// 且只发生在被误分进 r 的查询上。
func queryFamily[T any](ctx context.Context, s *Store, route readRoute, f *family, shape queryShape, from, to int64, lv Level, step int64, scan func(*sql.Rows) ([]T, error), summarize func(*sql.Tx, string, []any) error) ([]T, error) {
	i, err := checkStep(lv, step)
	if err != nil {
		return nil, err
	}
	from, to = alignWindow(from, to, step)
	if route == evaluationRead {
		return readFamily(ctx, s.ev, 0, f, shape, i, from, to, step, scan, summarize)
	}
	db, scanCap := s.readPoolFor(scanEstimate(from, to, lv, shape.estimatedSeries()))
	out, err := readFamily(ctx, db, scanCap, f, shape, i, from, to, step, scan, summarize)
	if errors.Is(err, errScanCapped) {
		// readFamily 返回前已回滚 r 上的事务、归还连接：先还 r 再取 hr，不持一个读连接等另一个（见 readPoolFor）。
		return readFamily(ctx, s.hr, 0, f, shape, i, from, to, step, scan, summarize)
	}
	return out, err
}

// errScanCapped 是 readFamily 在封顶池上计数触顶的信号，只在 queryFamily 内部流转，不返回给调用方。
var errScanCapped = errors.New("scan cap reached on the light read pool")

// readRoute 是 queryFamily 的选池方式，两条原则见 readPoolFor。
type readRoute int

const (
	// requestRead 是请求驱动的读：按预计扫描量在 r / hr 之间选池。
	requestRead readRoute = iota
	// evaluationRead 是 hub 自己的告警评估读：固定走 ev，只经 EvaluationReader 发出。
	evaluationRead
)

// readFamily 在 db 上的一个只读事务里完成 queryFamily 的计数与聚合，i 是已校验的级别下标，窗口已对齐。
// scan 与 summarize 只用这个事务，持着它再取别的读连接会违反读池不互等的约束（见 readPoolFor）。
//
// scanCap 为正时，各级计数累计读到的源行数不得超过它：每级 LIMIT 取 min(剩余额度, 剩余封顶)+1，两个余量都随层递减，
// 累计超过即返回 errScanCapped。事务由本函数帧的 defer 回滚，返回即归还连接；调用方据此在返回之后才去取另一个池，
// 所以封顶重跑不能与这次尝试同在一个持事务的函数帧里。scanCap 为 0 不封顶。
func readFamily[T any](ctx context.Context, db *sql.DB, scanCap int64, f *family, shape queryShape, i int, from, to, step int64, scan func(*sql.Rows) ([]T, error), summarize func(*sql.Tx, string, []any) error) ([]T, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var sources []string
	args := []any{step}
	// watermarks 记录参与本次查询的各粗级水位，只在超额时随错误带出。
	var watermarks []LevelWatermark
	remaining := quotaRowsPerSeries * shape.seriesLimit
	capLeft := scanCap
	for ; i >= 0; i-- {
		query := f.rangeSQL(i, shape)
		levelArgs := append(append([]any{}, shape.keyArgs...), from, to)
		args = append(args, levelArgs...)
		if i > 0 {
			var upto int64
			if err := tx.QueryRowContext(ctx, "SELECT upto_ts FROM rollup_state WHERE level = ?", f.states[i]).Scan(&upto); err != nil {
				return nil, err
			}
			query += " AND ts < ?"
			levelArgs = append(levelArgs, upto)
			args = append(args, upto)
			watermarks = append(watermarks, LevelWatermark{Level: f.states[i], Upto: upto})
			from = max(from, upto)
		}
		sources = append(sources, query)
		// 额度计数与聚合同一条计划（含 INDEXED BY），计数读到的行就是聚合要读的行。
		count := "SELECT count(*) FROM (SELECT 1 FROM " + f.tables[i] + byTaskHint(f, i, shape) + " WHERE " + shape.keyWhere + " AND ts >= ? AND ts <= ?"
		if i > 0 {
			count += " AND ts < ?"
		}
		limit := remaining
		if scanCap > 0 {
			limit = min(limit, capLeft)
		}
		countArgs := append(append([]any{}, levelArgs...), limit+1)
		var n int64
		if err := tx.QueryRowContext(ctx, count+" LIMIT ?)", countArgs...).Scan(&n); err != nil {
			return nil, err
		}
		// 先判额度：剩余额度不大于剩余封顶时，命中 LIMIT 即已超额，换池重跑读到的是同样的行，照样被拒。
		remaining -= n
		if remaining < 0 {
			return nil, ReadQuotaError{Quota: quotaRowsPerSeries * shape.seriesLimit, Series: shape.seriesLimit, Watermarks: watermarks}
		}
		if scanCap > 0 {
			capLeft -= n
			if capLeft < 0 {
				return nil, errScanCapped
			}
		}
	}
	union := strings.Join(sources, " UNION ALL ")
	if summarize != nil {
		if err := summarize(tx, union, args[1:]); err != nil {
			return nil, err
		}
	}
	rows, err := tx.QueryContext(ctx, f.aggregateSQL("("+union+")", shape.groupKey), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scan(rows)
}

// quotaRowsPerSeries（B）是读量额度的资源策略常量：一条序列在一次请求里至多计入这么多源行。
// 400 天窗口在 1h 级每条序列至多 9600 个桶，余量覆盖窗口对齐扩大与维护及时执行时的细级尾巴
// （5m、1h 水位分别落后约一个各自周期的量级）；这是对常量的说明，不是代码保证的最大落后。
const quotaRowsPerSeries = 12000

// QueryMetrics 返回 [from, to) 内按 step 聚合的桶；from 向下、to 向上对齐到 step，
// 结果的 TS 都是 step 的整数倍。只返回有行的桶：缺失的桶就是没有数据。
// 最新桶只含已刷出的分钟行，不读取 live 中尚未刷出的当前分钟。
// 指标族每节点每时刻一行，额度权重为 1。
func (s *Store) QueryMetrics(ctx context.Context, nodeID int64, from, to int64, lv Level, step int64) ([]metric.Row, error) {
	return s.queryMetrics(ctx, requestRead, nodeID, from, to, lv, step)
}

func (s *Store) queryMetrics(ctx context.Context, route readRoute, nodeID int64, from, to int64, lv Level, step int64) ([]metric.Row, error) {
	return queryFamily(ctx, s, route, metricFamily, queryShape{keyWhere: "node_id = ?", keyArgs: []any{nodeID}, seriesLimit: 1},
		from, to, lv, step,
		func(rows *sql.Rows) ([]metric.Row, error) { return scanBucketRows(rows, nodeID) }, nil)
}

// MaintenanceInterval 是维护循环的周期：每个周期边界后跑一轮上卷与清理。存储健康的超期阈值
// （SeriesHealth.Staleness）把它算进去，两处必须是同一个值。
const MaintenanceInterval = time.Minute

// nextMaintenanceAt 落在分钟边界后 2 秒：分钟刷出在 +0.5s，两者的顺序其实不
// 重要——上卷只碰至少 RollupLag 之前闭合的桶——错开只是避免同时争写协程。
func nextMaintenanceAt(wall time.Time) time.Time {
	return wall.Truncate(MaintenanceInterval).Add(MaintenanceInterval + 2*time.Second)
}

// RunMaintenance 按分钟边界调度上卷、清理与已删主体的历史清理；一轮维护使用 Background，
// 因而取消只在等待下一轮时生效，已开始的一轮会完成后再退出。
// AlertEvents 为零会以现在为截止点删除此前全部事件，不能视作禁用清理。
// 本入口拒绝非法装配，不依赖调用方记得校验；serve 的 Validate 另提供启动时的友好错误。
func (s *Store) RunMaintenance(ctx context.Context, r Retention) {
	if err := r.Validate(); err != nil {
		panic(err)
	}
	for {
		wall := s.clk.Now()
		timer := time.NewTimer(nextMaintenanceAt(wall).Sub(wall))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			if err := s.Rollup(context.Background()); err != nil {
				s.log.Error("rollup failed", "err", err)
				// 上卷失败只跳过时序行清理，保留未上卷的分钟行；Prune 的水位守卫仍独立生效。
			} else if n, err := s.Prune(context.Background(), r); err != nil {
				s.log.Error("prune failed", "err", err)
			} else if n > 0 {
				s.log.Info("pruned expired rows", "rows", n)
			}
			// 事件表不依赖上卷水位；时序表故障不能阻止它按保留期清理。
			if n, err := s.PruneAlertEvents(context.Background(), s.clk.Now().Add(-r.AlertEvents)); err != nil {
				s.log.Error("prune alert events failed", "err", err)
			} else if n > 0 {
				s.log.Info("pruned expired alert events", "events", n)
			}
			// 已删主体的历史清理也不依赖上卷：被删的行不再有读者，上卷失败时照样清。放在上卷与 prune 之后：清理有
			// 每轮预算（cleanupTimePerRound），排在前面会推迟本轮的上卷。
			if round, err := s.CleanupDeleted(context.Background()); err != nil {
				s.log.Error("listing cleanup jobs failed", "err", err)
			} else if round.Slices > 0 || round.Completed > 0 || round.Failed > 0 {
				s.log.Info("cleaned up deleted history", "slices", round.Slices, "completed_jobs", round.Completed, "failed_jobs", round.Failed)
			}
		}
	}
}

func levelIndex(lv Level) int {
	for i, level := range levels {
		if level.Name == lv.Name {
			return i
		}
	}
	return -1
}

// 表按 levels 的下标取值，级别名称和桶长必须与描述一致；伪造桶长会让对齐口径偏离表的桶长。
func checkStep(lv Level, step int64) (int, error) {
	i := levelIndex(lv)
	if i < 0 || lv.Bucket != levels[i].Bucket {
		return 0, fmt.Errorf("unknown level %q with bucket %d", lv.Name, lv.Bucket)
	}
	if step < lv.Bucket || step%lv.Bucket != 0 {
		return 0, fmt.Errorf("step %d is not a multiple of the %s bucket (%d)", step, lv.Name, lv.Bucket)
	}
	return i, nil
}

func alignWindow(from, to, step int64) (int64, int64) {
	from = alignDown(from, step)
	// 用最后一秒所在桶的闭区间上界表达对齐，避免 to + step 溢出。
	last := alignDown(to-1, step)
	to = math.MaxInt64
	if last <= math.MaxInt64-(step-1) {
		to = last + step - 1
	}

	return from, to
}

// QueryProbes 与 QueryMetrics 共用级别校验与窗口对齐；每任务的桶按 TaskID、TS 升序返回。
// 额度权重取每节点任务分配上限：这是对当前配置的计数，不是历史序列上限——从节点撤下、仍存在的任务的历史照常可读，
// 已删任务的行在清理完成前也被读到（只是不出现在结果里），任务更替频繁的节点在一个窗口里可以有远多于它的序列
// （ReadQuotaError 按实际行数裁决）。seriesEstimate 是节点当前的任务数，只用来选首次池（0 退回上限）；它同样数不到这些序列，
// 偏小时由轻池的封顶计数兜住（见 queryFamily）。
func (s *Store) QueryProbes(ctx context.Context, nodeID int64, from, to int64, lv Level, step int64, seriesEstimate int) ([]metric.ProbeRow, error) {
	return s.queryProbes(ctx, requestRead, nodeID, from, to, lv, step, seriesEstimate)
}

func (s *Store) queryProbes(ctx context.Context, route readRoute, nodeID int64, from, to int64, lv Level, step int64, seriesEstimate int) ([]metric.ProbeRow, error) {
	return queryFamily(ctx, s, route, probeFamily, queryShape{keyWhere: "node_id = ?", keyArgs: []any{nodeID}, groupKey: "task_id", seriesLimit: probelimit.MaxTasksPerNode, seriesEstimate: int64(seriesEstimate)},
		from, to, lv, step,
		func(rows *sql.Rows) ([]metric.ProbeRow, error) { return scanProbeRows(rows, nodeID) }, nil)
}
