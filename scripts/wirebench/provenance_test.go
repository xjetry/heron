package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 来源记录只能描述脚本所在的这棵树：树根就是 git 工作区的根时从它的 git 取（并记下工作区是否干净）；树在别的
// 工作区里面（发布 archive 解在某个检出的目录下）时拒绝，否则记下的是外层检出的 HEAD；树不在任何工作区里时
// 提交号与版本号都必须由调用方给出。
func TestProvenanceDescribesOnlyTheTreeItself(t *testing.T) {
	repo := t.TempDir()
	plain := t.TempDir()
	// 两个临时目录是兄弟；git 不越过它们的父目录向上找，测试结果就不依赖 TMPDIR 自己是否在某个检出里。
	ceiling := filepath.Dir(repo)
	if filepath.Dir(plain) != ceiling {
		t.Fatalf("temp dirs %s and %s have different parents", repo, plain)
	}
	gitEnv := append(os.Environ(),
		"GIT_CEILING_DIRECTORIES="+ceiling,
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
	git := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = gitEnv
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name string) {
		if err := os.WriteFile(filepath.Join(repo, name), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q")
	write("a")
	git("add", "a")
	git("commit", "-q", "-m", "a")
	head := git("rev-parse", "HEAD")
	describe := git("describe", "--tags", "--always")

	// 与脚本同样的调用顺序：先检查，再要提交号与版本号。
	run := func(dir string, extra ...string) (stdout, stderr string, err error) {
		cmd := exec.Command("sh", "-c", `. ./provenance.sh && tree_check "$1" && tree_commit && tree_version`, "sh", dir)
		cmd.Env = append(append([]string{}, gitEnv...), extra...)
		var out, errb strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &errb
		err = cmd.Run()
		return out.String(), errb.String(), err
	}

	out, stderr, err := run(repo)
	if err != nil || out != "commit="+head+" dirty=no\n"+describe+"\n" {
		t.Fatalf("clean checkout root: err=%v stdout=%q stderr=%q", err, out, stderr)
	}

	write("b")
	out, stderr, err = run(repo)
	if err != nil || out != "commit="+head+" dirty=yes\n"+describe+"\n" {
		t.Fatalf("checkout with an untracked file: err=%v stdout=%q stderr=%q", err, out, stderr)
	}

	nested := filepath.Join(repo, "build", "base")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	out, stderr, err = run(nested)
	if err == nil || out != "" || !strings.Contains(stderr, "lies inside the git checkout") {
		t.Fatalf("tree inside another checkout: err=%v stdout=%q stderr=%q", err, out, stderr)
	}

	out, stderr, err = run(plain)
	if err == nil || out != "" || !strings.Contains(stderr, "pass COMMIT") {
		t.Fatalf("tree without git, COMMIT unset: err=%v stdout=%q stderr=%q", err, out, stderr)
	}

	out, stderr, err = run(plain, "COMMIT=abc123")
	if err == nil || out != "commit=abc123 dirty=unknown\n" || !strings.Contains(stderr, "pass VERSION") {
		t.Fatalf("tree without git, COMMIT set: err=%v stdout=%q stderr=%q", err, out, stderr)
	}
}
