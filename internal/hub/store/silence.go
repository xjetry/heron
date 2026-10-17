package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"
)

type SilenceKind string

const (
	SilenceDaily SilenceKind = "daily"
	SilenceOnce  SilenceKind = "once"
)

// Silence 是一段维护静默窗口：同时覆盖规则与节点时，窗口内产生的告警事件照常生成但不投递。
// 作用域与 AlertRule 同一形状：AllNodes 为真时不存 silence_node 行；SelectorTags 非空时 NodeIDs 是当前标签
// 交集的展开结果，否则是显式集合；空覆盖不代表全部节点。
// AllRules 为真时覆盖全部可静默规则；否则只覆盖 RuleIDs，规则删除后留下的空集不覆盖任何告警。
type Silence struct {
	ID           int64
	Name         string
	Enabled      bool
	AllNodes     bool
	AllRules     bool
	RuleIDs      []int64
	NodeIDs      []int64
	SelectorTags []string
	Kind         SilenceKind
	// DAILY 的窗口：HH:MM，按 hub 时区（--timezone）的墙钟判定，允许跨午夜；ONCE 时为空。
	StartHHMM, EndHHMM string
	// ONCE 的窗口：Unix 秒，含 FromAt、不含 UntilAt；DAILY 时为 0。
	FromAt, UntilAt int64
	Reason          string
	CreatedAt       time.Time
}

// ParseHHMM 解析 HH:MM 为午夜起的分钟数。写侧校验与窗口判定（alert.SilenceActive）共用这一个口径：
// 恰为两位小时、冒号、两位分钟，小时 0–23、分钟 0–59；不带前导零或越界都不是 HH:MM。
func ParseHHMM(s string) (int, error) {
	if len(s) != 5 || s[2] != ':' {
		return 0, fmt.Errorf("%q is not HH:MM", s)
	}
	digits := func(c byte) (int, bool) { return int(c - '0'), c >= '0' && c <= '9' }
	h1, ok1 := digits(s[0])
	h2, ok2 := digits(s[1])
	m1, ok3 := digits(s[3])
	m2, ok4 := digits(s[4])
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return 0, fmt.Errorf("%q is not HH:MM", s)
	}
	h, m := h1*10+h2, m1*10+m2
	if h > 23 || m > 59 {
		return 0, fmt.Errorf("%q is not HH:MM", s)
	}
	return h*60 + m, nil
}

// CheckSilenceFields 裁决规则范围及种类与专用字段的组合：HH:MM 只属于 DAILY，起止时刻只属于 ONCE；
// 与 CheckKindFields 同一原则——非法组合报错而不改写，静默清零是放宽方向。
func CheckSilenceFields(s Silence) error {
	if s.AllRules == (len(s.RuleIDs) > 0) {
		return KindFieldError{"all_rules", "exactly one of all_rules or non-empty rule_ids is required"}
	}
	switch s.Kind {
	case SilenceDaily:
		if s.FromAt != 0 || s.UntilAt != 0 {
			return KindFieldError{"from_at", "must be 0 unless kind is once"}
		}
		start, err := ParseHHMM(s.StartHHMM)
		if err != nil {
			return KindFieldError{"start_hhmm", "must be HH:MM, e.g. 22:00"}
		}
		end, err := ParseHHMM(s.EndHHMM)
		if err != nil {
			return KindFieldError{"end_hhmm", "must be HH:MM, e.g. 06:00"}
		}
		// 起止相同的窗口没有可读的语义（全天还是空？），写侧拒绝而不是裁决。
		if start == end {
			return KindFieldError{"end_hhmm", "must differ from start_hhmm"}
		}
	case SilenceOnce:
		if s.StartHHMM != "" || s.EndHHMM != "" {
			return KindFieldError{"start_hhmm", "must be empty unless kind is daily"}
		}
		if s.FromAt >= s.UntilAt {
			return KindFieldError{"from_at", "must be less than until_at"}
		}
	default:
		return KindFieldError{"kind", "must be daily or once"}
	}
	if utf8.RuneCountInString(s.Reason) > 256 {
		return KindFieldError{"reason", "must be at most 256 characters"}
	}
	return nil
}

// silenceCoverage 与 alertCoverage 同形：三种作用域互斥由写侧保证，展开不会重复节点。
var silenceCoverage = coverageSQL("silence", "silence_id")

func (s *Store) ListSilences(ctx context.Context) ([]Silence, error) {
	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	out, err := listSilencesTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// 主表、关联与标签展开在同一读事务中裁决，与 listAlertRulesTx 同一形状。
func listSilencesTx(ctx context.Context, tx *sql.Tx) ([]Silence, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, name, enabled, all_nodes, kind, start_hhmm, end_hhmm, from_at, until_at, reason, created_at, all_rules FROM silence ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Silence
	index := map[int64]int{}
	for rows.Next() {
		var si Silence
		var created int64
		if err := rows.Scan(&si.ID, &si.Name, &si.Enabled, &si.AllNodes, &si.Kind, &si.StartHHMM, &si.EndHHMM, &si.FromAt, &si.UntilAt, &si.Reason, &created, &si.AllRules); err != nil {
			return nil, err
		}
		si.CreatedAt = time.Unix(created, 0).UTC()
		index[si.ID] = len(out)
		out = append(out, si)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows2, err := tx.QueryContext(ctx, "SELECT silence_id, node_id FROM silence_node ORDER BY silence_id, node_id")
	if err != nil {
		return nil, err
	}
	for rows2.Next() {
		var silence, id int64
		if err := rows2.Scan(&silence, &id); err != nil {
			rows2.Close()
			return nil, err
		}
		if i, ok := index[silence]; ok {
			out[i].NodeIDs = append(out[i].NodeIDs, id)
		}
	}
	rows2.Close()
	if err := rows2.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		out[i].RuleIDs, err = scanIDs(tx.QueryContext(ctx, "SELECT rule_id FROM silence_rule WHERE silence_id = ? ORDER BY rule_id", out[i].ID))
		if err != nil {
			return nil, err
		}
		out[i].SelectorTags, err = selectorTags(tx, "silence", "silence_id", out[i].ID)
		if err != nil {
			return nil, err
		}
		if len(out[i].SelectorTags) > 0 {
			out[i].NodeIDs, err = scanIDs(tx.Query("SELECT node_id FROM ("+silenceCoverage+") WHERE owner_id = ? ORDER BY node_id", out[i].ID))
			if err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// 引用检查与保存同在单写事务，删除不能插入两者之间造成孤儿引用（与 SaveAlertRule 同一不变式）。
func (s *Store) SaveSilence(ctx context.Context, si Silence) (Silence, error) {
	if err := (NodeSelector{AllNodes: si.AllNodes, NodeIDs: si.NodeIDs, Tags: si.SelectorTags}).Check(); err != nil {
		return Silence{}, err
	}
	if err := CheckSilenceFields(si); err != nil {
		return Silence{}, err
	}
	si.NodeIDs = sortedAlertIDs(si.NodeIDs)
	si.RuleIDs = sortedAlertIDs(si.RuleIDs)
	if si.AllNodes {
		si.NodeIDs = nil
	}
	err := s.write(ctx, func(tx *sql.Tx) error {
		for _, id := range si.RuleIDs {
			var kind AlertKind
			err := tx.QueryRowContext(ctx, "SELECT kind FROM alert_rule WHERE id = ?", id).Scan(&kind)
			if errors.Is(err, sql.ErrNoRows) {
				return KindFieldError{"rule_ids", fmt.Sprintf("rule %d does not exist", id)}
			}
			if err != nil {
				return err
			}
			if kind.IsCalendar() {
				return KindFieldError{"rule_ids", fmt.Sprintf("rule %d is a calendar rule (%s) and cannot be silenced", id, kind)}
			}
		}
		for _, id := range si.NodeIDs {
			exists, err := nodeExistsTx(tx, id)
			if err != nil {
				return err
			}
			if !exists {
				return NotFoundError{Kind: ObjectNode, ID: id}
			}
		}
		// 每种窗口只落自己的专用列，其余写缺省值；CheckSilenceFields 已保证别的种类的专用字段是零值。
		var startHHMM, endHHMM string
		var fromAt, untilAt int64
		if si.Kind == SilenceDaily {
			startHHMM, endHHMM = si.StartHHMM, si.EndHHMM
		}
		if si.Kind == SilenceOnce {
			fromAt, untilAt = si.FromAt, si.UntilAt
		}
		var created int64
		if si.ID == 0 {
			created = s.clk.Now().Unix()
			err := tx.QueryRow(`INSERT INTO silence (name, enabled, all_nodes, kind, start_hhmm, end_hhmm, from_at, until_at, reason, created_at, all_rules)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`, si.Name, si.Enabled, si.AllNodes, si.Kind, startHHMM, endHHMM, fromAt, untilAt, si.Reason, created, si.AllRules).Scan(&si.ID)
			if err != nil {
				return err
			}
		} else {
			err := tx.QueryRow(`UPDATE silence SET name = ?, enabled = ?, all_nodes = ?, kind = ?, start_hhmm = ?, end_hhmm = ?, from_at = ?, until_at = ?, reason = ?, all_rules = ?
				WHERE id = ? RETURNING created_at`, si.Name, si.Enabled, si.AllNodes, si.Kind, startHHMM, endHHMM, fromAt, untilAt, si.Reason, si.AllRules, si.ID).Scan(&created)
			if errors.Is(err, sql.ErrNoRows) {
				return NotFoundError{Kind: ObjectSilence, ID: si.ID}
			}
			if err != nil {
				return err
			}
		}
		si.CreatedAt = time.Unix(created, 0).UTC()
		if _, err := tx.Exec("DELETE FROM silence_rule WHERE silence_id = ?", si.ID); err != nil {
			return err
		}
		for _, id := range si.RuleIDs {
			if _, err := tx.Exec("INSERT INTO silence_rule (silence_id, rule_id) VALUES (?, ?)", si.ID, id); err != nil {
				return err
			}
		}
		if _, err := tx.Exec("DELETE FROM silence_node WHERE silence_id = ?", si.ID); err != nil {
			return err
		}
		for _, id := range si.NodeIDs {
			if _, err := tx.Exec("INSERT INTO silence_node (silence_id, node_id) VALUES (?, ?)", si.ID, id); err != nil {
				return err
			}
		}
		if err := setSelectorTags(tx, "silence", "silence_id", si.ID, si.SelectorTags); err != nil {
			return err
		}
		var err error
		si.SelectorTags, err = selectorTags(tx, "silence", "silence_id", si.ID)
		if err != nil {
			return err
		}
		if len(si.SelectorTags) > 0 {
			si.NodeIDs, err = scanIDs(tx.Query("SELECT node_id FROM ("+silenceCoverage+") WHERE owner_id = ? ORDER BY node_id", si.ID))
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return Silence{}, err
	}
	return si, nil
}

func (s *Store) DeleteSilence(ctx context.Context, id int64) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		if err := deleteAlertEntity(tx, "silence", ObjectSilence, id); err != nil {
			return err
		}
		for _, table := range []string{"silence_node", "silence_tag", "silence_rule"} {
			if _, err := tx.Exec("DELETE FROM "+table+" WHERE silence_id = ?", id); err != nil {
				return err
			}
		}
		return nil
	})
}
