// Command agentinputs 是只发 hub 的门禁：核对 agent 组的构建输入自绑定版本以来没有变化，没变
// 才允许这次 release 不带 agent 产物。release-hub-only 目标这样调用它：
//
//	agentinputs -base <绑定版本的 git 引用>
//
// 退出码：0 输入未变；1 有变化（stdout 列出变化并提示把 AGENT_VERSION 改成本次的版本号）；2 出错（stderr
// 说明）。经 go run 调用时 shell 看到的非 0 一律是 1（stderr 另打 exit status N），调用方靠输出文本区分
// "有变化"与"出错"。门禁没有跳过开关：放过一次真实的 agent 改动，结果是那次改动静默地没有发布出去。
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

func main() { os.Exit(run(os.Args[1:], ".", os.Stdout, os.Stderr)) }

const usage = "usage: agentinputs -base <git ref of the bound agent version, e.g. v0.5.3>"

func run(args []string, dir string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agentinputs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	base := fs.String("base", "", "bound agent version, as a git ref (e.g. v0.5.3)")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *base == "" {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		fmt.Fprintf(stderr, "agentinputs: %s is not inside a git repository: %v\n", dir, err)
		return 2
	}
	root := strings.TrimSpace(string(out))
	d, in, err := compareTrees(root, *base)
	if err != nil {
		// 基点没有这两个 make 目标：它早于 agent 组定义本身（deploy/agent.mk），绑定它意味着绑不上任何
		// agent 产物，只能失败，不能拿空清单当作"没有输入"放行。工作树一侧同理：门禁在没定义的树上运行
		// 一定是装配错了。
		var se sideError
		if errors.As(err, &se) && errors.Is(se.err, errNoBundleDefinition) {
			if se.side == sideBase {
				fmt.Fprintf(stderr, "agentinputs: %s predates the agent bundle definition (deploy/agent.mk); a hub-only release cannot bind it\n", *base)
			} else {
				fmt.Fprintln(stderr, "agentinputs: working tree has no agent bundle definition (deploy/agent.mk)")
			}
			return 2
		}
		fmt.Fprintln(stderr, "agentinputs:", err)
		return 2
	}
	if d.empty() {
		fmt.Fprintf(stdout, "agent inputs unchanged since %s (%d files, %d modules)\n", *base, len(in.files), len(in.modules))
		return 0
	}
	fmt.Fprintf(stdout, "agent inputs changed since %s:\n", *base)
	for _, f := range d.files {
		fmt.Fprintf(stdout, "  file  %s\n", f)
	}
	for _, m := range d.onlyOld {
		fmt.Fprintf(stdout, "  module  %s (only in %s)\n", m, *base)
	}
	for _, m := range d.onlyNew {
		fmt.Fprintf(stdout, "  module  %s (only in working tree)\n", m)
	}
	// go.mod 的指令行按位置配对：逐行列表只在两侧一致时才有"同一行的新旧值"可说，多出的行以 (absent) 呈现。
	for i := 0; i < len(d.goLines[0]) || i < len(d.goLines[1]); i++ {
		o, n := "(absent)", "(absent)"
		if i < len(d.goLines[0]) {
			o = fmt.Sprintf("%q", d.goLines[0][i])
		}
		if i < len(d.goLines[1]) {
			n = fmt.Sprintf("%q", d.goLines[1][i])
		}
		if o != n {
			fmt.Fprintf(stdout, "  go.mod  %s -> %s\n", o, n)
		}
	}
	fmt.Fprintln(stdout, "set AGENT_VERSION to this release's version: the agent bundle must be released together with this hub")
	return 1
}
