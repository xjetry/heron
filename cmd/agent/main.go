// heron-agent：register 用注册窗口 key 换 token 并写入配置；configure 修改宿主机本地策略；run 进入上报循环。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	iofs "io/fs"
	"log/slog"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/agent/agentlog"
	"github.com/xjetry/heron/internal/agent/client"
	"github.com/xjetry/heron/internal/agent/collect"
	"github.com/xjetry/heron/internal/agent/netinfo"
	"github.com/xjetry/heron/internal/agent/prober"
	"github.com/xjetry/heron/internal/agentconfig"
	"github.com/xjetry/heron/internal/agentwire"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hubclient"
	"github.com/xjetry/heron/internal/shutdown"
)

var version = "dev"

const defaultConfig = "/etc/heron-agent/config.json"

const usage = "usage: heron-agent register|configure|run|version [flags]"

// requestTimeout 是单次 RPC 的上限：hub 不应答时一次上报至多挂这么久才进入退避。
// initialInterval 是收到 hub 第一个响应之前的上报间隔，也是这段时间里 client.Backoff 的基数。
// 两者都写进启动行，scripts/e2e.sh 据此推出告警恢复的等待上限。
const (
	requestTimeout  = 15 * time.Second
	initialInterval = 10 * time.Second
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "register":
		err = runRegister(os.Args[2:])
	case "configure":
		err = runConfigure(os.Args[2:], os.Stdout)
	case "run":
		err = runRun(os.Args[2:])
	case "version":
		fmt.Println(version)
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		os.Stderr.WriteString(errorLine(err))
		os.Exit(exitCode(err))
	}
}

// exitCode 把宿主根契约不成立（collect.HostRootError：缺挂载、不在宿主网络命名空间、内核没有 openat2）
// 与其余失败分开：退出码 2 与 usage 错误同码，表示运行参数不对；镜像冒烟按它区分参数错误与其他失败。
func exitCode(err error) int {
	if _, ok := errors.AsType[*collect.HostRootError](err); ok {
		return 2
	}
	return 1
}

// maxErrorLine 是命令最终错误行的上限，由 errorLine 按整行保证：前缀、截断标记与换行都在内。
// register 的错误可含不可信的 hub 应答文本；若只限制消息而不计其余字节，stderr 的实际写出就会越界。
const maxErrorLine = 4 << 10

// errorLine 返回要写到 stderr 的整行。
func errorLine(err error) string {
	const prefix, marker, newline = "error: ", "…(truncated)", "\n"
	msg := strings.ToValidUTF8(err.Error(), "\uFFFD")
	if len(prefix)+len(msg)+len(newline) <= maxErrorLine {
		return prefix + msg + newline
	}
	keep := maxErrorLine - len(prefix) - len(marker) - len(newline)
	return prefix + strings.ToValidUTF8(msg[:keep], "") + marker + newline
}

func runRegister(args []string) error {
	fs := flag.NewFlagSet("register", flag.ContinueOnError)
	hub := fs.String("hub", "", "hub base URL, e.g. https://heron.example.com")
	key := fs.String("key", "", "registration window key or one-time node installation credential")
	name := fs.String("name", "", "node name (default: hostname)")
	cfgPath := fs.String("config", defaultConfig, "where to write the agent config")
	insecure := fs.Bool("insecure-http", false, "accept a plain http hub address (the node token and metrics travel unencrypted)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *hub == "" || *key == "" {
		return errors.New("--hub and --key are required")
	}
	if *name == "" {
		*name, _ = os.Hostname()
	}
	base := strings.TrimRight(*hub, "/")
	// 校验在发请求之前：明文地址被拒时 key 不能已经出过线，窗口名额也不能已经消耗。
	if err := agentconfig.CheckHub(base, *insecure); err != nil {
		return err
	}
	// 重新注册换的是 hub 身份，本地探测策略属于宿主机，沿用已有配置里的；已有配置读不出来时报错，不静默丢掉它。
	cfg := agentconfig.Config{Hub: base, Name: *name, InsecureHTTP: *insecure}
	if old, err := agentconfig.Read(*cfgPath); err == nil {
		cfg.ProbeAllow, cfg.ProbeDeny = old.ProbeAllow, old.ProbeDeny
	} else if !errors.Is(err, iofs.ErrNotExist) {
		return fmt.Errorf("existing config: %w", err)
	}
	if _, err := client.Policy(cfg); err != nil {
		return fmt.Errorf("existing config: %w", err)
	}
	c := hubclient.New(base, requestTimeout, agentwire.MaxResponseBytes)
	resp, err := c.Register(context.Background(), connect.NewRequest(&heronv1.RegisterRequest{Key: *key, Name: *name}))
	if err != nil {
		return fmt.Errorf("register: %w", err)
	}
	cfg.Token = resp.Msg.Token
	if err := agentconfig.Save(*cfgPath, cfg); err != nil {
		return err
	}
	fmt.Printf("registered as node %d; config written to %s\n", resp.Msg.NodeId, *cfgPath)
	return nil
}

func runRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	cfgPath := fs.String("config", defaultConfig, "agent config path")
	include := fs.String("net-include", "", "comma-separated interface globs to count (exclusive)")
	exclude := fs.String("net-exclude", "", "comma-separated interface globs to skip (default: "+strings.Join(collect.DefaultNetExclude(), ", ")+
		"; on Linux the default also skips stacked devices, virtual bridge ports, overlays and tunnels by their sysfs structure); giving this flag or --net-include replaces the whole default")
	hostRoot := fs.String("host-root", "", "linux only: directory where the host's root is mounted (e.g. /hostfs in a container run with --network host -v /:/hostfs:ro); metrics and facts describe that host (default: this process's root)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// 先接管标准库 log，再创建任何网络客户端：net/http 会用它写出对端给的内容（agentlog.Install）。
	log := agentlog.Install(newHandler(os.Stderr))
	cfg, err := client.LoadConfig(*cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	policy, err := client.Policy(cfg)
	if err != nil {
		return err
	}
	targets := prober.Targets{Policy: policy}
	clk := clock.Real()
	col, err := collect.NewPlatform(version, clk, splitList(*include), splitList(*exclude), *hostRoot)
	if err != nil {
		return err
	}
	// Collector 持有根 fd；最先 defer、最后关闭：调度停止与 ICMP 关闭之后不再有采集。
	defer col.Close()
	col.Log = log
	log.Info("probe target policy", "policy", policy.String())
	ic := prober.NewICMP(clk, log)
	ic.Targets = targets
	// defer 逆序执行，先停止调度再关闭 socket，避免仍在运行的任务入队 icmp closed。
	defer ic.Close()
	col.IcmpAvailable = ic.Available()
	if !ic.Available() {
		log.Warn("icmp probing unavailable; icmp tasks will report errors", "reasons", ic.InitErrors())
	}
	queue := prober.NewQueue(prober.QueueCap)
	sched := prober.NewScheduler(prober.Multi{
		ICMP: ic,
		TCP:  prober.TCP{Clock: clk, Targets: targets},
		HTTP: &prober.HTTP{Clock: clk, Targets: targets, Version: version},
		DNS:  prober.DNS{Clock: clk, Targets: targets},
	}, queue, clk, log)
	defer sched.Stop()
	r := &client.Runner{
		Collector: col,
		Client:    hubclient.New(cfg.Hub, requestTimeout, agentwire.MaxResponseBytes),
		Token:     cfg.Token,
		Clock:     clk,
		Log:       log,
		Interval:  initialInterval,
		Prober:    sched,
		Results:   queue,
	}
	ctx, stop, interruptIgnored := shutdown.Notify()
	defer stop()
	if interruptIgnored {
		log.Info(shutdown.IgnoredInterruptNotice)
	}
	network := netinfo.New()
	go network.Run(ctx)
	r.Network = network
	updater := client.NewUpdateCoordinator(version, log)
	go updater.Run(ctx)
	r.Updates = updater
	logStarting(log, cfg.Hub)
	if err := r.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// newHandler 是 run 的日志出口。启动行的文本格式有外部读者（scripts/e2e.sh 按整秒读字段），测试经同一个
// 函数装配日志，才钉得住读者实际看到的格式。agentlog 给全部输出设速率与长度上界：不少行由 hub 的应答触发。
func newHandler(w io.Writer) *agentlog.Handler {
	return agentlog.New(slog.NewTextHandler(w, nil), nil)
}

func newLogger(w io.Writer) *slog.Logger { return slog.New(newHandler(w)) }

func logStarting(log *slog.Logger, hub string) {
	log.Info("agent starting", "hub", hub, "request_timeout", requestTimeout, "initial_interval", initialInterval, "version", version)
}

func splitList(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// runConfigure 修改配置里的宿主机本地策略，字段更新规则见 agentconfig.Config。只改命令行上显式给出的项，列表给空串即清空；
// 读取时不校验，才能修正一份当前被 run 拒绝的配置；写入前按 run 的同一套规则校验整份配置。
func runConfigure(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("configure", flag.ContinueOnError)
	cfgPath := fs.String("config", defaultConfig, "agent config path")
	insecure := fs.Bool("insecure-http", false, "accept a plain http hub address (the node token and metrics travel unencrypted)")
	allow := fs.String("probe-allow", "", "comma-separated CIDR prefixes the agent may probe despite the defaults; empty clears")
	deny := fs.String("probe-deny", "", "comma-separated CIDR prefixes the agent refuses to probe in addition to the defaults; empty clears")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("configure: unexpected argument %q", fs.Arg(0))
	}
	cfg, err := agentconfig.Read(*cfgPath)
	if err != nil {
		return err
	}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "insecure-http":
			cfg.InsecureHTTP = *insecure
		case "probe-allow":
			cfg.ProbeAllow = splitList(*allow)
		case "probe-deny":
			cfg.ProbeDeny = splitList(*deny)
		}
	})
	if err := client.Validate(cfg); err != nil {
		return err
	}
	if err := agentconfig.Save(*cfgPath, cfg); err != nil {
		return err
	}
	policy, _ := client.Policy(cfg)
	fmt.Fprintf(out, "insecure_http=%v %s\nrestart the heron-agent service to apply\n", cfg.InsecureHTTP, policy)
	return nil
}
