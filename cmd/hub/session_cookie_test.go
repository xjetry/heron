package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/gen/heron/v1/heronv1connect"
	"github.com/xjetry/heron/internal/clock"
)

// 会话读取不设候选数上限，这里在真实 serve 上确认整条路径上也没有别的上限：用互不相同的 token 形状伪造值把 Cookie 头
// 填到服务端还肯读的最大长度（每个伪造值都要哈希、查表），有效值排在最后，断言它仍然 200；再多一个伪造值即被拒，
// 说明确实填到了头部上限。
//
// 探测用不复用连接的客户端：复用的连接上，设读取上限之前已进读缓冲区的字节数由那次读取实际拿到多少决定
// （至多 4096），边界随之浮动，二分找到的长度换一条连接未必还读得进；新连接上没有这部分，边界是确定的。
func TestServeSessionCookieFilledToHeaderLimit(t *testing.T) {
	t.Parallel()
	db := filepath.Join(t.TempDir(), "hub.db")
	const pw = "a sufficiently long password"
	if err := runPasswdWith([]string{"--db", db}, pipeWith(t, pw+"\n"), io.Discard); err != nil {
		t.Fatal(err)
	}
	url, _, _ := startTestHub(t, db, clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))
	logged, err := heronv1connect.NewAdminServiceClient(ownedClient(t), url).Login(context.Background(), connect.NewRequest(&heronv1.LoginRequest{Password: pw}))
	if err != nil {
		t.Fatal(err)
	}
	cookies := (&http.Response{Header: logged.Header()}).Cookies()
	if len(cookies) != 1 {
		t.Fatalf("login cookies: %v", cookies)
	}
	valid := cookies[0].Name + "=" + cookies[0].Value
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	// call 发 n 个伪造值加一个有效值；服务端拒读（431，或写到一半断开连接）时返回 0。
	call := func(n int) int {
		var b strings.Builder
		for i := range n {
			fmt.Fprintf(&b, "heron_session=%064x;", i)
		}
		b.WriteString(valid)
		req, err := http.NewRequest(http.MethodPost, url+"/heron.v1.AdminService/ListNodes", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Cookie", b.String())
		resp, err := client.Do(req)
		if err != nil {
			return 0
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		if resp.StatusCode == http.StatusRequestHeaderFieldsTooLarge {
			return 0
		}
		return resp.StatusCode
	}
	// hi 大于默认头部上限能装下的伪造值个数，二分落在上限处。
	const hi = 40000
	lo, top := 0, hi
	for lo < top {
		mid := (lo + top + 1) / 2
		if call(mid) != 0 {
			lo = mid
		} else {
			top = mid - 1
		}
	}
	if lo == hi {
		t.Fatalf("the server accepted %d forged values: raise hi so the search reaches the header limit", hi)
	}
	if got := call(lo); got != http.StatusOK {
		t.Fatalf("Cookie header filled to the server's limit with %d forged values ahead of the valid one: status %d, want 200", lo, got)
	}
	if got := call(lo + 1); got != 0 {
		t.Fatalf("%d forged values were accepted too (status %d): the search did not reach the header limit", lo+1, got)
	}
	t.Logf("header limit reached at %d forged values plus the valid one", lo)
}
