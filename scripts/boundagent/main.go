// Command boundagent 取回并核对绑定版本的 agent 产物。只发 hub 的 release 用 fetch 把绑定版本
// 已验签的 install.sh 与 install-macos.sh 原样放进本次 dist/：这两个脚本内嵌的是绑定版本的版本号、下载目录
// 与哈希，latest 的安装命令因此永远装 hub 绑定的 agent。pin 把同一份已验签清单里端到端要用的 agent 包摘要写成
// 兼容清单，readback 在发布后从 Release 页面回读脚本、核对它们仍是绑定版本那两份。
package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/xjetry/heron/internal/releasesig"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

// errUsage 标记参数错误（退出 2），与执行失败（退出 1）区分。
var errUsage = errors.New("usage")

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: boundagent <fetch|pin|readback> -version vY …")
		return 2
	}
	// 下载地址与受信公钥不给命令行开关：换信任根必须是仓库里一次可审阅的改动，不是发版命令的参数。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	client := &http.Client{}
	keys := releasesig.Trusted()
	var err error
	switch args[0] {
	case "fetch":
		err = runFetch(ctx, client, keys, args[1:], stderr)
	case "pin":
		err = runPin(ctx, client, keys, args[1:], stderr)
	case "readback":
		err = runReadback(ctx, client, keys, args[1:], stderr)
	default:
		fmt.Fprintln(stderr, "usage: boundagent <fetch|pin|readback> -version vY …")
		return 2
	}
	if errors.Is(err, errUsage) {
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, "boundagent:", err)
		return 1
	}
	return 0
}

// runFetch 实现子命令 fetch：取回绑定版本已验签的两个安装脚本。
func runFetch(ctx context.Context, client *http.Client, keys []ed25519.PublicKey, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("fetch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	version := fs.String("version", "", "bound agent version (AGENT_VERSION)")
	dir := fs.String("dir", "", "directory to write the installers into")
	linuxArches := fs.String("linux-arches", "", "space-separated Linux architectures of the agent bundle")
	darwinArches := fs.String("darwin-arches", "", "space-separated darwin architectures of the agent bundle")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return errUsage
	}
	// 架构清单也必填：缺了它 agent 组的完整性核对退化为只查两个脚本，门禁形同虚设。
	if *version == "" || *dir == "" || *linuxArches == "" || *darwinArches == "" {
		fmt.Fprintln(stderr, "boundagent fetch: -version, -dir, -linux-arches and -darwin-arches are all required")
		return errUsage
	}
	return fetchInstallers(ctx, client, officialDownloads, keys, *version, *dir,
		strings.Fields(*linuxArches), strings.Fields(*darwinArches))
}

// runPin 实现子命令 pin：把指定版本已验签的 agent 包摘要写成清单，供按该版本下载的端到端使用。
func runPin(ctx context.Context, client *http.Client, keys []ed25519.PublicKey, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("pin", flag.ContinueOnError)
	fs.SetOutput(stderr)
	version := fs.String("version", "", "published agent version to download in the end-to-end run")
	out := fs.String("out", "", "path of the pin manifest to write")
	linuxArches := fs.String("linux-arches", "", "space-separated Linux architectures of the agent bundle")
	darwinArches := fs.String("darwin-arches", "", "space-separated darwin architectures of the agent bundle")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return errUsage
	}
	// 架构清单必填的理由同 fetch：完整性核对的是整个 agent 组。
	if *version == "" || *out == "" || *linuxArches == "" || *darwinArches == "" {
		fmt.Fprintln(stderr, "boundagent pin: -version, -out, -linux-arches and -darwin-arches are all required")
		return errUsage
	}
	return pinManifest(ctx, client, officialDownloads, keys, *version, *out,
		strings.Fields(*linuxArches), strings.Fields(*darwinArches))
}

// runReadback 实现子命令 readback：核对 vX 的 Release 页面上的两个 agent 安装脚本就是绑定版本 vY 那两份。
func runReadback(ctx context.Context, client *http.Client, keys []ed25519.PublicKey, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("readback", flag.ContinueOnError)
	fs.SetOutput(stderr)
	version := fs.String("version", "", "released hub version (VERSION)")
	bound := fs.String("bound", "", "bound agent version (AGENT_VERSION)")
	dir := fs.String("dir", "", "directory holding the downloaded SHA256SUMS, install.sh and install-macos.sh")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return errUsage
	}
	if *version == "" || *bound == "" || *dir == "" {
		fmt.Fprintln(stderr, "boundagent readback: -version, -bound and -dir are all required")
		return errUsage
	}
	return readbackInstallers(ctx, client, officialDownloads, keys, *version, *bound, *dir)
}
