package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/xjetry/heron/internal/agentwire"
)

// TrafficCounter 是差分基线的一项：Name 为空是合计形态的唯一一项，非空是一张网卡。计数是入账时截到 MaxInt64 的值，
// 与 SQLite INTEGER 同宽，非负。
type TrafficCounter struct {
	Name   string
	Rx, Tx int64
}

// TrafficRecord 是 traffic 表的一行加上它在 traffic_baseline 表里的基线行。基线（BootID、NetCounterEpoch、Baseline）
// 与累计值必须同一事务写入：只要 agent 未换启动周期、计数未倒退（合计形态另要求集合摘要不变），崩溃后下一次上报
// 相对已落盘基线做差分能补回丢失的内存增量；若两者分开落盘，基线新、累计旧的一次崩溃就会永久少记一段。
//
// Baseline 恰是三种形态之一（CheckTrafficBaseline）：nil 表示没有基线；一项且名称为空是合计形态，NetCounterEpoch
// 是它的集合摘要；其余是逐网卡形态，按名称字节序、名称唯一，NetCounterEpoch 为空。
type TrafficRecord struct {
	NodeID                               int64
	BootID                               string
	NetCounterEpoch                      string
	Baseline                             []TrafficCounter
	TotalRx, TotalTx, PeriodRx, PeriodTx int64
	PeriodStart                          time.Time
}

// CheckTrafficBaseline 是三种基线形态的唯一判定，写库、读库与恢复预检共用：
//   - 没有基线：零项，摘要为空。旧库里校正建立、last_rx = last_tx = -1 的行迁移成这种形态。
//   - 合计形态：恰一项、名称为空，摘要为空或合法的 SHA-256 小写十六进制。
//   - 逐网卡形态：一到 agentwire.MaxNetInterfaces 项，名称经 agentwire.ValidateInterfaceName 准入、按字节序严格递增，
//     摘要为空——集合由名称本身给出，合计视图按 agentwire.SummarizeNetInterfaces 推导，不另存一份可能不一致的摘要。
//
// 计数都必须非负。形态混用（空名与网卡名并存、逐网卡带摘要）会让账本对同一份基线有两种读法，一律拒收。
func CheckTrafficBaseline(epoch string, baseline []TrafficCounter) error {
	if err := agentwire.ValidateCounterEpoch(epoch); err != nil {
		return err
	}
	for _, c := range baseline {
		if c.Rx < 0 || c.Tx < 0 {
			return fmt.Errorf("traffic baseline: counters must be non-negative")
		}
	}
	switch {
	case len(baseline) == 0:
		if epoch != "" {
			return fmt.Errorf("traffic baseline: net_counter_epoch must be empty without a baseline")
		}
	case len(baseline) == 1 && baseline[0].Name == "":
	default:
		if len(baseline) > agentwire.MaxNetInterfaces {
			return fmt.Errorf("traffic baseline: at most %d interfaces; got %d", agentwire.MaxNetInterfaces, len(baseline))
		}
		if epoch != "" {
			return fmt.Errorf("traffic baseline: net_counter_epoch must be empty for a per-interface baseline")
		}
		for i, c := range baseline {
			if err := agentwire.ValidateInterfaceName(c.Name); err != nil {
				return fmt.Errorf("traffic baseline: interface %w", err)
			}
			if i > 0 && baseline[i-1].Name >= c.Name {
				return fmt.Errorf("traffic baseline: interface names must be unique and sorted by bytes")
			}
		}
	}
	return nil
}

const upsertTraffic = `INSERT INTO traffic (node_id, boot_id, total_rx, total_tx, period_rx, period_tx, period_start, updated_at, net_counter_epoch)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (node_id) DO UPDATE SET boot_id = excluded.boot_id,
  total_rx = excluded.total_rx, total_tx = excluded.total_tx, period_rx = excluded.period_rx, period_tx = excluded.period_tx,
  period_start = excluded.period_start, updated_at = excluded.updated_at, net_counter_epoch = excluded.net_counter_epoch`

// WriteTraffic 在一个事务里写入全部记录，并返回实际写入的节点 ID。每条记录整体替换该节点的基线行。节点已删除的
// 记录跳过：删除后迟到的刷出不得重建从属行，与 WriteMinuteBatch 同一处理；存在性在写事务内判定，与 DeleteNode 串行。
// 任一记录的基线形态不合法则整批不写。
func (s *Store) WriteTraffic(ctx context.Context, recs []TrafficRecord) ([]int64, error) {
	var written []int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		written = nil
		now := s.clk.Now().Unix()
		for _, r := range recs {
			if err := CheckTrafficBaseline(r.NetCounterEpoch, r.Baseline); err != nil {
				return fmt.Errorf("node %d: %w", r.NodeID, err)
			}
			exists, err := nodeExistsTx(tx, r.NodeID)
			if err != nil {
				return err
			}
			if !exists {
				continue
			}
			if _, err := tx.Exec(upsertTraffic, r.NodeID, r.BootID, r.TotalRx, r.TotalTx,
				r.PeriodRx, r.PeriodTx, r.PeriodStart.Unix(), now, r.NetCounterEpoch); err != nil {
				return err
			}
			if _, err := tx.Exec("DELETE FROM traffic_baseline WHERE node_id = ?", r.NodeID); err != nil {
				return err
			}
			for _, c := range r.Baseline {
				if _, err := tx.Exec("INSERT INTO traffic_baseline (node_id, iface, rx, tx) VALUES (?, ?, ?, ?)", r.NodeID, c.Name, c.Rx, c.Tx); err != nil {
					return err
				}
			}
			written = append(written, r.NodeID)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return written, nil
}

// LoadTraffic 读出全部记录。两次查询在同一个读事务里，基线行与 traffic 行来自同一次提交。基线形态不合法、或基线行
// 没有对应的 traffic 行时报错而不是丢弃：账本会从不合法的基线算出错误的增量，宁可让 hub 起不来也不静默放过。
func (s *Store) LoadTraffic(ctx context.Context) ([]TrafficRecord, error) {
	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	baselines, err := readTrafficBaselines(ctx, tx, "main")
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT node_id, boot_id, total_rx, total_tx, period_rx, period_tx, period_start, net_counter_epoch FROM traffic`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TrafficRecord
	for rows.Next() {
		var r TrafficRecord
		var start int64
		if err := rows.Scan(&r.NodeID, &r.BootID, &r.TotalRx, &r.TotalTx, &r.PeriodRx, &r.PeriodTx, &start, &r.NetCounterEpoch); err != nil {
			return nil, err
		}
		r.Baseline = baselines[r.NodeID]
		delete(baselines, r.NodeID)
		if err := CheckTrafficBaseline(r.NetCounterEpoch, r.Baseline); err != nil {
			return nil, fmt.Errorf("node %d: %w", r.NodeID, err)
		}
		r.PeriodStart = time.Unix(start, 0).UTC()
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := orphanBaselines(baselines); err != nil {
		return nil, err
	}
	return out, nil
}

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// readTrafficBaselines 按节点读出 schema 里 traffic_baseline 的全部行，每个节点的行按名称字节序（空名排最前）。
func readTrafficBaselines(ctx context.Context, q queryer, schema string) (map[int64][]TrafficCounter, error) {
	rows, err := q.QueryContext(ctx, "SELECT node_id, iface, rx, tx FROM "+schema+".traffic_baseline ORDER BY node_id, iface")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]TrafficCounter{}
	for rows.Next() {
		var id int64
		var c TrafficCounter
		if err := rows.Scan(&id, &c.Name, &c.Rx, &c.Tx); err != nil {
			return nil, err
		}
		out[id] = append(out[id], c)
	}
	return out, rows.Err()
}

// orphanBaselines 报告取走全部 traffic 行之后剩下的基线行：WriteTraffic 与迁移总是连同 traffic 行一起写，
// 删除节点与恢复的孤儿清理按 nodeConfigTables 一起删，剩下的只能来自手工改库或损坏的快照。
func orphanBaselines(left map[int64][]TrafficCounter) error {
	for id := range left {
		return fmt.Errorf("node %d: traffic baseline rows without a traffic row", id)
	}
	return nil
}

// TrafficResetDays 返回每个节点的周期重置日。重置日住在 node 表：它是节点的配置，
// 在节点建好、尚无一次上报时就该可改，而 traffic 行只在有流量状态后才存在。
func (s *Store) TrafficResetDays(ctx context.Context) (map[int64]int, error) {
	rows, err := s.r.QueryContext(ctx, "SELECT id, traffic_reset_day FROM node")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var id int64
		var day int
		if err := rows.Scan(&id, &day); err != nil {
			return nil, err
		}
		out[id] = day
	}
	return out, rows.Err()
}
