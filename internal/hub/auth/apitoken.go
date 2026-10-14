package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"

	"github.com/xjetry/heron/internal/hub/store"
)

const (
	// APITokenPrefix 让泄漏到日志、配置或代码仓库里的 token 能被审查与 secret scanning 认出。
	APITokenPrefix = "heron_at_"
	MaxAPITokens   = 100
)

// NewAPIToken 生成明文与整串（含前缀）的哈希。高熵随机数用 SHA-256 足够，理由同节点 token。
func NewAPIToken() (string, [32]byte) {
	var b [32]byte
	rand.Read(b[:])
	plain := APITokenPrefix + hex.EncodeToString(b[:])
	return plain, HashToken(plain)
}

// CreateAPIToken 的 name 由调用方清洗与校验；这里只负责生成、限额与落库。
func (a *Auth) CreateAPIToken(ctx context.Context, name string, grant *store.TokenGrant) (store.APIToken, string, error) {
	plain, hash := NewAPIToken()
	tok, err := a.store.CreateAPIToken(ctx, name, hash, a.clk.Now(), MaxAPITokens, grant)
	if err != nil {
		return store.APIToken{}, "", err
	}
	return tok, plain, nil
}

// AuthenticateAPIToken 每次都查库、不缓存：吊销在下一个请求即生效，包括 heron-hub 在另一进程里的删除。
// 最近使用时刻与会话同一口径——从未使用或距已落库值满 touchEvery 才异步刷新，刷新只 UPDATE。
func (a *Auth) AuthenticateAPIToken(ctx context.Context, plain string) (*store.APIToken, error) {
	if !strings.HasPrefix(plain, APITokenPrefix) {
		return nil, nil
	}
	tok, ok, err := a.store.APITokenByHash(ctx, HashToken(plain))
	if err != nil || !ok {
		return nil, err
	}
	now := a.clk.Now()
	if tok.LastUsedAt.IsZero() || now.Sub(tok.LastUsedAt) >= touchEvery {
		a.store.TouchAPITokenAsync(tok.ID, now, func(err error) {
			if err != nil {
				a.log.Warn("recording API token use failed", "err", err)
			}
		})
	}
	return &tok, nil
}
