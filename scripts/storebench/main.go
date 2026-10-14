// Command storebench 测量 hub 存储层在目标规模下的库大小与操作耗时。实测条件：GitHub 托管 runner（ubuntu-24.04，Go 1.27.2，modernc.org/sqlite v1.59.0 / SQLite 3.53.4），500 节点、默认保留期；无探测任务的库约 3.26 GB，上卷空闲约 95 ms（0 任务）与 364 ms（4 任务），分块清理空闲约 3.7 s 与 13.5 s：
//
//	storebench gen  -db <path> [-nodes 500] [-tasks 0] [-end 2026-10-01T00:00:00Z] [-seed 1] [-container-frac 0.25] [-churn=true]
//	storebench size -db <path> [-db <path> ...] [-merge <earlier report.json> ...] [-json <file>] [-text <file>]
//	storebench time -db <path> -work <dir> [-reps 5] [-conditions idle,writes] [-ops rollup,query,prune] [-json <file>] [-text <file>]
//
// gen 只经 store 的迁移建表、经 store 的写接口建节点与任务，时序行按生产的写入与清理次序批量写入；size 用 dbstat 给出
// 每张表、每个索引的页占用；time 在库的新副本上经 store 的真实入口测上卷、窗口查询与分块清理，分空闲与并发写入两种条件。
// 耗时只在受控环境作结论；字节数不依赖 CPU。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"time"
)

func main() { os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr)) }

const usage = `usage:
  storebench gen  -db <path> [-nodes N] [-tasks T] [-end RFC3339] [-seed S] [-container-frac F] [-churn=true|false]
  storebench size -db <path> [-db <path> ...] [-merge <earlier report.json> ...] [-json <file>] [-text <file>]
  storebench time -db <path> -work <dir> [-reps R] [-conditions idle,writes] [-ops rollup,query,prune] [-write-period 1s] [-json <file>] [-text <file>]`

// defaultEnd 是固定的时间轴终点：同参数两次生成要逐字节相同，终点不能取当前时刻。
const defaultEnd = "2026-10-01T00:00:00Z"

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "gen":
		err = runGen(ctx, args[1:], stdout, stderr)
	case "size":
		err = runSize(ctx, args[1:], stdout, stderr)
	case "time":
		err = runTime(ctx, args[1:], stdout, stderr)
	default:
		fmt.Fprintln(stderr, usage)
		return 2
	}
	if errors.Is(err, flag.ErrHelp) {
		return 2
	}
	if err != nil {
		fmt.Fprintf(stderr, "storebench %s: %v\n", args[0], err)
		return 1
	}
	return 0
}

func runGen(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("gen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", "", "path of the database to create (must not exist)")
	nodes := fs.Int("nodes", 500, "number of nodes")
	tasks := fs.Int("tasks", 0, "probe tasks per node (every task is assigned to every node)")
	end := fs.String("end", defaultEnd, "end of the time axis, RFC 3339, on an hour boundary")
	seed := fs.Uint64("seed", 1, "random seed")
	containerFrac := fs.Float64("container-frac", 0.25, "fraction of container (environment-scope) nodes")
	churn := fs.Bool("churn", true, "replay a second retention period of inserts and prunes to reach the steady-state page layout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *db == "" || fs.NArg() != 0 {
		return errors.New("-db is required and no positional arguments are accepted")
	}
	e, err := time.Parse(time.RFC3339, *end)
	if err != nil {
		return err
	}
	cfg := genConfig{Nodes: *nodes, Tasks: *tasks, End: e.Unix(), Seed: *seed, ContainerFrac: *containerFrac, Churn: *churn}
	start := time.Now()
	m, err := gen(ctx, *db, cfg)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "generated %s in %s: nodes=%d tasks=%d churn=%v schema=%d\n", *db, time.Since(start).Round(time.Second), cfg.Nodes, cfg.Tasks, cfg.Churn, m.SchemaVersion)
	for _, t := range append(metricTables[:], probeTables[:]...) {
		fmt.Fprintf(stdout, "  %-10s rows=%d inserted=%d deleted=%d\n", t, m.Rows[t], m.Inserted[t], m.Deleted[t])
	}
	return nil
}

// envInfo 是测量环境的可复现事实：二进制自己知道的部分。机器层面的（CPU 型号、核数、内核、磁盘）由调用方另记
// （measure-store.yml 打印 nproc、lscpu、uname、df、磁盘类型）。
type envInfo struct {
	GoVersion     string `json:"go_version"`
	GOOS          string `json:"goos"`
	GOARCH        string `json:"goarch"`
	GOMAXPROCS    int    `json:"gomaxprocs"`
	NumCPU        int    `json:"num_cpu"`
	ModerncSQLite string `json:"modernc_sqlite"`
	SQLiteVersion string `json:"sqlite_version"`
}

func currentEnv(sqliteVersion string) envInfo {
	e := envInfo{GoVersion: runtime.Version(), GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, GOMAXPROCS: runtime.GOMAXPROCS(0), NumCPU: runtime.NumCPU(), SQLiteVersion: sqliteVersion}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, d := range bi.Deps {
			if d.Path == "modernc.org/sqlite" {
				e.ModerncSQLite = d.Version
			}
		}
	}
	return e
}

func (e envInfo) String() string {
	return fmt.Sprintf("%s %s/%s GOMAXPROCS=%d NumCPU=%d modernc.org/sqlite %s (SQLite %s)", e.GoVersion, e.GOOS, e.GOARCH, e.GOMAXPROCS, e.NumCPU, e.ModerncSQLite, e.SQLiteVersion)
}

// writeOutputs 把机器可读的 JSON 与人读摘要分别写到文件；路径为空时摘要写 stdout、JSON 不写。
func writeOutputs(jsonPath, textPath string, v any, text string, stdout io.Writer) error {
	if jsonPath != "" {
		raw, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(jsonPath, append(raw, '\n'), 0o644); err != nil {
			return err
		}
	}
	if textPath != "" {
		if err := os.WriteFile(textPath, []byte(text), 0o644); err != nil {
			return err
		}
	}
	_, err := io.WriteString(stdout, text)
	return err
}

// listFlag 收集可重复的 -db。
type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }
