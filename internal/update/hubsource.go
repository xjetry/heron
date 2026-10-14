package update

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/agentconfig"
	"github.com/xjetry/heron/internal/hubclient"
)

// maxRelayResponse 是 GetRelease 响应正文的上限：三份文件各自上限之和，加 protobuf 与 Connect 的编码余量。
const maxRelayResponse = maxFetchBytes + 64<<10

// hubFetchLimit 是节点经 hub 取回的总上限。GetRelease 是 unary：缓存未命中时 hub 先从 GitHub 取完、验过才应答
// （updates.Relay，总上限 DownloadLimit，停滞判定在 hub 读 GitHub 正文时做），节点这一侧先是一段没有字节的长等待，
// 再收 hub 的应答正文；应答正文就是那三份文件，按同一个最低速率传完同样要一个 DownloadLimit。所以取两个
// DownloadLimit：hub 侧的取回先于节点到期，节点收到的是 hub 的错误文案（含停滞或超时的进度），而不是自己的
// "context deadline exceeded"；反过来节点先放弃会让 hub 白取一次。hub 上的验签不经网络、耗时不随链路速率变化；
// 两段推导值之外还有两次向上取整留下的约 72 秒（TestDownloadLimitDerivation 从常量重算推导值）。
// 节点的期限不短于 hub 的取回期限，所以 hub 先到期并返回错误，节点不会先放弃。
const hubFetchLimit = 2 * DownloadLimit

// HubSource 经 hub 的 GetRelease 取产物，不做接受判定（见 Accept）。hub 地址、token 与明文许可在
// 每个任务现读 agent 配置：节点 token 轮换后无须同步。解析与地址规则和 agent 同一实现（agentconfig），连接与 agent
// 同一构造（hubclient），所以 https 要求、不跟随重定向、响应上限对两者同时成立。
type HubSource struct {
	// readConfig 返回 agent 配置原文；正式实现按服务用户属主等检查打开并限量读取（serve 里构造）。
	readConfig func() ([]byte, error)
}

func NewHubSource(readConfig func() ([]byte, error)) *HubSource {
	return &HubSource{readConfig: readConfig}
}

func (s *HubSource) Fetch(ctx context.Context, task Request, role, arch string) (Artifacts, error) {
	if _, err := timeLimit(ctx); err != nil {
		return Artifacts{}, err
	}
	if role != "agent" {
		return Artifacts{}, errors.New("hub source serves only agent updates")
	}
	raw, err := s.readConfig()
	if err != nil {
		return Artifacts{}, fmt.Errorf("read agent config: %w", err)
	}
	cfg, err := agentconfig.Decode(bytes.NewReader(raw), agentConfigPath)
	if err != nil {
		return Artifacts{}, err
	}
	if err := agentconfig.CheckHub(cfg.Hub, cfg.InsecureHTTP); err != nil {
		return Artifacts{}, err
	}
	req := connect.NewRequest(&heronv1.GetReleaseRequest{TaskId: task.ID, Arch: arch})
	req.Header().Set("Authorization", "Bearer "+cfg.Token)
	// 不设总时限也不限等响应头：缓存未命中时 hub 先从 GitHub 取完、验过才应答，期限只来自 ctx（见 source）。
	resp, err := hubclient.New(cfg.Hub, 0, maxRelayResponse).GetRelease(ctx, req)
	if err != nil {
		return Artifacts{}, fmt.Errorf("fetch release from hub: %w", err)
	}
	return Artifacts{Sums: resp.Msg.GetSums(), Signature: resp.Msg.GetSignature(), Archive: resp.Msg.GetArchive()}, nil
}
