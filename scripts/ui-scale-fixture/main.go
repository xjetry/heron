// ui-scale-fixture 为本机空 hub 建立节点并持续走正式上报接口，不直接改库。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type rpcClient struct {
	base string
	http *http.Client
}

func (c rpcClient) call(ctx context.Context, service, method, token string, input, output any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/heron.v1."+service+"/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%s.%s HTTP %d: %s", service, method, response.StatusCode, data)
	}
	if output == nil {
		return nil
	}
	return json.Unmarshal(data, output)
}

func run(ctx context.Context, base string, count int, password string) error {
	u, err := url.Parse(base)
	if err != nil {
		return err
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "http" || ip == nil || !ip.IsLoopback() || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("hub 必须是本机回环 HTTP 地址，不能连接已有部署")
	}
	if count < 1 || count > 500 || password == "" {
		return errors.New("nodes 必须在 1..500，且 HERON_FIXTURE_PASSWORD 必须非空")
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return err
	}
	client := rpcClient{strings.TrimRight(base, "/"), &http.Client{Timeout: 10 * time.Second, Jar: jar, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}}
	admin := func(method string, input, output any) error {
		return client.call(ctx, "AdminService", method, "", input, output)
	}
	if err := admin("Login", map[string]any{"password": password}, nil); err != nil {
		return err
	}
	var existing struct {
		Nodes []json.RawMessage `json:"nodes"`
	}
	if err := admin("ListNodes", map[string]any{}, &existing); err != nil {
		return err
	}
	// 夹具只接受空库，避免重复运行覆盖用户已有的节点和配置。
	if len(existing.Nodes) != 0 {
		return errors.New("hub 已有节点，请换用新的独立数据库")
	}
	tokens := make([]string, 0, count)
	// 注册入口每来源每秒补一个名额；夹具可建数百节点，按补充速率认领，不绕过真实鉴权入口。
	enrollment := time.NewTicker(time.Second)
	defer enrollment.Stop()
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("scale-%03d-生产节点-长名称-%s", i+1, strings.Repeat("x", 30))
		var created struct {
			Node struct {
				ID string `json:"id"`
			} `json:"node"`
			Token string `json:"token"`
		}
		if err := admin("CreateNode", map[string]any{"name": name}, &created); err != nil {
			return err
		}
		if created.Node.ID == "" || created.Token == "" {
			return errors.New("CreateNode 未返回 id 和 token")
		}
		if err := admin("UpdateNode", map[string]any{
			"id": created.Node.ID, "name": name, "public": true, "trafficResetDay": 1, "offlineGraceS": 0,
			"note": strings.Repeat("客户备注和续费说明需要完整阅读，不应撑宽整个管理页面。", 12),
			"tags": []string{fmt.Sprintf("region-%d", i%5), "production", "带空格的长标签 long-tag-without-breaks"}, "countryPin": []string{"US", "JP", "DE"}[i%3],
			"billing": map[string]any{"price": "12.50", "currency": "USD", "billingCycle": "BILLING_CYCLE_YEARLY", "expiresOn": "2030-01-31", "autoRenew": true},
		}, nil); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-enrollment.C:
		}
		var registered struct {
			Token string `json:"token"`
		}
		if err := client.call(ctx, "AgentService", "Register", "", map[string]any{"key": created.Token, "name": name}, &registered); err != nil {
			return err
		}
		if registered.Token == "" {
			return errors.New("hub returned no run token from Register")
		}
		tokens = append(tokens, registered.Token)
	}
	var confirmed struct {
		Nodes []json.RawMessage `json:"nodes"`
	}
	if err := admin("ListNodes", map[string]any{}, &confirmed); err != nil {
		return err
	}
	if len(confirmed.Nodes) != count {
		return fmt.Errorf("回读节点数 %d，期望 %d", len(confirmed.Nodes), count)
	}
	fmt.Printf("fixture ready: nodes=%d hub=%s/admin/nodes\n", count, base)
	var reports atomic.Uint64
	failures := make(chan error, count)
	ctx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	for i, token := range tokens {
		workers.Add(1)
		go func() {
			defer workers.Done()
			var tick uint64
			interval := time.Duration(i) * 20 * time.Millisecond
			for {
				select {
				case <-ctx.Done():
					return
				case <-time.After(interval):
				}
				tick++
				var response struct {
					ReportIntervalMS uint32 `json:"reportIntervalMs"`
				}
				err := client.call(ctx, "AgentService", "Report", token, map[string]any{
					"factsHash": strconv.Itoa(i + 1),
					"facts":     map[string]any{"hostname": fmt.Sprintf("scale-%03d.internal", i+1), "os": "Fixture Linux", "arch": "amd64", "cpuCores": 4, "agentVersion": "ui-scale-fixture"},
					"metrics": map[string]any{"bootId": fmt.Sprintf("00000000-0000-4000-8000-%012d", i), "cpuPct": (i + int(tick)) % 100,
						"memTotal": "8589934592", "memUsed": strconv.FormatUint(1<<30+tick*(1<<20), 10),
						"netRxTotal": strconv.FormatUint(tick*1048576, 10), "netTxTotal": strconv.FormatUint(tick*524288, 10),
						"netRxBps": "262144", "netTxBps": "131072", "uptimeS": strconv.FormatUint(tick*4, 10)},
				}, &response)
				if err != nil {
					if ctx.Err() == nil {
						failures <- fmt.Errorf("node %d: %w", i+1, err)
					}
					return
				}
				if response.ReportIntervalMS == 0 {
					failures <- fmt.Errorf("node %d: hub 未返回上报间隔", i+1)
					return
				}
				interval = time.Duration(response.ReportIntervalMS) * time.Millisecond
				reports.Add(1)
			}
		}()
	}
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-failures:
			cancel()
			return err
		case <-ticker.C:
			fmt.Printf("reports accepted=%d nodes=%d\n", reports.Load(), count)
		}
	}
}

func main() {
	base := flag.String("hub", "http://127.0.0.1:18089", "本机空 hub 地址")
	count := flag.Int("nodes", 100, "节点数")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *base, *count, os.Getenv("HERON_FIXTURE_PASSWORD")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
