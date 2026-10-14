// Command releasekind 判定一次 release 的种类，规则只在本文件的 kind：tag 与 AGENT_VERSION 逐字相同为完整发布，绑定的正式版按 semver 低于 tag 则为只发 hub，其余拒绝。Makefile 的 release-kind、两个发布目标的
// 开头与 release 流水线都调用它，规则不在别处另写。
//
//	releasekind -version vX -agent vY   打印 full 或 hub-only；不合规则退出 1 并说明
//	releasekind -check-file PATH        按原始字节核对 AGENT_VERSION 文件（make lint 调用）
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/xjetry/heron/internal/update"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("releasekind", flag.ContinueOnError)
	fs.SetOutput(stderr)
	version := fs.String("version", "", "release tag (VERSION)")
	agent := fs.String("agent", "", "bound agent version (AGENT_VERSION)")
	checkFile := fs.String("check-file", "", "validate the AGENT_VERSION file byte for byte")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: releasekind -version vX -agent vY | releasekind -check-file PATH")
		return 2
	}
	if *checkFile != "" {
		if err := checkAgentFile(*checkFile); err != nil {
			fmt.Fprintln(stderr, "releasekind:", err)
			return 1
		}
		return 0
	}
	k, err := kind(*version, *agent)
	if err != nil {
		fmt.Fprintln(stderr, "releasekind:", err)
		return 1
	}
	fmt.Fprintln(stdout, k)
	return 0
}

// checkAgentFile：文件恰好是一个 release tag 加一个换行。Makefile 读它时只取第一行并去掉首尾空白（地基提交的
// read/strip），多余的行、空白与 CR 在那里被悄悄丢掉，所以这里按原始字节核对，不核对读出来的值。
func checkAgentFile(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	tag, ok := strings.CutSuffix(string(b), "\n")
	if !ok || strings.ContainsAny(tag, " \t\r\n") {
		return fmt.Errorf("%s must be exactly one release tag followed by a newline, got %q", path, b)
	}
	if _, _, ok := update.ReleaseTag(tag); !ok {
		return fmt.Errorf("%s: %q is not a release tag vMAJOR.MINOR.PATCH[-PRERELEASE]", path, tag)
	}
	return nil
}

// kind：vY 与 vX 逐字相同为完整 release；vY 是正式版且按 semver 优先级低于 vX 为只发 hub（vX 是预发布时
// 比较它的正式版部分：vC 高于 vC-rc，低于 vC 的正式版都低于 vC-rc）；其余都不合规则。只发 hub 只能绑定正式版：
// 被绑定的版本要有签名与完整的 agent 组，预发布不被更新器接受（update.ValidVersion）。
func kind(version, agent string) (string, error) {
	versionCore, _, ok := update.ReleaseTag(version)
	if !ok {
		return "", fmt.Errorf("VERSION %q is not a release tag vMAJOR.MINOR.PATCH[-PRERELEASE]", version)
	}
	agentCore, agentPre, ok := update.ReleaseTag(agent)
	if !ok {
		return "", fmt.Errorf("AGENT_VERSION %q is not a release tag vMAJOR.MINOR.PATCH[-PRERELEASE]", agent)
	}
	if agent == version {
		return "full", nil
	}
	if agentPre {
		return "", fmt.Errorf("AGENT_VERSION %s is a prerelease and differs from VERSION %s: a hub-only release binds only a stable agent release; for a full release set AGENT_VERSION=%s", agent, version, version)
	}
	if update.Newer(versionCore, agentCore) {
		return "hub-only", nil
	}
	return "", fmt.Errorf("AGENT_VERSION %s is not lower than VERSION %s: a hub-only release binds an earlier agent release; for a full release set AGENT_VERSION=%s (local acceptance builds pass AGENT_VERSION=$VERSION)", agent, version, version)
}
