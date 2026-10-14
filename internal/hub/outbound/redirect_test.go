package outbound

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 真正运行 net/http 的重定向逻辑，域名只用于请求构造，拨号固定到本地测试服务，不依赖 DNS 或出网。
func TestRedirectDisclosure(t *testing.T) {
	for _, host := range []string{"origin.example", "sub.origin.example", "other.example"} {
		for _, status := range []int{302, 307, 308} {
			for _, blocked := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%d/blocked=%v", host, status, blocked), func(t *testing.T) {
					type received struct{ auth, referer, body string }
					receipts := make(chan received, 1)
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path != "/target" {
							w.Header().Set("Location", "http://"+host+"/target")
							w.WriteHeader(status)
							return
						}
						body, _ := io.ReadAll(r.Body)
						receipts <- received{r.Header.Get("Authorization"), r.Referer(), string(body)}
					}))
					defer srv.Close()
					tr := http.DefaultTransport.(*http.Transport).Clone()
					tr.Proxy = nil
					tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
						return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
					}
					defer tr.CloseIdleConnections()
					c := NewClient(time.Minute)
					c.Transport = tr
					if !blocked {
						c.CheckRedirect = nil
					}
					const original = "http://origin.example/bot-token/object?X-Amz-Signature=signature"
					req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, original, strings.NewReader("private-body"))
					if err != nil {
						t.Fatal(err)
					}
					req.Header.Set("Authorization", "private-authorization")
					resp, err := c.Do(req)
					if err != nil {
						t.Fatal(err)
					}
					resp.Body.Close()
					if blocked {
						if resp.StatusCode != status || len(receipts) != 0 {
							t.Fatalf("blocked redirect reached target: status=%d hits=%d", resp.StatusCode, len(receipts))
						}
						return
					}
					select {
					case got := <-receipts:
						wantAuth, wantBody := "private-authorization", "private-body"
						if host == "other.example" {
							wantAuth = ""
						}
						if status == 302 {
							wantBody = ""
						}
						if got != (received{wantAuth, original, wantBody}) {
							t.Fatalf("redirect disclosure=%+v, want auth=%q referer=%q body=%q", got, wantAuth, original, wantBody)
						}
						t.Logf("redirect disclosure: auth=%q referer=%q body=%q", got.auth, got.referer, got.body)
					default:
						t.Fatal("default redirect did not reach target")
					}
				})
			}
		}
	}
}
