package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/xjetry/heron/internal/shutdown"
	"github.com/xjetry/heron/internal/update"
)

var version = "dev"

func main() {
	if len(os.Args) == 2 && os.Args[1] == "version" {
		fmt.Println(version)
		return
	}
	role := flag.String("role", "", "managed service: hub or agent")
	maintenance := flag.Bool("maintenance", false, "reserve the local updater for a root installer")
	flag.Parse()
	// stderr 由 systemd 收进 journal：任务的状态迁移与失败原因在 journalctl -u heron-updater-<role> 里可查。
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop, interruptIgnored := shutdown.Notify()
	defer stop()
	if interruptIgnored {
		log.Info(shutdown.IgnoredInterruptNotice)
	}
	var err error
	if *role != "hub" && *role != "agent" {
		err = fmt.Errorf("role must be hub or agent")
	} else if *maintenance {
		err = update.NewClient(*role).Maintenance(ctx)
	} else {
		err = update.Serve(ctx, *role, log)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
