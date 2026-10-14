// heron-hub：serve 起服务；其余子命令直接操作数据库，供 serve 之外的运维动作。
//
// 离线子命令是库外写者（store.ExternalWriter，见 openOffline）：它们的每个提交的写事务推进库里的离线变更代数。运行中的
// hub 每 offlineReloadEvery 读一次代数，有变化就重载 token 映射与探测任务缓存、清理库外删除的节点（nodeops.Reloader），
// 所以 node create / delete / rotate-token 不需要重启，在下一次重载之后生效；重载前后各缓存独立发布，最终一致。
// passwd 与 token 本就立即生效：登录与 API token 每次读库，改密事务同时撤销旧会话。
package main

import (
	"fmt"
	"os"
	// hub 常跑在没有系统 tzdata 的精简容器里；嵌入时区库让 --timezone 不依赖宿主。
	_ "time/tzdata"
)

var version = "dev"

// agentVersion 是这个 hub 绑定的 agent 版本，构建时经 ldflags 从仓库根的 AGENT_VERSION 注入；绑定规则见 Makefile 的 AGENT_VERSION 注释。空串表示
// 没有绑定（直接 go build 的构建）：节点在线更新一律拒绝、面板不标落后——空值在这里是收紧，不是放宽。
var agentVersion string

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = runServe(os.Args[2:])
	case "passwd":
		err = runPasswd(os.Args[2:])
	case "security-reset":
		err = runSecurityReset(os.Args[2:])
	case "token":
		err = runToken(os.Args[2:])
	case "node":
		err = runNode(os.Args[2:])
	case "window":
		err = runWindow(os.Args[2:])
	case "stats":
		err = runStats(os.Args[2:])
	case "health":
		err = runHealth(os.Args[2:])
	case "restore":
		err = runRestore(os.Args[2:])
	case "version":
		fmt.Println(version)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: heron-hub <command> [flags]

commands:
  serve                     start the hub
  passwd                    set the admin password (reads stdin when not a terminal)
  security-reset            clear TOTP and Passkeys and revoke sessions (requires --yes)
  token list|revoke         list or revoke API tokens (effective immediately)
  node create|list|delete|rotate-token
  window open|close|show    manage the registration window
  stats                     database size (page_count × page_size) and row counts per table
  health                    GET <url>/healthz (default http://127.0.0.1:8080); exit 0 on 2xx
  restore                   restore snapshots offline (stop hub first; requires --yes)
  version`)
}
