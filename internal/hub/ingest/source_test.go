package ingest

import (
	"context"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
)

// reportFrom 以给定的请求头上报一次，返回 live 条目里记下的来源地址。headers 保序，
// 每个元素是一行 {字段名, 值}：map 表达不了同名字段的多行，而 X-Forwarded-For 的多行正是
// 要钉住的场景，所以用 Header().Add 逐行加。
func (h *hub) reportFrom(t *testing.T, id int64, tok string, headers [][2]string) string {
	t.Helper()
	req := report(tok, &heronv1.Metrics{})
	for _, kv := range headers {
		req.Header().Add(kv[0], kv[1])
	}
	if _, err := h.client.Report(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	// 上报限速按节点计：连续上报之间推进时钟。
	h.clk.Advance(10 * time.Second)
	e, ok := h.live.Get(id)
	if !ok {
		t.Fatal("report did not reach live")
	}
	return e.Source
}

// 来源地址只取 hub 看到的对端，经 auth.ClientIP 按可信代理解析：直连取 TCP 对端；不可信对端带的转发头不采信；
// 可信代理的 X-Forwarded-For 取解析后的地址；CF-Connecting-IP 无论对端是否可信都不读；可信代理追加的行覆盖
// 客户端自带的那一行——只读第一行会让来源取自客户端可自由填写的值。
func TestReportRecordsTheSourceHubSees(t *testing.T) {
	loopback := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	cases := []struct {
		name    string
		trusted []netip.Prefix
		headers [][2]string
		want    string
	}{
		{"直连取 TCP 对端", nil, nil, "127.0.0.1"},
		{"不可信对端的转发头不采信", nil, [][2]string{{"X-Forwarded-For", "203.0.113.7"}}, "127.0.0.1"},
		{"可信代理的 X-Forwarded-For 取解析后的地址", loopback, [][2]string{{"X-Forwarded-For", "203.0.113.7"}}, "203.0.113.7"},
		{"可信代理转发的 IPv6 写成压缩形式", loopback, [][2]string{{"X-Forwarded-For", "2001:DB8:0:0:0:0:0:7"}}, "2001:db8::7"},
		{"直连时 CF-Connecting-IP 被忽略", nil, [][2]string{{"CF-Connecting-IP", "198.51.100.9"}}, "127.0.0.1"},
		{"可信代理之后 CF-Connecting-IP 也被忽略", loopback, [][2]string{{"CF-Connecting-IP", "198.51.100.9"}}, "127.0.0.1"},
		{"可信代理之后 CF-Connecting-IP 不压过 X-Forwarded-For", loopback, [][2]string{{"X-Forwarded-For", "203.0.113.7"}, {"CF-Connecting-IP", "198.51.100.9"}}, "203.0.113.7"},
		{"可信代理追加的转发头覆盖客户端伪造的第一行", loopback, [][2]string{{"X-Forwarded-For", "198.51.100.66"}, {"X-Forwarded-For", "203.0.113.8"}}, "203.0.113.8"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHubWith(t, filepath.Join(t.TempDir(), "t.db"), Config{TTL: 30 * time.Second, TrustedProxies: c.trusted})
			id, tok := h.node(t)
			if got := h.reportFrom(t, id, tok, c.headers); got != c.want {
				t.Fatalf("source = %q, want %q", got, c.want)
			}
		})
	}
}

func (h *hub) storedSource(t *testing.T, id int64) string {
	t.Helper()
	n, err := h.store.GetNode(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return n.LastSource
}

// 上报只碰内存：刷出之前库里仍为空。分钟行刷出后落盘；只留最后一个，v4 之后的 v6 上报覆盖它。
func TestSourceIsFlushedWithMinuteRowsAndKeepsOnlyTheLast(t *testing.T) {
	h := newHubWith(t, filepath.Join(t.TempDir(), "t.db"), Config{TTL: 30 * time.Second, TrustedProxies: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}})
	id, tok := h.node(t)
	h.reportFrom(t, id, tok, [][2]string{{"X-Forwarded-For", "203.0.113.7"}})
	if got := h.storedSource(t, id); got != "" {
		t.Fatalf("source stored before any flush: %q", got)
	}
	h.clk.Advance(time.Minute)
	h.svc.Flush(context.Background(), false)
	if got := h.storedSource(t, id); got != "203.0.113.7" {
		t.Fatalf("after flush last_source = %q, want 203.0.113.7", got)
	}
	h.reportFrom(t, id, tok, [][2]string{{"X-Forwarded-For", "203.0.113.7"}})
	h.reportFrom(t, id, tok, [][2]string{{"X-Forwarded-For", "2001:db8::7"}})
	h.clk.Advance(time.Minute)
	h.svc.Flush(context.Background(), false)
	if got := h.storedSource(t, id); got != "2001:db8::7" {
		t.Fatalf("after v4 then v6 last_source = %q, want 2001:db8::7", got)
	}
}

// hub 退出时的排空（当前分钟尚未闭合）同样落盘来源地址。
func TestSourceIsFlushedOnShutdownDrain(t *testing.T) {
	h := newHub(t)
	id, tok := h.node(t)
	h.reportFrom(t, id, tok, nil)
	h.svc.Flush(context.Background(), true)
	if got := h.storedSource(t, id); got != "127.0.0.1" {
		t.Fatalf("after shutdown drain last_source = %q, want 127.0.0.1", got)
	}
}
