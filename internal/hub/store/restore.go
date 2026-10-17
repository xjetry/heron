package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

type RestoreResult struct {
	RestoredAt     int64               `json:"restored_at"`
	ConfigTakenAt  int64               `json:"config_taken_at"`
	MetricsTakenAt *int64              `json:"metrics_taken_at"`
	Orphans        map[string]int64    `json:"orphans"`
	Themes         ThemeRestoreSummary `json:"themes"`
}

// Restore 只供停止 hub 后的离线入口使用：直接覆写库不会同步运行中 hub 的设置缓存与鉴权索引。
// 不探测 WAL 写者来声称已停机；是否已停 hub 由命令行的显式确认承担。
func Restore(ctx context.Context, path, config, metrics, themesDir string, now time.Time, log *slog.Logger) (result RestoreResult, err error) {
	if config == "" {
		return result, errors.New("config snapshot is required")
	}
	sources := []restoreSource{{"config", config, configSnapshotTables}}
	if metrics != "" {
		sources = append(sources, restoreSource{"metrics", metrics, metricsSnapshotTables})
	}
	prepared, cleanup, err := prepareRestoreSources(ctx, sources)
	if err != nil {
		return result, err
	}
	defer cleanup()
	sources = prepared
	config = sources[0].path
	if err := preflightRestoreSources(ctx, path, sources); err != nil {
		return result, err
	}
	themes, err := prepareThemeRestore(ctx, config, themesDir)
	if err != nil {
		return result, err
	}
	// 独占创建成功才拥有失败清理权；预检后的 Stat 不能证明文件由本次恢复创建。
	f, createErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if createErr != nil && !errors.Is(createErr, os.ErrExist) {
		return result, createErr
	}
	created, committed := createErr == nil, false
	defer func() {
		// 后注册的事务回滚和连接关闭先执行；提交成功后即使 Close 报错也不能删掉恢复结果。
		if created && !committed {
			for _, suffix := range []string{"-wal", "-shm", ""} {
				if e := os.Remove(path + suffix); e != nil && !errors.Is(e, os.ErrNotExist) {
					err = errors.Join(err, e)
				}
			}
		}
	}()
	if created {
		if err = f.Close(); err != nil {
			return result, err
		}
	}
	db, err := sql.Open("sqlite", dsn(path, "&mode=rw"+writeTxLock))
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	db.SetMaxOpenConns(1)
	if err = attachRestoreSources(ctx, db, sources); err != nil {
		return result, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	// 两次校验之间目标或来源可能变化；在写事务内重新按目标校验，并固定来源的读取视图。
	takenAt, err := validateRestoreSources(ctx, tx, sources)
	if err != nil {
		return result, err
	}
	result.ConfigTakenAt = takenAt["config"]
	if at, ok := takenAt["metrics"]; ok {
		result.MetricsTakenAt = &at
	}
	action, _, err := schemaAdmission(ctx, tx, RequireCurrentSchema)
	if err != nil {
		return result, fmt.Errorf("open database %s: %w", path, err)
	}
	if action == schemaCreate {
		if err = createSchema(ctx, tx); err != nil {
			return result, err
		}
	}
	// 配置恢复改变账户凭据；会话不在快照中，也不能沿用目标数据库已有的授权。
	if _, err = tx.ExecContext(ctx, "DELETE FROM main.admin_session"); err != nil {
		return result, err
	}
	// 更新授权不随配置快照恢复；目标库残留的排队任务也必须撤下。
	if _, err = tx.ExecContext(ctx, "DELETE FROM main.node_update"); err != nil {
		return result, err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM main.register_window"); err != nil {
		return result, err
	}
	// 离线变更代数只对读它的那个 hub 进程有意义，恢复要求 hub 已停止，下次启动的 hub 从库里读起点；重新种子为 0
	// 同时修复目标库里缺失或损坏的那一行，不让它挡住之后的启动。
	if _, err = tx.ExecContext(ctx, "DELETE FROM main.hub_coordination"); err != nil {
		return result, err
	}
	if _, err = tx.ExecContext(ctx, seedHubCoordination); err != nil {
		return result, err
	}
	// 节点资料版本同样不进备份。目标库缺这一行时补种：nodeListRevision 把缺行当库损坏，触发器对缺行的 UPDATE 又不报错地
	// 什么都不改，不补就一直缺。已有的行保留计数而不归零：下面对 node 等表的整表搬运经触发器在它之上推进，库里的版本
	// 仍守 ddlNodeListRevision 的"严格增大"，不靠 hub 重启换纪元来兜（TestRestoreAdvancesNodeListRevision）。补种放在
	// 搬运之前，缺行时搬运的推进也落在补出来的这一行上。
	if _, err = tx.ExecContext(ctx, seedNodeListRevision+" ON CONFLICT (id) DO NOTHING"); err != nil {
		return result, err
	}
	for _, src := range sources {
		for _, table := range src.tables {
			columns, e := restoreColumnList(ctx, tx, table)
			if e != nil {
				return result, e
			}
			insert := "INSERT INTO "
			if table == "restore_record" || table == "operation" {
				// 审计事件以创建时的 id 标识，回退配置不能删除目标已有的历史，重复快照也不重复记账。
				insert = "INSERT OR IGNORE INTO "
			} else {
				if _, err = tx.ExecContext(ctx, "DELETE FROM main."+table); err != nil {
					return result, fmt.Errorf("restore %s.%s: %w", src.layer, table, err)
				}
			}
			if _, err = tx.ExecContext(ctx, insert+"main."+table+" ("+columns+") SELECT "+columns+" FROM "+src.layer+"."+table); err != nil {
				return result, fmt.Errorf("restore %s.%s: %w", src.layer, table, err)
			}
		}
	}
	sequenceSources := []string{"main", "config"}
	result.Themes, err = restoreThemeContent(ctx, tx, themes)
	if err != nil {
		return result, err
	}
	if metrics != "" {
		sequenceSources = append(sequenceSources, "metrics")
	}
	if err = restoreSequences(ctx, tx, sequenceSources); err != nil {
		return result, err
	}
	// 清单计数在备份恢复后可以重新走到 agent 已经持有的值，而那份清单的内容不同。
	// 推进一次让仍按计数对账的 agent 重取；新 agent 另有内容摘要，不依赖这一次推进。
	if _, err = bumpProbeVersion(tx, now.Unix()); err != nil {
		return result, err
	}
	// node 来自配置快照，始终由它决定哪些节点存在，与两层时刻的先后无关。
	// schema 没有级联外键，整表替换也不能表达跨层清理；逐表显式删除并记录数量。
	// 两份清单与 DeleteNode 共用，审计历史的排除口径不在恢复侧另列。时序行在这里同步删而不是登记作业：
	// 节点可能在配置快照与指标快照之间被删且当时的清理已完成，配置层里没有它的作业，不能指望作业兜底。
	result.Orphans = make(map[string]int64)
	for _, table := range append(slices.Clone(nodeConfigTables), nodeHistoryTables...) {
		res, e := tx.ExecContext(ctx, "DELETE FROM main."+table+" WHERE NOT EXISTS (SELECT 1 FROM main.node WHERE id = "+table+".node_id)")
		if e != nil {
			return result, e
		}
		count, e := res.RowsAffected()
		if e != nil {
			return result, e
		}
		result.Orphans[table] = count
	}
	// 任务同样以配置层为准：指标层里配置快照没有的任务（在两次快照之间建或删）的行是孤儿。它们量大且读侧已按
	// probe_task 过滤，所以登记作业交给维护循环，不在恢复事务里删。配置快照自带的作业已随 cleanup_job 整表恢复。
	if err = enqueueOrphanTaskCleanup(ctx, tx, now.Unix()); err != nil {
		return result, err
	}
	result.RestoredAt = now.Unix()
	orphans, err := json.Marshal(result.Orphans)
	if err != nil {
		return result, err
	}
	themeSummary, err := json.Marshal(result.Themes)
	if err != nil {
		return result, err
	}
	var id [16]byte
	if _, err = rand.Read(id[:]); err != nil {
		return result, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO main.restore_record (id,restored_at,config_taken_at,metrics_taken_at,orphans,themes) VALUES (?,?,?,?,?,?)",
		hex.EncodeToString(id[:]), result.RestoredAt, result.ConfigTakenAt, result.MetricsTakenAt, string(orphans), string(themeSummary)); err != nil {
		return result, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO main.alert_event (rule_id,node_id,transition,at,summary,value) VALUES (0,0,?,?,'已从备份恢复配置',0)", TransitionBackupRestored, result.RestoredAt); err != nil {
		return result, err
	}
	err = tx.Commit()
	committed = err == nil
	if err == nil && action == schemaCreate {
		logSchemaCreated(log)
	}
	return result, err
}

type restoreSource struct {
	layer, path string
	tables      []string
}

// 每层快照不是完整数据库，不能直接重放所有 live 迁移。这里只列出已审定的分层迁移，
// 新版本必须补上各层投影；版本不在清单内时拒绝恢复，不猜测缺失列的默认含义。
func prepareRestoreSources(ctx context.Context, sources []restoreSource) (prepared []restoreSource, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "heron-restore-*")
	if err != nil {
		return nil, nil, err
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	defer func() {
		if err != nil {
			cleanup()
		}
	}()
	for _, src := range sources {
		db, e := sql.Open("sqlite", dsn(src.path, "&mode=ro"))
		if e != nil {
			return nil, cleanup, e
		}
		db.SetMaxOpenConns(1)
		var count, version int
		e = db.QueryRowContext(ctx, "SELECT count(*),coalesce(max(schema_version),0) FROM snapshot_meta").Scan(&count, &version)
		if e != nil {
			db.Close()
			return nil, cleanup, fmt.Errorf("%s snapshot metadata: %w", src.layer, e)
		}
		if count != 1 || version < 17 || version > schemaVersion {
			db.Close()
			return nil, cleanup, fmt.Errorf("%s snapshot metadata: rows=%d schema_version=%d; supported schema versions 17..%d", src.layer, count, version, schemaVersion)
		}
		copyPath := filepath.Join(dir, src.layer+".db")
		e = copySnapshot(ctx, db, copyPath)
		e = errors.Join(e, db.Close())
		if e != nil {
			return nil, cleanup, e
		}
		if e := os.Chmod(copyPath, 0600); e != nil {
			return nil, cleanup, e
		}
		copyDB, e := sql.Open("sqlite", dsn(copyPath, "&mode=rw"))
		if e != nil {
			return nil, cleanup, e
		}
		var sequences int
		e = copyDB.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema WHERE type='table' AND name='sqlite_sequence'").Scan(&sequences)
		if e != nil || sequences != 1 {
			copyDB.Close()
			if e != nil {
				return nil, cleanup, e
			}
			return nil, cleanup, fmt.Errorf("%s snapshot missing table sqlite_sequence", src.layer)
		}
		e = copyDB.QueryRowContext(ctx, "SELECT count(*),coalesce(max(schema_version),0) FROM snapshot_meta").Scan(&count, &version)
		if e == nil && (count != 1 || version < 17 || version > schemaVersion) {
			e = fmt.Errorf("%s snapshot metadata changed: rows=%d schema_version=%d", src.layer, count, version)
		}
		if e == nil {
			e = migrateSnapshot(ctx, copyDB, src.layer, version)
		}
		e = errors.Join(e, copyDB.Close())
		if e != nil {
			return nil, cleanup, e
		}
		src.path = copyPath
		prepared = append(prepared, src)
	}
	return prepared, cleanup, nil
}

// 快照表没有 AUTOINCREMENT 约束，VACUUM 会丢弃它们单独保存的 sqlite_sequence；
// 因此逐表复制数据并显式保留序列，所有读取使用同一事务视图。
func copySnapshot(ctx context.Context, db *sql.DB, path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, "ATTACH DATABASE ? AS copy", path); err != nil {
		return err
	}
	var pageSize int
	if err := db.QueryRowContext(ctx, "PRAGMA main.page_size").Scan(&pageSize); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf("PRAGMA copy.page_size=%d", pageSize)); err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, "SELECT name FROM main.sqlite_schema WHERE type='table' ORDER BY name")
	if err != nil {
		return err
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		tables = append(tables, name)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, table := range tables {
		if strings.HasPrefix(table, "sqlite_") {
			if table != "sqlite_sequence" {
				continue
			}
			for _, query := range []string{"CREATE TABLE copy.sequence_seed (id INTEGER PRIMARY KEY AUTOINCREMENT)", "DROP TABLE copy.sequence_seed", "INSERT INTO copy.sqlite_sequence SELECT * FROM main.sqlite_sequence"} {
				if _, err := tx.ExecContext(ctx, query); err != nil {
					return err
				}
			}
			continue
		}
		quoted := `"` + strings.ReplaceAll(table, `"`, `""`) + `"`
		if _, err := tx.ExecContext(ctx, "CREATE TABLE copy."+quoted+" AS SELECT * FROM main."+quoted); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func migrateSnapshot(ctx context.Context, db *sql.DB, layer string, version int) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for next := version + 1; next <= schemaVersion; next++ {
		var statements []string
		switch next {
		case 18:
			if layer == "config" {
				statements = migrationV18
			}
		case 19:
			if layer == "config" {
				statements = []string{`ALTER TABLE restore_record ADD COLUMN themes TEXT NOT NULL DEFAULT '{}'`}
			}
		case 20:
			if layer == "config" {
				statements = []string{
					`CREATE TABLE admin_security (id INTEGER PRIMARY KEY CHECK(id=1),generation INTEGER NOT NULL DEFAULT 0,data TEXT NOT NULL DEFAULT '{}')`,
					`INSERT INTO admin_security (id) VALUES (1)`,
					`CREATE TABLE probe_task_tag (task_id INTEGER NOT NULL,tag_id INTEGER NOT NULL,PRIMARY KEY(task_id,tag_id))`,
					`CREATE TABLE alert_rule_tag (rule_id INTEGER NOT NULL,tag_id INTEGER NOT NULL,PRIMARY KEY(rule_id,tag_id))`,
					`ALTER TABLE alert_rule ADD COLUMN resource_metric TEXT`,
					`ALTER TABLE alert_rule ADD COLUMN recovery_threshold REAL`,
				}
			} else {
				for _, table := range []string{"metric_1m", "metric_5m", "metric_1h"} {
					for _, column := range []string{"memory_used_pct_sum REAL", "memory_used_pct_n INTEGER", "disk_used_pct_sum REAL", "disk_used_pct_n INTEGER"} {
						statements = append(statements, "ALTER TABLE "+table+" ADD COLUMN "+column+" NOT NULL DEFAULT 0")
					}
				}
			}
		case 21:
			if layer == "config" {
				statements = migrationV21Config
			} else {
				statements = migrationV21Metrics
			}
		case 22:
			if layer == "config" {
				if err := migrateThemeSnapshot(tx); err != nil {
					return fmt.Errorf("migrate config themes: %w", err)
				}
			} else {
				var columns int
				if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM pragma_table_info('snapshot_meta') WHERE name='format_version'").Scan(&columns); err != nil {
					return err
				}
				if columns == 1 {
					var format int
					if err := tx.QueryRowContext(ctx, "SELECT format_version FROM snapshot_meta").Scan(&format); err != nil {
						return err
					}
					if format != 2 {
						return fmt.Errorf("metrics snapshot unsupported format_version=%d", format)
					}
					statements = []string{"UPDATE snapshot_meta SET format_version=3"}
				} else {
					statements = []string{"ALTER TABLE snapshot_meta ADD COLUMN format_version INTEGER NOT NULL DEFAULT 3"}
				}
			}
		case 23:
			if layer == "config" {
				statements = migrationV23
			}
		case 24:
			// node_update 不属于任一备份层。
		case 25:
			if layer == "config" {
				statements = migrationV25Config
			}
		case 26:
			if layer == "config" {
				statements = migrationV26Config
			}
		case 27:
			// 维护静默只在配置层建结构，指标层无变化。
			if layer == "config" {
				statements = migrationV27Config
			}
		case 28:
			// 新指标只在指标层加列，配置层无变化。
			if layer == "metrics" {
				statements = migrationV28Metrics
			}
		case 29:
			// dns_server 在配置层的任务表上，指标层无变化。
			if layer == "config" {
				statements = migrationV29Config
			}
		case 30:
			// probe_cert 只在配置层建表，指标层无变化。
			if layer == "config" {
				statements = migrationV30Config
			}
		case 31:
			if layer == "metrics" {
				statements = migrationV31Metrics
			}
		case 32:
			// public_remark 只在配置层的节点表上加列，指标层无变化。
			if layer == "config" {
				statements = migrationV32Config
			}
		case 33:
			// (task_id, node_id, ts) 索引只在指标层的探测表上，配置层无变化。
			if layer == "metrics" {
				statements = migrationV33Metrics
			}
		case 34:
			// 配置层只有 node_facts 的新列。指标层的表是 CREATE TABLE AS 拷出来的，没有新建库的列序与约束；
			// 按冻结 DDL 重建后，恢复按列名搬运，旧行的按核负载列取默认值（n=0，没有采样）。
			if layer == "config" {
				statements = migrationV34Config
			} else if layer == "metrics" {
				if err := migrateV34Metrics(tx); err != nil {
					return fmt.Errorf("migrate metrics snapshot to 34: %w", err)
				}
			}
		case 35:
			// 配置身份与候选表只在配置层。指标层无变化。
			if layer == "config" {
				if err := migrateV35(tx); err != nil {
					return fmt.Errorf("migrate config snapshot to 35: %w", err)
				}
			}
		case 36:
			// 配额只在配置层的节点表加列，指标层无变化。
			if layer == "config" {
				statements = migrationV36Config
			}
		case 37:
			// 离线变更代数表不属于任一备份层；目标库的那一行由 Restore 重新种子。
		case 38:
			// 清理作业在配置层；旧快照没有作业，它的删除在当时已同步清掉了历史。
			if layer == "config" {
				statements = migrationV38Config
			}
		case 39:
			// rtt 相对判定的规则列、状态的 fired_at 与基线表都在配置层，指标层无变化。
			if layer == "config" {
				statements = migrationV39Config
			}
		case 40:
			// 节点的手填出口地址两列在配置层，指标层无变化。
			if layer == "config" {
				statements = migrationV40Config
			}
		case 41:
			// 流量基线表在配置层；快照的 traffic 是 CREATE TABLE AS 拷出的同名列，同一组语句适用。指标层无变化。
			if layer == "config" {
				statements = migrationV41Config
			}
		case 42:
			// 会话来源列在 admin_session；会话不进快照，目标库的那张表就是新建库 schema，两层都无变化。
		case 43:
			// TCP 速率四列只在指标层；指标层的表按冻结 DDL 重建后按列名搬运，新列取默认值。配置层无变化。
			if layer == "metrics" {
				if err := migrateV43Metrics(tx); err != nil {
					return fmt.Errorf("migrate metrics snapshot to 43: %w", err)
				}
			}
		case 44:
			// 与迁移 44 同源：node 表加 rented，只在配置层。
			if layer == "config" {
				statements = migrationV44Config
			}
		case 45:
			// 身份约束只影响配置层的证书观测，指标层无结构变化。
			if layer == "config" {
				if err := migrateV45Config(tx); err != nil {
					return fmt.Errorf("migrate config snapshot to 45: %w", err)
				}
			}
		case 46:
			// 节点资料版本表是运行态，不属于任一备份层；目标库自己的那一行由触发器随恢复的整表搬运推进。
		case 47:
			// token 失效时刻列在配置层，要补；推进节点资料版本的触发器不用：快照按 CREATE TABLE … AS SELECT 拷数据，
			// 本来就不带触发器，目标库的触发器由它自己的迁移建好。
			if layer == "config" {
				statements = migrationV47Config
			}
		case 48:
			if layer == "config" {
				if err := migrateV48Config(tx); err != nil {
					return fmt.Errorf("migrate config snapshot to 48: %w", err)
				}
			}
		case 49:
			if layer == "config" {
				statements = migrationV49Config
			}
		default:
			return fmt.Errorf("%s snapshot schema_version=%d: no reviewed migration to %d", layer, version, next)
		}
		for _, stmt := range statements {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("migrate %s snapshot to %d: %w", layer, next, err)
			}
		}
	}
	if _, err := tx.ExecContext(ctx, "UPDATE snapshot_meta SET schema_version=?", schemaVersion); err != nil {
		return err
	}
	return tx.Commit()
}

func attachRestoreSources(ctx context.Context, db *sql.DB, sources []restoreSource) error {
	for _, src := range sources {
		if _, err := db.ExecContext(ctx, "ATTACH DATABASE ? AS "+src.layer, dsn(src.path, "&mode=ro")); err != nil {
			return fmt.Errorf("attach %s snapshot: %w", src.layer, err)
		}
	}
	return nil
}

func preflightRestoreSources(ctx context.Context, target string, sources []restoreSource) (err error) {
	targetDSN := dsn(target, "&mode=ro")
	if _, err := os.Stat(target); errors.Is(err, os.ErrNotExist) {
		// 内存空库给出 SQLite 默认页大小；新目标采用它，在创建文件前拒绝不匹配的来源。
		targetDSN = ":memory:"
	} else if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", targetDSN)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	db.SetMaxOpenConns(1)
	if err := attachRestoreSources(ctx, db, sources); err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = validateRestoreSources(ctx, tx, sources)
	return err
}

func validateRestoreSources(ctx context.Context, tx *sql.Tx, sources []restoreSource) (map[string]int64, error) {
	var pageSize int
	if err := tx.QueryRowContext(ctx, "PRAGMA main.page_size").Scan(&pageSize); err != nil {
		return nil, err
	}
	takenAt := make(map[string]int64, len(sources))
	for _, src := range sources {
		at, err := validateSnapshot(ctx, tx, src.layer, src.tables, pageSize)
		if err != nil {
			return nil, err
		}
		takenAt[src.layer] = at
	}
	return takenAt, nil
}

func restoreColumnList(ctx context.Context, tx *sql.Tx, table string) (string, error) {
	// 目标 schema 决定要恢复的列，INSERT 与 SELECT 共用列名，不依赖快照中列的物理顺序。
	rows, err := tx.QueryContext(ctx, "SELECT name FROM pragma_table_info(?, 'main') ORDER BY cid", table)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return "", err
		}
		columns = append(columns, `"`+strings.ReplaceAll(name, `"`, `""`)+`"`)
	}
	return strings.Join(columns, ","), rows.Err()
}

func validateSnapshot(ctx context.Context, tx *sql.Tx, layer string, tables []string, pageSize int) (int64, error) {
	for _, table := range append(slices.Clone(tables), "snapshot_meta", "sqlite_sequence") {
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM "+layer+".sqlite_schema WHERE type='table' AND name=?", table).Scan(&count); err != nil {
			return 0, err
		}
		if count != 1 {
			return 0, fmt.Errorf("%s snapshot missing table %s", layer, table)
		}
	}
	var count, size int
	var version, at sql.NullInt64
	var gotLayer sql.NullString
	if err := tx.QueryRowContext(ctx, "SELECT count(*),max(schema_version),max(taken_at),max(layer) FROM "+layer+".snapshot_meta").Scan(&count, &version, &at, &gotLayer); err != nil {
		return 0, fmt.Errorf("%s snapshot metadata: %w", layer, err)
	}
	if count != 1 || version.Int64 != schemaVersion || gotLayer.String != layer {
		return 0, fmt.Errorf("%s snapshot metadata: rows=%d schema_version=%d layer=%q; want one row, schema_version=%d layer=%q", layer, count, version.Int64, gotLayer.String, schemaVersion, layer)
	}
	var formatColumn int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM pragma_table_info('snapshot_meta', ?) WHERE name='format_version'", layer).Scan(&formatColumn); err != nil {
		return 0, err
	}
	if formatColumn != 1 {
		return 0, fmt.Errorf("%s snapshot missing format_version", layer)
	}
	var format int
	if err := tx.QueryRowContext(ctx, "SELECT format_version FROM "+layer+".snapshot_meta").Scan(&format); err != nil {
		return 0, err
	}
	if format != 3 {
		return 0, fmt.Errorf("%s snapshot unsupported format_version=%d", layer, format)
	}
	if layer == "config" {
		if err := validateSnapshotFacts(ctx, tx); err != nil {
			return 0, err
		}
		if err := validateSnapshotTraffic(ctx, tx); err != nil {
			return 0, err
		}
		if err := validateSnapshotAddressPins(ctx, tx); err != nil {
			return 0, err
		}
		if err := validateSnapshotNodeFlags(ctx, tx); err != nil {
			return 0, err
		}
		var unmatched int
		if err := tx.QueryRowContext(ctx, `SELECT
				(SELECT count(*) FROM config.theme_version v WHERE NOT EXISTS(SELECT 1 FROM config.snapshot_theme s WHERE s.theme_id=v.theme_id AND s.digest=v.digest)) +
				(SELECT count(*) FROM config.snapshot_theme s WHERE NOT EXISTS(SELECT 1 FROM config.theme_version v WHERE s.theme_id=v.theme_id AND s.digest=v.digest)) +
				(SELECT count(*) FROM config.theme t WHERE NOT EXISTS(SELECT 1 FROM config.theme_version v WHERE v.theme_id=t.id)) +
				(SELECT count(*) FROM config.theme_version v WHERE NOT EXISTS(SELECT 1 FROM config.theme t WHERE v.theme_id=t.id))`).Scan(&unmatched); err != nil {
			return 0, err
		}
		if unmatched != 0 {
			return 0, errors.New("snapshot theme references do not match configuration")
		}
	}
	if err := tx.QueryRowContext(ctx, "PRAGMA "+layer+".page_size").Scan(&size); err != nil {
		return 0, err
	}
	if size != pageSize {
		return 0, fmt.Errorf("%s snapshot page_size=%d; expected page_size=%d", layer, size, pageSize)
	}
	return at.Int64, nil
}

func validateSnapshotFacts(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, "SELECT node_id,network,diagnostics,execution FROM config.node_facts")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var network, diagnostics, execution string
		if err := rows.Scan(&id, &network, &diagnostics, &execution); err != nil {
			return err
		}
		if _, err := decodeNetwork(network); err != nil {
			return fmt.Errorf("config node %d network: %w", id, err)
		}
		if _, err := decodeDiagnostics(diagnostics); err != nil {
			return fmt.Errorf("config node %d diagnostics: %w", id, err)
		}
		if _, err := decodeExecution(execution); err != nil {
			return fmt.Errorf("config node %d execution: %w", id, err)
		}
	}
	return rows.Err()
}

// validateSnapshotNodeFlags 与写入口同一个互斥判定：节点同时是维护中且已出租的快照拒绝恢复，
// 不让两条写路径（UpdateNodeTasks 与恢复）出现不同答案。
func validateSnapshotNodeFlags(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, "SELECT id,name,maintenance,rented FROM config.node")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var name string
		var maintenance, rented bool
		if err := rows.Scan(&id, &name, &maintenance, &rented); err != nil {
			return err
		}
		if err := checkNodeFlags(maintenance, rented); err != nil {
			return fmt.Errorf("config node %d (%s) has maintenance and rented both set", id, name)
		}
	}
	return rows.Err()
}

// validateSnapshotAddressPins 让恢复进来的手填出口地址与写入口写出的一样：空串或该族公网单播地址的规范形。显示值把
// 手填当作 AVAILABLE（Node.DisplayNetwork），快照里的值不经 UpdateNodeTasks，这里是它进库前唯一的检查。
func validateSnapshotAddressPins(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, "SELECT id,ipv4_pin,ipv6_pin FROM config.node")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var pins [2]string
		if err := rows.Scan(&id, &pins[0], &pins[1]); err != nil {
			return err
		}
		for i, pin := range pins {
			canonical, err := canonicalAddressPin(pin, i == 0)
			if err == nil && canonical != pin {
				err = fmt.Errorf("address pin %q is not in canonical form %q", pin, canonical)
			}
			if err != nil {
				return fmt.Errorf("config node %d %s: %w", id, []string{"ipv4_pin", "ipv6_pin"}[i], err)
			}
		}
	}
	return rows.Err()
}

// validateSnapshotTraffic 按账本读库的同一判定（CheckTrafficBaseline、基线行必有 traffic 行）检查快照里的流量基线：
// 快照不经 WriteTraffic，这里是它进库前唯一的检查；放过不合法的形态，恢复后 hub 在 LoadTraffic 处起不来。
func validateSnapshotTraffic(ctx context.Context, tx *sql.Tx) error {
	baselines, err := readTrafficBaselines(ctx, tx, "config")
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, "SELECT node_id,net_counter_epoch FROM config.traffic")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var epoch string
		if err := rows.Scan(&id, &epoch); err != nil {
			return err
		}
		baseline := baselines[id]
		delete(baselines, id)
		if err := CheckTrafficBaseline(epoch, baseline); err != nil {
			return fmt.Errorf("config node %d: %w", id, err)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := orphanBaselines(baselines); err != nil {
		return fmt.Errorf("config %w", err)
	}
	return nil
}

func restoreSequences(ctx context.Context, tx *sql.Tx, sources []string) error {
	// DELETE 数据行不降低 main.sqlite_sequence；因此此时 main 仍承载恢复前的高水位。
	// 三方按表名取最大，既不复用目标库的旧 ID，也不复用任一快照已删掉的 ID。
	var queries []string
	for _, source := range sources {
		queries = append(queries, "SELECT name,seq FROM "+source+".sqlite_sequence")
	}
	rows, err := tx.QueryContext(ctx, "SELECT name,max(seq) FROM ("+strings.Join(queries, " UNION ALL ")+") GROUP BY name")
	if err != nil {
		return err
	}
	seqs := make(map[string]int64)
	for rows.Next() {
		var name string
		var seq int64
		if err := rows.Scan(&name, &seq); err != nil {
			rows.Close()
			return err
		}
		seqs[name] = seq
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM main.sqlite_sequence"); err != nil {
		return err
	}
	for name, seq := range seqs {
		if _, err := tx.ExecContext(ctx, "INSERT INTO main.sqlite_sequence (name,seq) VALUES (?,?)", name, seq); err != nil {
			return err
		}
	}
	return nil
}
