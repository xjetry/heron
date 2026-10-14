package main

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 镜像组输入的注入矩阵（deploy/image.mk 开头与 deploy/agent.mk 的登记说明）：决定 agent 镜像字节的输入改了，只发
// hub 的门禁必须拒绝；只属于 hub 镜像的输入改了，门禁必须放行。合成夹具照不到这一层——判定取决于本仓库真实的
// Makefile、deploy/agent.mk 与 deploy/image.mk 怎样登记，所以这里在本仓库当前工作树的副本上跑：副本提交为基点，
// 每例只改一处，以基点跑门禁，再还原。
func TestImageGroupInputs(t *testing.T) {
	t.Setenv("GOTOOLCHAIN", "local")
	t.Setenv("GOPROXY", "off")
	t.Setenv("GOWORK", "off")
	dir := copyWorkingTree(t)

	for _, tc := range []struct {
		name     string
		path     string
		old, new string // old 为空时在文件末尾加一行 new
		reject   bool
	}{
		{name: "agent Dockerfile", path: "deploy/Dockerfile.agent", new: "# changed", reject: true},
		{name: "agent group line in image.mk", path: "deploy/image.mk", old: "IMAGE_agent_REPO := $(IMAGE_REGISTRY)/xjetry/heron-agent", new: "IMAGE_agent_REPO := $(IMAGE_REGISTRY)/xjetry/heron-agent-renamed", reject: true},
		{name: "shared BuildKit pin", path: "deploy/image.mk", old: "moby/buildkit:$(BUILDKIT_VERSION)@sha256:6c2fa84a", new: "moby/buildkit:$(BUILDKIT_VERSION)@sha256:0c2fa84a", reject: true},
		{name: "shared base image pin", path: "deploy/image.mk", old: "alpine:3.21@sha256:ce64758a", new: "alpine:3.21@sha256:0e64758a", reject: true},
		{name: "build context allow list", path: ".dockerignore", new: "!build/image/linux/*/extra", reject: true},
		{name: "builder script", path: "scripts/docker-builder.sh", new: "# changed", reject: true},
		{name: "hub Dockerfile", path: "Dockerfile", new: "# changed"},
		{name: "hub image name", path: "Makefile", old: "IMAGE_hub_REPO := $(IMAGE_REGISTRY)/xjetry/heron-hub", new: "IMAGE_hub_REPO := $(IMAGE_REGISTRY)/xjetry/heron-hub-renamed"},
		{name: "checkimage", path: "scripts/checkimage/main.go", new: "// changed"},
		{name: "hub smoke", path: "scripts/docker-smoke.sh", new: "# changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.old == "" {
				appendLine(t, dir, tc.path, tc.new)
			} else {
				replaceInFile(t, dir, tc.path, tc.old, tc.new)
			}
			defer runGit(t, dir, "checkout", "--", tc.path)
			// 改动落地了：工作树与基点确实不同，下面的"放行"才不是因为什么都没改。
			if out := runGit(t, dir, "status", "--porcelain", "--", tc.path); strings.TrimSpace(out) == "" {
				t.Fatalf("editing %s left the working tree unchanged", tc.path)
			}
			code, out := runTool(t, dir, "-base", "base")
			if tc.reject {
				check(t, code, out, 1, "  file  "+tc.path+"\n")
			} else {
				check(t, code, out, 0, "agent inputs unchanged since base")
			}
		})
	}
}

// copyWorkingTree 把本仓库工作树里的文件（已跟踪的与未忽略的未跟踪文件，取工作树里的内容）复制成一个新仓库，提交
// 并打 tag base，返回它的根。取工作树而不是 HEAD：门禁读的是当前这一版定义，未提交的改动也要在测试里生效。
func copyWorkingTree(t *testing.T) string {
	t.Helper()
	root := strings.TrimSpace(runGit(t, ".", "rev-parse", "--show-toplevel"))
	var list bytes.Buffer
	cmd := exec.Command("git", "-C", root, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	cmd.Stdout = &list
	if err := cmd.Run(); err != nil {
		t.Fatalf("git ls-files in %s: %v", root, err)
	}
	dir := filepath.Join(t.TempDir(), "repo")
	copied := 0
	for _, rel := range strings.Split(strings.TrimSuffix(list.String(), "\x00"), "\x00") {
		src := filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Lstat(src)
		if err != nil {
			if os.IsNotExist(err) {
				// 已跟踪但在工作树里删掉了：副本里同样没有它。
				continue
			}
			t.Fatal(err)
		}
		if !info.Mode().IsRegular() {
			t.Fatalf("%s is not a regular file (%v); copyWorkingTree only copies regular files", rel, info.Mode().Type())
		}
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, b, info.Mode().Perm()|fs.FileMode(0o200)); err != nil {
			t.Fatal(err)
		}
		copied++
	}
	// 冒烟：副本里确实有门禁要读的定义，空副本会让每一例都以"出错"收场。
	for _, must := range []string{"Makefile", "deploy/agent.mk", "deploy/image.mk", "go.mod"} {
		if _, err := os.Stat(filepath.Join(dir, must)); err != nil {
			t.Fatalf("the copy of %s (%d files) lacks %s: %v", root, copied, must, err)
		}
	}
	runGit(t, dir, "init", "-q")
	// 副本一次性全量入库：这里提交的是测试夹具，不是任何真实仓库的工作成果。
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "base")
	runGit(t, dir, "tag", "base")
	return dir
}
