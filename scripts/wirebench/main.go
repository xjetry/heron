// wirebench 测 agent 上报的线上字节。实测（agent v0.11.2）：稳态约 1.1 KB/上报，30 s 间隔约 3 KB/min；64 个间隔 5 s 的 TCP 任务时上报体约 7.3 KB、线上约 8.3–8.6 KB。部署形状与生产相同：agent → 公网 → 反代（终止 TLS，
// 对 agent 讲 HTTP/2）→ hub。两个计数点：
//   - relay：放在 agent 与反代之间，原样转发并按连接、方向计数，同时切出 TLS 记录头。计到的是"agent ↔ 反代"
//     这段的线上字节（TCP 载荷：TLS 记录、HTTP/2 帧与 HPACK 头块都在内，TCP/IP 头不在内）。
//   - tap：放在反代与 hub 之间，同样转发计数，另按 HTTP/1.1 解析并解码 Report 的请求与应答，只记各字段的
//     编码字节与随主机变化的事实，不记头的值（含节点 token）。
//
// 两份事件日志由 analyze 按时间对齐：每次上报在外侧是一段往返（exchange），在内侧是一次 HTTP 请求。
// 编排（容器、反代配置、注册、探测任务）在同目录的 run.sh。
package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"
)

const usage = `usage: wirebench <command> [flags]
  relay      count bytes between a client and a TCP target (-tls splits TLS records, -http decodes HTTP/1.1)
  certs      write a test root, intermediate and leaf certificate
  analyze    align relay and tap logs into per-report wire bytes
  sizes      attribute a Go binary's symbols to modules
  bounds     print the longest encoding of the host-dependent ReportRequest fields and the reply's capability fields
  gomaxprocs print the Go runtime's default GOMAXPROCS and CPU count in this environment`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "relay":
		err = runRelay(os.Args[2:])
	case "certs":
		err = runCerts(os.Args[2:])
	case "analyze":
		err = runAnalyze(os.Args[2:], os.Stdout)
	case "sizes":
		err = runSizes(os.Args[2:], os.Stdout)
	case "bounds":
		err = runBounds(os.Stdout)
	case "gomaxprocs":
		// 由与 agent 相同版本的 Go 运行时在同一环境里给出默认值：GOMAXPROCS 未设置时运行时按 CPU 亲和性与
		// cgroup 的 CPU 配额取值，进程外看不到这个数。
		fmt.Printf("gomaxprocs=%d numcpu=%d go=%s env_GOMAXPROCS=%q\n", runtime.GOMAXPROCS(0), runtime.NumCPU(), runtime.Version(), os.Getenv("GOMAXPROCS"))
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func runRelay(args []string) error {
	fs := flag.NewFlagSet("relay", flag.ContinueOnError)
	listen := fs.String("listen", "", "listen address")
	target := fs.String("target", "", "target host:port")
	logPath := fs.String("log", "", "event log path (JSON lines)")
	tlsRec := fs.Bool("tls", false, "split each direction into TLS records")
	httpTap := fs.Bool("http", false, "parse each direction as HTTP/1.1 and decode AgentService reports")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *listen == "" || *target == "" || *logPath == "" {
		return errors.New("-listen, -target and -log are required")
	}
	if *tlsRec && *httpTap {
		return errors.New("-tls and -http observe different hops; run one relay per hop")
	}
	f, err := os.OpenFile(*logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	log := NewEventLog(f)
	defer log.Close()
	r := &Relay{Target: *target, Log: log}
	switch {
	case *tlsRec:
		o := &TLSObserver{Log: log}
		r.NewObserver = func(uint64) Observer { return o }
	case *httpTap:
		o := &HTTPObserver{Log: log}
		r.NewObserver = func(uint64) Observer { return o }
	}
	l, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "relay listening on %s -> %s\n", l.Addr(), *target)
	done := make(chan error, 1)
	go func() { done <- r.Serve(l) }()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-done:
		return err
	case <-sig:
		// 长连接（HTTP/2 复用）不会自己结束；停止接受后给已有连接一点时间，日志每行已落盘，
		// 没等到 close 的连接在分析里按"运行结束时仍打开"处理。
		_ = l.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
		return nil
	}
}

func runCerts(args []string) error {
	fs := flag.NewFlagSet("certs", flag.ContinueOnError)
	dir := fs.String("dir", "", "output directory")
	names := fs.String("names", "", "comma-separated leaf DNS names or IPs; the first is the subject")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dir == "" || *names == "" {
		return errors.New("-dir and -names are required")
	}
	return writeCerts(*dir, strings.Split(*names, ","))
}
