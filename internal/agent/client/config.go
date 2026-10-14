package client

import (
	"github.com/xjetry/heron/internal/agent/prober"
	"github.com/xjetry/heron/internal/agentconfig"
)

// LoadConfig 读取并校验配置，run 只经过它。
func LoadConfig(path string) (agentconfig.Config, error) {
	c, err := agentconfig.Read(path)
	if err != nil {
		return c, err
	}
	return c, Validate(c)
}

// Validate 是 run 与 configure 共用的准入：hub 地址的传输规则与本地探测策略。
func Validate(c agentconfig.Config) error {
	if err := agentconfig.CheckHub(c.Hub, c.InsecureHTTP); err != nil {
		return err
	}
	_, err := Policy(c)
	return err
}

// Policy 解析本地探测策略；字段为空即只有默认拒绝集（prober.Policy 的零值）。
func Policy(c agentconfig.Config) (prober.Policy, error) {
	return prober.ParsePolicy(c.ProbeAllow, c.ProbeDeny)
}
