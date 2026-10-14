package store

import (
	"os"
	"strconv"
	"testing"
)

// TestWriteSchemaFixture 只在给出 HERON_SCHEMA_FIXTURE_OUT 时运行：把 HERON_SCHEMA_FIXTURE_VERSION 版的冻结库写入两个
// 节点后复制到该路径，供 scripts/e2e.sh 用真实的 serve 验证迁移前备份。夹具与单元测试同出 frozenSchemaFixture，
// 脚本不另抄一份旧版本 DDL。节点按 insertNodes 的列写入，适用于 node 表已有这几列且 token_hash 唯一的版本（含 40）。
func TestWriteSchemaFixture(t *testing.T) {
	out := os.Getenv("HERON_SCHEMA_FIXTURE_OUT")
	if out == "" {
		t.Skip("writes a fixture only when HERON_SCHEMA_FIXTURE_OUT is set (scripts/e2e.sh)")
	}
	version, err := strconv.Atoi(os.Getenv("HERON_SCHEMA_FIXTURE_VERSION"))
	if err != nil {
		t.Fatalf("HERON_SCHEMA_FIXTURE_VERSION: %v", err)
	}
	src, raw := frozenSchemaFixture(t, version)
	insertNodes(t, raw, 2)
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatalf("%s already exists; the fixture is written only to a new path", out)
	}
	copyFile(t, src, out)
}
