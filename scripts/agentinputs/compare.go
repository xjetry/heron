package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
)

// diff 是基点与工作树两侧输入的比较结果，各字段已排序。files 含两侧各只有一侧的路径：一侧有一侧没有与内容
// 不同同样是变化（未跟踪的新文件、工作树里删掉的文件都由此比到，git diff 看不见未跟踪文件）。
type diff struct {
	files   []string    // 内容不同或只在一侧的文件
	onlyOld []string    // 只在基点的模块
	onlyNew []string    // 只在工作树的模块
	goLines [2][]string // [基点, 工作树] 的 go.mod 指令行
}

func (d diff) empty() bool {
	return len(d.files) == 0 && len(d.onlyOld) == 0 && len(d.onlyNew) == 0 &&
		slices.Equal(d.goLines[0], d.goLines[1])
}

// side 标记一次展开发生在哪一侧：两侧对 errNoBundleDefinition 的说法不同（基点早于定义 / 工作树装配错了），
// 其余错误的定位也靠它。
type side string

const (
	sideWorking side = "working tree"
	sideBase    side = "base"
)

type sideError struct {
	side side
	err  error
}

func (e sideError) Error() string { return fmt.Sprintf("%s: %v", e.side, e.err) }
func (e sideError) Unwrap() error { return e.err }

// canonical 解析路径里的符号链接（macOS 的 /var → /private/var 等）。两侧的树根必须是同一种写法：go list
// 报告的包目录是物理路径，git rev-parse 也返回物理路径，临时目录拼接出来的却是逻辑路径，不统一时仓库内的
// 文件会被误判成在仓库外。
func canonical(path string) string {
	if p, err := filepath.EvalSymlinks(path); err == nil {
		return p
	}
	return path
}

// compareTrees 展开工作树与基点两侧的输入并比较。基点在临时 worktree 里展开（make 与 go list 要跑在那一版
// 内容上），返回前移除临时 worktree，失败也清理。文件内容的工作树一侧直接读文件、基点一侧取自 git 对象：
// 比较的是构建真正读到的字节，不是两边各自的暂存区状态。
func compareTrees(root, base string) (diff, inputs, error) {
	root = canonical(root)
	work, err := collect(root)
	if err != nil {
		return diff{}, inputs{}, sideError{side: sideWorking, err: err}
	}
	tmp, err := os.MkdirTemp("", "agentinputs-base-")
	if err != nil {
		return diff{}, inputs{}, err
	}
	defer os.RemoveAll(tmp)
	baseTree := filepath.Join(canonical(tmp), "base")
	if out, err := git(root, "worktree", "add", "--detach", baseTree, base); err != nil {
		return diff{}, inputs{}, fmt.Errorf("git worktree add %s: %v: %s", base, err, out)
	}
	// collect 在基点 worktree 里可能产生未跟踪文件（-mod=mod 就地解析模块图），--force 保证清理总能完成；
	// 清理失败不掩盖比较结果，交给下一次运行的人看。
	defer func() { _, _ = git(root, "worktree", "remove", "--force", baseTree) }()
	baseIn, err := collect(baseTree)
	if err != nil {
		return diff{}, inputs{}, sideError{side: sideBase, err: err}
	}

	d := diff{goLines: [2][]string{baseIn.goLines, work.goLines}}
	seen := map[string]bool{}
	for _, in := range []inputs{work, baseIn} {
		for path := range in.files {
			if !seen[path] {
				seen[path] = true
				if !sameFile(root, base, path) {
					d.files = append(d.files, path)
				}
			}
		}
	}
	slices.Sort(d.files)
	for m := range baseIn.modules {
		if !work.modules[m] {
			d.onlyOld = append(d.onlyOld, m)
		}
	}
	for m := range work.modules {
		if !baseIn.modules[m] {
			d.onlyNew = append(d.onlyNew, m)
		}
	}
	slices.Sort(d.onlyOld)
	slices.Sort(d.onlyNew)
	return d, work, nil
}

// sameFile 比较一个路径在基点与工作树的内容：基点用 cat-file -e 判存在、show 取内容，工作树直接读文件。
// 任一侧缺失都算不同——集合差与内容差在这里汇成同一个判定。空文件与缺失由此区分：两者都不走 bytes.Equal。
func sameFile(root, base, path string) bool {
	if err := exec.Command("git", append([]string{"-C", root}, "cat-file", "-e", base+":"+path)...).Run(); err != nil {
		return false
	}
	old, err := gitBytes(root, "show", base+":"+path)
	if err != nil {
		return false
	}
	new, err := os.ReadFile(filepath.Join(root, path))
	if err != nil {
		return false
	}
	return bytes.Equal(old, new)
}

// git 在 root 仓库里跑 git，返回合并的输出。
func git(root string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput()
	return string(out), err
}

func gitBytes(root string, args ...string) ([]byte, error) {
	return exec.Command("git", append([]string{"-C", root}, args...)...).Output()
}
