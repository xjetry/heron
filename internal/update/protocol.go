// Package update 承载固定官方发行版的本机更新协议与事务。
package update

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"time"

	"github.com/xjetry/heron/internal/deployenv"
)

const Protocol = 1

type Request struct {
	ID        string `json:"id"`
	Version   string `json:"version"`
	ExpiresAt int64  `json:"expires_at"`
}

type Job struct {
	Request
	State     string `json:"state"`
	Error     string `json:"error,omitempty"`
	UpdatedAt int64  `json:"updated_at"`
	Digest    string `json:"digest,omitempty"`
}

func (j Job) Active() bool {
	switch j.State {
	case "queued", "downloading", "stopping", "installing", "verifying", "rolling_back":
		return true
	}
	return false
}

type Status struct {
	Protocol  int    `json:"protocol"`
	Supported bool   `json:"supported"`
	Reason    string `json:"reason,omitempty"`
	Version   string `json:"version"`
	// Source 是本机取产物的来源：github 或 hub；旧更新器为空串。
	Source string `json:"source,omitempty"`
	Job    *Job   `json:"job,omitempty"`
}

var idPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{16,64}$`)

func (r Request) Validate(now time.Time) error {
	if !idPattern.MatchString(r.ID) || !ValidVersion(r.Version) {
		return errors.New("update requires a 16-64 character task ID and a canonical vMAJOR.MINOR.PATCH version")
	}
	if r.ExpiresAt <= now.Unix() || r.ExpiresAt > now.Add(24*time.Hour).Unix() {
		return errors.New("update expiry must be in the next 24 hours")
	}
	return nil
}

func Socket(role string) string { return "/run/heron-update-" + role + "/updater.sock" }

type Client struct {
	http *http.Client
	role string
	// reader 是本进程自己的根 "/"：在线更新资格看的是本进程怎么部署（镜像、systemd），
	// agent 以 --host-root 采集宿主时也不变。
	reader fs.FS
}

func NewClient(role string) *Client {
	return &Client{role: role, reader: os.DirFS("/"), http: &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", Socket(role))
		},
	}}}
}

func (c *Client) call(ctx context.Context, path string, body any, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost/"+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err = io.ReadAll(io.LimitReader(resp.Body, 16385))
	if err != nil {
		return err
	}
	if len(b) > 16384 {
		return errors.New("updater response exceeds limit")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("updater: %s", b)
	}
	return json.Unmarshal(b, out)
}

func (c *Client) Status(ctx context.Context) Status {
	var s Status
	if runtime.GOOS != "linux" {
		return Status{Reason: "online updates require Linux systemd"}
	}
	// 只有镜像部署（Docker / Podman 的标识）不能在线更新；LXC 等容器里的 systemd 安装有容器证据，
	// 却是本机更新器支持的形态，判定见 deployenv.Markers.ImageDeployment。
	if deployenv.Read(c.reader).ImageDeployment() {
		return Status{Reason: "container deployments must update their image; online updates are unsupported"}
	}
	if _, err := fs.Stat(c.reader, "run/systemd/system"); errors.Is(err, fs.ErrNotExist) {
		return Status{Reason: "online updates require Linux systemd; use the platform installer"}
	}
	if err := c.call(ctx, "status", nil, &s); err != nil {
		return Status{Reason: "local updater unavailable; install with the current systemd installer"}
	}
	if s.Protocol != Protocol {
		return Status{Reason: "local updater protocol is incompatible"}
	}
	return s
}

func (c *Client) Submit(ctx context.Context, r Request) (Job, error) {
	var j Job
	if err := r.Validate(time.Now()); err != nil {
		return j, err
	}
	err := c.call(ctx, "submit", r, &j)
	return j, err
}

// Ready 只在新进程初始化完成后调用；agent 还必须先完成一次成功的上报。
func (c *Client) Ready(ctx context.Context, version string) error {
	var s Status
	return c.call(ctx, "ready", struct {
		Version string `json:"version"`
	}{version}, &s)
}

// Maintenance 只供 root 安装器调用；服务用户不能借此暂停更新器。
func (c *Client) Maintenance(ctx context.Context) error {
	var out struct{}
	return c.call(ctx, "maintenance", nil, &out)
}

// Gate 在受管理的候选启动期间保持关闭；普通安装没有进行中的事务，无须等待。
func (c *Client) Gate(ctx context.Context, version string) error {
	if _, err := os.Stat("/var/lib/heron-update-" + c.role + "/pending"); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	s := c.Status(ctx)
	if !s.Supported || s.Job == nil {
		return errors.New("update gate is closed and local updater is unavailable")
	}
	if s.Job.State == "succeeded" {
		return nil
	}
	if s.Job.State != "verifying" {
		return errors.New("update gate is closed during recovery")
	}
	if s.Job.Version != version {
		return errors.New("running version does not match pending update")
	}
	return c.Ready(ctx, version)
}

func trailingJSON(d *json.Decoder) error {
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return errors.New("expected a single JSON object")
	}
	return nil
}
