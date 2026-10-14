package store

import (
	"database/sql"
	"fmt"
	"slices"

	"github.com/xjetry/heron/internal/probelimit"
)

// coverageSQL 是任务与规则共同的作用域谓词。标签选择器必须至少有一项；空关系不能借由全称条件放宽成全部节点。
// 写侧拒绝多模式混用，因此三个分支互斥，计数与展开都不会重复节点。
func coverageSQL(table, key string) string {
	return `SELECT t.id AS owner_id, n.id AS node_id FROM ` + table + ` t CROSS JOIN node n WHERE t.all_nodes = 1
UNION ALL SELECT ` + key + `, node_id FROM ` + table + `_node
UNION ALL SELECT t.id, n.id FROM ` + table + ` t CROSS JOIN node n
WHERE EXISTS (SELECT 1 FROM ` + table + `_tag st WHERE st.` + key + ` = t.id)
AND NOT EXISTS (SELECT 1 FROM ` + table + `_tag st WHERE st.` + key + ` = t.id
AND NOT EXISTS (SELECT 1 FROM node_tag nt WHERE nt.node_id = n.id AND nt.tag_id = st.tag_id))`
}

type NodeSelector struct {
	AllNodes bool
	NodeIDs  []int64
	Tags     []string
}

func (s NodeSelector) Check() error {
	all, ids, tags := s.AllNodes, s.NodeIDs, s.Tags
	if (all && (len(ids) != 0 || len(tags) != 0)) || (len(tags) != 0 && len(ids) != 0) {
		return KindFieldError{"selector_tags", "all_nodes, node_ids and selector_tags must not be combined"}
	}
	for _, tag := range tags {
		if tag == "" {
			return KindFieldError{"selector_tags", "must not contain an empty tag"}
		}
	}
	return nil
}

func setSelectorTags(tx *sql.Tx, table, key string, id int64, names []string) error {
	if _, err := tx.Exec("DELETE FROM "+table+"_tag WHERE "+key+" = ?", id); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, name := range names {
		fold := TagFold(name)
		if seen[fold] {
			continue
		}
		seen[fold] = true
		var tagID int64
		if err := tx.QueryRow("SELECT id FROM tag WHERE name_fold = ?", fold).Scan(&tagID); err != nil {
			if err == sql.ErrNoRows {
				return KindFieldError{"selector_tags", fmt.Sprintf("tag %q does not exist", name)}
			}
			return err
		}
		if _, err := tx.Exec("INSERT INTO "+table+"_tag ("+key+", tag_id) VALUES (?, ?)", id, tagID); err != nil {
			return err
		}
	}
	return nil
}

func selectorTags(tx *sql.Tx, table, key string, id int64) ([]string, error) {
	rows, err := tx.Query("SELECT t.name FROM "+table+"_tag st JOIN tag t ON t.id = st.tag_id WHERE st."+key+" = ? ORDER BY t.name_fold", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

func checkProbeCoverageLimit(tx *sql.Tx) error {
	var node, count int64
	err := tx.QueryRow("SELECT node_id, COUNT(*) FROM ("+probeCoverage+") GROUP BY node_id HAVING COUNT(*) > ? ORDER BY node_id LIMIT 1", probelimit.MaxTasksPerNode).Scan(&node, &count)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	return NodeLimitError{NodeID: node, Tasks: int(count), Max: probelimit.MaxTasksPerNode}
}

// ScopeContains 只解释已展开的覆盖；全部节点开关显式放宽，空显式集合或无匹配的标签集合均不覆盖节点。
func ScopeContains(all bool, ids []int64, id int64) bool { return all || slices.Contains(ids, id) }

var alertCoverage = coverageSQL("alert_rule", "rule_id")

func pruneAlertScopes(tx *sql.Tx) error {
	_, err := tx.Exec("DELETE FROM alert_state WHERE NOT EXISTS (SELECT 1 FROM (" + alertCoverage + ") c WHERE c.owner_id = alert_state.rule_id AND c.node_id = alert_state.node_id)")
	return err
}
