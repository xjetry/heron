package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// 超长名字在 CLI 预检（store.CleanName）被拒，报错原文含长度约束。
// 注意去掉 CLI 预检时这条用例仍红不了：store 准入（insertNode 的 checkShortName）会以同一句
// NameError 文案拒绝；区分两层的是 TestNodeCreateCleansName。
func TestNodeCreateRejectsOverlongName(t *testing.T) {
	db := filepath.Join(t.TempDir(), "hub.db")
	err := runNode([]string{"create", "--db", db, "--name", strings.Repeat("名", 65)})
	if err == nil || !strings.Contains(err.Error(), "name must be 1–64 characters after trimming whitespace and control characters; got 65") {
		t.Fatalf("err=%v, want the shared length constraint", err)
	}
}

// 首尾空白经 CLI 预检清洗后入库；去掉这层预检，store 准入会以 UncleanNameError
// （"name must not have leading or trailing whitespace or control characters"）拒绝同一个输入——
// 与超长用例互补：这条钉住 CLI 预检这层——预检在则成功且入库为清洗形，预检不在则 store 准入报 UncleanNameError。
func TestNodeCreateCleansName(t *testing.T) {
	db := filepath.Join(t.TempDir(), "hub.db")
	if err := runNode([]string{"create", "--db", db, "--name", "  web-01  "}); err != nil {
		t.Fatal(err)
	}
	st, _, err := openOffline(db, false)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	n, err := st.GetNode(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if n.Name != "web-01" {
		t.Fatalf("stored name = %q, want web-01", n.Name)
	}
}
