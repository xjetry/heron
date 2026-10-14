package api

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
)

// 与仓库里的文件逐个比对：嵌入的通配若漏了文件或内容过期，这里红。
func TestApiReferenceServesTheRepositoryProtoAndGuide(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	resp, err := h.admin.GetApiReference(context.Background(), connect.NewRequest(&heronv1.GetApiReferenceRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join("..", "..", "..", "proto")
	want := map[string]string{}
	entries, err := os.ReadDir(filepath.Join(root, "heron", "v1"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".proto") {
			b, err := os.ReadFile(filepath.Join(root, "heron", "v1", e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			want["heron/v1/"+e.Name()] = string(b)
		}
	}
	got := map[string]string{}
	var order []string
	for _, f := range resp.Msg.GetFiles() {
		got[f.GetPath()] = f.GetContent()
		order = append(order, f.GetPath())
	}
	if len(got) != len(want) {
		t.Fatalf("served %v, repository has %d proto files", order, len(want))
	}
	for path, content := range want {
		if got[path] != content {
			t.Errorf("%s: served content differs from the repository", path)
		}
	}
	for i := 1; i < len(order); i++ {
		if order[i-1] >= order[i] {
			t.Errorf("files not in ascending path order: %v", order)
		}
	}
	guide, err := os.ReadFile(filepath.Join(root, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.GetGuide() != string(guide) {
		t.Error("served guide differs from proto/SKILL.md")
	}
	for _, s := range []string{"HERON_HUB", "HERON_TOKEN", "```sh example"} {
		if !strings.Contains(resp.Msg.GetGuide(), s) {
			t.Errorf("guide lacks %q", s)
		}
	}
}

func TestApiReferenceIsReachableWithAToken(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	_, tok := createToken(t, h, "agent")
	if got := rawCall(t, h, "GetApiReference", "{}", bearer(tok)); got.status != 200 {
		t.Fatalf("GetApiReference with token: %+v", got)
	}
}
