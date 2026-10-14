package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/probelimit"
)

// probe_cert 是 (节点, 任务) 的最新一份证书到期观测：ingest 校验通过后的覆盖写，
// 与分钟桶的折叠路径分开——这是"最新值"，不是时间序列。
// 写入在写协程里，入口检查之后任务可能已被改或删。条件不满足就不写：旧身份不能重建行，也不能覆盖新身份的行。

// certWriteOK 在写事务里核对观测还能不能落库。
// 写队列可能晚于任务编辑执行，因此在同一事务内重查当前身份、HTTPS 类型与分配范围。
// 空身份无法证明观测属于哪个配置，即使任务未钉住也不能放宽准入。
func certWriteOK(tx *sql.Tx, nodeID int64, taskID uint64, configID []byte) (bool, error) {
	if len(configID) == 0 {
		return false, nil
	}
	var kind int64
	var target string
	var current []byte
	err := tx.QueryRow(`SELECT kind, target, config_id FROM probe_task WHERE id = ?`, int64(taskID)).Scan(&kind, &target, &current)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !probelimit.IsHTTPSTarget(heronv1.ProbeKind(kind), target) {
		return false, nil
	}
	if !bytes.Equal(configID, current) {
		return false, nil
	}
	// 只问"节点是否在覆盖里"。覆盖是三种选择器模式的 UNION ALL，不能拿行数是否恰为 1 当判据：
	// 那依赖"同一任务只存一种模式的行"这一条另由保存路径维持的约束，约束一旦被打破，观测就会静默写不进来。
	var assigned bool
	if err := tx.QueryRow("SELECT EXISTS (SELECT 1 FROM ("+probeCoverage+") WHERE task_id = ? AND node_id = ?)", int64(taskID), nodeID).Scan(&assigned); err != nil {
		return false, err
	}
	return assigned, nil
}

// upsertProbeCertTx 覆盖写一份观测，返回 not_after 是否发生变化（含首次写入）。
// 条件不满足时不写，返回 false, nil：调用方不会因此重试，也不会把整批上报判失败。
// 变化信号给调用方决定是否触发一次证书到期评估：同值重复上报不必重新评估。
func upsertProbeCertTx(tx *sql.Tx, nodeID int64, taskID uint64, notAfter, observedAt int64, configID []byte) (bool, error) {
	ok, err := certWriteOK(tx, nodeID, taskID, configID)
	if err != nil || !ok {
		return false, err
	}
	var prev int64
	err = tx.QueryRow("SELECT not_after FROM probe_cert WHERE node_id = ? AND task_id = ?", nodeID, int64(taskID)).Scan(&prev)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		prev = 0
	case err != nil:
		return false, err
	}
	if _, err := tx.Exec(`INSERT INTO probe_cert (node_id, task_id, not_after, observed_at, config_id) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (node_id, task_id) DO UPDATE SET not_after = excluded.not_after, observed_at = excluded.observed_at, config_id = excluded.config_id`,
		nodeID, int64(taskID), notAfter, observedAt, configID); err != nil {
		return false, err
	}
	return prev != notAfter, nil
}

// UpsertProbeCert 同步覆盖写一份观测，返回 not_after 是否变化。管理面与测试用它；
// 上报路径用 UpsertProbeCertAsync（上报只碰内存，写走写协程）。
// configID 必须非空且等于当前任务身份，否则不写入。
func (s *Store) UpsertProbeCert(ctx context.Context, nodeID int64, taskID uint64, notAfter, observedAt int64, configID []byte) (bool, error) {
	var changed bool
	err := s.write(ctx, func(tx *sql.Tx) error {
		var err error
		changed, err = upsertProbeCertTx(tx, nodeID, taskID, notAfter, observedAt, configID)
		return err
	})
	return changed, err
}

// UpsertProbeCertAsync 经写协程覆盖写一份观测；done 在写协程里收到 not_after 是否变化，
// 需要据此调用会写库的入口（如证书到期评估）时必须在回调里另起协程，不能让写协程等自己。
func (s *Store) UpsertProbeCertAsync(nodeID int64, taskID uint64, notAfter, observedAt int64, configID []byte, done func(changed bool, err error)) {
	var changed bool
	s.writeAsync(func(tx *sql.Tx) error {
		var err error
		changed, err = upsertProbeCertTx(tx, nodeID, taskID, notAfter, observedAt, configID)
		return err
	}, func(err error) {
		done(changed, err)
	})
}

// upsertPresentedTx 覆盖写一份信任候选。身份为空、身份不符、任务不存在或节点已不分配时不写。
func upsertPresentedTx(tx *sql.Tx, nodeID int64, taskID uint64, configID, spki []byte, notAfter, observedAt int64, reason int32) error {
	ok, err := certWriteOK(tx, nodeID, taskID, configID)
	if err != nil || !ok {
		return err
	}
	_, err = tx.Exec(`INSERT INTO probe_cert_presented (node_id, task_id, config_id, spki_sha256, not_after, reason, observed_at) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (node_id, task_id) DO UPDATE SET config_id = excluded.config_id, spki_sha256 = excluded.spki_sha256, not_after = excluded.not_after, reason = excluded.reason, observed_at = excluded.observed_at`,
		nodeID, int64(taskID), configID, spki, notAfter, reason, observedAt)
	return err
}

// UpsertPresentedAsync 经写协程写一份候选。done 只收到错误；不写不是错误。
func (s *Store) UpsertPresentedAsync(nodeID int64, taskID uint64, configID, spki []byte, notAfter, observedAt int64, reason int32, done func(error)) {
	s.writeAsync(func(tx *sql.Tx) error {
		return upsertPresentedTx(tx, nodeID, taskID, configID, spki, notAfter, observedAt, reason)
	}, done)
}

// ProbeCertNodeView 只展示与当前配置身份相符的成功观测和候选。
type ProbeCertNodeView struct {
	NodeID    int64
	Current   *ProbeCertObs
	Candidate *ProbeCertCandidate
}

type ProbeCertObs struct {
	NotAfter   int64
	ObservedAt int64
}

type ProbeCertCandidate struct {
	SPKI       []byte
	NotAfter   int64
	Reason     int32
	ObservedAt int64
}

// ProbeCertificateView 是一次读快照里的任务身份、完整覆盖（供调用方做可标注判断）与可见节点上的证书行。
type ProbeCertificateView struct {
	Task    ProbeTaskRecord
	Visible []ProbeCertNodeView
	Found   bool
}

// ListProbeCertificates 在一个只读事务里读任务、覆盖与证书行。任务不存在时 Found 为 false。
// Visible 只含调用方可见、且当前分配了该任务的节点，按节点全序。
func (s *Store) ListProbeCertificates(ctx context.Context, taskID uint64) (ProbeCertificateView, error) {
	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ProbeCertificateView{}, err
	}
	defer tx.Rollback()
	rec, err := loadProbeTaskTx(tx, taskID)
	if errors.Is(err, ErrNotFound) {
		return ProbeCertificateView{}, nil
	}
	if err != nil {
		return ProbeCertificateView{}, err
	}
	rows, err := tx.QueryContext(ctx,
		"SELECT n.id FROM node n WHERE "+nodeScopeSQL(ctx, "n.id")+" AND n.id IN (SELECT node_id FROM ("+probeCoverage+") WHERE task_id = ?) ORDER BY n.sort_order, n.id",
		int64(taskID))
	if err != nil {
		return ProbeCertificateView{}, err
	}
	defer rows.Close()
	var visible []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return ProbeCertificateView{}, err
		}
		visible = append(visible, id)
	}
	if err := rows.Err(); err != nil {
		return ProbeCertificateView{}, err
	}
	current := map[int64]ProbeCertObs{}
	certRows, err := tx.QueryContext(ctx, "SELECT node_id, config_id, not_after, observed_at FROM probe_cert WHERE task_id = ?", int64(taskID))
	if err != nil {
		return ProbeCertificateView{}, err
	}
	defer certRows.Close()
	for certRows.Next() {
		var node, notAfter, observed int64
		var cid []byte
		if err := certRows.Scan(&node, &cid, &notAfter, &observed); err != nil {
			return ProbeCertificateView{}, err
		}
		ob := ProbeCertObs{NotAfter: notAfter, ObservedAt: observed}
		if len(cid) > 0 && bytes.Equal(cid, rec.Task.GetConfigId()) {
			current[node] = ob
		}
	}
	if err := certRows.Err(); err != nil {
		return ProbeCertificateView{}, err
	}
	candidates := map[int64]ProbeCertCandidate{}
	presented, err := tx.QueryContext(ctx, "SELECT node_id, config_id, spki_sha256, not_after, reason, observed_at FROM probe_cert_presented WHERE task_id = ?", int64(taskID))
	if err != nil {
		return ProbeCertificateView{}, err
	}
	defer presented.Close()
	for presented.Next() {
		var node, notAfter, observed int64
		var reason int32
		var cid, spki []byte
		if err := presented.Scan(&node, &cid, &spki, &notAfter, &reason, &observed); err != nil {
			return ProbeCertificateView{}, err
		}
		if bytes.Equal(cid, rec.Task.GetConfigId()) && len(cid) > 0 {
			candidates[node] = ProbeCertCandidate{SPKI: spki, NotAfter: notAfter, Reason: reason, ObservedAt: observed}
		}
	}
	if err := presented.Err(); err != nil {
		return ProbeCertificateView{}, err
	}
	out := ProbeCertificateView{Task: rec, Found: true}
	for _, id := range visible {
		node := ProbeCertNodeView{NodeID: id}
		if ob, ok := current[id]; ok {
			node.Current = &ob
		}
		if c, ok := candidates[id]; ok {
			node.Candidate = &c
		}
		out.Visible = append(out.Visible, node)
	}
	return out, nil
}

// ProbeCertsByTask 返回该任务全部节点的最新证书观测（node_id → not_after，Unix 秒），
// 供证书到期告警评估；没有行的 (节点, 任务) 不在结果里——无读数，不是任何证书状态。
func (s *Store) ProbeCertsByTask(ctx context.Context, taskID uint64) (map[int64]int64, error) {
	rows, err := s.r.QueryContext(ctx, "SELECT node_id, not_after FROM probe_cert WHERE task_id = ? ORDER BY node_id", int64(taskID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var nodeID, notAfter int64
		if err := rows.Scan(&nodeID, &notAfter); err != nil {
			return nil, err
		}
		out[nodeID] = notAfter
	}
	return out, rows.Err()
}
