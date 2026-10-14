package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// maintenance_state 的 name 取值：清理、上卷及两层备份各记最近一次整轮成功时刻。
const (
	MaintenancePrune         = "prune"
	MaintenanceRollup        = "rollup"
	MaintenanceBackupConfig  = "backup_config"
	MaintenanceBackupMetrics = "backup_metrics"
)

// 流量报告每种周期最近一次已发的那一期也记在 maintenance_state：它与上面几项一样是"某件周期性的事做到了
// 哪里"的簿记，hub 重启读回它才不重发。这几行的 finished_at 不是时刻，而是周期键（ReportPeriod.Key 那一日的 UTC 零点
// 的 Unix 秒），只经 RecordTrafficReportEvent 写、TrafficReportSent 读；存储健康面（lastMaintenance）只读清理与上卷
// 两行，不会把周期键当成完成时刻展示。
const (
	MaintenanceTrafficReportDaily   = "traffic_report.daily"
	MaintenanceTrafficReportWeekly  = "traffic_report.weekly"
	MaintenanceTrafficReportMonthly = "traffic_report.monthly"
)

// recordMaintenance 记下 name 这一轮整轮成功完成的时刻，取写协程执行时的时钟。调用方只在整轮成功之后调用
// （Rollup、Prune 与备份上传及保留的末尾）：中途失败若也写，一直失败与正常运行读出的都是一个新鲜的时刻，读侧无法把两者分开。
func (s *Store) recordMaintenance(ctx context.Context, name string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO maintenance_state (name, finished_at) VALUES (?, ?)
			ON CONFLICT (name) DO UPDATE SET finished_at = excluded.finished_at`, name, s.clk.Now().Unix())
		return err
	})
}

// SeriesHealth 是一张时序表的健康读数，全是库里的原值，不做换算。
type SeriesHealth struct {
	Table string
	Level Level
	// Oldest 是表里最老一行的 ts（桶起点，Unix 秒）；表为空时为 nil。
	Oldest *int64
	// Watermark 是本级在 rollup_state 里的 upto_ts 原值，与上卷读写的是同一个值；最细一级不经上卷写入、
	// 没有水位，为 nil。
	Watermark *int64
}

// Staleness 是一张表的两项标红结论。
type Staleness struct {
	Oldest, Watermark bool
}

// Staleness 是存储健康的唯一判定：API 响应带上它的结论，面板只渲染、不重算，阈值因此只有这一份，不会在
// 前端与 hub 之间各改各的。保留期取调用方传入的当前配置，与 Prune 用的是同一个 Retention。
//
//   - 最老桶早于 now − 保留期 − 一个桶长 − MaintenanceInterval 即标红。Prune 的截止点是 now − 保留期向下对齐到
//     桶长，所以刚清理完时最老桶比 now − 保留期早不到一个桶长；截止点跨过桶边界之后、下一轮 prune 删掉那一桶之前，
//     还要再多等至多一个维护间隔，少了这一段每个桶长都会误报一次。越过这个阈值说明超期的行没有被清掉：prune 停了，
//     或者上卷停了（Prune 对细一级只清到粗一级的水位为止）。余量覆盖的是跨过边界到下一轮开始的等待，在每一轮都
//     在下一个周期边界之前做完时它不超过一个维护间隔；这一轮上卷、清理与已删主体的历史清理（cleanup.go，每轮至多约
//     cleanupTimePerRound）本身的耗时不在其内，耗时长时健康的表仍可能在删完之前短暂越过。前提不成立时上界不是一个间隔：RunMaintenance（rollup.go）在上一轮做完之后才用当时的
//     墙钟算 nextMaintenanceAt，某一轮跑超了 n 个周期边界，下一轮就从做完的时刻起跳过 n 个周期——每多跑超一个
//     边界，多跳过一个周期，等待最长接近 n+1 个维护间隔，健康的表也可能在其中标红。
//   - 水位落后 now 超过三个桶长即标红。上卷只推进到 now − RollupLag 向下对齐到桶长，再加至多一个维护间隔（同样
//     以每一轮不跑超为前提），健康的落后量 5m 级不到 300 + 300 + 60 秒、1h 级不到 300 + 3600 + 60 秒，都在三个
//     桶长之内。每跳过一个周期两级都多落后 60 秒；5m 级的余量最紧，跳过不超过 4 个周期时仍在三个桶长之内。
//
// 两项都是严格大于阈值才标红：恰好等于阈值不标。
func (h SeriesHealth) Staleness(now time.Time, r Retention) Staleness {
	n, bucket := now.Unix(), h.Level.Bucket
	var out Staleness
	if h.Oldest != nil {
		out.Oldest = *h.Oldest < n-int64(r.ForLevel(h.Level.Name)/time.Second)-bucket-int64(MaintenanceInterval/time.Second)
	}
	if h.Watermark != nil {
		out.Watermark = n-*h.Watermark > 3*bucket
	}
	return out
}

// seriesHealth 使用同一事务统计行数时已读出的最老桶，并按 families × levels 的顺序读取水位：按表分别给、
// 不合并，因为各级保留期不同，合并后的最老值无法与任何一级的保留期对比。
func seriesHealth(ctx context.Context, tx *sql.Tx, oldest map[string]*int64) ([]SeriesHealth, error) {
	var out []SeriesHealth
	for _, f := range families {
		for i, lv := range levels {
			h := SeriesHealth{Table: f.tables[i], Level: lv, Oldest: oldest[f.tables[i]]}
			if f.states[i] != "" {
				var upto int64
				if err := tx.QueryRowContext(ctx, "SELECT upto_ts FROM rollup_state WHERE level = ?", f.states[i]).Scan(&upto); err != nil {
					return nil, fmt.Errorf("rollup_state %s: %w", f.states[i], err)
				}
				h.Watermark = &upto
			}
			out = append(out, h)
		}
	}
	return out, nil
}

// lastMaintenance 读出存储健康面展示的两项维护任务（清理、上卷）最近一次整轮成功的完成时刻；没有行的任务不在映射里
// （从未成功跑过）。按名字取而不是整表读：表里另有备份的完成时刻与流量报告的周期键，后者不是时刻。
func lastMaintenance(ctx context.Context, tx *sql.Tx) (map[string]int64, error) {
	rows, err := tx.QueryContext(ctx, "SELECT name, finished_at FROM maintenance_state WHERE name IN (?, ?)", MaintenancePrune, MaintenanceRollup)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var name string
		var at int64
		if err := rows.Scan(&name, &at); err != nil {
			return nil, err
		}
		out[name] = at
	}
	return out, rows.Err()
}
