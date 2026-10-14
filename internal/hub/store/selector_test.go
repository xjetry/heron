package store

import (
	"database/sql"
	"testing"
)

func TestNodeUpdateScopeReadFailureRollsBackAllWrites(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	id, _, err := s.CreateNode(t.Context(), NewNode{Name: "original"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	version, _, err := s.LoadProbeTasks(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// 破坏仅规则回读依赖的列；节点更新与版本推进已能执行，失败必须让这些写入一起回滚。
	if err := s.write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.Exec("ALTER TABLE alert_rule RENAME COLUMN resource_metric TO unavailable_metric")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_, err = s.UpdateNodeTasks(t.Context(), id, NodeEdit{Name: "must rollback", TrafficResetDay: 1, Tags: []string{"new tag"}})
	if err == nil {
		t.Fatal("scope read failure was accepted")
	}
	node, err := s.GetNode(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	current, _, err := s.LoadProbeTasks(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	tags, err := s.ListTags(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if node.Name != "original" || len(node.Tags) != 0 || len(tags) != 0 || current != version {
		t.Fatalf("scope read failure committed node=%+v tags=%+v version=%d want=%d", node, tags, current, version)
	}
}
