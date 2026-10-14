package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"text/tabwriter"
	"time"

	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/auth"
	"github.com/xjetry/heron/internal/hub/probe"
	"github.com/xjetry/heron/internal/hub/store"
)

// openOffline 打开已有库，或在 create 时建立新库。
// 写错 --db 时静默建空库，吊销、删除这类操作会对空库"成功"而真正的库原封不动；
// 建立状态的子命令例外，因为第一次 serve 之前要能准备库。
func openOffline(db string, create bool) (*store.Store, *auth.Auth, error) {
	if !create {
		if _, err := os.Stat(db); err != nil {
			if os.IsNotExist(err) {
				return nil, nil, fmt.Errorf("database %s does not exist; pass --db pointing at the hub's database", db)
			}
			return nil, nil, err
		}
	}
	// 用默认级别（Info 起放行）而不是过滤到 Warn：store.Open 建库时只在 Info 级记一行
	// "database schema created"（离线入口用 RequireCurrentSchema，不会迁移），过滤掉 Info 会让这一行
	// 消失。上面这条注释说的"写错 --db 时静默建空库"，运维唯一能在离线命令的输出里看出
	// 库被新建的信号就是这一行；建立状态的子命令（passwd、node create、window open）因此
	// 必须放出它，否则退出码与其余输出跟"库已存在、操作在原库上完成"完全一样。
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	// 离线子命令是 hub 进程之外的写者：每个提交的写事务都推进离线变更代数，运行中的 hub 据此重载（见 store.ExternalWriter）。
	// 除 restore（要求 hub 已停止，见 store.Restore）外，离线入口都经这个函数打开库，这里是它们共同的声明处。
	st, err := store.Open(db, clock.Real(), log, store.RequireCurrentSchema, store.ExternalWriter())
	if err != nil {
		return nil, nil, err
	}
	// 离线进程只需要注册表的落库入口：建节点经它进入 store 的建节点事务，继承任务的上限检查与任务版本的推进都在
	// 那个事务里，与运行中的 hub 走同一段代码，不因注册表未 Load 而跳过。注册表的内存发布随进程退出丢弃，本进程
	// 里也没有读它的调用方，所以不 Load；运行中的 hub 由重载循环发现这次写入并重载自己的缓存（nodeops.Reloader）。
	// token 映射要 Load：换发以映射里的当前凭据为期望值做比较并换发（auth.Auth.RotateToken），映射就是此刻的库。
	// 离线子命令不经过 Login，不写登录通知：发送者给 nil（nil 只写库、不入队，见 auth.New），时区用不到，给 UTC 只为
	// 满足 auth.New 的非 nil 要求。
	a := auth.New(st, probe.New(st, log), nil, clock.Real(), time.UTC, log)
	if err := a.Load(context.Background()); err != nil {
		st.Close()
		return nil, nil, err
	}
	return st, a, nil
}

func runNode(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: heron-hub node create|list|delete|rotate-token [flags]")
	}
	fs := flag.NewFlagSet("node "+args[0], flag.ContinueOnError)
	db := fs.String("db", "heron.db", "SQLite database path")
	name := fs.String("name", "", "node name (create)")
	id := fs.Int64("id", 0, "node id (delete, rotate-token)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	st, a, err := openOffline(*db, args[0] == "create")
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()
	switch args[0] {
	case "create":
		if *name == "" {
			return errors.New("--name is required")
		}
		nid, tok, err := a.CreateNode(ctx, store.NewNode{Name: *name})
		if err != nil {
			return err
		}
		fmt.Printf("id: %d\ntoken: %s\n", nid, tok)
		fmt.Fprintln(os.Stderr, "one-time installation credential: use as heron-agent register --key, not as a reporting token")
	case "list":
		nodes, err := st.ListNodes(ctx)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 8, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tNAME\tPUBLIC\tLAST SEEN")
		for _, n := range nodes {
			seen := "never"
			if !n.LastSeenAt.IsZero() {
				seen = n.LastSeenAt.Format("2006-01-02 15:04:05Z")
			}
			fmt.Fprintf(w, "%d\t%s\t%v\t%s\n", n.ID, n.Name, n.Public, seen)
		}
		return w.Flush()
	case "delete":
		if *id == 0 {
			return errors.New("--id is required")
		}
		if err := a.DeleteNode(ctx, *id); err != nil {
			return err
		}
	case "rotate-token":
		if *id == 0 {
			return errors.New("--id is required")
		}
		tok, err := a.RotateToken(ctx, *id)
		if err != nil {
			return err
		}
		fmt.Printf("token: %s\n", tok)
		fmt.Fprintln(os.Stderr, "old credential revoked; use the installation script with --re-register --hub URL --key TOKEN on the agent host")
	default:
		return fmt.Errorf("unknown node command %q", args[0])
	}
	return nil
}
