package store

import (
	"crypto/rand"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"
)

// migrations[v] 把 user_version = v−1 的库升到 v。空库不重放历史，直接建到当前版本；
// 所以 schemaStatements 必须始终是"当前版本的完整 DDL"，迁移测试用逐表、逐索引比对钉住这一点。
//
// 不变式：每个迁移只引用冻结的该版本 DDL（下面的 *V<n> 常量），不引用 schema.go 里的当前 DDL。
var migrations = map[int]func(*sql.Tx) error{
	2: execAll(migrationV2),
	3: func(tx *sql.Tx) error {
		if _, err := tx.Exec(ddlTrafficV3); err != nil {
			return fmt.Errorf("%w in %q", err, ddlTrafficV3)
		}
		for _, t := range metricTablesV3 {
			if err := rebuildTable(tx, t, metricDDLV3); err != nil {
				return fmt.Errorf("rebuilding %s: %w", t, err)
			}
		}
		return nil
	},
	4:  execAll(migrationV4),
	5:  execAll(alertStatementsV5),
	6:  execAll([]string{ddlAPITokenV6}),
	7:  migrateDeliveryFailure,
	8:  execAll([]string{ddlSettingV8}),
	9:  execAll(migrationV9),
	10: execAll(migrationV10),
	11: execAll([]string{ddlMaintenanceStateV11}),
	12: execAll(migrationV12),
	13: execAll(migrationV13),
	14: execAll(migrationV14),
	15: execAll(migrationV15),
	16: execAll(migrationV16),
	17: execAll([]string{ddlRestoreRecordV17}),
	18: execAll(migrationV18),
	19: execAll(migrationV19),
	20: execAll(migrationV20),
	21: execAll(append(append([]string{}, migrationV21Config...), migrationV21Metrics...)),
	22: migrateThemeVersions,
	23: execAll(migrationV23),
	24: execAll([]string{ddlNodeUpdateV24}),
	25: execAll(append(append([]string{}, migrationV25Config...),
		"ALTER TABLE register_window RENAME TO register_window_old", ddlRegisterWindowV25,
		"INSERT INTO register_window SELECT 0, key_hash, expires_at, remaining FROM register_window_old",
		"DROP TABLE register_window_old", "ALTER TABLE node_update ADD COLUMN owner_id INTEGER NOT NULL DEFAULT 0")),
	26: execAll(migrationV26Config),
	27: execAll(migrationV27Config),
	28: execAll(migrationV28Metrics),
	29: execAll(migrationV29Config),
	30: execAll(migrationV30Config),
	31: execAll(append(append([]string{}, migrationV31Metrics...), coverageFallbackV31)),
	32: execAll(migrationV32Config),
	33: execAll(migrationV33Metrics),
	34: migrateV34,
	35: migrateV35,
	36: execAll(migrationV36Config),
	37: execAll(migrationV37),
	38: execAll(append(append([]string{}, migrationV38Config...), migrationV38OrphanTasks...)),
	39: execAll(migrationV39Config),
	40: execAll(migrationV40Config),
	41: execAll(migrationV41Config),
	42: execAll(migrationV42Config),
	43: migrateV43,
	44: execAll(migrationV44Config),
	45: migrateV45Config,
	46: execAll(migrationV46Config),
	47: execAll(migrationV47),
	48: migrateV48Config,
	49: execAll(migrationV49Config),
}

// 流量观测时刻与节点名都属于配置层，生产库与配置快照使用同一步迁移。
// 旧库没有记录带计数上报的时刻，observed_at 保持默认的未知值，不从 updated_at 猜测。
func migrateV48Config(tx *sql.Tx) error {
	if _, err := tx.Exec(`ALTER TABLE traffic ADD COLUMN observed_at INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	rows, err := tx.Query("SELECT id, name FROM node")
	if err != nil {
		return err
	}
	defer rows.Close()
	type renamedNode struct {
		id   int64
		name string
	}
	var renamed []renamedNode
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return err
		}
		if clean := AgentNodeName(name); clean != name {
			renamed = append(renamed, renamedNode{id: id, name: clean})
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, node := range renamed {
		if _, err := tx.Exec("UPDATE node SET name = ? WHERE id = ?", node.name, node.id); err != nil {
			return err
		}
	}
	return nil
}

// 原有静默覆盖全部可静默规则，必须显式回填；新列的默认值不代表全部规则。
var migrationV49Config = []string{
	`ALTER TABLE silence ADD COLUMN all_rules INTEGER NOT NULL DEFAULT 0`,
	`UPDATE silence SET all_rules = 1`,
	`CREATE TABLE silence_rule (
  silence_id INTEGER NOT NULL,
  rule_id INTEGER NOT NULL,
  PRIMARY KEY (silence_id, rule_id)
) WITHOUT ROWID`,
	`CREATE INDEX silence_rule_by_rule ON silence_rule (rule_id)`,
}

// v46：节点资料版本表、种子行与推进它的触发器（含义见 schema.go 的 ddlNodeListRevision）。只建新对象、不改已有行；
// 回滚边界同其它迁移：写入后旧二进制不能直接启动，回退须恢复升级前的自动备份。
var migrationV46Config = []string{
	`CREATE TABLE node_list_revision (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  revision INTEGER NOT NULL CHECK (revision >= 0)
)`,
	`INSERT INTO node_list_revision (id, revision) VALUES (1, 0)`,
	`CREATE TRIGGER node_list_revision_node_insert AFTER INSERT ON node BEGIN UPDATE node_list_revision SET revision = revision + 1 WHERE id = 1; END`,
	`CREATE TRIGGER node_list_revision_node_delete AFTER DELETE ON node BEGIN UPDATE node_list_revision SET revision = revision + 1 WHERE id = 1; END`,
	`CREATE TRIGGER node_list_revision_node_update AFTER UPDATE ON node WHEN
  OLD.id IS NOT NEW.id OR OLD.name IS NOT NEW.name OR OLD.sort_order IS NOT NEW.sort_order OR OLD.public IS NOT NEW.public
  OR OLD.note IS NOT NEW.note OR OLD.offline_grace_s IS NOT NEW.offline_grace_s OR OLD.traffic_reset_day IS NOT NEW.traffic_reset_day
  OR OLD.created_at IS NOT NEW.created_at OR OLD.price IS NOT NEW.price OR OLD.currency IS NOT NEW.currency
  OR OLD.billing_cycle IS NOT NEW.billing_cycle OR OLD.expires_on IS NOT NEW.expires_on OR OLD.auto_renew IS NOT NEW.auto_renew
  OR OLD.last_source IS NOT NEW.last_source OR OLD.country IS NOT NEW.country OR OLD.country_ip IS NOT NEW.country_ip
  OR OLD.country_pin IS NOT NEW.country_pin OR OLD.maintenance IS NOT NEW.maintenance OR OLD.public_remark IS NOT NEW.public_remark
  OR OLD.traffic_quota_bytes IS NOT NEW.traffic_quota_bytes OR OLD.traffic_quota_mode IS NOT NEW.traffic_quota_mode
  OR OLD.ipv4_pin IS NOT NEW.ipv4_pin OR OLD.ipv6_pin IS NOT NEW.ipv6_pin OR OLD.rented IS NOT NEW.rented
BEGIN UPDATE node_list_revision SET revision = revision + 1 WHERE id = 1; END`,
	`CREATE TRIGGER node_list_revision_facts_insert AFTER INSERT ON node_facts BEGIN UPDATE node_list_revision SET revision = revision + 1 WHERE id = 1; END`,
	`CREATE TRIGGER node_list_revision_facts_delete AFTER DELETE ON node_facts BEGIN UPDATE node_list_revision SET revision = revision + 1 WHERE id = 1; END`,
	`CREATE TRIGGER node_list_revision_facts_update AFTER UPDATE ON node_facts WHEN
  OLD.node_id IS NOT NEW.node_id OR OLD.hostname IS NOT NEW.hostname OR OLD.os IS NOT NEW.os OR OLD.kernel IS NOT NEW.kernel
  OR OLD.arch IS NOT NEW.arch OR OLD.virtualization IS NOT NEW.virtualization OR OLD.cpu_model IS NOT NEW.cpu_model
  OR OLD.cpu_cores IS NOT NEW.cpu_cores OR OLD.agent_version IS NOT NEW.agent_version OR OLD.icmp_available IS NOT NEW.icmp_available
  OR OLD.updated_at IS NOT NEW.updated_at OR OLD.network IS NOT NEW.network OR OLD.diagnostics IS NOT NEW.diagnostics
  OR OLD.execution IS NOT NEW.execution
BEGIN UPDATE node_list_revision SET revision = revision + 1 WHERE id = 1; END`,
	`CREATE TRIGGER node_list_revision_node_tag_insert AFTER INSERT ON node_tag BEGIN UPDATE node_list_revision SET revision = revision + 1 WHERE id = 1; END`,
	`CREATE TRIGGER node_list_revision_node_tag_delete AFTER DELETE ON node_tag BEGIN UPDATE node_list_revision SET revision = revision + 1 WHERE id = 1; END`,
	`CREATE TRIGGER node_list_revision_node_tag_update AFTER UPDATE ON node_tag BEGIN UPDATE node_list_revision SET revision = revision + 1 WHERE id = 1; END`,
	`CREATE TRIGGER node_list_revision_tag_update AFTER UPDATE ON tag BEGIN UPDATE node_list_revision SET revision = revision + 1 WHERE id = 1; END`,
	`CREATE TRIGGER node_list_revision_tag_delete AFTER DELETE ON tag BEGIN UPDATE node_list_revision SET revision = revision + 1 WHERE id = 1; END`,
	`CREATE TRIGGER node_list_revision_token_node_insert AFTER INSERT ON api_token_node BEGIN UPDATE node_list_revision SET revision = revision + 1 WHERE id = 1; END`,
	`CREATE TRIGGER node_list_revision_token_node_delete AFTER DELETE ON api_token_node BEGIN UPDATE node_list_revision SET revision = revision + 1 WHERE id = 1; END`,
	`CREATE TRIGGER node_list_revision_token_node_update AFTER UPDATE ON api_token_node BEGIN UPDATE node_list_revision SET revision = revision + 1 WHERE id = 1; END`,
}

// v47：API token 的失效时刻。只加可空列，已有 token 取 NULL（不过期），行为不变。范围可以编辑之后，api_token.all_nodes
// 的变化会改变 ListNodes 对该 token 的内容，节点资料版本须随之推进（含义见 schema.go 的 ddlNodeListRevision）。
// 列与触发器分两组登记：快照经 CREATE TABLE … AS SELECT 逐表拷数据（snapshot.go 的 Store.snapshot），不带触发器、约束与
// 索引，恢复时目标库的触发器由它自己的 createSchema 与迁移建好；所以 restore.go 的 migrateSnapshot 只补配置层的列。
// 回滚边界同其它迁移：写入后旧二进制不能直接启动，回退须恢复升级前的自动备份。
var migrationV47Config = []string{`ALTER TABLE api_token ADD COLUMN expires_at INTEGER CHECK (expires_at IS NULL OR expires_at > 0)`}

var migrationV47Runtime = []string{`CREATE TRIGGER node_list_revision_token_update AFTER UPDATE ON api_token WHEN
  OLD.id IS NOT NEW.id OR OLD.all_nodes IS NOT NEW.all_nodes
BEGIN UPDATE node_list_revision SET revision = revision + 1 WHERE id = 1; END`}

var migrationV47 = append(slices.Clone(migrationV47Config), migrationV47Runtime...)

// 证书观测必须能绑定到配置身份；没有身份的存量观测无法补证，由迁移删除。
// 告警状态不在本迁移中改写，无读数时由到期评估保留，等待下一次有效观测。
func migrateV45Config(tx *sql.Tx) error {
	if _, err := tx.Exec(`DELETE FROM probe_cert WHERE config_id IS NULL`); err != nil {
		return err
	}
	return rebuildTable(tx, "probe_cert", func(table string) string {
		return strings.Replace(ddlProbeCertV45, "CREATE TABLE probe_cert (", "CREATE TABLE "+table+" (", 1)
	})
}

const ddlProbeCertV45 = `CREATE TABLE probe_cert (
  node_id INTEGER NOT NULL,
  task_id INTEGER NOT NULL,
  not_after INTEGER NOT NULL,
  observed_at INTEGER NOT NULL,
  config_id BLOB NOT NULL,
  PRIMARY KEY (node_id, task_id)
) WITHOUT ROWID`

// metricDDLV43 冻结引入 TCP 速率四列时的指标表 DDL：v34 的列加上末尾四列。以后再追加指标必须新开迁移，不能改这里。
func metricDDLV43(table string) string {
	type col struct {
		name, sqlType string
		max           bool
	}
	cols := []col{
		{"cpu", "REAL", true}, {"mem_used", "INTEGER", true}, {"swap_used", "INTEGER", false}, {"disk_used", "INTEGER", false},
		{"load1", "REAL", false}, {"tcp", "INTEGER", false}, {"udp", "INTEGER", false}, {"procs", "INTEGER", false},
		{"rx_bytes", "INTEGER", false}, {"tx_bytes", "INTEGER", false}, {"memory_used_pct", "REAL", false}, {"disk_used_pct", "REAL", false},
		{"net_rx_bps", "INTEGER", true}, {"net_tx_bps", "INTEGER", true}, {"disk_read_bps", "INTEGER", true}, {"disk_write_bps", "INTEGER", true},
		{"cpu_steal_pct", "REAL", true}, {"cpu_iowait_pct", "REAL", true}, {"load1_per_core", "REAL", false},
		{"tcp_active_opens_ps", "REAL", true}, {"tcp_passive_opens_ps", "REAL", true}, {"tcp_out_segs_ps", "REAL", false}, {"tcp_retrans_segs_ps", "REAL", false},
	}
	parts := []string{"node_id INTEGER NOT NULL", "ts INTEGER NOT NULL"}
	for _, c := range cols {
		parts = append(parts, c.name+"_sum "+c.sqlType+" NOT NULL DEFAULT 0", c.name+"_n INTEGER NOT NULL DEFAULT 0")
		if c.max {
			parts = append(parts, c.name+"_max "+c.sqlType+" NOT NULL DEFAULT 0")
		}
	}
	if strings.TrimSuffix(table, "_new") == "metric_1m" {
		parts = append(parts, "reported INTEGER NOT NULL DEFAULT 1", "observed INTEGER")
	} else {
		parts = append(parts, "minutes INTEGER", "observed INTEGER", "both INTEGER")
	}
	return "CREATE TABLE " + table + " (" + strings.Join(parts, ", ") + ", PRIMARY KEY (node_id, ts)) WITHOUT ROWID"
}

// v43：TCP 建连与段的采样速率四列。与 v34 同理：覆盖列已在指标列之后，ADD COLUMN 只能加在表尾、
// 列序会与新建库不同，所以按冻结 DDL 重建三张表；旧行新列取默认值，n=0 表示这一分钟没有采样，不是测得的零。
// 回滚边界同 v42：写入后 schema 42 的二进制不能直接启动，回退须恢复升级前的数据库备份。
func migrateV43(tx *sql.Tx) error { return migrateV43Metrics(tx) }

func migrateV43Metrics(tx *sql.Tx) error {
	for _, table := range []string{"metric_1m", "metric_5m", "metric_1h"} {
		if err := rebuildTable(tx, table, metricDDLV43); err != nil {
			return fmt.Errorf("rebuilding %s: %w", table, err)
		}
	}
	return nil
}

// v44：节点的已出租开关。已有节点都未出租。只在配置层，迁移与配置快照共用。
// 与 maintenance 的互斥不进库：裁决在 UpdateNodeTasks（checkNodeFlags），快照恢复进库前由
// validateSnapshotNodes 复核。
var migrationV44Config = []string{`ALTER TABLE node ADD COLUMN rented INTEGER NOT NULL DEFAULT 0`}

// v42：会话签发时的来源地址（会话可列可撤）。已有会话取空串：升级前签发的会话没有来源记录，
// 空串 = 未知，与"没有请求的签发路径"同一含义，读侧按此呈现，不回填别的值。
// 回滚边界：Open 拒绝打开 user_version 高于二进制支持版本的库（store.go schemaAdmission），本迁移写入后
// schema 41 的二进制不能直接启动；回退须恢复升级前的数据库备份再启动旧二进制，不做 down migration。
var migrationV42Config = []string{
	`ALTER TABLE admin_session ADD COLUMN source TEXT NOT NULL DEFAULT ''`,
}

// v41：流量差分基线移出 traffic 行，成为 traffic_baseline 的行（一种表示承载合计与逐网卡两种形态，见 ddlTrafficBaseline）。
// 旧行的合计基线（last_rx、last_tx 都非负）搬成 iface 为空的一行，集合摘要留在 traffic 行上；校正先于首次上报建立的
// last_rx = last_tx = -1 行没有基线，迁移成零行并清空摘要，不把哨兵当成真实计数。只在配置层，迁移与配置快照共用。
var migrationV41Config = []string{
	`CREATE TABLE traffic_baseline (
  node_id INTEGER NOT NULL,
  iface TEXT NOT NULL,
  rx INTEGER NOT NULL CHECK (rx >= 0),
  tx INTEGER NOT NULL CHECK (tx >= 0),
  PRIMARY KEY (node_id, iface)
) WITHOUT ROWID`,
	`INSERT INTO traffic_baseline (node_id, iface, rx, tx) SELECT node_id, '', last_rx, last_tx FROM traffic WHERE last_rx >= 0 AND last_tx >= 0`,
	`UPDATE traffic SET net_counter_epoch = '' WHERE NOT (last_rx >= 0 AND last_tx >= 0)`,
	`ALTER TABLE traffic DROP COLUMN last_rx`,
	`ALTER TABLE traffic DROP COLUMN last_tx`,
}

// v40：节点的手填出口地址（出口地址手填）。已有节点都是空串，即照旧显示 agent 探测值。只在配置层。
var migrationV40Config = []string{
	`ALTER TABLE node ADD COLUMN ipv4_pin TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE node ADD COLUMN ipv6_pin TEXT NOT NULL DEFAULT ''`,
}

// v39：rtt 规则的相对基线判定。已有 rtt 规则显式写成固定阈值，库里不以 NULL 代表它；已在 firing 的状态以进入
// firing 的时刻（since_at）作 fired_at，其余为 0（从未触发或已恢复，恢复前那次触发的时刻不可考）。全部是配置层的表，
// 指标层无变化。
var migrationV39Config = []string{
	`ALTER TABLE alert_rule ADD COLUMN rtt_mode TEXT`,
	`ALTER TABLE alert_rule ADD COLUMN baseline_mode TEXT`,
	`ALTER TABLE alert_rule ADD COLUMN baseline_window_s INTEGER`,
	`ALTER TABLE alert_rule ADD COLUMN baseline_min_samples INTEGER`,
	`ALTER TABLE alert_rule ADD COLUMN upper_deviation_pct REAL`,
	`ALTER TABLE alert_rule ADD COLUMN lower_deviation_pct REAL`,
	`ALTER TABLE alert_rule ADD COLUMN cooldown_s INTEGER`,
	`ALTER TABLE alert_rule ADD COLUMN fixed_baseline_ms REAL`,
	`UPDATE alert_rule SET rtt_mode = 'threshold' WHERE kind = 'probe' AND metric = 'rtt_ms'`,
	`ALTER TABLE alert_state ADD COLUMN fired_at INTEGER NOT NULL DEFAULT 0`,
	`UPDATE alert_state SET fired_at = since_at WHERE state = 'firing'`,
	`CREATE TABLE alert_baseline (
  rule_id INTEGER NOT NULL,
  node_id INTEGER NOT NULL,
  baseline_us INTEGER NOT NULL,
  buckets INTEGER NOT NULL,
  computed_at INTEGER NOT NULL,
  task_fingerprint TEXT NOT NULL,
  accumulate_from INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (rule_id, node_id)
)`,
}

// 清理作业表只在配置层。迁移前删节点是同步删历史的，库里没有节点的孤儿行；删任务却从不删历史，已删任务的行一直
// 留到保留期。读侧从这一版起按 probe_task 过滤掉它们，迁移给每个这样的任务补登记作业，由维护循环清掉，不再等保留期。
// 任务 id 沿 (task_id, node_id, ts) 索引（迁移 33 起恒存在）逐个跳读，不扫描全部行。
var migrationV38OrphanTasks = []string{
	migrationV38OrphanTaskJobs("probe_1m"),
	migrationV38OrphanTaskJobs("probe_5m"),
	migrationV38OrphanTaskJobs("probe_1h"),
}

func migrationV38OrphanTaskJobs(table string) string {
	return `INSERT OR IGNORE INTO cleanup_job (kind, node_id, task_id, created_at)
WITH RECURSIVE seen(id) AS (
  SELECT min(task_id) FROM ` + table + `
  UNION ALL
  SELECT (SELECT min(task_id) FROM ` + table + ` WHERE task_id > seen.id) FROM seen WHERE seen.id IS NOT NULL
)
SELECT 'task', 0, id, CAST(strftime('%s', 'now') AS INTEGER) FROM seen WHERE id IS NOT NULL AND id NOT IN (SELECT id FROM probe_task)`
}

var migrationV38Config = []string{
	`CREATE TABLE cleanup_job (
  kind TEXT NOT NULL,
  node_id INTEGER NOT NULL DEFAULT 0,
  task_id INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (kind, node_id, task_id)
) WITHOUT ROWID`,
}

// 离线变更代数只对读它的那个 hub 进程有意义（hub 启动时先读起点，再加载缓存，见 coordination.go），起点取多少
// 都不影响判定，所以迁移与新建库一样种子为 0。
var migrationV37 = []string{
	`CREATE TABLE hub_coordination (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  offline_generation INTEGER NOT NULL CHECK (offline_generation >= 0)
)`,
	`INSERT INTO hub_coordination (id, offline_generation) VALUES (1, 0)`,
}

var migrationV36Config = []string{
	"ALTER TABLE node ADD COLUMN traffic_quota_bytes INTEGER NOT NULL DEFAULT 0",
	"ALTER TABLE node ADD COLUMN traffic_quota_mode TEXT NOT NULL DEFAULT 'sum'",
}

// 覆盖列（metric_1m 的 reported/observed，上卷表的 minutes/observed/both）已经排在指标列之后。
// ADD COLUMN 只能加在表尾，新列会落在覆盖列后面；新建库按描述表生成，指标列在覆盖列之前。
// 列序进 schema 签名，两边不一致就会被判成不同的库。因此用冻结 DDL 重建三张表，只搬运两边都有的列，
// 新列取 DDL 默认值（n=0 表示这一分钟没有采样）。
func migrateV34(tx *sql.Tx) error {
	if err := execAll(migrationV34Config)(tx); err != nil {
		return err
	}
	return migrateV34Metrics(tx)
}

func migrateV34Metrics(tx *sql.Tx) error {
	for _, table := range []string{"metric_1m", "metric_5m", "metric_1h"} {
		if err := rebuildTable(tx, table, metricDDLV34); err != nil {
			return fmt.Errorf("rebuilding %s: %w", table, err)
		}
	}
	return nil
}

// ddlProbeTaskV35 冻结引入 pin 与配置身份时的 probe_task。必须与当时的 schema.go 逐字相同：
// 迁移后的库与新建库按规范化后的建表语句比较，注释与空白之外的差异会被判成不同的库。
const ddlProbeTaskV35 = `CREATE TABLE probe_task (
  -- AUTOINCREMENT：历史行只带 task_id，删除任务后 id 若复用，旧历史会挂到新任务上。
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  kind INTEGER NOT NULL,
  target TEXT NOT NULL,
  interval_s INTEGER NOT NULL,
  timeout_ms INTEGER NOT NULL,
  created_at INTEGER NOT NULL,
  -- 与 alert_rule.all_nodes 同一语义：为真时 SaveProbeTask 不写 probe_task_node 行，任务覆盖全部节点，
  -- 之后新建的节点也在内；为假时分配行就是全部覆盖，空集不覆盖任何节点，DeleteNode 删掉最后一个分配行
  -- 也不会放宽到全部。覆盖的读法只有 probeCoverage 一处。列序与迁移 10 的 ADD COLUMN 结果一致。
  all_nodes INTEGER NOT NULL DEFAULT 0,
  sort_order INTEGER NOT NULL DEFAULT 0,
  -- DNS 任务要查询的解析器（ip:port）；其他种类恒为空串。列序与迁移 29 的 ADD COLUMN 结果一致。
  dns_server TEXT NOT NULL DEFAULT '',
  -- 叶证书 SubjectPublicKeyInfo 的 SHA-256；NULL 表示不钉。空 blob 与 NULL 都是不钉，写侧只写 NULL。
  cert_spki_sha256 BLOB,
  -- 配置身份，16 字节随机数。任务内容（除 id 与本列外的列，含 pin）变化时重新生成；只比较相等。
  -- 不用 probe_meta.version：那是清单计数，备份恢复后可以重新走到同一个值却对应另一份配置。
  config_id BLOB NOT NULL
)`

const ddlProbeCertPresentedV35 = `CREATE TABLE probe_cert_presented (
  node_id INTEGER NOT NULL,
  task_id INTEGER NOT NULL,
  config_id BLOB NOT NULL,
  spki_sha256 BLOB NOT NULL,
  not_after INTEGER NOT NULL,
  reason INTEGER NOT NULL,
  observed_at INTEGER NOT NULL,
  PRIMARY KEY (node_id, task_id)
) WITHOUT ROWID`

// migrateV35 给已有任务各生成一个配置身份，并在确有任务时按 bumpProbeVersion 推进清单版本。
// 身份必须在升级事务里生成：agent 下一次对账拿到的清单已经带身份，不能先下发一份身份为空的任务再补。
// 有任务才推进版本。空清单的内容没有变，推进只会让"迁移不改清单计数"的空库断言失效，也没有 agent 需要重取。
// config_id 是 NOT NULL 且没有默认值，ADD COLUMN 加不上；重建表才能与新建库的列定义一致。
// AUTOINCREMENT 的 sqlite_sequence 记的是发过的最大 id，不是当前行的最大 id。重建若丢掉它，已删除任务的 id 会复用，旧探测历史会挂到新任务上。
func migrateV35(tx *sql.Tx) error {
	var seq sql.NullInt64
	err := tx.QueryRow(`SELECT seq FROM sqlite_sequence WHERE name = 'probe_task'`).Scan(&seq)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if _, err := tx.Exec(strings.Replace(ddlProbeTaskV35, "CREATE TABLE probe_task (", "CREATE TABLE probe_task_new (", 1)); err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT id, kind, target, interval_s, timeout_ms, created_at, all_nodes, sort_order, dns_server FROM probe_task ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	stmt, err := tx.Prepare(`INSERT INTO probe_task_new (id, kind, target, interval_s, timeout_ms, created_at, all_nodes, sort_order, dns_server, cert_spki_sha256, config_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	n := 0
	for rows.Next() {
		var id, kind, interval, timeout, created, order int64
		var all bool
		var target, dns string
		if err := rows.Scan(&id, &kind, &target, &interval, &timeout, &created, &all, &order, &dns); err != nil {
			return err
		}
		buf := make([]byte, 16)
		if _, err := rand.Read(buf); err != nil {
			return err
		}
		if _, err := stmt.Exec(id, kind, target, interval, timeout, created, all, order, dns, buf); err != nil {
			return err
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := stmt.Close(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if _, err := tx.Exec(`DROP TABLE probe_task`); err != nil {
		return err
	}
	if _, err := tx.Exec(`ALTER TABLE probe_task_new RENAME TO probe_task`); err != nil {
		return err
	}
	if seq.Valid {
		// sqlite_sequence 没有主键，不能用 ON CONFLICT。改名后的行记的是现存 id 的最大值，
		// 已删除任务的 id 只留在旧的 seq 里，必须取较大者，否则旧探测历史会挂到复用的新 id 上。
		res, err := tx.Exec(`UPDATE sqlite_sequence SET seq = max(seq, ?) WHERE name = 'probe_task'`, seq.Int64)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			if _, err := tx.Exec(`INSERT INTO sqlite_sequence (name, seq) VALUES ('probe_task', ?)`, seq.Int64); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(`ALTER TABLE probe_cert ADD COLUMN config_id BLOB`); err != nil {
		return err
	}
	if _, err := tx.Exec(ddlProbeCertPresentedV35); err != nil {
		return err
	}
	if n > 0 {
		// 墙钟而不是注入的时钟：迁移发生在 Open 里，那时还没有把 Store 的时钟交给这条路径。
		// 与 bumpProbeVersion 同一条 SQL，已有计数大于当前秒时仍然 +1，不会停在 agent 已经见过的值。
		if _, err := bumpProbeVersion(tx, time.Now().Unix()); err != nil {
			return err
		}
	}
	return nil
}

// metricDDLV34 冻结引入 load1_per_core 时的指标表 DDL。列集合与当时的描述表一致；以后再追加指标必须新开迁移，不能改这里。
func metricDDLV34(table string) string {
	type col struct {
		name, sqlType string
		max           bool
	}
	cols := []col{
		{"cpu", "REAL", true},
		{"mem_used", "INTEGER", true},
		{"swap_used", "INTEGER", false},
		{"disk_used", "INTEGER", false},
		{"load1", "REAL", false},
		{"tcp", "INTEGER", false},
		{"udp", "INTEGER", false},
		{"procs", "INTEGER", false},
		{"rx_bytes", "INTEGER", false},
		{"tx_bytes", "INTEGER", false},
		{"memory_used_pct", "REAL", false},
		{"disk_used_pct", "REAL", false},
		{"net_rx_bps", "INTEGER", true},
		{"net_tx_bps", "INTEGER", true},
		{"disk_read_bps", "INTEGER", true},
		{"disk_write_bps", "INTEGER", true},
		{"cpu_steal_pct", "REAL", true},
		{"cpu_iowait_pct", "REAL", true},
		{"load1_per_core", "REAL", false},
	}
	parts := []string{"node_id INTEGER NOT NULL", "ts INTEGER NOT NULL"}
	for _, c := range cols {
		parts = append(parts, c.name+"_sum "+c.sqlType+" NOT NULL DEFAULT 0", c.name+"_n INTEGER NOT NULL DEFAULT 0")
		if c.max {
			parts = append(parts, c.name+"_max "+c.sqlType+" NOT NULL DEFAULT 0")
		}
	}
	// rebuildTable 用 name_new 建临时表，表名不是 metric_1m 本身。覆盖列按去掉这个后缀后的表决定：
	// 只有 1 分钟表有 reported，上卷表才有 minutes/both。
	if strings.TrimSuffix(table, "_new") == "metric_1m" {
		parts = append(parts, "reported INTEGER NOT NULL DEFAULT 1", "observed INTEGER")
	} else {
		parts = append(parts, "minutes INTEGER", "observed INTEGER", "both INTEGER")
	}
	return "CREATE TABLE " + table + " (" + strings.Join(parts, ", ") + ", PRIMARY KEY (node_id, ts)) WITHOUT ROWID"
}

// execution 缺省 'null'：旧行没有上报执行环境，空对象会被校验拒绝（kind 未指定）。
// facts_rev 缺省 0：本列出现之前写入的摘要只覆盖更少的列，不能当作当前字段集合已经确认。
var migrationV34Config = []string{
	`ALTER TABLE node_facts ADD COLUMN execution TEXT NOT NULL DEFAULT 'null'`,
	`ALTER TABLE node_facts ADD COLUMN facts_rev INTEGER NOT NULL DEFAULT 0`,
}

// v33：探测表加 (task_id, node_id, ts) 索引，承载跨节点对比的按任务读取；不改动任何数据，
// 旧行为全部不变，只是多出一条读取路径。仅指标层：探测表属于指标层，配置层无变化。
// 列序的理由见 probeByTaskIndexV33：前导等值键之后 ts 才能作为范围约束进入同一个 SEARCH。
var migrationV33Metrics = []string{
	probeByTaskIndexV33("probe_1m"),
	probeByTaskIndexV33("probe_5m"),
	probeByTaskIndexV33("probe_1h"),
}

func probeByTaskIndexV33(table string) string {
	return "CREATE INDEX " + table + "_by_task ON " + table + " (task_id, node_id, ts)"
}

// v32：节点的公开备注。旧库升级后没有公开备注：空串与"未设置"同值，公开端对空串不下发有意义的字段。
var migrationV32Config = []string{`ALTER TABLE node ADD COLUMN public_remark TEXT NOT NULL DEFAULT ''`}

var migrationV31Metrics = []string{
	`ALTER TABLE metric_1m ADD COLUMN reported INTEGER NOT NULL DEFAULT 1`,
	`ALTER TABLE metric_1m ADD COLUMN observed INTEGER`,
	`ALTER TABLE metric_5m ADD COLUMN minutes INTEGER`,
	`ALTER TABLE metric_5m ADD COLUMN observed INTEGER`,
	`ALTER TABLE metric_5m ADD COLUMN both INTEGER`,
	`ALTER TABLE metric_1h ADD COLUMN minutes INTEGER`,
	`ALTER TABLE metric_1h ADD COLUMN observed INTEGER`,
	`ALTER TABLE metric_1h ADD COLUMN both INTEGER`,
	`CREATE TABLE node_coverage (node_id INTEGER PRIMARY KEY, start_ts INTEGER NOT NULL)`,
	`INSERT INTO node_coverage SELECT node_id, min(ts) FROM (
	 SELECT node_id, ts FROM metric_1m UNION ALL SELECT node_id, ts FROM metric_5m UNION ALL SELECT node_id, ts FROM metric_1h
	) GROUP BY node_id`,
}

// 完整库有 node；指标快照没有配置层，只能从自身留存行回填起点。
const coverageFallbackV31 = `INSERT INTO node_coverage SELECT id, last_seen_at - last_seen_at % 60 FROM node
 WHERE last_seen_at IS NOT NULL AND id NOT IN (SELECT node_id FROM node_coverage)`

// v30：HTTPS 证书到期观测的"最新值"表。旧库升级后没有行：升级前没有证书观测，空表就是"无读数"，
// 证书到期评估对没有行的 (节点, 任务) 不评估，与从未上报过证书同态。
var migrationV30Config = []string{ddlProbeCertV30}

const ddlProbeCertV30 = `CREATE TABLE probe_cert (
  node_id INTEGER NOT NULL,
  task_id INTEGER NOT NULL,
  not_after INTEGER NOT NULL,
  observed_at INTEGER NOT NULL,
  PRIMARY KEY (node_id, task_id)
) WITHOUT ROWID`

// v29：DNS 探测任务要查询的解析器。旧行取空串：升级前没有 DNS 任务，空串与"非 DNS 任务不携带该字段"同值。
var migrationV29Config = []string{`ALTER TABLE probe_task ADD COLUMN dns_server TEXT NOT NULL DEFAULT ''`}

// 新列的 n=0 表示旧行没有该指标的历史采样，不把缺失伪装成已测的零流量或零占用。
var migrationV28Metrics = func() []string {
	columns := []struct{ name, typ string }{
		{"disk_read_bps", "INTEGER"},
		{"disk_write_bps", "INTEGER"},
		{"cpu_steal_pct", "REAL"},
		{"cpu_iowait_pct", "REAL"},
	}
	var out []string
	for _, table := range []string{"metric_1m", "metric_5m", "metric_1h"} {
		for _, c := range columns {
			out = append(out, "ALTER TABLE "+table+" ADD COLUMN "+c.name+"_sum "+c.typ+" NOT NULL DEFAULT 0",
				"ALTER TABLE "+table+" ADD COLUMN "+c.name+"_n INTEGER NOT NULL DEFAULT 0",
				"ALTER TABLE "+table+" ADD COLUMN "+c.name+"_max "+c.typ+" NOT NULL DEFAULT 0")
		}
	}
	return out
}()

// v27：维护静默的存储结构。新增列都带 NOT NULL 默认值，使旧行取「未维护、未静默」——不得把历史误标成维护中或已静默。
var migrationV27Config = []string{
	`ALTER TABLE node ADD COLUMN maintenance INTEGER NOT NULL DEFAULT 0`,
	`CREATE TABLE silence (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL,
  enabled INTEGER NOT NULL DEFAULT 1,
  all_nodes INTEGER NOT NULL DEFAULT 0,
  kind TEXT NOT NULL,
  start_hhmm TEXT NOT NULL DEFAULT '',
  end_hhmm TEXT NOT NULL DEFAULT '',
  from_at INTEGER NOT NULL DEFAULT 0,
  until_at INTEGER NOT NULL DEFAULT 0,
  reason TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
)`,
	`CREATE TABLE silence_node (
  silence_id INTEGER NOT NULL,
  node_id INTEGER NOT NULL,
  PRIMARY KEY (silence_id, node_id)
)`,
	`CREATE INDEX silence_node_by_node ON silence_node(node_id)`,
	`CREATE TABLE silence_tag (
  silence_id INTEGER NOT NULL,
  tag_id INTEGER NOT NULL,
  PRIMARY KEY (silence_id, tag_id)
) WITHOUT ROWID`,
	`CREATE INDEX silence_tag_by_tag ON silence_tag (tag_id)`,
	`ALTER TABLE alert_event ADD COLUMN silenced INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE alert_state ADD COLUMN fired_silenced INTEGER NOT NULL DEFAULT 0`,
}

// 旧版未报告诊断和统计作用域；空值保留未知，不伪造健康或已确认的基线。
var migrationV26Config = []string{
	`ALTER TABLE node_facts ADD COLUMN diagnostics TEXT NOT NULL DEFAULT 'null'`,
	`ALTER TABLE traffic ADD COLUMN net_counter_epoch TEXT NOT NULL DEFAULT ''`,
}

const ddlRegisterWindowV25 = `CREATE TABLE register_window (owner_id INTEGER PRIMARY KEY,key_hash BLOB NOT NULL UNIQUE,expires_at INTEGER NOT NULL,remaining INTEGER NOT NULL)`

var migrationV25Config = []string{
	"ALTER TABLE api_token ADD COLUMN permissions TEXT NOT NULL DEFAULT '[]'",
	"ALTER TABLE api_token ADD COLUMN all_nodes INTEGER NOT NULL DEFAULT 1 CHECK (all_nodes IN (0, 1))",
	`CREATE TABLE api_token_node (token_id INTEGER NOT NULL,node_id INTEGER NOT NULL,PRIMARY KEY(token_id,node_id)) WITHOUT ROWID`,
	`CREATE TABLE operation (owner_key TEXT NOT NULL,owner_id INTEGER NOT NULL,request_id TEXT NOT NULL,request_hash TEXT NOT NULL,action TEXT NOT NULL,resource_id INTEGER NOT NULL,before_json TEXT NOT NULL,after_json TEXT NOT NULL,committed_at INTEGER NOT NULL,PRIMARY KEY(owner_key,request_id)) WITHOUT ROWID`,
	`CREATE INDEX operation_by_owner ON operation(owner_id,committed_at DESC,request_id)`,
	`CREATE INDEX operation_details_by_time ON operation(committed_at) WHERE before_json!='' OR after_json!=''`,
}

const ddlNodeUpdateV24 = `CREATE TABLE node_update (node_id INTEGER PRIMARY KEY, data TEXT NOT NULL)`

var migrationV23 = []string{`ALTER TABLE node_facts ADD COLUMN network TEXT NOT NULL DEFAULT '{}'`}

var migrationV21Config = []string{`ALTER TABLE probe_task ADD COLUMN sort_order INTEGER NOT NULL DEFAULT 0`}

// 新列的 n=0 表示历史没有速率采样，不把旧平均速率伪装成已测峰值。
var migrationV21Metrics = func() []string {
	var out []string
	for _, table := range []string{"metric_1m", "metric_5m", "metric_1h"} {
		for _, column := range []string{"net_rx_bps", "net_tx_bps"} {
			for _, suffix := range []string{"_sum", "_n", "_max"} {
				out = append(out, "ALTER TABLE "+table+" ADD COLUMN "+column+suffix+" INTEGER NOT NULL DEFAULT 0")
			}
		}
	}
	return out
}()

var migrationV20 = func() []string {
	out := []string{
		`CREATE TABLE admin_security (id INTEGER PRIMARY KEY CHECK (id = 1), generation INTEGER NOT NULL DEFAULT 0, data TEXT NOT NULL DEFAULT '{}')`,
		`INSERT INTO admin_security (id) VALUES (1)`,
		`CREATE TABLE probe_task_tag (task_id INTEGER NOT NULL, tag_id INTEGER NOT NULL, PRIMARY KEY (task_id, tag_id)) WITHOUT ROWID`,
		`CREATE INDEX probe_task_tag_by_tag ON probe_task_tag (tag_id)`,
		`CREATE TABLE alert_rule_tag (rule_id INTEGER NOT NULL, tag_id INTEGER NOT NULL, PRIMARY KEY (rule_id, tag_id)) WITHOUT ROWID`,
		`CREATE INDEX alert_rule_tag_by_tag ON alert_rule_tag (tag_id)`,
		`ALTER TABLE alert_rule ADD COLUMN resource_metric TEXT`,
		`ALTER TABLE alert_rule ADD COLUMN recovery_threshold REAL`,
	}
	for _, table := range []string{"metric_1m", "metric_5m", "metric_1h"} {
		for _, column := range []string{"memory_used_pct", "disk_used_pct"} {
			out = append(out, "ALTER TABLE "+table+" ADD COLUMN "+column+"_sum REAL NOT NULL DEFAULT 0",
				"ALTER TABLE "+table+" ADD COLUMN "+column+"_n INTEGER NOT NULL DEFAULT 0")
		}
	}
	return out
}()

const ddlRestoreRecordV17 = `CREATE TABLE restore_record (
  id TEXT PRIMARY KEY NOT NULL,
  restored_at INTEGER NOT NULL,
  config_taken_at INTEGER NOT NULL,
  metrics_taken_at INTEGER,
  orphans TEXT NOT NULL
)`

func execAll(stmts []string) func(*sql.Tx) error {
	return func(tx *sql.Tx) error {
		for _, stmt := range stmts {
			if _, err := tx.Exec(stmt); err != nil {
				return fmt.Errorf("%w in %q", err, stmt)
			}
		}
		return nil
	}
}

// 旧库没有原始 zip，不能由展开文件重造；原包由管理员重新上传后补齐。
var migrationV19 = []string{`CREATE TABLE theme_package (
  theme_id TEXT PRIMARY KEY,
  content BLOB NOT NULL,
  revision INTEGER NOT NULL CHECK (revision > 0),
  uploaded INTEGER NOT NULL DEFAULT 0 CHECK (uploaded IN (0, 1))
)`, `ALTER TABLE restore_record ADD COLUMN themes TEXT NOT NULL DEFAULT '{}'`}

// 各迁移的输入逐字冻结在这里。当前版本的 DDL 只给 schemaStatements 用；迁移若引用它，DDL 以后
// 一变，旧库升级就会建出与后续迁移假设不符的表（例如先带上新列，再在加列的迁移里撞上重复列）。
// 冻结点是该 schema 版本在仓库里的最后状态（下一次升版本的父提交），不是迁移引入时：版本 5 期间
// 告警表在不升版本的情况下被改过（加了 alert_rule.all_nodes、alert_delivery.done 等），之后的迁移
// 与 migrate_test 的 schemaV5 夹具都以最后状态为准。

// v2（b5feb18）：管理员、会话与 5m/1h 指标表。指标表此时还没有流量列与列默认值，由迁移 3 重建。
var migrationV2 = []string{
	`CREATE TABLE admin (
  -- 单管理员：CHECK 让第二行无法插入，"多用户"在 schema 上就不成立。
  id INTEGER PRIMARY KEY CHECK (id = 1),
  -- PHC 字符串，argon2id 的参数随哈希走：改参数不需要迁移，旧哈希按自带参数校验。
  password_hash TEXT NOT NULL,
  updated_at INTEGER NOT NULL
)`,
	`CREATE TABLE admin_session (
  token_hash BLOB PRIMARY KEY,
  created_at INTEGER NOT NULL,
  last_used_at INTEGER NOT NULL,
  -- 绝对过期，墙钟 Unix 秒。会话要跨 hub 重启存活，只能用墙钟；
  -- 墙钟回拨会推迟按绝对过期时刻判定失效的时间。
  expires_at INTEGER NOT NULL
)`,
	`CREATE TABLE metric_5m (node_id INTEGER NOT NULL, ts INTEGER NOT NULL, cpu_sum REAL NOT NULL, cpu_n INTEGER NOT NULL, cpu_max REAL NOT NULL, mem_used_sum INTEGER NOT NULL, mem_used_n INTEGER NOT NULL, mem_used_max INTEGER NOT NULL, swap_used_sum INTEGER NOT NULL, swap_used_n INTEGER NOT NULL, disk_used_sum INTEGER NOT NULL, disk_used_n INTEGER NOT NULL, load1_sum REAL NOT NULL, load1_n INTEGER NOT NULL, tcp_sum INTEGER NOT NULL, tcp_n INTEGER NOT NULL, udp_sum INTEGER NOT NULL, udp_n INTEGER NOT NULL, procs_sum INTEGER NOT NULL, procs_n INTEGER NOT NULL, PRIMARY KEY (node_id, ts)) WITHOUT ROWID`,
	`CREATE TABLE metric_1h (node_id INTEGER NOT NULL, ts INTEGER NOT NULL, cpu_sum REAL NOT NULL, cpu_n INTEGER NOT NULL, cpu_max REAL NOT NULL, mem_used_sum INTEGER NOT NULL, mem_used_n INTEGER NOT NULL, mem_used_max INTEGER NOT NULL, swap_used_sum INTEGER NOT NULL, swap_used_n INTEGER NOT NULL, disk_used_sum INTEGER NOT NULL, disk_used_n INTEGER NOT NULL, load1_sum REAL NOT NULL, load1_n INTEGER NOT NULL, tcp_sum INTEGER NOT NULL, tcp_n INTEGER NOT NULL, udp_sum INTEGER NOT NULL, udp_n INTEGER NOT NULL, procs_sum INTEGER NOT NULL, procs_n INTEGER NOT NULL, PRIMARY KEY (node_id, ts)) WITHOUT ROWID`,
}

// v3（fce35e9）：流量表与按当时描述表重建的指标表。
const ddlTrafficV3 = `CREATE TABLE traffic (
  node_id INTEGER PRIMARY KEY,
  boot_id TEXT NOT NULL,
  last_rx INTEGER NOT NULL, -- -1 表示尚无基线，与 traffic.NoBaseline 同值；由校正建立的条目才有。
  last_tx INTEGER NOT NULL, -- -1 的含义与 last_rx 相同。
  total_rx INTEGER NOT NULL,
  total_tx INTEGER NOT NULL,
  period_rx INTEGER NOT NULL,
  period_tx INTEGER NOT NULL,
  -- 当前周期起点，Unix 秒；重置日零点按 hub 时区换算。
  period_start INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
)`

var metricTablesV3 = []string{"metric_1m", "metric_5m", "metric_1h"}

// rebuildTable 以 name_new 建临时表，所以冻结的是按表名生成的函数，列定义取自 fce35e9 的 metricDDL。
func metricDDLV3(table string) string {
	return "CREATE TABLE " + table + " (" + metricColumnsV3 + ") WITHOUT ROWID"
}

const metricColumnsV3 = `node_id INTEGER NOT NULL, ts INTEGER NOT NULL, cpu_sum REAL NOT NULL DEFAULT 0, cpu_n INTEGER NOT NULL DEFAULT 0, cpu_max REAL NOT NULL DEFAULT 0, mem_used_sum INTEGER NOT NULL DEFAULT 0, mem_used_n INTEGER NOT NULL DEFAULT 0, mem_used_max INTEGER NOT NULL DEFAULT 0, swap_used_sum INTEGER NOT NULL DEFAULT 0, swap_used_n INTEGER NOT NULL DEFAULT 0, disk_used_sum INTEGER NOT NULL DEFAULT 0, disk_used_n INTEGER NOT NULL DEFAULT 0, load1_sum REAL NOT NULL DEFAULT 0, load1_n INTEGER NOT NULL DEFAULT 0, tcp_sum INTEGER NOT NULL DEFAULT 0, tcp_n INTEGER NOT NULL DEFAULT 0, udp_sum INTEGER NOT NULL DEFAULT 0, udp_n INTEGER NOT NULL DEFAULT 0, procs_sum INTEGER NOT NULL DEFAULT 0, procs_n INTEGER NOT NULL DEFAULT 0, rx_bytes_sum INTEGER NOT NULL DEFAULT 0, rx_bytes_n INTEGER NOT NULL DEFAULT 0, tx_bytes_sum INTEGER NOT NULL DEFAULT 0, tx_bytes_n INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (node_id, ts)`

// v4（1996a98）：探测任务、分配、版本与探测表族。
var migrationV4 = []string{
	`CREATE TABLE probe_task (
  -- AUTOINCREMENT：历史行只带 task_id，删除任务后 id 若复用，旧历史会挂到新任务上。
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  kind INTEGER NOT NULL,
  target TEXT NOT NULL,
  interval_s INTEGER NOT NULL,
  timeout_ms INTEGER NOT NULL,
  created_at INTEGER NOT NULL
)`,
	`CREATE TABLE probe_task_node (
  task_id INTEGER NOT NULL,
  node_id INTEGER NOT NULL,
  PRIMARY KEY (task_id, node_id)
) WITHOUT ROWID`,
	`CREATE INDEX probe_task_node_by_node ON probe_task_node (node_id)`,
	`CREATE TABLE probe_meta (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  version INTEGER NOT NULL
)`,
	`INSERT INTO probe_meta (id, version) VALUES (1, 0)`,
	`INSERT INTO rollup_state (level, upto_ts) VALUES ('probe_5m', 0), ('probe_1h', 0)`,
	`CREATE TABLE probe_1m (
  node_id INTEGER NOT NULL, ts INTEGER NOT NULL, task_id INTEGER NOT NULL,
  sent INTEGER NOT NULL DEFAULT 0, lost INTEGER NOT NULL DEFAULT 0, errors INTEGER NOT NULL DEFAULT 0,
  rtt_sum_us INTEGER NOT NULL DEFAULT 0, rtt_min_us INTEGER, rtt_max_us INTEGER,
  PRIMARY KEY (node_id, ts, task_id)
) WITHOUT ROWID`,
	`CREATE TABLE probe_5m (
  node_id INTEGER NOT NULL, ts INTEGER NOT NULL, task_id INTEGER NOT NULL,
  sent INTEGER NOT NULL DEFAULT 0, lost INTEGER NOT NULL DEFAULT 0, errors INTEGER NOT NULL DEFAULT 0,
  rtt_sum_us INTEGER NOT NULL DEFAULT 0, rtt_min_us INTEGER, rtt_max_us INTEGER,
  PRIMARY KEY (node_id, ts, task_id)
) WITHOUT ROWID`,
	`CREATE TABLE probe_1h (
  node_id INTEGER NOT NULL, ts INTEGER NOT NULL, task_id INTEGER NOT NULL,
  sent INTEGER NOT NULL DEFAULT 0, lost INTEGER NOT NULL DEFAULT 0, errors INTEGER NOT NULL DEFAULT 0,
  rtt_sum_us INTEGER NOT NULL DEFAULT 0, rtt_min_us INTEGER, rtt_max_us INTEGER,
  PRIMARY KEY (node_id, ts, task_id)
) WITHOUT ROWID`,
}

// v5（9140ab1）：告警各表。
var alertStatementsV5 = []string{
	`CREATE TABLE alert_rule (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL,
  kind TEXT NOT NULL,
  enabled INTEGER NOT NULL DEFAULT 1,
  all_nodes INTEGER NOT NULL DEFAULT 0,
  task_id INTEGER,
  metric TEXT,
  threshold REAL,
  for_minutes INTEGER,
  created_at INTEGER NOT NULL
)`,
	`CREATE TABLE alert_rule_node (
  rule_id INTEGER NOT NULL,
  node_id INTEGER NOT NULL,
  PRIMARY KEY (rule_id, node_id)
)`,
	`CREATE INDEX alert_rule_node_by_node ON alert_rule_node(node_id)`,
	`CREATE TABLE alert_rule_channel (
  rule_id INTEGER NOT NULL,
  channel_id INTEGER NOT NULL,
  PRIMARY KEY (rule_id, channel_id)
)`,
	`CREATE INDEX alert_rule_channel_by_channel ON alert_rule_channel(channel_id)`,
	`CREATE TABLE notify_channel (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL,
  kind TEXT NOT NULL,
  config TEXT NOT NULL,
  created_at INTEGER NOT NULL
)`,
	`CREATE TABLE alert_state (
  rule_id INTEGER NOT NULL,
  node_id INTEGER NOT NULL,
  state TEXT NOT NULL,
  since_at INTEGER NOT NULL,
  PRIMARY KEY (rule_id, node_id)
)`,
	`CREATE TABLE alert_event (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  rule_id INTEGER NOT NULL,
  node_id INTEGER NOT NULL,
  transition TEXT NOT NULL,
  at INTEGER NOT NULL,
  summary TEXT NOT NULL,
  value REAL NOT NULL
)`,
	`CREATE INDEX alert_event_by_node ON alert_event(node_id, id)`,
	`CREATE INDEX alert_event_by_at ON alert_event(at)`,
	`CREATE TABLE alert_delivery (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  event_id INTEGER NOT NULL,
  channel_id INTEGER NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0,
  ok INTEGER NOT NULL DEFAULT 0,
  done INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  delivered_at INTEGER
)`,
	`CREATE INDEX alert_delivery_by_event ON alert_delivery(event_id)`,
	`CREATE INDEX alert_delivery_pending ON alert_delivery(done, id)`,
}

// v6（8d989b0）：API token。
const ddlAPITokenV6 = `CREATE TABLE api_token (
  -- AUTOINCREMENT：id 永不复用。吊销按 id 进行，复用会让针对旧 token 的吊销落到新 token 上。
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL,
  -- 整串明文（含前缀）的 SHA-256；明文不落库。
  token_hash BLOB NOT NULL UNIQUE,
  created_at INTEGER NOT NULL,
  -- NULL 表示从未使用。只供展示：距已落库值满一分钟才刷新。
  last_used_at INTEGER
)`

// 旧版本把类别与原文混写在 last_error 里。只按旧版本写入的确定形状归类，其余失败无法确定类别，
// 归入 unclassified 且原文不动；成功行与尚无结果（未终态且没有文本）的行不碰。终态失败却没有
// 文本的行旧版本不会写出，若有也归 unclassified，不能迁成"没有失败"。
// 下面两个字符串是旧版本写入 last_error 的文本，是库里的历史值，不随代码变。
// 旧 HTTP 失败的格式是 "HTTP %d %s: %s"：%d 不带前导 0，所以只认首位 1–9 的三位数，写入的状态码
// 因而落在 100–999（http_status 列的不变式）；http.StatusText 不含 ": "，所以第一个 ": " 之后就是
// 响应体片段；没有 ": " 的不是这个格式写出的，不按 HTTP 形状归类。
//
// 历史数据的边界：旧版本在"次数耗尽而最后一次结果未落盘"时沿用更早一次的失败作终态，写下的行
// 与"最后一次真的以同样原因失败"在库里分不出来，这里按那次失败归类（例如 http_status 503）；
// 这部分历史无法修正。只有一次失败都没记下的行才带着 result_unrecorded 的旧文本。
func migrateDeliveryFailure(tx *sql.Tx) error {
	for _, stmt := range []string{
		`ALTER TABLE alert_delivery ADD COLUMN failure TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE alert_delivery ADD COLUMN http_status INTEGER`,
		`UPDATE alert_delivery SET failure = 'channel_deleted', last_error = ''
		  WHERE ok = 0 AND last_error = 'channel deleted'`,
		`UPDATE alert_delivery SET failure = 'result_unrecorded', last_error = ''
		  WHERE ok = 0 AND last_error = 'attempts exhausted but last result was not recorded'`,
		`UPDATE alert_delivery SET failure = 'http_status', http_status = CAST(substr(last_error, 6, 3) AS INTEGER),
		    last_error = substr(last_error, instr(last_error, ': ') + 2)
		  WHERE ok = 0 AND failure = '' AND last_error GLOB 'HTTP [1-9][0-9][0-9] *' AND instr(last_error, ': ') > 0`,
		`UPDATE alert_delivery SET failure = 'unclassified'
		  WHERE ok = 0 AND failure = '' AND (last_error <> '' OR done = 1)`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("%w in %q", err, stmt)
		}
	}
	return nil
}

// v8：全站设置的键值表。
const ddlSettingV8 = `CREATE TABLE setting (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
)`

// v9：节点的计费与到期五列、到期规则的提前天数、到期告警状态记下的触发时到期日。旧行取列默认值：没有计费信息，
// 规则的 days_before 为 NULL，已有状态的 fired_expires_on 为空（它们都属于离线与探测规则）。
var migrationV9 = []string{
	`ALTER TABLE node ADD COLUMN price TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE node ADD COLUMN currency TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE node ADD COLUMN billing_cycle TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE node ADD COLUMN expires_on TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE node ADD COLUMN auto_renew INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE alert_rule ADD COLUMN days_before INTEGER`,
	`ALTER TABLE alert_state ADD COLUMN fired_expires_on TEXT NOT NULL DEFAULT ''`,
}

// v10：探测任务的全部节点开关。旧任务取默认值 0，保持原来的显式分配。
var migrationV10 = []string{
	`ALTER TABLE probe_task ADD COLUMN all_nodes INTEGER NOT NULL DEFAULT 0`,
}

// v11：维护任务的簿记表。旧库升级后没有行，读侧按"从未成功跑过"呈现，直到下一轮成功的上卷与 prune 写入。
const ddlMaintenanceStateV11 = `CREATE TABLE maintenance_state (
  name TEXT PRIMARY KEY,
  finished_at INTEGER NOT NULL
)`

// v12：离线规则×节点上次恢复的时刻。旧行取 NULL（从未恢复过）：升级前的恢复没有记录，升级后的第一次恢复开始计窗口。
var migrationV12 = []string{
	`ALTER TABLE alert_state ADD COLUMN recovered_at INTEGER`,
}

// v13：节点最近一次上报的来源地址。旧行取空串：hub 在这个版本之前不记录来源，不区分从未上报与升级前上报过
// 但此后未再上报（这类节点的 last_seen_at 仍停在升级前的值）；下一次上报后随分钟行刷出补上。
var migrationV13 = []string{
	`ALTER TABLE node ADD COLUMN last_source TEXT NOT NULL DEFAULT ''`,
}

// v14：节点的国家（查得值与所属地址成对）与手动指定的国家。旧行取空串：没有国家；查询开启后按来源地址补查。
var migrationV14 = []string{
	`ALTER TABLE node ADD COLUMN country TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE node ADD COLUMN country_ip TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE node ADD COLUMN country_pin TEXT NOT NULL DEFAULT ''`,
}

// v15：节点标签与关联。旧库升级后没有标签，节点的标签集合为空。
var migrationV15 = []string{
	`CREATE TABLE tag (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL,
  name_fold TEXT NOT NULL UNIQUE
)`,
	`CREATE TABLE node_tag (
  node_id INTEGER NOT NULL,
  tag_id INTEGER NOT NULL,
  PRIMARY KEY (node_id, tag_id)
) WITHOUT ROWID`,
	`CREATE INDEX node_tag_by_tag ON node_tag (tag_id)`,
}

// v16：公开页主题的 theme 与 theme_file 两张表。
var migrationV16 = []string{
	`CREATE TABLE theme (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  version TEXT NOT NULL,
  preview TEXT NOT NULL,
  uploaded_at INTEGER NOT NULL,
  enabled INTEGER NOT NULL DEFAULT 0 CHECK (enabled IN (0, 1))
)`,
	`CREATE UNIQUE INDEX theme_enabled ON theme (enabled) WHERE enabled = 1`,
	`CREATE TABLE theme_file (
  theme_id TEXT NOT NULL,
  path TEXT NOT NULL,
  content BLOB NOT NULL,
  PRIMARY KEY (theme_id, path)
)`,
}

// v18：投递的发送批次、下一次尝试的最早时刻与渠道的出站节奏。
//
// alert_delivery 重建而不是 ADD COLUMN：batch_id 要 NOT NULL，而 ADD COLUMN 的 NOT NULL 列必须带一个对所有旧行都相同的
// 默认值，给不出"等于自己的 id"。升级前每行各自发送过，所以旧行各成一批（batch_id = id）；not_before 取 0（没有限制）。
// 重建时旧表的 sqlite_sequence 一并带到新表：batch_id 就是行 id，序列若退回到现存最大的 id，第一行已被清理的批次号
// 会被新行复用。新表以 alert_delivery_v18 建成、复制、接上序列，再删旧表并改名——改名时 SQLite 连 sqlite_sequence 里
// 的表名一起改。待投递行的索引改按批次号与渠道排列（续投按批次装填、窗口按渠道判节奏）。
// 旧渠道按种类取缺省节奏：Telegram 20（群聊的文档值），Webhook 0（不限）。
var migrationV18 = []string{
	`CREATE TABLE alert_delivery_v18 (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  event_id INTEGER NOT NULL,
  channel_id INTEGER NOT NULL,
  batch_id INTEGER NOT NULL CHECK (batch_id > 0),
  attempts INTEGER NOT NULL DEFAULT 0,
  ok INTEGER NOT NULL DEFAULT 0,
  done INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  delivered_at INTEGER,
  failure TEXT NOT NULL DEFAULT '',
  http_status INTEGER,
  not_before INTEGER NOT NULL DEFAULT 0
)`,
	`INSERT INTO alert_delivery_v18 (id, event_id, channel_id, batch_id, attempts, ok, done, last_error, delivered_at, failure, http_status)
  SELECT id, event_id, channel_id, id, attempts, ok, done, last_error, delivered_at, failure, http_status FROM alert_delivery`,
	`DELETE FROM sqlite_sequence WHERE name = 'alert_delivery_v18'`,
	`INSERT INTO sqlite_sequence (name, seq) SELECT 'alert_delivery_v18', seq FROM sqlite_sequence WHERE name = 'alert_delivery'`,
	`DROP TABLE alert_delivery`,
	`ALTER TABLE alert_delivery_v18 RENAME TO alert_delivery`,
	`CREATE INDEX alert_delivery_by_event ON alert_delivery(event_id)`,
	`CREATE INDEX alert_delivery_pending ON alert_delivery(done, batch_id, channel_id)`,
	`CREATE INDEX alert_delivery_by_batch ON alert_delivery(batch_id)`,
	`ALTER TABLE notify_channel ADD COLUMN rate_per_minute INTEGER NOT NULL DEFAULT 0 CHECK (rate_per_minute >= 0)`,
	`UPDATE notify_channel SET rate_per_minute = 20 WHERE kind = 'telegram'`,
}
