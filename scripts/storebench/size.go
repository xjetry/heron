package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
)

// objectSize 是 dbstat 对一张表或一个索引的汇总：页数与页字节（含溢出页）、有效载荷、页内未用字节、单元格数。
// WITHOUT ROWID 表本身就是按主键组织的索引式 B 树，没有另外的主键索引；每行恰好是一个单元格，在叶页或内部页里。
type objectSize struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Table    string `json:"table"`
	Rows     int64  `json:"rows,omitempty"`
	Pages    int64  `json:"pages"`
	Bytes    int64  `json:"bytes"`
	Payload  int64  `json:"payload"`
	Unused   int64  `json:"unused"`
	Cells    int64  `json:"cells"`
	BytesRow int64  `json:"bytes_per_row,omitempty"`
}

type dbSize struct {
	Path string `json:"path"`
	// Env 随每份库记录：-merge 汇入的库可能是在另一台机器上量的。
	Env           envInfo      `json:"env"`
	Manifest      *genManifest `json:"manifest,omitempty"`
	FileBytes     int64        `json:"file_bytes"`
	WALBytes      *int64       `json:"wal_bytes"`
	PageSize      int64        `json:"page_size"`
	PageCount     int64        `json:"page_count"`
	FreelistCount int64        `json:"freelist_count"`
	DBBytes       int64        `json:"db_bytes"`
	Objects       []objectSize `json:"objects"`
	// MetricRows 是三张指标表的行数和；DBBytesPerMetricRow = DBBytes / MetricRows，与生产的 dbBytes / 指标三表行数
	// 同一口径（分子含其余全部表、探测表与 freelist），MetricBytesPerRow 只计三张指标表自己的页。
	MetricRows          int64 `json:"metric_rows"`
	MetricBytes         int64 `json:"metric_bytes"`
	DBBytesPerMetricRow int64 `json:"db_bytes_per_metric_row"`
	MetricBytesPerRow   int64 `json:"metric_bytes_per_row"`
	// ProbeBytes 是三张探测表与它们的 _by_task 索引的页字节和，ProbeIndexBytes 只计三个索引。
	ProbeBytes      int64 `json:"probe_bytes"`
	ProbeIndexBytes int64 `json:"probe_index_bytes"`
	// Columns 是时序表逐列的记录字节核算（抽样节点），解释每行字节花在哪些列上。
	Columns []tableColumns `json:"columns"`
}

// tableColumns 是一张时序表在抽样行上的记录构成：记录 = 头（头长 varint + 每列一个类型码 varint）+ 各列的值。
// 加上单元格开销（载荷长度 varint + 2 字节单元格指针）与页内未用字节，才是 dbstat 的页字节。
type tableColumns struct {
	Table       string        `json:"table"`
	SampleNodes int           `json:"sample_nodes"`
	SampledRows int64         `json:"sampled_rows"`
	RecordBytes int64         `json:"record_bytes"`
	AvgHeader   float64       `json:"avg_header_bytes"`
	AvgRecord   float64       `json:"avg_record_bytes"`
	Columns     []columnBytes `json:"columns"`
}

type columnBytes struct {
	Column   string  `json:"column"`
	AvgBytes float64 `json:"avg_value_bytes"`
	// ZeroFrac 是值占 0 字节（NULL、整数 0 或 1、可按整数存的 0.0 / 1.0）的行比例。
	ZeroFrac float64 `json:"zero_width_frac"`
	NullFrac float64 `json:"null_frac"`
}

type probeIncrement struct {
	From, To   string
	Nodes      int
	TasksFrom  int
	TasksTo    int
	ProbeDelta int64
	// PerNodeTask 是探测表（含索引）每增加一个 (节点, 任务) 对的字节数；DBPerNodeTask 是整库的同一增量。
	PerNodeTask   float64
	DBPerNodeTask float64
}

type sizeReport struct {
	Databases  []dbSize         `json:"databases"`
	Increments []probeIncrement `json:"probe_increments,omitempty"`
}

func runSize(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("size", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var dbs listFlag
	fs.Var(&dbs, "db", "database to measure (repeatable; same node count with different task counts yields per-task increments)")
	var merge listFlag
	fs.Var(&merge, "merge", "earlier size report (JSON) whose databases join the per-task increments (repeatable; lets the databases be measured one at a time)")
	jsonPath := fs.String("json", "", "write the machine-readable report here")
	textPath := fs.String("text", "", "write the human-readable summary here (also printed)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(dbs)+len(merge) == 0 || fs.NArg() != 0 {
		return errors.New("at least one -db or -merge is required and no positional arguments are accepted")
	}
	var rep sizeReport
	for _, p := range merge {
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		var earlier sizeReport
		if err := json.Unmarshal(raw, &earlier); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		rep.Databases = append(rep.Databases, earlier.Databases...)
	}
	for _, p := range dbs {
		s, version, err := measureSize(ctx, p)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		s.Env = currentEnv(version)
		rep.Databases = append(rep.Databases, s)
	}
	rep.Increments = probeIncrements(rep.Databases)
	return writeOutputs(*jsonPath, *textPath, rep, rep.text(), stdout)
}

// measureSize 先 stat 文件再打开：打开会建出 -wal / -shm，先读才是库在交给 size 时的状态。
func measureSize(ctx context.Context, path string) (dbSize, string, error) {
	out := dbSize{Path: path}
	info, err := os.Stat(path)
	if err != nil {
		return out, "", err
	}
	out.FileBytes = info.Size()
	switch info, err := os.Stat(path + "-wal"); {
	case err == nil:
		n := info.Size()
		out.WALBytes = &n
	case !errors.Is(err, fs.ErrNotExist):
		return out, "", err
	}
	if m, err := readManifest(path); err == nil {
		out.Manifest = &m
	} else if !errors.Is(err, fs.ErrNotExist) {
		return out, "", err
	}
	db, err := sql.Open("sqlite", sqliteDSN(path, "_pragma=busy_timeout(5000)&_pragma=query_only(1)"))
	if err != nil {
		return out, "", err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var version string
	for q, dst := range map[string]any{"SELECT sqlite_version()": &version, "PRAGMA page_size": &out.PageSize, "PRAGMA page_count": &out.PageCount, "PRAGMA freelist_count": &out.FreelistCount} {
		if err := db.QueryRowContext(ctx, q).Scan(dst); err != nil {
			return out, "", fmt.Errorf("%s: %w", q, err)
		}
	}
	out.DBBytes = out.PageCount * out.PageSize
	// dbstat 是 SQLite 的编译期可选虚表；modernc.org/sqlite 带 SQLITE_ENABLE_DBSTAT_VTAB 编译，没有它这里直接报错，
	// 不退回估算。sqlite_schema 自身（第 1 页）也是一个对象，计入 Objects，使各对象页数之和 + freelist = page_count。
	rows, err := db.QueryContext(ctx, `SELECT s.name, coalesce(m.type, 'table'), coalesce(m.tbl_name, s.name), count(*), sum(s.pgsize), sum(s.payload), sum(s.unused), sum(s.ncell)
		FROM dbstat s LEFT JOIN sqlite_schema m ON m.name = s.name GROUP BY s.name ORDER BY sum(s.pgsize) DESC, s.name`)
	if err != nil {
		return out, "", fmt.Errorf("dbstat: %w", err)
	}
	for rows.Next() {
		var o objectSize
		if err := rows.Scan(&o.Name, &o.Type, &o.Table, &o.Pages, &o.Bytes, &o.Payload, &o.Unused, &o.Cells); err != nil {
			rows.Close()
			return out, "", err
		}
		out.Objects = append(out.Objects, o)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return out, "", err
	}
	var pages int64
	for i := range out.Objects {
		o := &out.Objects[i]
		pages += o.Pages
		if o.Type != "table" || strings.HasPrefix(o.Name, "sqlite_") {
			continue
		}
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM "`+strings.ReplaceAll(o.Name, `"`, `""`)+`"`).Scan(&o.Rows); err != nil {
			return out, "", err
		}
		if o.Rows > 0 {
			o.BytesRow = o.Bytes / o.Rows
		}
	}
	if pages+out.FreelistCount+lockBytePages(out.PageCount, out.PageSize) != out.PageCount {
		return out, "", fmt.Errorf("dbstat accounts for %d pages + %d free + %d lock-byte, page_count is %d", pages, out.FreelistCount, lockBytePages(out.PageCount, out.PageSize), out.PageCount)
	}
	for _, o := range out.Objects {
		switch {
		case slices.Contains(metricTables[:], o.Name):
			out.MetricRows += o.Rows
			out.MetricBytes += o.Bytes
		case slices.Contains(probeTables[:], o.Table):
			out.ProbeBytes += o.Bytes
			if o.Type == "index" {
				out.ProbeIndexBytes += o.Bytes
			}
		}
	}
	if out.MetricRows > 0 {
		out.DBBytesPerMetricRow = out.DBBytes / out.MetricRows
		out.MetricBytesPerRow = out.MetricBytes / out.MetricRows
	}
	for _, t := range append(metricTables[:], probeTables[:]...) {
		tc, err := recordColumns(ctx, db, t, columnSampleNodes)
		if err != nil {
			return out, "", fmt.Errorf("column accounting for %s: %w", t, err)
		}
		out.Columns = append(out.Columns, tc)
	}
	return out, version, nil
}

// columnSampleNodes 是逐列核算抽样的节点数，按 id 顺序等间隔取：节点的取值剖面各不相同，抽一个节点代表不了整表；
// 只取 id 最小的那些会低估 node_id 列（id ≤ 127 占 1 字节，更大的占 2 字节）。节点不多于它时全部计入。
const columnSampleNodes = 20

// recordColumns 按 SQLite 的记录格式核算 table 在抽样节点上的每列字节。宽度规则（valueWidth）在 modernc.org/sqlite
// v1.59.0（SQLite 3.53.4）上实测过：整数 0 与 1 占 0 字节，其余按量级 1/2/3/4/6/8；REAL 列里绝对值小于 2^47 的整值按
// 整数存，其余 8 字节。
func recordColumns(ctx context.Context, db *sql.DB, table string, sampleNodes int) (tableColumns, error) {
	out := tableColumns{Table: table}
	all, err := scanInt64s(db.QueryContext(ctx, "SELECT id FROM node ORDER BY id"))
	if err != nil {
		return out, err
	}
	var ids []string
	for i := range min(sampleNodes, len(all)) {
		ids = append(ids, strconv.FormatInt(all[i*len(all)/min(sampleNodes, len(all))], 10))
	}
	out.SampleNodes = len(ids)
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := db.QueryContext(ctx, "SELECT * FROM "+table+" WHERE node_id IN ("+strings.Join(ids, ",")+")")
	if err != nil {
		return out, err
	}
	defer rows.Close()
	names, err := rows.Columns()
	if err != nil {
		return out, err
	}
	types, err := rows.ColumnTypes()
	if err != nil {
		return out, err
	}
	real := make([]bool, len(types))
	for i, t := range types {
		real[i] = strings.EqualFold(t.DatabaseTypeName(), "REAL")
	}
	vals := make([]any, len(names))
	ptrs := make([]any, len(names))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	bytes := make([]int64, len(names))
	zero := make([]int64, len(names))
	nulls := make([]int64, len(names))
	var header int64
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return out, err
		}
		h, values := 0, 0
		for i, v := range vals {
			w, serial := valueWidth(v, real[i])
			bytes[i] += int64(w)
			values += w
			if w == 0 {
				zero[i]++
			}
			if v == nil {
				nulls[i]++
			}
			h += varintLen(serial)
		}
		hdr := int64(h + varintLen(int64(h+1)))
		header += hdr
		out.RecordBytes += hdr + int64(values)
		out.SampledRows++
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	if out.SampledRows == 0 {
		return out, nil
	}
	n := float64(out.SampledRows)
	out.AvgHeader = float64(header) / n
	out.AvgRecord = out.AvgHeader
	for i, name := range names {
		c := columnBytes{Column: name, AvgBytes: float64(bytes[i]) / n, ZeroFrac: float64(zero[i]) / n, NullFrac: float64(nulls[i]) / n}
		out.AvgRecord += c.AvgBytes
		out.Columns = append(out.Columns, c)
	}
	return out, nil
}

// valueWidth 返回一个值在记录里的字节数与类型码。
func valueWidth(v any, realColumn bool) (int, int64) {
	switch x := v.(type) {
	case nil:
		return 0, 0
	case int64:
		return intWidth(x)
	case float64:
		if realColumn && x == math.Trunc(x) && math.Abs(x) < 1<<47 {
			return intWidth(int64(x))
		}
		return 8, 7
	case []byte:
		return len(x), int64(12 + 2*len(x))
	case string:
		return len(x), int64(13 + 2*len(x))
	}
	panic(fmt.Sprintf("unexpected value type %T", v))
}

func intWidth(x int64) (int, int64) {
	switch {
	case x == 0:
		return 0, 8
	case x == 1:
		return 0, 9
	case x >= -128 && x <= 127:
		return 1, 1
	case x >= -32768 && x <= 32767:
		return 2, 2
	case x >= -8388608 && x <= 8388607:
		return 3, 3
	case x >= -1<<31 && x <= 1<<31-1:
		return 4, 4
	case x >= -1<<47 && x <= 1<<47-1:
		return 6, 5
	}
	return 8, 6
}

func varintLen(v int64) int {
	n := 1
	for u := uint64(v); u >= 0x80; u >>= 7 {
		n++
	}
	return n
}

func scanInt64s(rows *sql.Rows, err error) ([]int64, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// lockBytePages 是文件里锁字节页的个数：文件超过 2^30 字节时，偏移 2^30 起的那一页留给文件锁，不属于任何对象、
// 也不在 freelist，却计入 page_count。
func lockBytePages(pageCount, pageSize int64) int64 {
	if pageCount > (1<<30)/pageSize {
		return 1
	}
	return 0
}

// probeIncrements 把除任务数之外生成参数全同的库归为一组，组内按任务数升序两两相邻求增量。线性与否看各段 PerNodeTask
// 是否相等，不预设：页填充随行数变化，增量可以不是常数。
func probeIncrements(dbs []dbSize) []probeIncrement {
	groups := map[genConfig][]dbSize{}
	var keys []genConfig
	for _, d := range dbs {
		if d.Manifest == nil {
			continue
		}
		k := d.Manifest.Config
		k.Tasks = 0
		if _, ok := groups[k]; !ok {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], d)
	}
	var out []probeIncrement
	for _, k := range keys {
		g := groups[k]
		slices.SortStableFunc(g, func(a, b dbSize) int { return a.Manifest.Config.Tasks - b.Manifest.Config.Tasks })
		for i := 1; i < len(g); i++ {
			a, b := g[i-1], g[i]
			ta, tb := a.Manifest.Config.Tasks, b.Manifest.Config.Tasks
			if ta == tb {
				continue
			}
			pairs := float64(k.Nodes * (tb - ta))
			out = append(out, probeIncrement{From: a.Path, To: b.Path, Nodes: k.Nodes, TasksFrom: ta, TasksTo: tb,
				ProbeDelta: b.ProbeBytes - a.ProbeBytes, PerNodeTask: float64(b.ProbeBytes-a.ProbeBytes) / pairs, DBPerNodeTask: float64(b.DBBytes-a.DBBytes) / pairs})
		}
	}
	return out
}

func (r sizeReport) text() string {
	var b strings.Builder
	for i, d := range r.Databases {
		if i > 0 {
			fmt.Fprintln(&b)
		}
		fmt.Fprintf(&b, "%s\n  environment: %s\n", d.Path, d.Env)
		if m := d.Manifest; m != nil {
			fmt.Fprintf(&b, "  generated: nodes=%d tasks=%d seed=%d end=%d churn=%v container_frac=%v schema=%d\n", m.Config.Nodes, m.Config.Tasks, m.Config.Seed, m.Config.End, m.Config.Churn, m.Config.ContainerFrac, m.SchemaVersion)
		}
		wal := "absent"
		if d.WALBytes != nil {
			wal = fmt.Sprint(*d.WALBytes)
		}
		fmt.Fprintf(&b, "  file=%d B  -wal=%s  page_size=%d  page_count=%d  freelist=%d  page_count×page_size=%d B (%.1f MiB)\n",
			d.FileBytes, wal, d.PageSize, d.PageCount, d.FreelistCount, d.DBBytes, float64(d.DBBytes)/(1<<20))
		fmt.Fprintf(&b, "  metric rows=%d  metric tables=%d B (%d B/row)  db_bytes/metric_row=%d B  probe tables+indexes=%d B (indexes %d B)\n",
			d.MetricRows, d.MetricBytes, d.MetricBytesPerRow, d.DBBytesPerMetricRow, d.ProbeBytes, d.ProbeIndexBytes)
		w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', tabwriter.AlignRight)
		fmt.Fprintln(w, "  object\ttype\trows\tpages\tbytes\tB/row\tpayload\tunused\t")
		for _, o := range d.Objects {
			if o.Bytes < 64*d.PageSize && !strings.HasPrefix(o.Name, "metric_") && !strings.HasPrefix(o.Name, "probe_") {
				continue
			}
			fmt.Fprintf(w, "  %s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t\n", o.Name, o.Type, o.Rows, o.Pages, o.Bytes, o.BytesRow, o.Payload, o.Unused)
		}
		w.Flush()
		fmt.Fprintln(&b, "  (objects under 64 pages other than metric_* / probe_* are omitted here; the JSON lists all)")
		for _, tc := range d.Columns {
			if tc.SampledRows == 0 {
				continue
			}
			fmt.Fprintf(&b, "  %s record (sampled %d rows of %d nodes): %.1f B = header %.1f B + values %.1f B\n", tc.Table, tc.SampledRows, tc.SampleNodes, tc.AvgRecord, tc.AvgHeader, tc.AvgRecord-tc.AvgHeader)
			var parts []string
			for _, c := range tc.Columns {
				parts = append(parts, fmt.Sprintf("%s=%.1f", c.Column, c.AvgBytes))
			}
			fmt.Fprintf(&b, "    %s\n", strings.Join(parts, " "))
		}
	}
	for _, inc := range r.Increments {
		fmt.Fprintf(&b, "\nprobe increment N=%d, T %d→%d: probe tables+indexes %+d B = %.1f B per node per task (whole db %.1f B)\n",
			inc.Nodes, inc.TasksFrom, inc.TasksTo, inc.ProbeDelta, inc.PerNodeTask, inc.DBPerNodeTask)
	}
	return b.String()
}
