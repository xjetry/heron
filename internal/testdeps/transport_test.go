package testdeps

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// 每种经共享 Transport 的写法各报一处、行号指向那一行；带 Transport 的客户端、DefaultTransport 的克隆、局部遮蔽的
// http、没有导入 net/http 的文件都不报。
func TestSharedTransportUsesReportsEveryShape(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	files := map[string]string{
		"a_test.go": `package p

import (
	"net/http"
	"time"
)

func f(tr http.RoundTripper) {
	_ = http.DefaultClient
	_, _ = http.Get("http://x")
	_, _ = http.Post("http://x", "", nil)
	_ = http.DefaultTransport
	_ = http.DefaultTransport.(*http.Transport).Clone()
	_ = &http.Client{Timeout: time.Second}
	_ = &http.Client{Transport: tr, Timeout: time.Second}
	_ = new(http.Client)
	var c http.Client
	_ = c
	var d = http.Client{Transport: tr}
	_ = d
}
`,
		"b_test.go": `package p

import nethttp "net/http"

func g() {
	_ = nethttp.DefaultClient
	_ = nethttp.Client{}
}
`,
		"c_test.go": `package p

type client struct{ DefaultClient int }

func h() {
	http := client{}
	_ = http.DefaultClient
}
`,
		"d.go": `package p

import "net/http"

var _ = http.DefaultClient
`,
	}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := SharedTransportUses(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"a_test.go:9: http.DefaultClient sends through the shared http.DefaultTransport",
		"a_test.go:10: http.Get sends through http.DefaultClient",
		"a_test.go:11: http.Post sends through http.DefaultClient",
		"a_test.go:12: http.DefaultTransport is shared by the whole test process",
		"a_test.go:14: http.Client literal without Transport falls back to http.DefaultTransport",
		"a_test.go:16: new(http.Client) has no Transport and falls back to http.DefaultTransport",
		"a_test.go:17: zero http.Client has no Transport and falls back to http.DefaultTransport",
		"b_test.go:6: http.DefaultClient sends through the shared http.DefaultTransport",
		"b_test.go:7: http.Client literal without Transport falls back to http.DefaultTransport",
	}
	if !slices.Equal(got, want) {
		t.Errorf("findings:\n%q\nwant:\n%q", got, want)
	}
}
