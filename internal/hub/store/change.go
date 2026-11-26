// 写操作的记录链在 store 一侧的写边界：类型化变更（ExecuteChange）、会话直调的写与 hub 自身对用户可见配置的修改，
// 都经同一个入口 writeChange 在业务写的同一事务里写下一条操作记录（operation 表）。
//
// handler 照常调用 store 的写方法。每个有副作用的写方法经 writeChange 声明自己的目标 ChangeTarget{Action,
// ResourceID, ResourceKey}，派生写与内部写经 write。writeChange 在执行任何 SQL 之前按 ctx 携带的状态裁决：
//   - ctx 带类型化变更（WithChange）：变更尚未用过它唯一的一次主写，Action 相同，且建资源的操作 ResourceID 为 0、
//     其它操作 ResourceID 相同时，这是主写，按 changeWrite 在同一事务里完成授权、重放、版本比对、业务写、回执；
//     否则返回 ErrChangeTarget，不开事务；
//   - ctx 带直调审计（WithAudit，api 的鉴权拦截器给声明了 AUDIT_WRITE 的会话方法放入；systemContext 给 hub 自身
//     的修改放入）：审计尚未被认领时这是主写，按 auditWrite 在同一事务里完成快照、业务写与记录；已被认领时返回
//     ErrChangeTarget，不开事务；
//   - 都没有：等同 write（维护循环、离线命令、登录签发会话等不属于记录链的写）。
//
// 比对带 Action 而不只是种类与资源：改节点与轮换 token 是同一节点上的兄弟操作，开始与取消更新同理。只比种类与
// 资源时，轮换的 handler 若先做了一次改节点目标的写，那次写会被当成主写审计提交并用掉变更，真正的凭据写随后作为
// 普通写绕过审计；节点快照不含凭据哈希，回执上看不出凭据被换过，那次写若没改快照列，回执差异就是空的。
//
// 一次变更或一次直调审计只有一次主写机会：主写（无论成败）或一次不匹配的 writeChange 都会用掉它，之后的
// writeChange 一律返回 ErrChangeTarget；提交之后的派生写走 write，不受影响。write 永不消费、永不审计。
//
// 类型化变更各状态的结果：
//   - 成功：主写提交并写回执，派生写照常提交；
//   - 预览：主写在回执前以 ErrPreview 回滚，Change 带回写前写后快照与版本，库无变化；
//   - 重放：同键回执已存在，主写以 ErrReplay 返回、不执行业务写，Change 带回原回执；
//   - 业务拒绝：handler 在主写之前返回错误，变更未被使用，ExecuteChange 原样返回该错误，operation 无行；
//   - 零消费：handler 成功返回却没有提交主写，ExecuteChange 回答 Internal 并记 Error 日志点名操作；
//   - 错误首写：第一次 writeChange 与目标不符，返回 ErrChangeTarget 并记进 Change.Err，这次与之后的 writeChange
//     都不开事务，ExecuteChange 回答 Internal；
//   - 重复主写：主写之后再来一次 writeChange，返回 ErrChangeTarget，已提交的主写与回执保留。
//
// 直调审计的结果：主写提交即带一条记录（Audit.Recorded）；业务写在事务里返回错误时记录随之回滚；handler 成功返回却
// 没有记录（零消费）由 api 的拦截器回答 Internal 并记 Error 日志，与类型化变更的零消费同一口径。

package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

var (
	// ErrChangeTarget 是写方法声明的目标与 ctx 里的变更不符，或变更的主写已被用掉；它总是 handler 接线错误。
	ErrChangeTarget = errors.New("write does not match the change it runs under")
	ErrPreview      = errors.New("change preview rolled back")
	ErrReplay       = errors.New("change already committed")
	ErrConflict     = errors.New("resource changed; preview again")
	ErrRequestID    = errors.New("request_id was used for a different change")
)

// Operation 是 operation 表的一行：一条操作记录。OwnerID 是 API token 主体的 token id（会话与系统主体为 0）；
// RequestID 在 ExecuteChange 是调用方给的、其余由 newRecordID 生成；RequestHash 只有 ExecuteChange 的回执有（幂等
// 比对用），其余为空。
type Operation struct {
	ID                             string
	OwnerID                        int64
	RequestID, RequestHash, Action string
	ResourceID                     int64
	ResourceKey                    string
	BeforeJSON, AfterJSON          string
	CommittedAt                    int64
	Principal                      PrincipalKind
	SessionID, Source, Method      string
}

// PrincipalKind 是操作记录的主体种类，取值是 operation.principal 列的持久值（列上的 CHECK 列出全集），改名要迁移。
type PrincipalKind string

const (
	PrincipalSession  PrincipalKind = "session"
	PrincipalAPIToken PrincipalKind = "api_token"
	PrincipalSystem   PrincipalKind = "system"
)

// Caller 是一次写的发起方，由入口放进 ctx：api 的鉴权拦截器给每个通过鉴权的请求放一份，hub 自身的修改由
// systemContext 放。写操作记录时主体种类、会话、来源与方法只从这里取；token 的 id 与身份仍取自 Principal，
// 两者由 recordOwner 互相印证。
type Caller struct {
	Kind PrincipalKind
	// Session 是会话 id：会话 token_hash 的小写十六进制，与 ListSessions 的 Session.id 同值，不是 cookie 明文。
	// 只在 PrincipalSession 时非空。
	Session string
	// Source 是请求来源（auth.SourceText 规范文本）；系统主体为空。
	Source string
	// Method 是 AdminService 方法名；系统主体为空。
	Method string
}

type callerKey struct{}

func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

// Audit 是一次直调写的操作记录状态。api 的鉴权拦截器给声明了 AUDIT_WRITE 的会话方法放一份进 ctx（WithAudit），
// 方法的主写经 writeChange 认领它、在同一事务里写下记录，提交后内嵌的 Operation 即是那条记录。
type Audit struct {
	Operation
	// claimed 记录唯一的主写机会是否已用掉，只由 claim 置位。
	claimed bool
}

type auditKey struct{}

func WithAudit(ctx context.Context, a *Audit) context.Context {
	return context.WithValue(ctx, auditKey{}, a)
}

// Recorded 报告主写是否已连同记录提交。
func (a *Audit) Recorded() bool { return a.CommittedAt != 0 }

func (a *Audit) claim(target ChangeTarget) error {
	if _, _, err := actionSpecs(target.Action); err != nil {
		return err
	}
	if a.claimed {
		return fmt.Errorf("%w: audited call already used its primary write; %s is a second one", ErrChangeTarget, target.Action)
	}
	a.claimed = true
	return nil
}

// systemContext 是 hub 自身对用户可见配置的一次修改所在的 ctx：主体换成系统，带一份新的 Audit，并遮住调用方 ctx 里的
// 类型化变更与 token 主体。这类修改常由某个请求顺带触发（改计费之后的到期扫描），记在请求名下会把系统的推后算成
// 调用方的操作，还会撞上请求已经用掉的主写机会。
func systemContext(ctx context.Context) context.Context {
	ctx = context.WithValue(ctx, changeKey{}, (*Change)(nil))
	ctx = context.WithValue(ctx, principalKey{}, nil)
	ctx = WithCaller(ctx, Caller{Kind: PrincipalSystem})
	return WithAudit(ctx, &Audit{})
}

// recordOwner 给出一条记录的主体与幂等命名空间 owner_key：API token 是它的身份（token_hash 的十六进制），会话与系统
// 主体各用一个固定键。Caller 与 Principal 必须互相印证——token 主体必有 Principal，其余主体必无，会话主体必有会话
// id——不符或缺 Caller 都是装配错误，拒绝写记录（事务随之回滚）。
func recordOwner(ctx context.Context) (Caller, string, int64, error) {
	c, ok := ctx.Value(callerKey{}).(Caller)
	if !ok {
		return Caller{}, "", 0, errors.New("operation record without a caller in context")
	}
	p, isToken := Principal(ctx)
	switch {
	case c.Kind == PrincipalAPIToken && isToken:
		return c, p.Identity, p.ID, nil
	case c.Kind == PrincipalSession && !isToken && c.Session != "":
		return c, "session", 0, nil
	case c.Kind == PrincipalSystem && !isToken:
		return c, "system", 0, nil
	}
	return Caller{}, "", 0, fmt.Errorf("operation record: caller %q does not match the request principal (api token: %v, session id set: %v)", c.Kind, isToken, c.Session != "")
}

// newRecordID 是直调与系统记录的 request_id：前缀里的冒号不在 ExecuteChange 接受的 request_id 字母表里（api 的
// requestIDPattern），生成的键因此不会与调用方的键相撞，也就不会被当作某次 ExecuteChange 的回执重放；随机 128 位
// 让同一主体下的生成键互不相撞。
func newRecordID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "op:" + hex.EncodeToString(b[:])
}

// detailRetention 是记录详细差异的保留期：之后的写入把更早记录的 before_json、after_json 清空，其余列永久保留——
// 幂等墓碑不能因清理让旧 request_id 再次执行。
const detailRetention = 90 * 24 * time.Hour

// commitOperation 在调用方的事务里写下 op：主体、会话、来源、方法与 owner 取自 ctx（recordOwner），提交时刻取 Store
// 的时钟；顺带清掉超过保留期的详细差异。返回后 op 的 ID 与 CommittedAt 已填好，事务提交失败时由调用方清零。
func (s *Store) commitOperation(ctx context.Context, tx *sql.Tx, op *Operation) error {
	caller, owner, ownerID, err := recordOwner(ctx)
	if err != nil {
		return err
	}
	op.OwnerID, op.Principal, op.SessionID, op.Source, op.Method = ownerID, caller.Kind, caller.Session, caller.Source, caller.Method
	op.CommittedAt = s.clk.Now().Unix()
	op.ID = operationID(owner, op.RequestID)
	if _, err := tx.Exec("UPDATE operation SET before_json='',after_json='' WHERE committed_at < ? AND (before_json!='' OR after_json!='')", op.CommittedAt-int64(detailRetention/time.Second)); err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO operation (owner_key,owner_id,request_id,request_hash,action,resource_id,before_json,after_json,committed_at,principal,session_id,source,method,resource_key) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		owner, op.OwnerID, op.RequestID, op.RequestHash, op.Action, op.ResourceID, op.BeforeJSON, op.AfterJSON, op.CommittedAt, string(op.Principal), op.SessionID, op.Source, op.Method, op.ResourceKey)
	return err
}

// ChangeKind 是写目标的资源种类。同种资源共用快照列白名单、凭据摘要与 ResourceID 的分配来源（changeKinds）；
// String 的取值进入版本摘要与快照的主行键，改名会让进行中的预览全部失配、记录里的 JSON 键漂移。
type ChangeKind uint8

const (
	ChangeNode ChangeKind = iota + 1
	ChangeProbe
	ChangeAlert
	ChangeWindow
	ChangeUpdate
	ChangeTag
	ChangeTraffic
	ChangeNodeOrder
	ChangeNodeTags
	ChangeProbeOrder
	ChangeChannel
	ChangeSilence
	ChangeSettings
	ChangeAPIToken
	ChangeTheme
	ChangeThemeSelection
	ChangeSession
	ChangeSecurity
)

var changeKindNames = map[ChangeKind]string{
	ChangeNode: "node", ChangeProbe: "probe", ChangeAlert: "alert", ChangeWindow: "window", ChangeUpdate: "update", ChangeTag: "tag",
	ChangeTraffic: "traffic", ChangeNodeOrder: "node_order", ChangeNodeTags: "node_tags", ChangeProbeOrder: "probe_order",
	ChangeChannel: "channel", ChangeSilence: "silence", ChangeSettings: "settings", ChangeAPIToken: "api_token",
	ChangeTheme: "theme", ChangeThemeSelection: "theme_selection", ChangeSession: "session", ChangeSecurity: "security",
}

func (k ChangeKind) String() string {
	if name, ok := changeKindNames[k]; ok {
		return name
	}
	return fmt.Sprintf("ChangeKind(%d)", uint8(k))
}

// ChangeAction 是一次写操作的动作。登记了权限（permission 非空）的是类型化变更，与 ExecuteChangeRequest.change 的
// oneof 分支一一对应；其余只经会话直调（session-only）或由 hub 自身发起（auto_renew_node）。零值非法：未登记的
// 动作在 WithChange 处 panic、在 ChangeVersion 与 writeChange 处报错，不会落到某个默认分支被当作合法操作执行。
type ChangeAction uint8

const (
	ActionCreateNode ChangeAction = iota + 1
	ActionUpdateNode
	ActionDeleteNode
	ActionRotateNodeToken
	ActionOpenRegisterWindow
	ActionCloseRegisterWindow
	ActionSaveProbeTask
	ActionDeleteProbeTask
	ActionSaveAlertRule
	ActionDeleteAlertRule
	ActionStartUpdate
	ActionCancelUpdate
	ActionDeleteTag
	ActionRenewNodeBilling
	ActionAutoRenewNode
	ActionReorderNodes
	ActionMoveNodes
	ActionBatchUpdateNodeTags
	ActionAdjustTraffic
	ActionReorderProbeTasks
	ActionTestAlertNotification
	ActionSaveSilence
	ActionDeleteSilence
	ActionSaveNotifyChannel
	ActionDeleteNotifyChannel
	ActionTestNotifyChannel
	ActionUpdateSettings
	ActionInstallTheme
	ActionEnableTheme
	ActionDeleteTheme
	ActionDeleteThemeVersion
	ActionCreateAPIToken
	ActionUpdateAPIToken
	ActionDeleteAPIToken
	ActionRevokeSession
	ActionLogout
	ActionSecurityTOTPBegin
	ActionSecurityTOTPEnable
	ActionSecurityTOTPDisable
	ActionSecurityPasskeyBegin
	ActionSecurityPasskeyRebindBegin
	ActionSecurityPasskeyRegister
	ActionSecurityPasskeyDelete
	ActionSecurityReauthBegin
	ActionSecurityReauthFinish
	ActionSecurityRecoveryRegenerate
)

// String 是 operation.action 列与记录里的动作名：已持久化的记录按它展示与过滤，改名会让新旧记录的同一操作名字不同。
func (a ChangeAction) String() string {
	if spec, ok := changeActions[a]; ok {
		return spec.name
	}
	return fmt.Sprintf("ChangeAction(%d)", uint8(a))
}

// Kind 与 Permission 都由登记表推出，Change 不另存一份，二者无法与操作失配。未登记的操作返回零值；只经会话直调或
// 由 hub 发起的动作 Permission 为空，不能作为类型化变更执行。
func (a ChangeAction) Kind() ChangeKind { return changeActions[a].kind }

func (a ChangeAction) Permission() Permission { return changeActions[a].permission }

type changeActionSpec struct {
	name       string
	kind       ChangeKind
	permission Permission
	// creates 的操作在写之前没有资源身份：作为类型化变更时 ResourceID 必须为 0。
	creates bool
	// assigns 的操作在 ResourceID 为 0 时由写分配身份（建资源，或不带 id 的保存）：写后从种类的 sequence 读出新身份
	// 填进记录。种类必须登记 sequence。
	assigns bool
	// authorizeAfterWrite 的操作在写后、ResourceID 已知时再做一次资源授权：保存可能新建资源或改变它的作用域，
	// 写前只能裁决旧状态与请求里的选择器。删除同样登记，写后读不到资源即放行（见 RuleInScope）。
	authorizeAfterWrite bool
}

// changeActions 是写操作的唯一登记表：changeWrite、auditWrite、snapshot、ChangeVersion 与 api 的 prepareChange 都只从
// 这里取操作的种类、权限与写后授权。新增 oneof 分支必须在这里登记并给出权限，否则 WithChange 拒绝它；新增会话直调
// 的写同样在这里登记，否则 writeChange 拒绝它。
var changeActions = map[ChangeAction]changeActionSpec{
	ActionCreateNode:          {name: "create_node", kind: ChangeNode, permission: PermissionCreate, creates: true, assigns: true},
	ActionUpdateNode:          {name: "update_node", kind: ChangeNode, permission: PermissionConfigure},
	ActionDeleteNode:          {name: "delete_node", kind: ChangeNode, permission: PermissionDelete},
	ActionRotateNodeToken:     {name: "rotate_node_token", kind: ChangeNode, permission: PermissionRotate},
	ActionOpenRegisterWindow:  {name: "open_register_window", kind: ChangeWindow, permission: PermissionRegister},
	ActionCloseRegisterWindow: {name: "close_register_window", kind: ChangeWindow, permission: PermissionRegister},
	ActionSaveProbeTask:       {name: "save_probe_task", kind: ChangeProbe, permission: PermissionConfigure, assigns: true, authorizeAfterWrite: true},
	ActionDeleteProbeTask:     {name: "delete_probe_task", kind: ChangeProbe, permission: PermissionConfigure, authorizeAfterWrite: true},
	ActionSaveAlertRule:       {name: "save_alert_rule", kind: ChangeAlert, permission: PermissionConfigure, assigns: true, authorizeAfterWrite: true},
	ActionDeleteAlertRule:     {name: "delete_alert_rule", kind: ChangeAlert, permission: PermissionConfigure, authorizeAfterWrite: true},
	ActionStartUpdate:         {name: "start_update", kind: ChangeUpdate, permission: PermissionUpdate},
	ActionCancelUpdate:        {name: "cancel_update", kind: ChangeUpdate, permission: PermissionUpdate},
	ActionDeleteTag:           {name: "delete_tag", kind: ChangeTag, permission: PermissionConfigure},
	ActionRenewNodeBilling:    {name: "renew_node_billing", kind: ChangeNode, permission: PermissionConfigure},

	// hub 自身：到期扫描的自动续期（RenewExpiry，经 systemContext）。
	ActionAutoRenewNode: {name: "auto_renew_node", kind: ChangeNode},

	// 只经会话直调。
	ActionReorderNodes:               {name: "reorder_nodes", kind: ChangeNodeOrder},
	ActionMoveNodes:                  {name: "move_nodes", kind: ChangeNodeOrder},
	ActionBatchUpdateNodeTags:        {name: "batch_update_node_tags", kind: ChangeNodeTags},
	ActionAdjustTraffic:              {name: "adjust_traffic", kind: ChangeTraffic},
	ActionReorderProbeTasks:          {name: "reorder_probe_tasks", kind: ChangeProbeOrder},
	ActionTestAlertNotification:      {name: "test_alert_notification", kind: ChangeAlert},
	ActionSaveSilence:                {name: "save_silence", kind: ChangeSilence, assigns: true},
	ActionDeleteSilence:              {name: "delete_silence", kind: ChangeSilence},
	ActionSaveNotifyChannel:          {name: "save_notify_channel", kind: ChangeChannel, assigns: true},
	ActionDeleteNotifyChannel:        {name: "delete_notify_channel", kind: ChangeChannel},
	ActionTestNotifyChannel:          {name: "test_notify_channel", kind: ChangeChannel},
	ActionUpdateSettings:             {name: "update_settings", kind: ChangeSettings},
	ActionInstallTheme:               {name: "install_theme", kind: ChangeTheme},
	ActionEnableTheme:                {name: "enable_theme", kind: ChangeThemeSelection},
	ActionDeleteTheme:                {name: "delete_theme", kind: ChangeTheme},
	ActionDeleteThemeVersion:         {name: "delete_theme_version", kind: ChangeTheme},
	ActionCreateAPIToken:             {name: "create_api_token", kind: ChangeAPIToken, creates: true, assigns: true},
	ActionUpdateAPIToken:             {name: "update_api_token", kind: ChangeAPIToken},
	ActionDeleteAPIToken:             {name: "delete_api_token", kind: ChangeAPIToken},
	ActionRevokeSession:              {name: "revoke_session", kind: ChangeSession},
	ActionLogout:                     {name: "logout", kind: ChangeSession},
	ActionSecurityTOTPBegin:          {name: "security_totp_begin", kind: ChangeSecurity},
	ActionSecurityTOTPEnable:         {name: "security_totp_enable", kind: ChangeSecurity},
	ActionSecurityTOTPDisable:        {name: "security_totp_disable", kind: ChangeSecurity},
	ActionSecurityPasskeyBegin:       {name: "security_passkey_begin", kind: ChangeSecurity},
	ActionSecurityPasskeyRebindBegin: {name: "security_passkey_rebind_begin", kind: ChangeSecurity},
	ActionSecurityPasskeyRegister:    {name: "security_passkey_register", kind: ChangeSecurity},
	ActionSecurityPasskeyDelete:      {name: "security_passkey_delete", kind: ChangeSecurity},
	ActionSecurityReauthBegin:        {name: "security_reauth_begin", kind: ChangeSecurity},
	ActionSecurityReauthFinish:       {name: "security_reauth_finish", kind: ChangeSecurity},
	ActionSecurityRecoveryRegenerate: {name: "security_recovery_regenerate", kind: ChangeSecurity},
}

// snapshotArg 是快照查询绑定的参数：资源的整数 id、文本标识，或不绑定（全站状态）。
type snapshotArg uint8

const (
	byResourceID snapshotArg = iota
	byResourceKey
	global
)

type snapshotQuery struct{ name, sql string }

type changeKindSpec struct {
	// row 读资源主行，快照里以种类名为键；relations 读从属关系，各以自己的名字为键。二者都按 arg 绑定参数。
	row       string
	relations []snapshotQuery
	arg       snapshotArg
	// secret 读凭据哈希，只进入版本摘要、不进快照：换发凭据必须改变版本，审计与预览不能带出哈希。
	secret string
	// sequence 读 AUTOINCREMENT 刚分配的身份；为空的种类没有由写分配的资源身份。
	sequence string
	// scope 读带节点选择器的资源（探测任务、告警规则）的作用域，供 RuleScope 使用；其它种类为 nil。
	scope *ruleScopeQueries
}

// ruleScopeQueries 的 head 读 (all_nodes, 引用的探测任务 id)，tags 计选择器标签数，nodes 列显式节点。
type ruleScopeQueries struct{ head, tags, nodes string }

// 白名单列同时决定预览和记录，不从原始请求或响应复制内容。凡是凭据或可换出凭据的内容都不在任何一条查询里：
// 节点与注册窗口的哈希只进版本摘要（secret）；渠道的 config（webhook 地址、请求头、请求体模板、Telegram token）整列
// 不读；API token 不读 token_hash；会话的 token_hash 只以 ListSessions 公开的会话 id 形式出现（它是 cookie 的摘要，
// 知道它不能冒用会话），不读别的；
// 管理员安全设置只读出是否启用、剩余份数、绑定来源与 Passkey 名称，不读 TOTP 密钥、恢复码哈希、凭据公钥与用户句柄；
// 设置按 settingSnapshotKeys 白名单读，只写不读的键只记是否已设置。TestSnapshotsCarryNoSecrets 按列名扫每一条查询。
var changeKinds = map[ChangeKind]changeKindSpec{
	ChangeNode: {
		row:       `SELECT id,name,public,note,traffic_reset_day,traffic_reset_minute,billing_timezone,offline_grace_s,price,currency,billing_cycle,expires_on,auto_renew,country_pin,maintenance,public_remark,traffic_quota_bytes,traffic_quota_mode,ipv4_pin,ipv6_pin,rented FROM node WHERE id=?`,
		relations: []snapshotQuery{{"tags", `SELECT t.name FROM node_tag n JOIN tag t ON t.id=n.tag_id WHERE n.node_id=? ORDER BY t.name_fold`}},
		secret:    "SELECT hex(token_hash) FROM node WHERE id=?",
		sequence:  "SELECT seq FROM sqlite_sequence WHERE name='node'",
	},
	ChangeProbe: {
		row:       "SELECT id,kind,target,interval_s,timeout_ms,created_at,all_nodes,sort_order,dns_server,cert_spki_sha256,config_id,ip_family,dns_record_type,http_status_policy,http_expected_status FROM probe_task WHERE id=?",
		relations: []snapshotQuery{{"nodes", "SELECT node_id FROM probe_task_node WHERE task_id=? ORDER BY node_id"}, {"tags", "SELECT tag_id FROM probe_task_tag WHERE task_id=? ORDER BY tag_id"}},
		sequence:  "SELECT seq FROM sqlite_sequence WHERE name='probe_task'",
		scope: &ruleScopeQueries{
			head:  "SELECT all_nodes, 0 FROM probe_task WHERE id = ?",
			tags:  "SELECT COUNT(*) FROM probe_task_tag WHERE task_id = ?",
			nodes: "SELECT node_id FROM probe_task_node WHERE task_id = ?",
		},
	},
	ChangeAlert: {
		row:       "SELECT id,name,kind,enabled,all_nodes,task_id,metric,threshold,for_minutes,created_at,days_before,resource_metric,recovery_threshold,rtt_mode,baseline_mode,baseline_window_s,baseline_min_samples,upper_deviation_pct,lower_deviation_pct,cooldown_s,fixed_baseline_ms FROM alert_rule WHERE id=?",
		relations: []snapshotQuery{{"nodes", "SELECT node_id FROM alert_rule_node WHERE rule_id=? ORDER BY node_id"}, {"tags", "SELECT tag_id FROM alert_rule_tag WHERE rule_id=? ORDER BY tag_id"}, {"channels", "SELECT channel_id FROM alert_rule_channel WHERE rule_id=? ORDER BY channel_id"}},
		sequence:  "SELECT seq FROM sqlite_sequence WHERE name='alert_rule'",
		scope: &ruleScopeQueries{
			head:  "SELECT all_nodes, COALESCE(task_id, 0) FROM alert_rule WHERE id = ?",
			tags:  "SELECT COUNT(*) FROM alert_rule_tag WHERE rule_id = ?",
			nodes: "SELECT node_id FROM alert_rule_node WHERE rule_id = ?",
		},
	},
	ChangeWindow: {
		row:    "SELECT owner_id,expires_at,remaining FROM register_window WHERE owner_id=?",
		secret: "SELECT hex(key_hash) FROM register_window WHERE owner_id=?",
	},
	ChangeUpdate: {
		row: "SELECT node_id,data FROM node_update WHERE node_id=?",
	},
	ChangeTag: {
		row:       "SELECT id,name FROM tag WHERE id=?",
		relations: []snapshotQuery{{"nodes", "SELECT node_id FROM node_tag WHERE tag_id=? ORDER BY node_id"}},
	},
	ChangeTraffic: {
		row: "SELECT node_id,total_rx,total_tx,period_rx,period_tx,period_start FROM traffic WHERE node_id=?",
	},
	// 顺序是全站状态：一次重排或移动改的是全序，快照列出每个节点的位置。
	ChangeNodeOrder: {
		row: "SELECT id,sort_order FROM node ORDER BY sort_order,id",
		arg: global,
	},
	// 批量改标签的对象是一组节点，ResourceKey 是它们的 id 的 JSON 数组。
	ChangeNodeTags: {
		row: "SELECT n.node_id,t.name FROM node_tag n JOIN tag t ON t.id=n.tag_id WHERE n.node_id IN (SELECT value FROM json_each(?)) ORDER BY n.node_id,t.name_fold",
		arg: byResourceKey,
	},
	ChangeProbeOrder: {
		row: "SELECT id,sort_order FROM probe_task ORDER BY sort_order,id",
		arg: global,
	},
	// config 整列不读：webhook 地址、请求头值、请求体模板与 Telegram token 都在里面。
	ChangeChannel: {
		row:      "SELECT id,name,kind,rate_per_minute,created_at FROM notify_channel WHERE id=?",
		sequence: "SELECT seq FROM sqlite_sequence WHERE name='notify_channel'",
	},
	ChangeSilence: {
		row: "SELECT id,name,enabled,all_nodes,all_rules,kind,start_hhmm,end_hhmm,from_at,until_at,reason,created_at FROM silence WHERE id=?",
		relations: []snapshotQuery{{"nodes", "SELECT node_id FROM silence_node WHERE silence_id=? ORDER BY node_id"}, {"tags", "SELECT tag_id FROM silence_tag WHERE silence_id=? ORDER BY tag_id"},
			{"rules", "SELECT rule_id FROM silence_rule WHERE silence_id=? ORDER BY rule_id"}},
		sequence: "SELECT seq FROM sqlite_sequence WHERE name='silence'",
	},
	ChangeSettings: {
		row:       settingSnapshotSQL,
		relations: []snapshotQuery{{"write_only", settingWriteOnlySQL}},
		arg:       global,
	},
	// 不读 token_hash（明文的摘要）与 last_used_at（每次鉴权都可能刷新，不是配置）。
	ChangeAPIToken: {
		row:       "SELECT id,name,created_at,permissions,all_nodes,expires_at FROM api_token WHERE id=?",
		relations: []snapshotQuery{{"nodes", "SELECT node_id FROM api_token_node WHERE token_id=? ORDER BY node_id"}},
		sequence:  "SELECT seq FROM sqlite_sequence WHERE name='api_token'",
	},
	// 主题以 id 标识：版本列表与这个主题是否被选为当前或回滚版本。
	ChangeTheme: {
		row:       "SELECT digest,name,version,preview,uploaded_at,sdk,published,repository,release,asset FROM theme_version WHERE theme_id=?1 ORDER BY uploaded_at,digest",
		relations: []snapshotQuery{{"selection", "SELECT current_digest,previous_digest FROM theme_selection WHERE current_id=?1 OR previous_id=?1"}},
		arg:       byResourceKey,
	},
	ChangeThemeSelection: {
		row: "SELECT current_id,current_digest,previous_id,previous_digest FROM theme_selection WHERE id=1",
		arg: global,
	},
	// 会话以 ListSessions 公开的 id 标识；那个 id 就是 token_hash 的十六进制，知道它不能冒用会话。
	ChangeSession: {
		row: "SELECT lower(hex(token_hash)) AS id,created_at,expires_at,source FROM admin_session WHERE token_hash=unhex(?)",
		arg: byResourceKey,
	},
	// data 是 JSON，只按路径取出非秘密的几项：TOTP 是否启用（不取密钥）、剩余恢复码份数（不取哈希）、绑定的来源，
	// 以及 Passkey 的名称（不取凭据 id、公钥与用户句柄）。
	ChangeSecurity: {
		row:       `SELECT coalesce(json_extract(data,'$.secret'),'')!='' AS totp_enabled,coalesce(json_array_length(data,'$.recovery'),0) AS recovery_codes_remaining,coalesce(json_extract(data,'$.origin'),'') AS origin FROM admin_security WHERE id=1`,
		relations: []snapshotQuery{{"passkeys", `SELECT json_extract(p.value,'$.name') AS name FROM admin_security s,json_each(s.data,'$.passkeys') p WHERE s.id=1 ORDER BY p.key`}},
		arg:       global,
	},
}

var errUnregisteredAction = errors.New("change action is not registered")

// actionSpecs 取操作与它所属种类的登记；两张表任一缺项都是装配错误。
func actionSpecs(a ChangeAction) (changeActionSpec, changeKindSpec, error) {
	action, ok := changeActions[a]
	if !ok {
		return changeActionSpec{}, changeKindSpec{}, fmt.Errorf("%w: %s", errUnregisteredAction, a)
	}
	kind, ok := changeKinds[action.kind]
	if !ok {
		return changeActionSpec{}, changeKindSpec{}, fmt.Errorf("%w: %s has no kind %s", errUnregisteredAction, a, action.kind)
	}
	return action, kind, nil
}

// spec 取变更的操作登记；只经会话直调或由 hub 发起的动作不是类型化变更，同样当作装配错误。
func (c *Change) spec() (changeActionSpec, changeKindSpec, error) {
	action, kind, err := actionSpecs(c.Action)
	if err == nil && action.permission == "" {
		err = fmt.Errorf("%w: %s is not a typed change", errUnregisteredAction, c.Action)
	}
	return action, kind, err
}

// Change 携带一次类型化变更穿过 handler 到达 store 的写边界。Action 是操作身份；内嵌的 Operation.Action 是
// 回执里持久化的动作名，由写边界按 Action.String() 填写，调用方不设置它。
type Change struct {
	Operation
	Action ChangeAction
	// Policy 裁决这次变更是否被允许；store 只按固定次序调用它，自己不作允许或拒绝的决定。
	Policy          ChangePolicy
	Preview         bool
	ExpectedVersion string
	Version         string
	Err             error
	// claimed 记录唯一的主写机会是否已用掉，只由 claim 置位。
	claimed       bool
	Selector      *NodeSelector
	ReferenceTask int64
}

// ChangeReader 是策略在写事务内（或 ChangeVersion 的读事务内）可做的全部读取。策略只经它读库，裁决与写
// 看到的是同一份快照；接口只列策略需要的读，不暴露事务本身。
type ChangeReader interface {
	// APIToken 读 token 此刻的状态（lookupAPIToken）；不存在或已过期都返回 ErrNotFound。
	APIToken(id int64) (APIToken, error)
	RuleScopeReader
}

// RuleScopeReader 读带节点选择器的资源的作用域；RuleInScope 只需要它。
type RuleScopeReader interface {
	// RuleScope 读探测任务或告警规则的当前作用域；不存在时返回 ErrNotFound。
	RuleScope(kind ChangeKind, id int64) (RuleScope, error)
}

// RuleScope 是带节点选择器的资源的作用域。TaskID 是告警规则引用的探测任务（没有引用或资源是探测任务时为 0）。
type RuleScope struct {
	AllNodes bool
	TagCount int
	NodeIDs  []int64
	TaskID   int64
}

// ChangePolicy 是类型化变更的授权策略。changeWrite 的调用次序固定为 主体 → 重放 → 作用域 → 写 → 资源：
//   - Principal 鉴别主体并核对操作所需的权限位，会话主体返回 nil token；
//   - 重放检查由 store 在 Principal 之后、Scope 之前完成，已提交的请求在作用域被收回后仍能取回回执；
//   - Scope 按当前库状态裁决作用域，ChangeVersion 与写前共用它，预览与执行的准入口径因此相同；
//   - Resource 只对登记了 authorizeAfterWrite 的操作在写之后调用，此时 ResourceID 已知、资源已是写后状态。
//
// 拒绝的错误契约写在 permission.go 的权限哨兵上；写边界把拒绝记进 Change.Err，其它错误按普通写失败处理。
type ChangePolicy interface {
	Principal(ctx context.Context, r ChangeReader, c *Change) (*APIToken, error)
	Scope(ctx context.Context, r ChangeReader, c *Change, token *APIToken) error
	Resource(ctx context.Context, r ChangeReader, c *Change, token *APIToken) error
}

// ruleScopeReader 在调用方的事务上只读作用域、不读 token：告警列表与引用检查只需要它，也就不需要时钟。
type ruleScopeReader struct{ tx *sql.Tx }

// txReader 在调用方的事务上实现 ChangeReader。now 是这次裁决判定 token 是否过期的时刻，由 Store.changeReader 从
// Store 的时钟取；零值的 txReader 读 token 时由 lookupAPIToken 报错，不会把每枚 token 都当作未过期。
type txReader struct {
	ruleScopeReader
	now time.Time
}

func (s *Store) changeReader(tx *sql.Tx) txReader {
	return txReader{ruleScopeReader{tx}, s.clk.Now()}
}

func (r txReader) APIToken(id int64) (APIToken, error) {
	t, state, err := lookupAPIToken(r.tx.QueryRow, "id = ?", id, r.now)
	if err != nil {
		return APIToken{}, err
	}
	if state != TokenLive {
		return APIToken{}, ErrNotFound
	}
	return t, nil
}

func (r ruleScopeReader) RuleScope(kind ChangeKind, id int64) (RuleScope, error) {
	q := changeKinds[kind].scope
	if q == nil {
		return RuleScope{}, fmt.Errorf("change kind %s has no rule scope", kind)
	}
	var out RuleScope
	err := r.tx.QueryRow(q.head, id).Scan(&out.AllNodes, &out.TaskID)
	if errors.Is(err, sql.ErrNoRows) {
		return RuleScope{}, ErrNotFound
	}
	if err != nil {
		return RuleScope{}, err
	}
	if err := r.tx.QueryRow(q.tags, id).Scan(&out.TagCount); err != nil {
		return RuleScope{}, err
	}
	out.NodeIDs, err = scanIDs(r.tx.Query(q.nodes, id))
	return out, err
}

type changeKey struct{}

// WithChange 把 c 装进 ctx；没有策略、操作未登记或不是类型化变更、ctx 里没有 Caller 都是装配错误，在这里 panic 而不是
// 等到写边界才发现。
func WithChange(ctx context.Context, c *Change) context.Context {
	if c.Policy == nil {
		panic("store.WithChange: Change.Policy is nil")
	}
	if _, _, err := c.spec(); err != nil {
		panic("store.WithChange: " + err.Error())
	}
	if _, ok := ctx.Value(callerKey{}).(Caller); !ok {
		panic("store.WithChange: no Caller in context; the receipt would have no principal")
	}
	return context.WithValue(ctx, changeKey{}, c)
}

// ChangeTarget 是写方法声明的写目标。建资源的操作没有写前身份，ResourceID 不参与比对。ResourceKey 是以文本标识的
// 对象（主题 id、会话 id），或一次涉及多个对象的操作的成员（JSON 整数数组）；按种类的 snapshotArg 绑定给快照查询，
// 并原样进入记录。类型化变更的目标都以整数标识，ResourceKey 为空。
type ChangeTarget struct {
	Action      ChangeAction
	ResourceID  int64
	ResourceKey string
}

// Claimed 报告这次变更是否已用掉它唯一的主写机会。
func (c *Change) Claimed() bool { return c.claimed }

// claim 在执行任何 SQL 之前裁决 target 是否是 c 的主写，规则见包注释。第一次 writeChange 就不符时把错误记进
// c.Err：此时没有提交任何东西，ExecuteChange 据 c.Err 回答，handler 吞掉这个错误也改变不了结果。
func (c *Change) claim(target ChangeTarget) error {
	action, _, err := c.spec()
	if err != nil {
		return err
	}
	var mismatch string
	switch {
	case c.claimed:
		mismatch = "the change has already used its primary write"
	case target.Action != c.Action:
		mismatch = fmt.Sprintf("the write targets %s", target.Action)
	case action.creates && c.ResourceID != 0:
		mismatch = fmt.Sprintf("a create carries resource %d", c.ResourceID)
	case !action.creates && target.ResourceID != c.ResourceID:
		mismatch = fmt.Sprintf("the write targets resource %d", target.ResourceID)
	}
	if mismatch == "" {
		c.claimed = true
		return nil
	}
	err = fmt.Errorf("%w: change %s on resource %d: %s", ErrChangeTarget, c.Action, c.ResourceID, mismatch)
	if !c.claimed {
		c.claimed = true
		c.Err = err
	}
	return err
}

// writeChange 是有副作用的写方法的入口，按 ctx 携带的变更或直调审计裁决（规则见包注释）。write 的契约不变：错误
// （含预览、重放）表示事务未应用。
func (s *Store) writeChange(ctx context.Context, target ChangeTarget, fn func(*sql.Tx) error) error {
	if c, _ := ctx.Value(changeKey{}).(*Change); c != nil {
		if err := c.claim(target); err != nil {
			s.log.Error("typed change write refused", "err", err)
			return err
		}
		err := s.write(ctx, s.changeWrite(ctx, c, fn))
		// changeWrite 在事务内填好 CommittedAt；提交失败时回执并未落库，不能留给调用方当作已提交。重放带回的是库里
		// 已提交的原回执，保留。
		if err != nil && !errors.Is(err, ErrReplay) {
			c.CommittedAt = 0
		}
		return err
	}
	if a, _ := ctx.Value(auditKey{}).(*Audit); a != nil {
		if err := a.claim(target); err != nil {
			s.log.Error("audited write refused", "err", err)
			return err
		}
		err := s.write(ctx, s.auditWrite(ctx, a, target, fn))
		if err != nil {
			a.CommittedAt = 0
		}
		return err
	}
	return s.write(ctx, fn)
}

// idsKey 是一组整数 id 作为 ResourceKey 的写法：去重、升序的 JSON 数组，供按 json_each 读成员的快照查询绑定，也是
// 记录里展示的对象。
func idsKey(ids []int64) string {
	set := append([]int64{}, ids...)
	slices.Sort(set)
	b, err := json.Marshal(slices.Compact(set))
	if err != nil {
		panic(err) // 整数切片不会编码失败
	}
	return string(b)
}

// RecordOperation 写一条没有业务写的记录：方法的副作用在库之外（发送测试通知、提交 hub 自身更新），或只在进程内
// （安全操作的挑战），调用方在副作用完成之后调用它。快照照常读取，写前写后相同。没有直调审计的 ctx 里它什么都
// 不写；类型化变更下调用它返回 ErrChangeTarget——这些动作不是类型化变更。
func (s *Store) RecordOperation(ctx context.Context, target ChangeTarget) error {
	return s.writeChange(ctx, target, func(*sql.Tx) error { return nil })
}

// auditWrite 在一个事务里依次读写前快照、执行业务写、读写后快照并写记录；业务写返回错误时整个事务回滚，记录随之
// 不存在（业务拒绝不记）。
func (s *Store) auditWrite(ctx context.Context, a *Audit, target ChangeTarget, fn func(*sql.Tx) error) func(*sql.Tx) error {
	return func(tx *sql.Tx) error {
		action, kind, err := actionSpecs(target.Action)
		if err != nil {
			return err
		}
		before, _, err := snapshot(tx, target.Action, target.ResourceID, target.ResourceKey)
		if err != nil {
			return err
		}
		if err := fn(tx); err != nil {
			return err
		}
		op := Operation{RequestID: newRecordID(), Action: target.Action.String(), ResourceID: target.ResourceID, ResourceKey: target.ResourceKey, BeforeJSON: before}
		if op.ResourceID == 0 && action.assigns {
			if err := tx.QueryRow(kind.sequence).Scan(&op.ResourceID); err != nil {
				return err
			}
		}
		if op.AfterJSON, _, err = snapshot(tx, target.Action, op.ResourceID, target.ResourceKey); err != nil {
			return err
		}
		if err := s.commitOperation(ctx, tx, &op); err != nil {
			return err
		}
		a.Operation = op
		return nil
	}
}

// changeTarget 把 writeChange 绑定到一个固定目标，供共用实现的写方法按操作各自声明目标。
func (s *Store) changeTarget(action ChangeAction, id int64) func(context.Context, func(*sql.Tx) error) error {
	return func(ctx context.Context, fn func(*sql.Tx) error) error {
		return s.writeChange(ctx, ChangeTarget{Action: action, ResourceID: id}, fn)
	}
}

// changeWrite 包住一个业务提交，不包住提交后的后台派生工作。错误（含预览、重放）仍按
// write 的契约表示本次事务没有应用，auth、探测和告警缓存因此不会发布回滚后的状态。
func (s *Store) changeWrite(ctx context.Context, c *Change, fn func(*sql.Tx) error) func(*sql.Tx) error {
	return func(tx *sql.Tx) error {
		action, kind, err := c.spec()
		if err != nil {
			return err
		}
		r := s.changeReader(tx)
		token, err := c.Policy.Principal(ctx, r, c)
		if err != nil {
			c.Err = err
			return err
		}
		if !c.Preview {
			_, owner, _, err := recordOwner(ctx)
			if err != nil {
				return err
			}
			op, err := readOperation(tx, owner, c.RequestID)
			if err == nil {
				if op.RequestHash != c.RequestHash {
					c.Err = ErrRequestID
				} else {
					c.Operation = op
					c.Err = ErrReplay
				}
				return c.Err
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		if err := c.Policy.Scope(ctx, r, c, token); err != nil {
			c.Err = err
			return err
		}
		c.Operation.Action = c.Action.String()
		before, version, err := c.snapshot(tx)
		if err != nil {
			return err
		}
		c.Version = version
		if c.ExpectedVersion != version {
			c.Err = ErrConflict
			return c.Err
		}
		c.BeforeJSON = before
		if err := fn(tx); err != nil {
			return err
		}
		// assigns 的操作在 ResourceID 为 0 时是新建（建节点，或不带 id 的保存）：身份由 AUTOINCREMENT 在写里分配，
		// 0 不是任何现存行的身份。
		if c.ResourceID == 0 && action.assigns {
			if err := tx.QueryRow(kind.sequence).Scan(&c.ResourceID); err != nil {
				return err
			}
		}
		if action.authorizeAfterWrite {
			if err := c.Policy.Resource(ctx, r, c, token); err != nil {
				c.Err = err
				return err
			}
		}
		c.AfterJSON, _, err = c.snapshot(tx)
		if err != nil {
			return err
		}
		if c.Preview {
			c.Err = ErrPreview
			return c.Err
		}
		return s.commitOperation(ctx, tx, &c.Operation)
	}
}

func (c *Change) snapshot(tx *sql.Tx) (string, string, error) {
	if _, _, err := c.spec(); err != nil {
		return "", "", err
	}
	return snapshot(tx, c.Action, c.ResourceID, "")
}

// snapshot 按动作所属种类的白名单读出对象此刻的状态（JSON）与版本摘要。版本摘要另含种类的凭据哈希，快照不含。
func snapshot(tx *sql.Tx, a ChangeAction, id int64, key string) (string, string, error) {
	action, spec, err := actionSpecs(a)
	if err != nil {
		return "", "", err
	}
	var args []any
	switch spec.arg {
	case byResourceID:
		args = []any{id}
	case byResourceKey:
		args = []any{key}
	}
	queries := append([]snapshotQuery{{action.kind.String(), spec.row}}, spec.relations...)
	data := map[string]any{}
	for _, q := range queries {
		rows, err := tx.Query(q.sql, args...)
		if err != nil {
			return "", "", err
		}
		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			return "", "", err
		}
		list := make([]map[string]any, 0)
		for rows.Next() {
			values, scan := make([]any, len(columns)), make([]any, len(columns))
			for i := range scan {
				scan[i] = &values[i]
			}
			if err := rows.Scan(scan...); err != nil {
				rows.Close()
				return "", "", err
			}
			row := map[string]any{}
			for i, name := range columns {
				row[name] = values[i]
			}
			list = append(list, row)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return "", "", err
		}
		data[q.name] = list
	}
	b, err := json.Marshal(data)
	if err != nil {
		return "", "", err
	}
	var secret string
	if spec.secret != "" {
		if err := tx.QueryRow(spec.secret, args...).Scan(&secret); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", "", err
		}
	}
	h := sha256.Sum256([]byte(fmt.Sprintf("%s:%d:%s:%s", action.kind, id, b, secret)))
	return string(b), hex.EncodeToString(h[:]), nil
}

func (s *Store) ChangeVersion(ctx context.Context, c *Change) (string, error) {
	if _, _, err := c.spec(); err != nil {
		return "", err
	}
	if c.Policy == nil {
		panic("store.ChangeVersion: Change.Policy is nil")
	}
	tx, err := s.r.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	r := s.changeReader(tx)
	token, err := c.Policy.Principal(ctx, r, c)
	if err != nil {
		return "", err
	}
	if err := c.Policy.Scope(ctx, r, c, token); err != nil {
		return "", err
	}
	_, version, err := c.snapshot(tx)
	return version, err
}

const selectOperation = "SELECT owner_key,owner_id,request_id,request_hash,action,resource_id,before_json,after_json,committed_at,principal,session_id,source,method,resource_key FROM operation"

// 回执身份取自数据库主键而非可复用的数字 token ID；摘要不向 API 消费者暴露凭据哈希。
func operationID(owner, request string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(owner+"\x00"+request)))
}

func scanOperation(scan func(...any) error) (Operation, error) {
	var o Operation
	var owner, principal string
	err := scan(&owner, &o.OwnerID, &o.RequestID, &o.RequestHash, &o.Action, &o.ResourceID, &o.BeforeJSON, &o.AfterJSON, &o.CommittedAt, &principal, &o.SessionID, &o.Source, &o.Method, &o.ResourceKey)
	if err == nil {
		o.ID = operationID(owner, o.RequestID)
		o.Principal = PrincipalKind(principal)
	}
	return o, err
}

// operationOwner 是请求方在 ExecuteChange 回执里的幂等命名空间：API token 是它的身份，会话共用一个键。
func operationOwner(ctx context.Context) string {
	if p, ok := Principal(ctx); ok {
		return p.Identity
	}
	return "session"
}
func readOperation(tx *sql.Tx, owner string, id string) (Operation, error) {
	return scanOperation(tx.QueryRow(selectOperation+" WHERE owner_key=? AND request_id=?", owner, id).Scan)
}
func (s *Store) FindOperation(ctx context.Context, owner int64, id string) (Operation, error) {
	o, err := scanOperation(s.r.QueryRowContext(ctx, selectOperation+" WHERE owner_id=? AND owner_key=? AND request_id=?", owner, operationOwner(ctx), id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return o, err
}

// OperationFilter 是 ListOperations 的条件，零值字段不参与过滤。
type OperationFilter struct {
	// OwnerID 非 0 时只列这枚 API token 的记录。
	OwnerID   int64
	Principal PrincipalKind
	Action    string
	RequestID string
	Limit     int
}

// ListOperations 按提交时刻倒序、同刻按 request_id 升序列出记录。可见性在这里裁决，不靠调用方：ctx 带 API token
// 主体时只列它自己的（owner_key 是它的身份，恢复后数字 id 相同的另一枚 token 的历史不混进来），会话看得到全部。
func (s *Store) ListOperations(ctx context.Context, f OperationFilter) ([]Operation, error) {
	var where []string
	var args []any
	if p, ok := Principal(ctx); ok {
		where, args = append(where, "owner_key=?", "owner_id=?"), append(args, p.Identity, p.ID)
	}
	if f.OwnerID != 0 {
		where, args = append(where, "principal=?", "owner_id=?"), append(args, string(PrincipalAPIToken), f.OwnerID)
	}
	if f.Principal != "" {
		where, args = append(where, "principal=?"), append(args, string(f.Principal))
	}
	if f.Action != "" {
		where, args = append(where, "action=?"), append(args, f.Action)
	}
	if f.RequestID != "" {
		where, args = append(where, "request_id=?"), append(args, f.RequestID)
	}
	query := selectOperation
	if len(where) != 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY committed_at DESC,request_id LIMIT ?"
	args = append(args, f.Limit)
	rows, err := s.r.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Operation
	for rows.Next() {
		o, err := scanOperation(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
