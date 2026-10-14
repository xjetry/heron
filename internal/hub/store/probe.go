package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"math"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/metric"
	"github.com/xjetry/heron/internal/probelimit"
	"google.golang.org/protobuf/proto"
)

func probeValueColumns() []string {
	return []string{"sent", "lost", "errors", "rtt_sum_us", "rtt_min_us", "rtt_max_us"}
}

// 聚合 min()/max() 忽略 NULL：没有 rtt 样本的桶不会把 0 带进最小值。
func probeAggregates() []string {
	return []string{"sum(sent)", "sum(lost)", "sum(errors)", "sum(rtt_sum_us)", "min(rtt_min_us)", "max(rtt_max_us)"}
}

// 标量 min(a, b) 在任一参数为 NULL 时返回 NULL，所以两侧先各自 coalesce：
// 双方都 NULL 得 NULL，一方 NULL 得另一方，都有值取更小者；max 同理。
const upsertProbeMinute = `INSERT INTO probe_1m (node_id, ts, task_id, sent, lost, errors, rtt_sum_us, rtt_min_us, rtt_max_us)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (node_id, ts, task_id) DO UPDATE SET
  sent = sent + excluded.sent, lost = lost + excluded.lost, errors = errors + excluded.errors,
  rtt_sum_us = rtt_sum_us + excluded.rtt_sum_us,
  rtt_min_us = min(coalesce(rtt_min_us, excluded.rtt_min_us), coalesce(excluded.rtt_min_us, rtt_min_us)),
  rtt_max_us = max(coalesce(rtt_max_us, excluded.rtt_max_us), coalesce(excluded.rtt_max_us, rtt_max_us))`

func probeArgs(r metric.ProbeRow) []any {
	b := r.Bucket
	var mn, mx any
	if b.RttN > 0 {
		mn, mx = int64(b.RttMinUs), int64(b.RttMaxUs)
	}
	return []any{r.NodeID, r.TS, int64(r.TaskID), int64(b.Sent), int64(b.Lost), int64(b.Errors), int64(b.RttSumUs), mn, mx}
}

// scanProbeRows 扫描 (ts, task_id, 六个值列)。RttN 由 sent − lost − errors 推出，
// 与 rtt_min_us 是否为 NULL 必须一致：写侧 RttN 为 0 时才写 NULL。
func scanProbeRows(rows *sql.Rows, nodeID int64) ([]metric.ProbeRow, error) {
	var out []metric.ProbeRow
	for rows.Next() {
		var ts, taskID, sent, lost, errs, sum int64
		var mn, mx sql.NullInt64
		if err := rows.Scan(&ts, &taskID, &sent, &lost, &errs, &sum, &mn, &mx); err != nil {
			return nil, err
		}
		out = append(out, metric.ProbeRow{NodeID: nodeID, TS: ts, TaskID: uint64(taskID), Bucket: probeBucket(sent, lost, errs, sum, mn, mx)})
	}
	return out, rows.Err()
}

// probeBucket 把六个存储值列折回内存桶。RttN 由 sent − lost − errors 推出，与 rtt_min_us
// 是否为 NULL 必须一致：写侧 RttN 为 0 时才写 NULL。单节点与跨节点对比的扫描共用它。
func probeBucket(sent, lost, errs, sum int64, mn, mx sql.NullInt64) *metric.ProbeBucket {
	b := &metric.ProbeBucket{Sent: uint32(sent), Lost: uint32(lost), Errors: uint32(errs), RttSumUs: uint64(sum)}
	if mn.Valid {
		b.RttN = uint32(sent - lost - errs)
		b.RttMinUs, b.RttMaxUs = uint32(mn.Int64), uint32(mx.Int64)
	}
	return b
}

// ProbeTaskRecord 的 NodeIDs 始终是读取时刻的覆盖集合：全部节点与标签交集先展开，显式分配直接读取关联。
// SelectorTags 只描述动态条件，写入请求不能把已展开的 NodeIDs 与条件同时提交。
type ProbeTaskRecord struct {
	Task         *heronv1.ProbeTask
	AllNodes     bool
	NodeIDs      []int64
	SelectorTags []string
	SortOrder    int64
}

// probeCoverage 是"任务覆盖哪些节点"的唯一读法：展开（LoadProbeTasks、SaveProbeTask 的回读、ProbeTaskNodeIDs、
// insertNode 返回的新节点覆盖）与每节点上限的计数（SaveProbeTask、insertNode、UpdateNodeTasks）都从它取。
// 注册表在重载、保存、建节点和修改标签后发布的覆盖也来自这些事务的回读，因此所有读者使用同一口径。
var probeCoverage = "SELECT owner_id AS task_id, node_id FROM (" + coverageSQL("probe_task", "task_id") + ")"

var coveredNodesQuery = "SELECT node_id FROM (" + probeCoverage + ") WHERE task_id = ? ORDER BY node_id"

// LoadProbeTasks 在同一读事务内取得版本、任务和覆盖；并发保存不能把不同版本的行拼成一个清单。
func (s *Store) LoadProbeTasks(ctx context.Context) (uint64, []ProbeTaskRecord, error) {
	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return 0, nil, err
	}
	defer tx.Rollback()
	var version int64
	if err := tx.QueryRowContext(ctx, "SELECT version FROM probe_meta WHERE id = 1").Scan(&version); err != nil {
		return 0, nil, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT id, kind, target, interval_s, timeout_ms, all_nodes, sort_order, dns_server, cert_spki_sha256, config_id FROM probe_task ORDER BY sort_order, id")
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	var out []ProbeTaskRecord
	index := map[uint64]int{}
	for rows.Next() {
		t := &heronv1.ProbeTask{}
		var id, kind, interval, timeout, order int64
		var all bool
		var pin, configID []byte
		if err := rows.Scan(&id, &kind, &t.Target, &interval, &timeout, &all, &order, &t.DnsServer, &pin, &configID); err != nil {
			return 0, nil, err
		}
		t.Id, t.Kind, t.IntervalS, t.TimeoutMs = uint64(id), heronv1.ProbeKind(kind), uint32(interval), uint32(timeout)
		t.CertSpkiSha256, t.ConfigId = pin, configID
		index[t.Id] = len(out)
		out = append(out, ProbeTaskRecord{Task: t, AllNodes: all, SortOrder: order})
	}
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}
	cover, err := tx.QueryContext(ctx, "SELECT task_id, node_id FROM ("+probeCoverage+") ORDER BY task_id, node_id")
	if err != nil {
		return 0, nil, err
	}
	defer cover.Close()
	for cover.Next() {
		var taskID, nodeID int64
		if err := cover.Scan(&taskID, &nodeID); err != nil {
			return 0, nil, err
		}
		if i, ok := index[uint64(taskID)]; ok {
			out[i].NodeIDs = append(out[i].NodeIDs, nodeID)
		}
	}
	if err := cover.Err(); err != nil {
		return 0, nil, err
	}
	for i := range out {
		out[i].SelectorTags, err = selectorTags(tx, "probe_task", "task_id", int64(out[i].Task.Id))
		if err != nil {
			return 0, nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, nil, err
	}
	return uint64(version), out, nil
}

// 指纹动作。缺席（PinKeep）保持已存的 pin，新建任务即不钉。空字节不是清除。
const (
	pinKeep = iota
	pinSet
	pinClear
)

type probeSaveOpt struct {
	action      int
	pin         []byte
	expected    []byte
	expectedSet bool
}

// ProbeSaveOption 是 SaveProbeTask 的可选写入动作。零值是"不改 pin、不带前置条件"，
// 已有调用点因此不必为了新参数改签名。
type ProbeSaveOption func(*probeSaveOpt)

// WithCertPin 把 pin 设为 spki。空切片不是清除，保存会拒绝。
func WithCertPin(spki []byte) ProbeSaveOption {
	return func(o *probeSaveOpt) {
		o.action = pinSet
		o.pin = append([]byte(nil), spki...)
	}
}

// ClearCertPin 显式去掉 pin。与缺席动作不同：缺席保持原值。
func ClearCertPin() ProbeSaveOption {
	return func(o *probeSaveOpt) { o.action = pinClear; o.pin = nil }
}

// WithExpectedConfigID 在写事务里核对任务当前身份。空切片也算"给出了"，既有任务上它不等于当前身份。
func WithExpectedConfigID(id []byte) ProbeSaveOption {
	return func(o *probeSaveOpt) {
		o.expectedSet = true
		o.expected = append([]byte(nil), id...)
	}
}

// SaveProbeTask 在一个事务里写任务、整份替换分配、检查每节点上限、更新版本，并回读保存后的覆盖。
// 三种选择器模式互斥：全部节点、非空标签交集、显式节点；只有显式模式写 nodeIDs 分配行。
//
// 上限在这里而不是调用方检查，因为只有事务内的计数才与其他保存互斥。它按写入之后的覆盖计数，所以 all_nodes 任务
// 计入每个现有节点，显式分配与标签选择器只计入命中节点。insertNode 与 UpdateNodeTasks 在建节点和修改标签时
// 再按同一覆盖谓词检查，任务保存不能替代未来的覆盖变更检查。
//
// task.cert_spki_sha256 与 task.config_id 是只输出字段，这里忽略。pin 只由 ProbeSaveOption 决定。
// 任务内容（ProbeTask 除 id 与 config_id 外的全部字段，含最终的 pin）不变时保留原 config_id；变了就重新生成，
// 并在同一事务删除该任务的证书观测与候选。只做指纹动作、其余字段都是零值时，保留已存的任务字段与选择器。
func (s *Store) SaveProbeTask(ctx context.Context, t *heronv1.ProbeTask, selector NodeSelector, opts ...ProbeSaveOption) (ProbeTaskRecord, uint64, error) {
	var opt probeSaveOpt
	for _, apply := range opts {
		apply(&opt)
	}
	if opt.action == pinSet && len(opt.pin) == 0 {
		return ProbeTaskRecord{}, 0, InvalidTaskError{Detail: "cert_pin.set_spki_sha256: must not be empty; clearing the pin requires clear"}
	}
	if err := selector.Check(); err != nil {
		return ProbeTaskRecord{}, 0, err
	}
	allNodes, nodeIDs, tags := selector.AllNodes, append([]int64(nil), selector.NodeIDs...), append([]string(nil), selector.Tags...)
	saved := &heronv1.ProbeTask{Id: t.GetId(), Kind: t.GetKind(), Target: t.GetTarget(), IntervalS: t.GetIntervalS(), TimeoutMs: t.GetTimeoutMs(), DnsServer: t.GetDnsServer()}
	rec := ProbeTaskRecord{Task: saved, AllNodes: allNodes}
	var version int64
	err := s.writeChange(ctx, ChangeTarget{Action: ActionSaveProbeTask, ResourceID: int64(saved.Id)}, func(tx *sql.Tx) error {
		var stored *heronv1.ProbeTask
		if saved.Id != 0 {
			existing, err := loadProbeTaskTx(tx, saved.Id)
			if err != nil {
				return err
			}
			stored = existing.Task
			// 只做动作：其余字段保持零值。这时请求里的选择器也不是一次编辑，沿用库里的模式，
			// 不能把标签选择器展开后的节点当成显式分配写回去。
			if opt.action != pinKeep && probeTaskBodyZero(t) {
				saved.Kind, saved.Target, saved.IntervalS, saved.TimeoutMs, saved.DnsServer = stored.Kind, stored.Target, stored.IntervalS, stored.TimeoutMs, stored.DnsServer
				allNodes, tags = existing.AllNodes, append([]string(nil), existing.SelectorTags...)
				nodeIDs = nil
				if !allNodes && len(tags) == 0 {
					nodeIDs = append([]int64(nil), existing.NodeIDs...)
				}
				rec.AllNodes = allNodes
			}
		}
		switch opt.action {
		case pinSet:
			saved.CertSpkiSha256 = append([]byte(nil), opt.pin...)
		case pinClear:
			saved.CertSpkiSha256 = nil
		case pinKeep:
			if stored != nil {
				saved.CertSpkiSha256 = append([]byte(nil), stored.CertSpkiSha256...)
			}
		}
		if saved.Id == 0 {
			if opt.expectedSet {
				return InvalidTaskError{Detail: "expected_config_id: must not be set when creating a task"}
			}
		} else {
			if opt.action == pinSet && !opt.expectedSet {
				return InvalidTaskError{Detail: "expected_config_id: required when setting cert_pin on an existing task"}
			}
			if opt.expectedSet && !bytes.Equal(opt.expected, stored.GetConfigId()) {
				return PreconditionError{Detail: "expected_config_id: does not match the task's current config_id"}
			}
		}
		var err error
		if stored == nil || !proto.Equal(taskContent(saved), taskContent(stored)) {
			saved.ConfigId, err = randomConfigID()
			if err != nil {
				return err
			}
		} else {
			saved.ConfigId = append([]byte(nil), stored.ConfigId...)
		}
		if err := probelimit.CheckTask(saved); err != nil {
			return InvalidTaskError{Detail: err.Error()}
		}
		if saved.Id == 0 {
			res, err := tx.Exec("INSERT INTO probe_task (kind, target, interval_s, timeout_ms, created_at, all_nodes, sort_order, dns_server, cert_spki_sha256, config_id) SELECT ?, ?, ?, ?, ?, ?, COALESCE(MAX(sort_order), -1) + 1, ?, ?, ? FROM probe_task",
				int64(saved.Kind), saved.Target, int64(saved.IntervalS), int64(saved.TimeoutMs), s.clk.Now().Unix(), allNodes, saved.DnsServer, pinArg(saved.CertSpkiSha256), saved.ConfigId)
			if err != nil {
				return err
			}
			id, err := res.LastInsertId()
			if err != nil {
				return err
			}
			saved.Id = uint64(id)
		} else {
			// 证书到期规则要求任务保持 https:// 的 HTTP 任务（requireHTTPSProbeTask）；改成别的形状会让规则
			// 永远等不到新观测。与删除同一形状地拒绝，而不是改出一个违反规则约束的库。
			if !probelimit.IsHTTPSTarget(saved.Kind, saved.Target) {
				if err := checkAlertReferences(ctx, tx, "SELECT id, name FROM alert_rule WHERE task_id = ? AND kind = '"+string(KindCertExpiry)+"' ORDER BY id", ObjectProbeTask, int64(saved.Id)); err != nil {
					return err
				}
			}
			res, err := tx.Exec("UPDATE probe_task SET kind = ?, target = ?, interval_s = ?, timeout_ms = ?, all_nodes = ?, dns_server = ?, cert_spki_sha256 = ?, config_id = ? WHERE id = ?",
				int64(saved.Kind), saved.Target, int64(saved.IntervalS), int64(saved.TimeoutMs), allNodes, saved.DnsServer, pinArg(saved.CertSpkiSha256), saved.ConfigId, int64(saved.Id))
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return NotFoundError{Kind: ObjectProbeTask, ID: int64(saved.Id)}
			}
			if !bytes.Equal(saved.ConfigId, stored.GetConfigId()) {
				if err := deleteTaskCertsTx(tx, saved.Id); err != nil {
					return err
				}
			}
			if _, err := tx.Exec("DELETE FROM probe_task_node WHERE task_id = ?", int64(saved.Id)); err != nil {
				return err
			}
		}
		if !allNodes {
			for _, nodeID := range nodeIDs {
				exists, err := nodeExistsTx(tx, nodeID)
				if err != nil {
					return err
				}
				if !exists {
					return NotFoundError{Kind: ObjectNode, ID: nodeID}
				}
				if _, err := tx.Exec("INSERT INTO probe_task_node (task_id, node_id) VALUES (?, ?)", int64(saved.Id), nodeID); err != nil {
					return err
				}
			}
		}
		if err := setSelectorTags(tx, "probe_task", "task_id", int64(saved.Id), tags); err != nil {
			return err
		}
		if err := checkProbeCoverageLimit(tx); err != nil {
			return err
		}
		if rec.SelectorTags, err = selectorTags(tx, "probe_task", "task_id", int64(saved.Id)); err != nil {
			return err
		}
		if rec.NodeIDs, err = coveredNodesTx(tx, saved.Id); err != nil {
			return err
		}
		if err := tx.QueryRow("SELECT sort_order FROM probe_task WHERE id = ?", int64(saved.Id)).Scan(&rec.SortOrder); err != nil {
			return err
		}
		v, err := bumpProbeVersion(tx, s.clk.Now().Unix())
		version = v
		return err
	})
	if err != nil {
		return ProbeTaskRecord{}, 0, err
	}
	return rec, uint64(version), nil
}

func probeTaskBodyZero(t *heronv1.ProbeTask) bool {
	return t.GetKind() == 0 && t.GetTarget() == "" && t.GetIntervalS() == 0 && t.GetTimeoutMs() == 0 && t.GetDnsServer() == ""
}

// taskContent 是配置身份比较的对象：id 与 config_id 不参与，其余字段（含 pin）都参与。
func taskContent(t *heronv1.ProbeTask) *heronv1.ProbeTask {
	c := proto.Clone(t).(*heronv1.ProbeTask)
	c.Id = 0
	c.ConfigId = nil
	return c
}

func randomConfigID() ([]byte, error) {
	b := make([]byte, probelimit.ConfigIDLen)
	_, err := rand.Read(b)
	return b, err
}

func pinArg(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

func loadProbeTaskTx(tx *sql.Tx, id uint64) (ProbeTaskRecord, error) {
	t := &heronv1.ProbeTask{Id: id}
	var kind, interval, timeout, order int64
	var all bool
	var pin, configID []byte
	err := tx.QueryRow(`SELECT kind, target, interval_s, timeout_ms, all_nodes, sort_order, dns_server, cert_spki_sha256, config_id FROM probe_task WHERE id = ?`, int64(id)).
		Scan(&kind, &t.Target, &interval, &timeout, &all, &order, &t.DnsServer, &pin, &configID)
	if errors.Is(err, sql.ErrNoRows) {
		return ProbeTaskRecord{}, NotFoundError{Kind: ObjectProbeTask, ID: int64(id)}
	}
	if err != nil {
		return ProbeTaskRecord{}, err
	}
	t.Kind, t.IntervalS, t.TimeoutMs = heronv1.ProbeKind(kind), uint32(interval), uint32(timeout)
	t.CertSpkiSha256, t.ConfigId = pin, configID
	rec := ProbeTaskRecord{Task: t, AllNodes: all, SortOrder: order}
	if rec.SelectorTags, err = selectorTags(tx, "probe_task", "task_id", int64(id)); err != nil {
		return ProbeTaskRecord{}, err
	}
	if rec.NodeIDs, err = coveredNodesTx(tx, id); err != nil {
		return ProbeTaskRecord{}, err
	}
	return rec, nil
}

func deleteTaskCertsTx(tx *sql.Tx, id uint64) error {
	if _, err := tx.Exec("DELETE FROM probe_cert WHERE task_id = ?", int64(id)); err != nil {
		return err
	}
	_, err := tx.Exec("DELETE FROM probe_cert_presented WHERE task_id = ?", int64(id))
	return err
}

func coveredNodesTx(tx *sql.Tx, taskID uint64) ([]int64, error) {
	return scanIDs(tx.Query(coveredNodesQuery, int64(taskID)))
}

// ReorderProbeTasks 只改变展示顺序，不改变 agent 的任务配置或版本。
func (s *Store) ReorderProbeTasks(ctx context.Context, ids []uint64) error {
	signed := make([]int64, len(ids))
	for i, id := range ids {
		if id == 0 || id > math.MaxInt64 {
			return ErrBadOrder
		}
		signed[i] = int64(id)
	}
	return s.reorder(ctx, "probe_task", signed)
}

// DeleteProbeTask 删任务、分配、标签关联与该任务的证书观测并加版本，并在同一事务里登记 kind=task 的清理作业；
// 探测历史行由维护循环分块清理（cleanup.go），不在这个事务里删。
// 被证书到期规则引用的任务由 checkAlertReferences 拦下，删不掉，probe_cert 行不会因删任务而成为孤儿。
func (s *Store) DeleteProbeTask(ctx context.Context, id uint64) (uint64, error) {
	var version int64
	err := s.writeChange(ctx, ChangeTarget{Action: ActionDeleteProbeTask, ResourceID: int64(id)}, func(tx *sql.Tx) error {
		if err := checkAlertReferences(ctx, tx, "SELECT id, name FROM alert_rule WHERE task_id = ? ORDER BY id", ObjectProbeTask, int64(id)); err != nil {
			return err
		}
		res, err := tx.Exec("DELETE FROM probe_task WHERE id = ?", int64(id))
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return NotFoundError{Kind: ObjectProbeTask, ID: int64(id)}
		}
		if _, err := tx.Exec("DELETE FROM probe_task_node WHERE task_id = ?", int64(id)); err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM probe_task_tag WHERE task_id = ?", int64(id)); err != nil {
			return err
		}
		if err := deleteTaskCertsTx(tx, id); err != nil {
			return err
		}
		now := s.clk.Now().Unix()
		if err := enqueueCleanup(tx, cleanupKindTask, 0, int64(id), now); err != nil {
			return err
		}
		v, err := bumpProbeVersion(tx, now)
		version = v
		return err
	})
	return uint64(version), err
}

// 版本的不变式：任何一个节点的清单变了，版本就变。agent 只比较相等，版本相同即认定清单未变、不再重取，所以
// 每个可能改变节点清单的事务都调用它：任务保存与删除、insertNode 建节点、UpdateNodeTasks 替换节点标签。
// 删除节点不调用：只有被删节点的清单消失，它的 token 已撤销，其余节点的清单不变。
//
// 版本与任务及分配同事务更新，并以修改时刻的 Unix 秒托底。恢复后修改的 Unix 秒大于旧库最后版本值时不会碰撞；
// 同秒重做、时钟回拨或旧版本超前仍可能碰撞，此时需重启 agent 使它重新对账，不能把时间托底当作全局唯一保证。
func bumpProbeVersion(tx *sql.Tx, now int64) (int64, error) {
	var version int64
	err := tx.QueryRow("UPDATE probe_meta SET version = max(version + 1, ?) WHERE id = 1 RETURNING version", now).Scan(&version)
	return version, err
}

// ProbeTaskNodeIDs 返回任务当前覆盖的节点（all_nodes 任务展开为全部节点），升序。
func (s *Store) ProbeTaskNodeIDs(ctx context.Context, taskID uint64) ([]int64, error) {
	return scanIDs(s.r.QueryContext(ctx, coveredNodesQuery, int64(taskID)))
}

func scanIDs(rows *sql.Rows, err error) ([]int64, error) {
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
