package store

import (
	"context"
	"database/sql"
	"strings"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/agentwire"
	"google.golang.org/protobuf/encoding/protojson"
)

// factsPersistRev 是当前写入 node_facts 的 Facts 字段集合版本。只有 facts_rev 等于它的行，
// 摘要才覆盖了这一版要持久化的字段；旧版写入的行（迁移缺省 0）摘要对不上，启动加载时不采用，
// 从而重新索取。以后持久化的字段集合变了，只把这个常量加一。
const factsPersistRev = 1

func (s *Store) factsTx(nodeID int64, hash uint64, f *heronv1.Facts) func(*sql.Tx) error {
	return func(tx *sql.Tx) error {
		if err := agentwire.ValidateCPUCores(f.GetCpuCores()); err != nil {
			return err
		}
		if err := agentwire.ValidateNetwork(f.GetNetwork()); err != nil {
			return err
		}
		if err := agentwire.ValidateDiagnostics(f.GetDiagnostics()); err != nil {
			return err
		}
		if err := agentwire.ValidateExecutionScope(f.GetExecution()); err != nil {
			return err
		}
		exists, err := nodeExistsTx(tx, nodeID)
		if err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
		network, err := protojson.Marshal(f.GetNetwork())
		if err != nil {
			return err
		}
		diagnostics := []byte("null")
		if f.GetDiagnostics() != nil {
			diagnostics, err = protojson.Marshal(f.GetDiagnostics())
			if err != nil {
				return err
			}
		}
		// 'null' 表示未上报。空对象的 kind 是未指定，读回来会被校验拒绝，不能拿它当缺省。
		execution := []byte("null")
		if f.GetExecution() != nil {
			execution, err = protojson.Marshal(f.GetExecution())
			if err != nil {
				return err
			}
		}
		_, err = tx.Exec(`INSERT INTO node_facts (node_id, facts_hash, hostname, os, kernel, arch, virtualization, cpu_model, cpu_cores, agent_version, icmp_available, updated_at, network, diagnostics, execution, facts_rev)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (node_id) DO UPDATE SET facts_hash = excluded.facts_hash, hostname = excluded.hostname, os = excluded.os,
			kernel = excluded.kernel, arch = excluded.arch, virtualization = excluded.virtualization, cpu_model = excluded.cpu_model,
			cpu_cores = excluded.cpu_cores, agent_version = excluded.agent_version, icmp_available = excluded.icmp_available, updated_at = excluded.updated_at, network = excluded.network, diagnostics = excluded.diagnostics,
			execution = excluded.execution, facts_rev = excluded.facts_rev`,
			nodeID, int64(hash), f.GetHostname(), f.GetOs(), f.GetKernel(), f.GetArch(), f.GetVirtualization(),
			f.GetCpuModel(), f.GetCpuCores(), f.GetAgentVersion(), f.GetIcmpAvailable(), s.clk.Now().Unix(), string(network), string(diagnostics), string(execution), factsPersistRev)
		return err
	}
}

// 解码和结构校验共用同一入口，读库与快照恢复不能绕过上报的状态、地址族和时间约束。
func decodeNetwork(text string) (*heronv1.NetworkInfo, error) {
	network := &heronv1.NetworkInfo{}
	if err := protojson.Unmarshal([]byte(text), network); err != nil {
		return nil, err
	}
	if err := agentwire.ValidateNetwork(network); err != nil {
		return nil, err
	}
	if network.Ipv4 == nil && network.Ipv6 == nil {
		return nil, nil
	}
	return network, nil
}

// null 表示旧版尚未提供诊断，与已报告但字段皆为空的对象不同；读库和恢复共用严格解码。
func decodeDiagnostics(text string) (*heronv1.AgentDiagnostics, error) {
	if strings.Trim(text, " \t\r\n") == "null" {
		return nil, nil
	}
	diagnostics := &heronv1.AgentDiagnostics{}
	if err := protojson.Unmarshal([]byte(text), diagnostics); err != nil {
		return nil, err
	}
	if err := agentwire.ValidateDiagnostics(diagnostics); err != nil {
		return nil, err
	}
	return diagnostics, nil
}

// 'null' 表示未上报，与已上报的对象不同。读库和恢复共用同一校验，不能绕过上报的范围约束。
func decodeExecution(text string) (*heronv1.ExecutionScope, error) {
	if strings.Trim(text, " \t\r\n") == "null" {
		return nil, nil
	}
	execution := &heronv1.ExecutionScope{}
	if err := protojson.Unmarshal([]byte(text), execution); err != nil {
		return nil, err
	}
	if err := agentwire.ValidateExecutionScope(execution); err != nil {
		return nil, err
	}
	return execution, nil
}

func (s *Store) UpsertFacts(ctx context.Context, nodeID int64, hash uint64, f *heronv1.Facts) error {
	return s.write(ctx, s.factsTx(nodeID, hash, f))
}

// UpsertFactsAsync 供上报路径使用：投递即返回，上报不等待数据库。
func (s *Store) UpsertFactsAsync(nodeID int64, hash uint64, f *heronv1.Facts, done func(error)) {
	s.writeAsync(s.factsTx(nodeID, hash, f), done)
}

// FactsHashes 只返回当前字段集合已经确认过的摘要。facts_rev 不等于 factsPersistRev 的行
// （升级前写入、或旧快照恢复出来的）摘要没有覆盖现在持久化的字段，不能当成 hub 已持有。
// 存的是 int64，读回按位转回 uint64；fixed64 的全部取值都能往返。
func (s *Store) FactsHashes(ctx context.Context) (map[int64]uint64, error) {
	rows, err := s.r.QueryContext(ctx, "SELECT node_id, facts_hash FROM node_facts WHERE facts_rev = ?", factsPersistRev)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]uint64{}
	for rows.Next() {
		var id, h int64
		if err := rows.Scan(&id, &h); err != nil {
			return nil, err
		}
		out[id] = uint64(h)
	}
	return out, rows.Err()
}

func (s *Store) QueryFacts(ctx context.Context, nodeID int64, hostname, os *string) error {
	return s.r.QueryRowContext(ctx, "SELECT hostname, os FROM node_facts WHERE node_id = ?", nodeID).Scan(hostname, os)
}
