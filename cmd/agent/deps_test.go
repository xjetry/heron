package main

import (
	"go/build"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const purego = "github.com/ebitengine/purego"

func deps(t *testing.T, goos, pkg string) []string {
	t.Helper()
	cmd := exec.Command("go", "list", "-deps", pkg)
	cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH=arm64", "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps %s (GOOS=%s): %v\n%s", pkg, goos, err, out)
	}
	return strings.Fields(string(out))
}

// darwin 的采集依赖只在带 darwin 约束的文件里引用，不链入 Linux 二进制。
// purego 在 Linux 上也能编译：不带 darwin 约束的文件（darwinraw.go 就是一个）一旦引用它，
// 各平台照样编译通过，Linux 产物却链入了它。这里按依赖图检查，缺陷在引入它的那次提交就红。
// darwin 一侧必须看得见 purego：否则这条检查对"列表为空"与"确实没有"分不出来。
func TestPuregoOnlyLinksIntoDarwin(t *testing.T) {
	if !slices.Contains(deps(t, "darwin", "github.com/xjetry/heron/cmd/agent"), purego) {
		t.Fatal("darwin agent does not depend on purego; the Linux check below would pass vacuously")
	}
	for _, pkg := range []string{"github.com/xjetry/heron/cmd/agent", "github.com/xjetry/heron/cmd/hub"} {
		if slices.Contains(deps(t, "linux", pkg), purego) {
			t.Fatalf("%s links %s on linux", pkg, purego)
		}
	}
}

// puregoImporters 返回 root 下 import purego 的 .go 文件。
// 遍历范围必须等于 go 工具链认定的本模块文件集：进入一个目录前，若不是模块根且含 go.mod，
// 就按嵌套模块停步（go build ./... 不进入带自己 go.mod 的子目录）。边界由模块语义给出，
// 不由目录名给出：工作区里放着自带 go.mod 的第三方检出时，按名字排除会漏掉换了名字的那一个。
func puregoImporters(t *testing.T, root string) []string {
	t.Helper()
	var importers []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == root {
				return nil
			}
			if strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules" || d.Name() == "testdata" {
				return filepath.SkipDir
			}
			if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p == purego || strings.HasPrefix(p, purego+"/") {
				importers = append(importers, path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return importers
}

// 嵌套模块（自带 go.mod 的子目录）不属于本模块：它的文件不由本模块的 go 工具链构建，
// 因此也不该被本模块的 purego 检查纳入。这里用临时模块钉住这条边界：只有模块根下的
// a/x.go 会被报出，nested/ 被按 go.mod 挡下。
func TestPuregoImportScopeExcludesNestedModules(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"go.mod":        "module example.com/root\n\ngo 1.27\n",
		"a/x.go":        "package a\n\nimport _ \"" + purego + "\"\n",
		"nested/go.mod": "module example.com/nested\n\ngo 1.27\n",
		"nested/y.go":   "package nested\n\nimport _ \"" + purego + "\"\n",
	}
	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{filepath.Join(root, "a", "x.go")}
	if got := puregoImporters(t, root); !slices.Equal(got, want) {
		t.Fatalf("importers = %v, want %v", got, want)
	}
}

// 依赖图检查两侧都固定 GOARCH=arm64，按架构分的文件（如 *_linux_amd64.go）引用 purego 时它看不见。
// 这里按文件检查：凡 import purego 的 .go 文件，对工具链支持的、不满足 darwin 构建标签的每个平台
// 都不能进入构建（GOOS=ios 也满足 darwin 标签，go/build 的约定）。
// 平台集合取自 go tool dist list，文件名后缀与 //go:build 由 go/build 的 MatchFile 判定，cgo 开关两种都试。
func TestPuregoImportedOnlyByDarwinFiles(t *testing.T) {
	out, err := exec.Command("go", "tool", "dist", "list").Output()
	if err != nil {
		t.Fatalf("go tool dist list: %v", err)
	}
	platforms := strings.Fields(string(out))
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	importers := puregoImporters(t, root)
	if len(importers) == 0 {
		t.Fatal("no file imports purego; the check below would pass vacuously")
	}
	for _, path := range importers {
		for _, p := range platforms {
			goos, goarch, _ := strings.Cut(p, "/")
			if goos == "darwin" || goos == "ios" {
				continue
			}
			for _, cgo := range []bool{false, true} {
				ctx := build.Default
				ctx.GOOS, ctx.GOARCH, ctx.CgoEnabled = goos, goarch, cgo
				ok, err := ctx.MatchFile(filepath.Dir(path), filepath.Base(path))
				if err != nil {
					t.Fatalf("%s: %v", path, err)
				}
				if ok {
					t.Errorf("%s imports %s but builds for %s (cgo %v)", path, purego, p, cgo)
				}
			}
		}
	}
}
