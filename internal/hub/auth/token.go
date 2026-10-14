package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
)

// tokenBytes 是 NewToken 的随机字节数；明文是它的小写十六进制，长 2*tokenBytes。isTokenShaped 按同一个数判定形状。
const tokenBytes = 32

const installTokenPrefix = "heron_install_"

// 用途前缀参与完整哈希；库中只存哈希也不会丢失用途约束。给运行 token 加前缀或从安装凭据去掉前缀
// 会改变哈希，不能将一种授权变成另一种。不带前缀的存量 token 只保留运行权限，不能升级成安装授权。
func newInstallToken() (string, [32]byte) {
	random, _ := NewToken()
	plain := installTokenPrefix + random
	return plain, HashToken(plain)
}

// NewToken 生成节点 token、注册窗口 key 与会话 token：tokenBytes 字节随机数，hex 编码。
// token 是高熵随机数，SHA-256 足够——不需要抗字典攻击的慢哈希，而慢哈希撑不住
// 每秒数百次的上报校验。
func NewToken() (string, [32]byte) {
	var b [tokenBytes]byte
	rand.Read(b[:])
	plain := hex.EncodeToString(b[:])
	return plain, HashToken(plain)
}

func HashToken(plain string) [32]byte { return sha256.Sum256([]byte(plain)) }

// isTokenShaped 判定 s 是否具有 NewToken 明文的形状：2*tokenBytes 个小写十六进制字符（hex.EncodeToString 只产生小写）。
// 形状不符的字符串不可能是 NewToken 签发的 token，鉴权可以不哈希就丢弃它。
func isTokenShaped(s string) bool {
	if len(s) != 2*tokenBytes {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}
