package store

import (
	"slices"
	"strings"

	"github.com/xjetry/heron/internal/hub/metric"
)

// 每张表一个常量：全新建库与增量迁移复用同一段 DDL，不存在第二份字段清单。
const ddlNode = `CREATE TABLE node (
  -- AUTOINCREMENT 使 id 永不复用：分层备份恢复后两层可能各自漂移，
  -- id 若复用，指标层里已删节点的历史会挂到同 id 的新节点上且无法肉眼分辨。
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL,
  sort_order INTEGER NOT NULL DEFAULT 0,
  public INTEGER NOT NULL DEFAULT 0,
  note TEXT NOT NULL DEFAULT '',
  -- NULL 表示"用默认值（TTL）"，是缺省不是放宽；读侧遇 NULL 必须取 TTL。
  offline_grace_s INTEGER,
  traffic_reset_day INTEGER NOT NULL DEFAULT 1,
  token_hash BLOB NOT NULL UNIQUE,
  created_at INTEGER NOT NULL,
  -- 墙钟，供展示、告警文案与 hub 重启后抖动窗口判定里的离线开始，不参与离线时长计算。
  last_seen_at INTEGER,
  -- 计费与到期：提醒用的展示值，空串与 0 是"未填"。取值约束由 api 的 UpdateNode 裁决，库里不设 CHECK。
  -- 列序与迁移 9 的 ADD COLUMN 结果一致。
  price TEXT NOT NULL DEFAULT '',
  currency TEXT NOT NULL DEFAULT '',
  billing_cycle TEXT NOT NULL DEFAULT '',
  expires_on TEXT NOT NULL DEFAULT '',
  auto_renew INTEGER NOT NULL DEFAULT 0,
  -- 最近一次上报的来源地址（auth.SourceText 的规范文本），空串表示 hub 没有记录到来源：从未上报，或最近一次
  -- 上报早于 hub 开始记录来源的版本（此时 last_seen_at 有值）。与 last_seen_at 同一路径写入：分钟行刷出
  -- 与退出时由 WriteMinuteBatch 写，上报路径只碰内存。只存最后一个，是观测事实，不设手动覆盖。
  -- 列序与迁移 13 的 ADD COLUMN 结果一致。
  last_source TEXT NOT NULL DEFAULT '',
  -- 国家 / 地区，ISO 3166-1 alpha-2，空串表示没有。列序与迁移 14 的 ADD COLUMN 结果一致。
  -- country 与 country_ip 成对：country 是对 country_ip 这个地址的查询答案，不是节点属性，换了出口的节点不得沿用
  -- 旧答案。不变式 country_ip ∈ {'', last_source} 且 country 与 country_ip 同空同非空，由两个写者各自承载：
  -- WriteMinuteBatch 写入与 country_ip 不同的来源时同一条语句清空两列；SetLookupCountry 只在 last_source 仍是
  -- 所查地址时写入两列，并自己拒绝空地址与不是国家码的值。查询器本就只查非空的来源、只写国家码，写者的检查让
  -- 不变式不依赖这一点。
  country TEXT NOT NULL DEFAULT '',
  country_ip TEXT NOT NULL DEFAULT '',
  -- 管理员手动指定的国家，只由 UpdateNode 写；查得两列的写者（WriteMinuteBatch、SetLookupCountry）不碰它，
  -- UpdateNode 也不碰查得两列。没有哪个写者同时写两边，手动值不会被查询覆盖，清空手动值即回落到查得值，冲突不需要
  -- 裁决（显示值见 Node.DisplayCountry）。
  country_pin TEXT NOT NULL DEFAULT '',
  -- 维护状态：为真时该节点按维护静默语义暂停告警投递，读侧据此展示"维护中"。列序与迁移 27 的 ADD COLUMN 结果一致。
  maintenance INTEGER NOT NULL DEFAULT 0,
  -- 公开备注：站长写给访客的一行说明（如线路类型），空串表示没有。准入在 api 的 cleanPublicRemark（单行、
  -- 至多 100 个码点、不含控制字符），与私有备注 note 并列。列序与迁移 32 的 ADD COLUMN 结果一致。
  public_remark TEXT NOT NULL DEFAULT '',
  traffic_quota_bytes INTEGER NOT NULL DEFAULT 0,
  traffic_quota_mode TEXT NOT NULL DEFAULT 'sum',
  -- 管理员手填的出口地址（出口地址手填），空串表示未填、显示 agent 探测值。非空即该族的公网单播地址的规范形：
  -- 唯一的写者 UpdateNodeTasks 用 netaddr.ParsePublicFamily 裁决并写 String()，与 agent 自报出口同一个谓词，
  -- 显示值"AVAILABLE 的地址是该族公网地址"由此成立。只决定显示值、公开标记与是否让 agent 停探该族，不参与国家查询
  -- （查询按 last_source）。列序与迁移 40 的 ADD COLUMN 结果一致。
  ipv4_pin TEXT NOT NULL DEFAULT '',
  ipv6_pin TEXT NOT NULL DEFAULT '',
  -- 已出租：机器租出去了、不装 agent，离线是预期状态。与 maintenance 互斥，裁决在 UpdateNodeTasks
  -- （checkNodeFlags），快照恢复进库前由 validateSnapshotNodeFlags 复核；库里不设 CHECK。
  -- 列序与迁移 44 的 ADD COLUMN 结果一致。
  rented INTEGER NOT NULL DEFAULT 0
)`

const ddlNodeFacts = `CREATE TABLE node_facts (
  node_id INTEGER PRIMARY KEY,
  facts_hash INTEGER NOT NULL,
  hostname TEXT NOT NULL,
  os TEXT NOT NULL,
  kernel TEXT NOT NULL,
  arch TEXT NOT NULL,
  virtualization TEXT NOT NULL,
  cpu_model TEXT NOT NULL,
  cpu_cores INTEGER NOT NULL,
  agent_version TEXT NOT NULL,
  icmp_available INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  network TEXT NOT NULL DEFAULT '{}',
  diagnostics TEXT NOT NULL DEFAULT 'null',
  -- 执行环境（ExecutionScope 的 protojson）。'null' 表示这一行没有上报执行环境，
  -- 与已上报的对象不同。缺省不是空对象——空对象的 kind 是未指定，校验会拒绝。
  execution TEXT NOT NULL DEFAULT 'null',
  -- 写入时的持久化字段集合版本。0 是本列出现之前的行：当时的摘要只覆盖更少的列，
  -- 不能当作当前字段集合已经确认。
  facts_rev INTEGER NOT NULL DEFAULT 0
)`

const ddlRegisterWindow = `CREATE TABLE register_window (
  owner_id INTEGER PRIMARY KEY,
  key_hash BLOB NOT NULL UNIQUE,
  expires_at INTEGER NOT NULL,
  remaining INTEGER NOT NULL
)`

const ddlRollupState = `CREATE TABLE rollup_state (
  level TEXT PRIMARY KEY,
  upto_ts INTEGER NOT NULL
)`

const seedRollupState = `INSERT INTO rollup_state (level, upto_ts) VALUES ('5m', 0), ('1h', 0)`

// maintenance_state 与 rollup_state 同类，是维护任务的簿记：name 取 health.go 的 Maintenance 系列常量，涵盖清理、上卷与两层备份；finished_at 是该任务
// 最近一次整轮成功完成的时刻（Unix 秒）。只在整轮成功后写（recordMaintenance），失败不写、不清，所以"无行"只表示
// 从未成功跑过，"有行但很旧"表示此后一直失败或没跑——两者在读侧可区分，不会被一次失败抹成同一个样子。
// 流量报告的几行例外：finished_at 存最近一次已发的周期键，不是时刻（见 health.go）。
const ddlMaintenanceState = `CREATE TABLE maintenance_state (
  name TEXT PRIMARY KEY,
  finished_at INTEGER NOT NULL
)`

const ddlAdmin = `CREATE TABLE admin (
  -- 单管理员：CHECK 让第二行无法插入，"多用户"在 schema 上就不成立。
  id INTEGER PRIMARY KEY CHECK (id = 1),
  -- PHC 字符串，argon2id 的参数随哈希走：改参数不需要迁移，旧哈希按自带参数校验。
  password_hash TEXT NOT NULL,
  updated_at INTEGER NOT NULL
)`

const ddlAdminSession = `CREATE TABLE admin_session (
  token_hash BLOB PRIMARY KEY,
  created_at INTEGER NOT NULL,
  last_used_at INTEGER NOT NULL,
  -- 绝对过期，墙钟 Unix 秒。会话要跨 hub 重启存活，只能用墙钟；
  -- 墙钟回拨会推迟按绝对过期时刻判定失效的时间。
  expires_at INTEGER NOT NULL,
  -- 签发会话的登录请求的来源地址，auth.SourceText 的规范文本，与 node.last_source、登录通知同一口径；
  -- 空串 = 未知（迁移前已存在的会话，以及没有请求的签发路径）。列序与迁移 42 的 ADD COLUMN 结果一致。
  source TEXT NOT NULL DEFAULT ''
)`

// traffic 是累加器的持久化形态：基线（boot_id、net_counter_epoch 与 traffic_baseline 的行）与累计值同一事务落盘，
// agent 未换启动周期、计数器未倒退（合计形态另要求集合摘要不变）时，崩溃后首次上报相对已落盘基线的差分会补回丢失的
// 内存增量。基线三种形态的判定只在 CheckTrafficBaseline 一处。
const ddlTraffic = `CREATE TABLE traffic (
  node_id INTEGER PRIMARY KEY,
  boot_id TEXT NOT NULL,
  total_rx INTEGER NOT NULL,
  total_tx INTEGER NOT NULL,
  period_rx INTEGER NOT NULL,
  period_tx INTEGER NOT NULL,
  -- 当前周期起点，Unix 秒；重置日零点按 hub 时区换算。
  period_start INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  -- 合计形态基线的集合摘要；逐网卡形态与没有基线时为空。
  net_counter_epoch TEXT NOT NULL DEFAULT ''
)`

// traffic_baseline 是 traffic 行的差分基线，一个节点零到多行：零行是没有基线（校正先于首次上报建立的条目）；
// iface 为空的唯一一行是合计形态（计数是网卡合计）；一到 64 行非空 iface 是逐网卡形态。三种形态不混用，
// 由 CheckTrafficBaseline 在写库、读库与恢复预检时判定。计数是截到 MaxInt64 的值，非负。
const ddlTrafficBaseline = `CREATE TABLE traffic_baseline (
  node_id INTEGER NOT NULL,
  iface TEXT NOT NULL,
  rx INTEGER NOT NULL CHECK (rx >= 0),
  tx INTEGER NOT NULL CHECK (tx >= 0),
  PRIMARY KEY (node_id, iface)
) WITHOUT ROWID`

// metricTables 按级别从细到粗，供建库、节点从属清单、清理作业与上卷使用（rollup.go 的 metricFamily.tables，
// 必须与 levels、states 同序同长，上卷按 levels 循环，多出的表不会被上卷也不报错）。新增级别要同步上卷配置，
// 已有库还需对应的增量迁移。存储统计的行数按 sqlite_master 列表，不读它；存储健康经 families 读它。
var metricTables = []string{"metric_1m", "metric_5m", "metric_1h"}

// 节点从属行分两份清单，显式删除不依赖外键开启或级联行为。
//   - nodeConfigTables 是配置层的从属行，DeleteNode 在删 node 行的同一写事务里删完：它们决定节点对读者是否存在、
//     属于哪些任务与规则，必须与 node 行同时消失。
//   - nodeHistoryTables 是时序行，体量随保留期增长，DeleteNode 只登记 kind=node 的清理作业（cleanup_job），
//     由维护循环分块删除（cleanup.go）。作业完成前这些行是孤儿，读侧按配置层判定不让它们出现（rollup.go 的 family.live）。
//
// Restore 的孤儿清理遍历两份清单：恢复把 node 表整表替换，孤儿可能没有对应作业（节点在配置快照与指标快照之间被删、
// 且清理已完成），不能指望作业兜底。node_coverage 在指标层，但它是每节点一行的覆盖起点而不是时序行，与 node 行同删。
// alert_event 是审计历史，删节点时也保留；系统事件的 node_id=0，不属于节点从属状态。cleanup_job 的 node_id 指向的
// 正是已删节点，它是待办而不是从属行，两份清单都不含它。
var nodeConfigTables = []string{
	"node_facts", "traffic", "traffic_baseline", "probe_task_node", "alert_rule_node", "alert_state", "silence_node", "node_tag", "node_update", "api_token_node", "probe_cert", "probe_cert_presented", "node_coverage", "alert_baseline",
}

var nodeHistoryTables = append(slices.Clone(metricTables), probeTables...)

// schemaStatements 是当前版本的完整 DDL：空库直接建到当前版本，不重放历史。
func schemaStatements() []string {
	out := []string{ddlNode, ddlNodeFacts, ddlRegisterWindow, ddlRollupState, seedRollupState, seedProbeRollupState,
		ddlAdmin, ddlAdminSession, ddlTraffic, ddlTrafficBaseline, ddlProbeTask, ddlProbeTaskNode, ddlProbeTaskNodeIndex, ddlProbeMeta, seedProbeMeta}
	for _, t := range metricTables {
		out = append(out, metricDDL(t))
	}
	for _, t := range probeTables {
		out = append(out, probeDDL(t))
	}
	// 探测表的 (task_id, node_id, ts) 索引承载跨节点对比：按任务取一组节点的窗口样本时，
	// 前导 task_id 等值 + node_id 等值（IN 清单）+ ts 范围直接定位，读量与其他任务、其他节点的
	// 行无关；主键 (node_id, ts, task_id) 只能按节点定位，按任务读会扫遍节点全部历史。
	for _, t := range probeTables {
		out = append(out, probeByTaskIndex(t))
	}
	out = append(append(out, alertStatements()...), ddlAPIToken, ddlSetting, ddlMaintenanceState, ddlTag, ddlNodeTag, ddlNodeTagByTag,
		ddlTheme, ddlThemeVersion, ddlThemeSelection, seedThemeSelection, ddlThemeFile, ddlRestoreRecord, ddlThemePackage,
		ddlAdminSecurity, seedAdminSecurity, ddlProbeTaskTag, ddlProbeTaskTagIndex, ddlAlertRuleTag, ddlAlertRuleTagIndex, ddlNodeUpdate,
		ddlSilence, ddlSilenceNode, ddlSilenceNodeByNode, ddlSilenceTag, ddlSilenceTagByTag,
		ddlAPITokenNode, ddlOperation, ddlOperationByOwner, ddlOperationDetailsByTime, ddlProbeCert, ddlProbeCertPresented, ddlNodeCoverage,
		ddlHubCoordination, seedHubCoordination, ddlCleanupJob, ddlNodeListRevision, seedNodeListRevision)
	// 触发器挂在 node、node_facts、node_tag、tag、api_token_node 上，放在全部建表之后。
	return append(out, ddlNodeListRevisionTriggers...)
}

// node_list_revision 是节点资料的版本计数，ListNodes 的条件读取据它判断调用方手里那份列表是否仍成立（ReadNodeList）。
// 不变式：凡改变 ListNodes 返回内容的写入，都在同一条语句里经下面的触发器把它加一，与写入一起提交或一起回滚；写者
// 不必各自记得推进，动态表名的排序写入、分钟刷出、国家查询、续期扫描、离线子命令与恢复的整表搬运都落在触发器上。
// 不推进的写入：只改 node.last_seen_at（实时状态，面板从快照取；分钟刷出每分钟写它）或 node.token_hash（不在
// ListNodes 里）；只改 node_facts.facts_hash、facts_rev（摘要与字段集合版本，展示列变了它们才变）；新建标签行
// （挂上节点时 node_tag 的插入已推进）。node 与 node_facts 的 UPDATE 触发器逐列比较新旧值，写回原值不推进；列的
// 归类由 TestNodeListRevisionColumnClassification 逐列核对，加列必须在那里归类并改这里的 WHEN。
// 只有一行（id = 1），建库时种子为 0；是运行态，不进任何备份层，恢复对 node 等表的 DELETE + INSERT 经触发器推进它。
const ddlNodeListRevision = `CREATE TABLE node_list_revision (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  revision INTEGER NOT NULL CHECK (revision >= 0)
)`
const seedNodeListRevision = `INSERT INTO node_list_revision (id, revision) VALUES (1, 0)`

const bumpNodeListRevision = `UPDATE node_list_revision SET revision = revision + 1 WHERE id = 1;`

var ddlNodeListRevisionTriggers = []string{
	`CREATE TRIGGER node_list_revision_node_insert AFTER INSERT ON node BEGIN ` + bumpNodeListRevision + ` END`,
	`CREATE TRIGGER node_list_revision_node_delete AFTER DELETE ON node BEGIN ` + bumpNodeListRevision + ` END`,
	`CREATE TRIGGER node_list_revision_node_update AFTER UPDATE ON node WHEN
  OLD.id IS NOT NEW.id OR OLD.name IS NOT NEW.name OR OLD.sort_order IS NOT NEW.sort_order OR OLD.public IS NOT NEW.public
  OR OLD.note IS NOT NEW.note OR OLD.offline_grace_s IS NOT NEW.offline_grace_s OR OLD.traffic_reset_day IS NOT NEW.traffic_reset_day
  OR OLD.created_at IS NOT NEW.created_at OR OLD.price IS NOT NEW.price OR OLD.currency IS NOT NEW.currency
  OR OLD.billing_cycle IS NOT NEW.billing_cycle OR OLD.expires_on IS NOT NEW.expires_on OR OLD.auto_renew IS NOT NEW.auto_renew
  OR OLD.last_source IS NOT NEW.last_source OR OLD.country IS NOT NEW.country OR OLD.country_ip IS NOT NEW.country_ip
  OR OLD.country_pin IS NOT NEW.country_pin OR OLD.maintenance IS NOT NEW.maintenance OR OLD.public_remark IS NOT NEW.public_remark
  OR OLD.traffic_quota_bytes IS NOT NEW.traffic_quota_bytes OR OLD.traffic_quota_mode IS NOT NEW.traffic_quota_mode
  OR OLD.ipv4_pin IS NOT NEW.ipv4_pin OR OLD.ipv6_pin IS NOT NEW.ipv6_pin OR OLD.rented IS NOT NEW.rented
BEGIN ` + bumpNodeListRevision + ` END`,
	`CREATE TRIGGER node_list_revision_facts_insert AFTER INSERT ON node_facts BEGIN ` + bumpNodeListRevision + ` END`,
	`CREATE TRIGGER node_list_revision_facts_delete AFTER DELETE ON node_facts BEGIN ` + bumpNodeListRevision + ` END`,
	`CREATE TRIGGER node_list_revision_facts_update AFTER UPDATE ON node_facts WHEN
  OLD.node_id IS NOT NEW.node_id OR OLD.hostname IS NOT NEW.hostname OR OLD.os IS NOT NEW.os OR OLD.kernel IS NOT NEW.kernel
  OR OLD.arch IS NOT NEW.arch OR OLD.virtualization IS NOT NEW.virtualization OR OLD.cpu_model IS NOT NEW.cpu_model
  OR OLD.cpu_cores IS NOT NEW.cpu_cores OR OLD.agent_version IS NOT NEW.agent_version OR OLD.icmp_available IS NOT NEW.icmp_available
  OR OLD.updated_at IS NOT NEW.updated_at OR OLD.network IS NOT NEW.network OR OLD.diagnostics IS NOT NEW.diagnostics
  OR OLD.execution IS NOT NEW.execution
BEGIN ` + bumpNodeListRevision + ` END`,
	`CREATE TRIGGER node_list_revision_node_tag_insert AFTER INSERT ON node_tag BEGIN ` + bumpNodeListRevision + ` END`,
	`CREATE TRIGGER node_list_revision_node_tag_delete AFTER DELETE ON node_tag BEGIN ` + bumpNodeListRevision + ` END`,
	`CREATE TRIGGER node_list_revision_node_tag_update AFTER UPDATE ON node_tag BEGIN ` + bumpNodeListRevision + ` END`,
	`CREATE TRIGGER node_list_revision_tag_update AFTER UPDATE ON tag BEGIN ` + bumpNodeListRevision + ` END`,
	`CREATE TRIGGER node_list_revision_tag_delete AFTER DELETE ON tag BEGIN ` + bumpNodeListRevision + ` END`,
	`CREATE TRIGGER node_list_revision_token_node_insert AFTER INSERT ON api_token_node BEGIN ` + bumpNodeListRevision + ` END`,
	`CREATE TRIGGER node_list_revision_token_node_delete AFTER DELETE ON api_token_node BEGIN ` + bumpNodeListRevision + ` END`,
	`CREATE TRIGGER node_list_revision_token_node_update AFTER UPDATE ON api_token_node BEGIN ` + bumpNodeListRevision + ` END`,
}

// cleanup_job 是删除节点或探测任务后待清的时序行（cleanup.go）：kind 为 'node' 时 node_id 是被删节点、task_id 为 0，
// 为 'task' 时 task_id 是被删任务、node_id 为 0。作业与删除同一写事务登记（INSERT OR IGNORE，同一主体重复登记是同一份
// 待办），只在复扫确认该主体在全部时序表里没有剩余行时删除；失败只累加 attempts、记下 last_error，从不放弃——
// 删掉未完成的作业等于留下无人认领的孤儿行。属于配置层快照：恢复后作业随删除记录一起回来，继续清理。
const ddlCleanupJob = `CREATE TABLE cleanup_job (
  kind TEXT NOT NULL,
  node_id INTEGER NOT NULL DEFAULT 0,
  task_id INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (kind, node_id, task_id)
) WITHOUT ROWID`

// hub_coordination 是运行中的 hub 与库外写者（离线子命令）之间的协调状态，不是站点配置，所以不放 setting 表，
// 也不进任何备份层。offline_generation 是库外写者已提交的写事务计数：带 ExternalWriter 打开的 Store 在每个提交的
// 写事务里把它加一，hub 按周期读它决定是否重载内存缓存（见 coordination.go）。只有一行（id = 1），建库时种子为 0；
// 缺行或负值都是库损坏，读侧报错而不是当作"没有外部变更"。
const ddlHubCoordination = `CREATE TABLE hub_coordination (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  offline_generation INTEGER NOT NULL CHECK (offline_generation >= 0)
)`
const seedHubCoordination = `INSERT INTO hub_coordination (id, offline_generation) VALUES (1, 0)`

const ddlNodeCoverage = `CREATE TABLE node_coverage (node_id INTEGER PRIMARY KEY, start_ts INTEGER NOT NULL)`

const ddlNodeUpdate = `CREATE TABLE node_update (node_id INTEGER PRIMARY KEY, data TEXT NOT NULL, owner_id INTEGER NOT NULL DEFAULT 0)`

const ddlAdminSecurity = `CREATE TABLE admin_security (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  generation INTEGER NOT NULL DEFAULT 0,
  data TEXT NOT NULL DEFAULT '{}'
)`
const seedAdminSecurity = `INSERT INTO admin_security (id) VALUES (1)`
const ddlProbeTaskTag = `CREATE TABLE probe_task_tag (
  task_id INTEGER NOT NULL,
  tag_id INTEGER NOT NULL,
  PRIMARY KEY (task_id, tag_id)
) WITHOUT ROWID`
const ddlProbeTaskTagIndex = `CREATE INDEX probe_task_tag_by_tag ON probe_task_tag (tag_id)`
const ddlAlertRuleTag = `CREATE TABLE alert_rule_tag (
  rule_id INTEGER NOT NULL,
  tag_id INTEGER NOT NULL,
  PRIMARY KEY (rule_id, tag_id)
) WITHOUT ROWID`
const ddlAlertRuleTagIndex = `CREATE INDEX alert_rule_tag_by_tag ON alert_rule_tag (tag_id)`

// silence 是维护静默窗口：all_nodes 为真时不写 silence_node 行、覆盖全部节点，落 silence_node 时就是全部覆盖；
// silence_node 与 silence_tag 与 alert_rule_node / alert_rule_tag 同形，作用域三选一由写侧裁决，存储只承载关联。
const ddlSilence = `CREATE TABLE silence (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL,
  -- 0/1：关闭的静默保留关联但不抑制任何投递。
  enabled INTEGER NOT NULL DEFAULT 1,
  -- 与 alert_rule.all_nodes 同一语义：为真时覆盖全部节点，之后新建的节点也在内。
  all_nodes INTEGER NOT NULL DEFAULT 0,
  -- daily 用 start_hhmm/end_hhmm（HH:MM，按 hub 时区），once 用 from_at/until_at（Unix 秒）；取值约束由写侧裁决。
  kind TEXT NOT NULL,
  start_hhmm TEXT NOT NULL DEFAULT '',
  end_hhmm TEXT NOT NULL DEFAULT '',
  from_at INTEGER NOT NULL DEFAULT 0,
  until_at INTEGER NOT NULL DEFAULT 0,
  reason TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
)`

const ddlSilenceNode = `CREATE TABLE silence_node (
  silence_id INTEGER NOT NULL,
  node_id INTEGER NOT NULL,
  PRIMARY KEY (silence_id, node_id)
)`
const ddlSilenceNodeByNode = `CREATE INDEX silence_node_by_node ON silence_node(node_id)`
const ddlSilenceTag = `CREATE TABLE silence_tag (
  silence_id INTEGER NOT NULL,
  tag_id INTEGER NOT NULL,
  PRIMARY KEY (silence_id, tag_id)
) WITHOUT ROWID`
const ddlSilenceTagByTag = `CREATE INDEX silence_tag_by_tag ON silence_tag (tag_id)`

// 恢复记录随配置备份并按 id 与目标取并集；随机标识由 Restore 在创建时生成，避免秒级时刻相同的事件合并。
const ddlRestoreRecord = `CREATE TABLE restore_record (
  id TEXT PRIMARY KEY NOT NULL,
  restored_at INTEGER NOT NULL,
  config_taken_at INTEGER NOT NULL,
  metrics_taken_at INTEGER,
  orphans TEXT NOT NULL,
  themes TEXT NOT NULL DEFAULT '{}'
)`

// metricDDL 从描述表生成分钟表。主键顺序 (node_id, ts) 即唯一查询路径，
// WITHOUT ROWID 使主键索引就是表本身。
// 每列都带 DEFAULT 0：后加的列在迁移里要由重建搬运旧行，旧行在新列上没有值，
// 只能取默认值；而全新建库与迁移后的库必须逐列相同（含默认值），所以默认值
// 由这一处统一给出。所有写路径都显式写全部列，默认值不参与任何业务取值。
func metricDDL(table string) string {
	cols := []string{"node_id INTEGER NOT NULL", "ts INTEGER NOT NULL"}
	for _, c := range metric.Columns {
		cols = append(cols, c.Name+"_sum "+c.SQLType()+" NOT NULL DEFAULT 0", c.Name+"_n INTEGER NOT NULL DEFAULT 0")
		if c.Kind == metric.MeanMax {
			cols = append(cols, c.Name+"_max "+c.SQLType()+" NOT NULL DEFAULT 0")
		}
	}
	if table == "metric_1m" {
		cols = append(cols, "reported INTEGER NOT NULL DEFAULT 1", "observed INTEGER")
	} else {
		cols = append(cols, "minutes INTEGER", "observed INTEGER", "both INTEGER")
	}
	return "CREATE TABLE " + table + " (" + strings.Join(cols, ", ") + ", PRIMARY KEY (node_id, ts)) WITHOUT ROWID"
}

// metricColumnNames 是 SQL 里列的顺序：与 Bucket 切片按描述表下标对应。
func metricColumnNames() []string {
	var names []string
	for _, c := range metric.Columns {
		names = append(names, c.Name+"_sum", c.Name+"_n")
		if c.Kind == metric.MeanMax {
			names = append(names, c.Name+"_max")
		}
	}
	return names
}

// metricUpsert 生成加法合并语句。合并正确的前提由 live 保证：每个内存桶至多
// 成功写入一次（刷出即取走）；事务原子性保证失败即未应用，重试不会重复计入。
func metricUpsert(table string) string {
	names := metricColumnNames()
	var sets []string
	for _, c := range metric.Columns {
		sets = append(sets,
			c.Name+"_sum = "+c.Name+"_sum + excluded."+c.Name+"_sum",
			c.Name+"_n = "+c.Name+"_n + excluded."+c.Name+"_n")
		if c.Kind == metric.MeanMax {
			sets = append(sets, c.Name+"_max = max("+c.Name+"_max, excluded."+c.Name+"_max)")
		}
	}
	names = append(names, "reported", "observed")
	for _, name := range []string{"reported", "observed"} {
		sets = append(sets, name+" = "+coverageOr(name, "excluded."+name))
	}
	all := append([]string{"node_id", "ts"}, names...)
	return "INSERT INTO " + table + " (" + strings.Join(all, ", ") + ") VALUES (" +
		strings.TrimSuffix(strings.Repeat("?, ", len(all)), ", ") + ") ON CONFLICT (node_id, ts) DO UPDATE SET " +
		strings.Join(sets, ", ")
}

func metricSelect(table string) string {
	i := 1
	if table == "metric_1m" {
		i = 0
	}
	return "SELECT ts, " + strings.Join(append(metricColumnNames(), coverageSource(i)...), ", ") + " FROM " + table +
		" WHERE node_id = ? AND ts >= ? AND ts < ? ORDER BY ts"
}

// bucketArgs 把桶按描述表顺序展开成绑定参数；整数列在此处转回整型。
func bucketArgs(b *metric.Bucket) []any {
	var args []any
	for i, c := range metric.Columns {
		if c.Type == metric.Int {
			args = append(args, int64(b.Sum[i]), int64(b.N[i]))
			if c.Kind == metric.MeanMax {
				args = append(args, int64(b.Max[i]))
			}
		} else {
			args = append(args, b.Sum[i], int64(b.N[i]))
			if c.Kind == metric.MeanMax {
				args = append(args, b.Max[i])
			}
		}
	}
	return args
}

// 探测表族：键比指标表多一维 task_id，值列固定六个。rtt_min_us / rtt_max_us 可空——
// 全部丢包或错误的桶没有 rtt 样本，NULL 让 min()/max() 聚合自动跳过它。
func probeDDL(table string) string {
	return "CREATE TABLE " + table + ` (
  node_id INTEGER NOT NULL, ts INTEGER NOT NULL, task_id INTEGER NOT NULL,
  sent INTEGER NOT NULL DEFAULT 0, lost INTEGER NOT NULL DEFAULT 0, errors INTEGER NOT NULL DEFAULT 0,
  rtt_sum_us INTEGER NOT NULL DEFAULT 0, rtt_min_us INTEGER, rtt_max_us INTEGER,
  PRIMARY KEY (node_id, ts, task_id)
) WITHOUT ROWID`
}

// probeTables 与 metricTables 同一口径：建库、节点从属清单、清理作业与上卷（rollup.go 的 probeFamily.tables，
// 与 levels、states 同序同长）共用。
var probeTables = []string{"probe_1m", "probe_5m", "probe_1h"}

// probeByTaskIndex 是探测表按任务读取的索引，名字由表名派生（probe_1m_by_task …）。
// 列序 (task_id, node_id, ts)：前导等值键之后 ts 才能作为范围约束进入同一个 SEARCH，
// (task_id, ts, node_id) 会让 node_id 落到范围之后，对比查询退化为逐节点扫描后的逐行过滤。
func probeByTaskIndex(table string) string {
	return "CREATE INDEX " + table + "_by_task ON " + table + " (task_id, node_id, ts)"
}

const ddlProbeTask = `CREATE TABLE probe_task (
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

const ddlProbeTaskNode = `CREATE TABLE probe_task_node (
  task_id INTEGER NOT NULL,
  node_id INTEGER NOT NULL,
  PRIMARY KEY (task_id, node_id)
) WITHOUT ROWID`

const ddlProbeTaskNodeIndex = `CREATE INDEX probe_task_node_by_node ON probe_task_node (node_id)`

// probe_meta.version 由任务保存与删除、建节点的事务递增，agent 用它对账任务清单（见 bumpProbeVersion）。
// 删除节点时仅清理其分配，不递增：auth.DeleteNode 在删除成功后撤销 token，
// 其余节点的清单不变，无需因此重新对账。
// 单行表，CHECK 让第二行无法插入。
const ddlProbeMeta = `CREATE TABLE probe_meta (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  version INTEGER NOT NULL
)`

const seedProbeMeta = `INSERT INTO probe_meta (id, version) VALUES (1, 0)`

const seedProbeRollupState = `INSERT INTO rollup_state (level, upto_ts) VALUES ('probe_5m', 0), ('probe_1h', 0)`

// probe_cert 是 (节点, 任务) 的最新一份 HTTPS 证书到期观测，与探测表族分开：它不是时间序列，写侧是
// ingest 校验通过后的覆盖写，读侧是证书到期告警评估。没有行即"无读数"——不是任何证书状态，评估对无行的
// (节点, 任务) 不评估也不恢复。行随任务删除（DeleteProbeTask）与节点删除（DeleteNode）消失。
const ddlProbeCert = `CREATE TABLE probe_cert (
  node_id INTEGER NOT NULL,
  task_id INTEGER NOT NULL,
  -- 服务端证书链首枚证书的到期时刻（Unix 秒）。
  not_after INTEGER NOT NULL,
  -- hub 收到这次观测的墙钟（Unix 秒），供展示"观测于何时"。
  observed_at INTEGER NOT NULL,
  -- certWriteOK 在写事务内要求身份非空且等于当前任务身份；任务身份变化时，保存路径经 deleteTaskCertsTx 删除该任务的旧行。
  -- 因而每行都属于当前身份，到期告警（ProbeCertsByTask）只读 not_after，不必再次比较此列。
  config_id BLOB NOT NULL,
  PRIMARY KEY (node_id, task_id)
) WITHOUT ROWID`

// probe_cert_presented 是（节点, 任务）当前配置身份下的信任候选：证书相关丢包带回的叶证书。从不自动生效，到期告警不读它。
// 行随任务身份变化、任务删除与节点删除消失；旧身份的观测不能重建或覆盖。
const ddlProbeCertPresented = `CREATE TABLE probe_cert_presented (
  node_id INTEGER NOT NULL,
  task_id INTEGER NOT NULL,
  config_id BLOB NOT NULL,
  spki_sha256 BLOB NOT NULL,
  not_after INTEGER NOT NULL,
  reason INTEGER NOT NULL,
  observed_at INTEGER NOT NULL,
  PRIMARY KEY (node_id, task_id)
) WITHOUT ROWID`

// all_nodes 显式区分全部节点与有限作用域：为真时 SaveAlertRule 不写节点联结行；
// 为假时空联结集合不覆盖任何节点，DeleteNode 删除最后一个联结也不会放宽规则。
const ddlAlertRule = `CREATE TABLE alert_rule (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL,
  kind TEXT NOT NULL,
  enabled INTEGER NOT NULL DEFAULT 1,
  all_nodes INTEGER NOT NULL DEFAULT 0,
  task_id INTEGER,
  metric TEXT,
  threshold REAL,
  for_minutes INTEGER,
  created_at INTEGER NOT NULL,
  -- 非到期规则写 NULL：SaveAlertRule 只为到期规则落这一列，CheckKindFields 拒绝别的种类带非零值。到期规则的
  -- 1–365 由 alert.CheckRule 在保存与载入时裁决，存储层不查。列序与迁移 9 的 ADD COLUMN 结果一致。
  days_before INTEGER,
  resource_metric TEXT,
  recovery_threshold REAL,
  -- rtt 判定方式。非 rtt 规则整组为 NULL；rtt 规则的 rtt_mode 恒为 threshold 或 relative（不以 NULL 代表固定阈值），
  -- 固定阈值下其余七列为 NULL，相对判定下只落所选基线来源用到的列。取值矩阵由 CheckKindFields 裁决。
  -- 列序与迁移 39 的 ADD COLUMN 结果一致。
  rtt_mode TEXT,
  baseline_mode TEXT,
  baseline_window_s INTEGER,
  baseline_min_samples INTEGER,
  upper_deviation_pct REAL,
  lower_deviation_pct REAL,
  cooldown_s INTEGER,
  fixed_baseline_ms REAL
)`
const ddlAlertRuleNode = `CREATE TABLE alert_rule_node (
  rule_id INTEGER NOT NULL,
  node_id INTEGER NOT NULL,
  PRIMARY KEY (rule_id, node_id)
)`
const ddlAlertRuleNodeByNode = `CREATE INDEX alert_rule_node_by_node ON alert_rule_node(node_id)`
const ddlAlertRuleChannel = `CREATE TABLE alert_rule_channel (
  rule_id INTEGER NOT NULL,
  channel_id INTEGER NOT NULL,
  PRIMARY KEY (rule_id, channel_id)
)`
const ddlAlertRuleChannelByChannel = `CREATE INDEX alert_rule_channel_by_channel ON alert_rule_channel(channel_id)`
const ddlNotifyChannel = `CREATE TABLE notify_channel (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL,
  kind TEXT NOT NULL,
  config TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  -- 每分钟至多发出的请求数（含重试），0 表示不限。0 是放宽方向：DEFAULT 只为迁移 17 的 ADD COLUMN
  -- 给旧行一个值（随后按种类改写），唯一写者 SaveNotifyChannel 总是显式写入，按种类取缺省值的是 api 层。
  -- 列序与迁移 17 的 ADD COLUMN 结果一致。
  rate_per_minute INTEGER NOT NULL DEFAULT 0 CHECK (rate_per_minute >= 0)
)`

// 状态只在转换时写；since_at 是墙钟，只用于展示"自何时起"。计时用引擎内存里的单调钟。
const ddlAlertState = `CREATE TABLE alert_state (
  rule_id INTEGER NOT NULL,
  node_id INTEGER NOT NULL,
  state TEXT NOT NULL,
  since_at INTEGER NOT NULL,
  -- 引擎只在到期规则进入 firing 时写入非空值：当时节点的到期日（见 StateRow.FiredExpiresOn）。
  -- 列序与迁移 9 的 ADD COLUMN 结果一致：ADD COLUMN 把列排在最后，与写在 PRIMARY KEY 约束之前的这一行同为第五列。
  fired_expires_on TEXT NOT NULL DEFAULT '',
  -- 离线规则×节点上一次从 firing 恢复的墙钟（Unix 秒），NULL 表示从未恢复过；其余种类恒为 NULL。离线抖动抑制按它判定
  -- 这次离线是否落在恢复后的窗口里（见 StateRow.RecoveredAt）。整行写入时由调用方给出：恢复转换写当下时刻，其余写入
  -- 沿用当前值——恢复之后的再次离线先写成 pending，那一次写若清掉它，窗口恰在要用时丢失。
  -- 列序与迁移 12 的 ADD COLUMN 结果一致，同 fired_expires_on 写在 PRIMARY KEY 约束之前。
  recovered_at INTEGER,
  -- 该 firing 进入时是否被静默覆盖；静默只抑制投递，恢复投递据此判断配对的 firing 是否真的投递过。恒为 0/1。
  -- 列序与迁移 27 的 ADD COLUMN 结果一致，同样写在 PRIMARY KEY 约束之前。
  fired_silenced INTEGER NOT NULL DEFAULT 0,
  -- 最近一次进入 firing 的墙钟（Unix 秒），0 表示从未触发；由 setAlertState 按上一行推出（见 StateRow.FiredAt），冷却判定读它。
  -- 列序与迁移 39 的 ADD COLUMN 结果一致，同样写在 PRIMARY KEY 约束之前。
  fired_at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (rule_id, node_id)
)`

// alert_baseline 是自适应基线的缓存：每对 (规则, 节点) 一行，引擎的重算轮次每小时写一次，每分钟的评估只读它。
// baseline_us 是 buckets 个 5 分钟桶均值的中位数（微秒，四舍五入）；buckets 为 0 时 baseline_us 无意义。
// task_fingerprint 是算出这一行时规则所指任务的种类与目标（TaskFingerprint），与任务当前的指纹不同即作废；
// accumulate_from 是重新累积的起点（Unix 秒，0 = 无下界）：发现指纹变化时写当下，此后的重算只读它之后的桶。
// 行随规则删除（DeleteAlertRule）与节点删除（nodeConfigTables）消失；规则不再用自适应基线或身份变化时由
// SaveAlertRule 删除。
const ddlAlertBaseline = `CREATE TABLE alert_baseline (
  rule_id INTEGER NOT NULL,
  node_id INTEGER NOT NULL,
  baseline_us INTEGER NOT NULL,
  buckets INTEGER NOT NULL,
  computed_at INTEGER NOT NULL,
  task_fingerprint TEXT NOT NULL,
  accumulate_from INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (rule_id, node_id)
)`
const ddlAlertEvent = `CREATE TABLE alert_event (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  rule_id INTEGER NOT NULL,
  node_id INTEGER NOT NULL,
  transition TEXT NOT NULL,
  at INTEGER NOT NULL,
  summary TEXT NOT NULL,
  value REAL NOT NULL,
  -- 该事件生成时是否处于静默覆盖内；静默只抑制投递、不改状态机，事件仍完整保留。列序与迁移 27 的 ADD COLUMN 结果一致。
  silenced INTEGER NOT NULL DEFAULT 0
)`
const ddlAlertEventByNode = `CREATE INDEX alert_event_by_node ON alert_event(node_id, id)`
const ddlAlertEventByAt = `CREATE INDEX alert_event_by_at ON alert_event(at)`

// 每渠道一行；done 显式区分可续投与终态，不用虚增 attempts 冒充不可重试。列序是迁移 17 重建这张表之后的列序。
const ddlAlertDelivery = `CREATE TABLE alert_delivery (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  event_id INTEGER NOT NULL,
  channel_id INTEGER NOT NULL,
  -- 发送批次：同一次发送覆盖的行共享它，值是批次第一行的 id。AUTOINCREMENT 不复用 id，批次号因此也不复用——批次的
  -- 第一行随事件清理掉、其余行还在时，新行也不会拿到这个号；迁移 17 重建这张表时把 sqlite_sequence 一并带过来，
  -- 正是为了这一点。同批各行的尝试次数与结果一起写（BeginBatchAttempt、UpdateBatch 按 batch_id 整批更新），新行只在
  -- 批次尚未开始尝试时加入（recordAlertEvent），所以同批各行的 attempts、ok、done、失败各列与 not_before 始终相同。
  -- NOT NULL 与 CHECK 让不知道批次的插入在写时失败，而不是留下一行让之后每个读它的查询报错。
  batch_id INTEGER NOT NULL CHECK (batch_id > 0),
  attempts INTEGER NOT NULL DEFAULT 0,
  ok INTEGER NOT NULL DEFAULT 0,
  done INTEGER NOT NULL DEFAULT 0,
  -- 最近一次失败的原文，谁能读到它见 Delivery.LastError。
  last_error TEXT NOT NULL DEFAULT '',
  delivered_at INTEGER,
  -- 最近一次失败的类别（DeliveryFailure），空表示没有失败。
  failure TEXT NOT NULL DEFAULT '',
  -- 仅 failure = 'http_status' 时非 NULL，且在 100–999。三条写路径各自保证：UpdateBatch 经
  -- DeliveryResult.check；DeleteNotifyChannel 写 channel_deleted 时一并写 NULL；迁移 7 只对首位
  -- 1–9 的三位数写入，其余行保持 ADD COLUMN 的 NULL（迁移 17 原样复制）。
  http_status INTEGER,
  -- 下一次尝试不早于这个墙钟时刻（Unix 秒），0 表示没有限制。只由 UpdateBatch 随可重试的失败写入（重试间隔，含
  -- 429 的 Retry-After），所以补货与重启后按库里的行续投时仍遵守它（Queue.attempt）。0 是缺省：新行、成功与终态
  -- 都不再等待。
  not_before INTEGER NOT NULL DEFAULT 0
)`
const ddlAlertDeliveryByEvent = `CREATE INDEX alert_delivery_by_event ON alert_delivery(event_id)`

// PendingBatches 按它顺序扫描未终态行（覆盖索引），得到升序去重的批次号与渠道而不排序。
const ddlAlertDeliveryPending = `CREATE INDEX alert_delivery_pending ON alert_delivery(done, batch_id, channel_id)`

// 按批次读、开始尝试与写结果（GetDeliveryBatch、BeginBatchAttempt、UpdateBatch、joinableBatch）走它。
const ddlAlertDeliveryByBatch = `CREATE INDEX alert_delivery_by_batch ON alert_delivery(batch_id)`

// api_token 是 AdminService 的程序化凭据。
const ddlAPIToken = `CREATE TABLE api_token (
  -- AUTOINCREMENT：id 永不复用。吊销按 id 进行，复用会让针对旧 token 的吊销落到新 token 上。
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL,
  -- 整串明文（含前缀）的 SHA-256；明文不落库。
  token_hash BLOB NOT NULL UNIQUE,
  created_at INTEGER NOT NULL,
  -- NULL 表示从未使用。只供展示：距已落库值满一分钟才刷新。
  last_used_at INTEGER,
  permissions TEXT NOT NULL DEFAULT '[]',
  all_nodes INTEGER NOT NULL DEFAULT 1 CHECK (all_nodes IN (0, 1))
)`

const ddlAPITokenNode = `CREATE TABLE api_token_node (
  token_id INTEGER NOT NULL,
  node_id INTEGER NOT NULL,
  PRIMARY KEY (token_id, node_id)
) WITHOUT ROWID`

const ddlOperation = `CREATE TABLE operation (
  owner_key TEXT NOT NULL,
  owner_id INTEGER NOT NULL,
  request_id TEXT NOT NULL,
  request_hash TEXT NOT NULL,
  action TEXT NOT NULL,
  resource_id INTEGER NOT NULL,
  before_json TEXT NOT NULL,
  after_json TEXT NOT NULL,
  committed_at INTEGER NOT NULL,
  PRIMARY KEY (owner_key, request_id)
) WITHOUT ROWID`

const ddlOperationByOwner = `CREATE INDEX operation_by_owner ON operation(owner_id,committed_at DESC,request_id)`
const ddlOperationDetailsByTime = `CREATE INDEX operation_details_by_time ON operation(committed_at) WHERE before_json!='' OR after_json!=''`

// setting 是全站设置的键值表（公开页外观等）。值可达 128 KiB（logo 的 data: URL），不用 WITHOUT ROWID：
// 那种表把整行放进主键 B 树，SQLite 文档建议其行不超过页大小的约 1/20。
const ddlSetting = `CREATE TABLE setting (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
)`

func alertStatements() []string {
	return []string{ddlAlertRule, ddlAlertRuleNode, ddlAlertRuleNodeByNode, ddlAlertRuleChannel,
		ddlAlertRuleChannelByChannel, ddlNotifyChannel, ddlAlertState, ddlAlertEvent,
		ddlAlertEventByNode, ddlAlertEventByAt, ddlAlertDelivery, ddlAlertDeliveryByEvent, ddlAlertDeliveryPending, ddlAlertDeliveryByBatch, ddlAlertBaseline}
}

// tag 是运维自定义的节点标签。name 是先建的写法，回显用；name_fold 是 TagFold(name)，UNIQUE 承载"大小写不敏感
// 唯一"：两个只差大小写的名字落到同一行，后来的写法不覆盖先建的（插入 tag 行的只有 setNodeTags，冲突时不改行；
// DeleteTag 只删行）。
// AUTOINCREMENT 使 id 永不复用：node_tag 只存 tag_id，id 若复用，任何一条没被清掉的关联行都会静默挂到之后新建的
// 同 id 标签上；不复用时这样的行 JOIN 不到标签，读侧不显示。
const ddlTag = `CREATE TABLE tag (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL,
  name_fold TEXT NOT NULL UNIQUE
)`

// node_tag 是节点与标签的多对多关联。不声明外键：删节点（DeleteNode）与删标签（DeleteTag）各在自己的写事务里显式删掉
// 关联行，与其余从属表同一做法，不依赖连接是否开启外键约束。
//
// 两个索引各自服务的语句如下，依据是 EXPLAIN QUERY PLAN（不跑 ANALYZE，与生产一致：store 从不跑 ANALYZE），
// 由 TestNodeTagQueryPlans 对 tag.go 里的这些语句本身核对：
//   - 主键 (node_id, tag_id)：按节点读标签（nodeTagsQuery），替换标签前按节点清空（setNodeTags 的 clearNodeTags），
//     无标签过滤按节点判关联存在性（untaggedWhere 的 NOT EXISTS）。
//   - node_tag_by_tag：按标签过滤（tagFilterWhere 的交集子查询走它，按节点分组另用临时 B 树），ListTags 的计数，
//     DeleteTag 解除关联（detachTag）。
const ddlNodeTag = `CREATE TABLE node_tag (
  node_id INTEGER NOT NULL,
  tag_id INTEGER NOT NULL,
  PRIMARY KEY (node_id, tag_id)
) WITHOUT ROWID`

const ddlNodeTagByTag = `CREATE INDEX node_tag_by_tag ON node_tag (tag_id)`

// 主题身份与产物分离；全站选择独立保存，安装不隐式替换在线内容。
const ddlTheme = `CREATE TABLE theme (
  id TEXT PRIMARY KEY
)`

// digest 为空仅表示无法重构原始 zip 的归档占位，不是可以执行的包。
const ddlThemeVersion = `CREATE TABLE theme_version (
  theme_id TEXT NOT NULL,
  digest TEXT NOT NULL,
  name TEXT NOT NULL,
  version TEXT NOT NULL,
  preview TEXT NOT NULL,
  uploaded_at INTEGER NOT NULL,
  sdk INTEGER NOT NULL,
  published INTEGER NOT NULL DEFAULT 0 CHECK (published IN (0, 1)),
  repository TEXT NOT NULL DEFAULT '',
  release TEXT NOT NULL DEFAULT '',
  asset TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (theme_id, digest)
)`

const ddlThemeSelection = `CREATE TABLE theme_selection (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  current_id TEXT NOT NULL DEFAULT '',
  current_digest TEXT NOT NULL DEFAULT '',
  previous_id TEXT NOT NULL DEFAULT '',
  previous_digest TEXT NOT NULL DEFAULT '',
  CHECK ((current_id = '') = (current_digest = '')),
  CHECK ((previous_id = '') = (previous_digest = ''))
)`

const seedThemeSelection = `INSERT INTO theme_selection (id) VALUES (1)`

// 原始 zip 只按变更备份，不进入任一快照层。revision 是随机写入标识而非递增版本，
// 上传完成以它匹配所读的包；删除重装不依赖已删除行的计数。
const ddlThemePackage = `CREATE TABLE theme_package (
  theme_id TEXT NOT NULL,
  digest TEXT NOT NULL,
  content BLOB NOT NULL,
  revision INTEGER NOT NULL CHECK (revision > 0),
  uploaded INTEGER NOT NULL DEFAULT 0 CHECK (uploaded IN (0, 1)),
  PRIMARY KEY (theme_id, digest)
)`

// theme_file 是主题包里的普通文件，path 是包内规范路径（theme.Parse 的 File.Path），也是托管时的键。不用
// WITHOUT ROWID：单个文件可达 16 MiB，远超 SQLite 对无 rowid 表建议的行大小。删除版本在事务里显式删除
// 所有从属行，不依赖连接是否启用外键。
const ddlThemeFile = `CREATE TABLE theme_file (
  theme_id TEXT NOT NULL,
  digest TEXT NOT NULL,
  path TEXT NOT NULL,
  content BLOB NOT NULL,
  PRIMARY KEY (theme_id, digest, path)
)`
