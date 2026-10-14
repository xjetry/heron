//go:build linux

package update

import (
	"context"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// statusWith 让 Client 读给定的读者根，并把本机更新器接到一个总是回答"支持"的假更新器；
// 回答 supported 即说明 Status 走到了本机更新器。
func statusWith(t *testing.T, reader fstest.MapFS) Status {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"protocol":1,"supported":true,"version":"v1.0.0"}`))
	}))
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	c := &Client{role: "agent", reader: reader, http: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		},
	}}}
	return c.Status(context.Background())
}

// 在线更新资格只看镜像部署（deployenv.ImageDeployment）：Docker 与 Podman 镜像里拒绝，
// LXC / OrbStack 机器里的 systemd 安装（有容器证据、没有镜像标识）照常到达本机更新器。
func TestStatusEligibilityByDeployment(t *testing.T) {
	systemd := func(extra fstest.MapFS) fstest.MapFS {
		m := fstest.MapFS{"run/systemd/system": {Mode: fs.ModeDir | 0o755}}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	for name, tc := range map[string]struct {
		reader    fstest.MapFS
		supported bool
		reason    string
	}{
		"docker image": {systemd(fstest.MapFS{".dockerenv": {}}), false, "container deployments must update their image"},
		"podman image": {systemd(fstest.MapFS{"run/.containerenv": {}}), false, "container deployments must update their image"},
		"systemd in lxc": {systemd(fstest.MapFS{
			"run/systemd/container": {Data: []byte("lxc\n")},
			"proc/1/environ":        {Data: []byte("container=lxc\x00")},
		}), true, ""},
		"plain systemd host": {systemd(nil), true, ""},
		"no systemd":         {fstest.MapFS{}, false, "online updates require Linux systemd"},
	} {
		s := statusWith(t, tc.reader)
		if s.Supported != tc.supported || !strings.Contains(s.Reason, tc.reason) {
			t.Errorf("%s: supported=%v reason=%q, want %v %q", name, s.Supported, s.Reason, tc.supported, tc.reason)
		}
	}
}
