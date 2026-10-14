package releasesig

import (
	"crypto/ed25519"
	"encoding/base64"
	"slices"
)

// trusted 是正式产物的受信公钥：标准 base64 的 32 字节 Ed25519 公钥；正式签名只在 release 流水线里用
// Actions secret 中的私钥。公钥写在源码里而不经 ldflags 或 build tag 注入：换钥必然是仓库里一次可审阅的改动，
// 正式二进制里没有第二把。更新器只能由 root 安装器升级，这里的增删要每台主机重跑安装器才生效。
var trusted = mustParse("x5bcmXXnQ0kdpXfLfl+YsHVHgbivdOr/YHdrVb/5n4Q=")

// Trusted 返回受信公钥的深拷贝：ed25519.PublicKey 本身是字节切片，只复制外层切片时调用方改写某把公钥的字节
// 会改到包内的受信列表，此后所有验签都换了信任根。
func Trusted() []ed25519.PublicKey {
	keys := make([]ed25519.PublicKey, len(trusted))
	for i, k := range trusted {
		keys[i] = slices.Clone(k)
	}
	return keys
}

func mustParse(encoded ...string) []ed25519.PublicKey {
	keys := make([]ed25519.PublicKey, 0, len(encoded))
	for _, s := range encoded {
		raw, err := base64.StdEncoding.Strict().DecodeString(s)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			panic("releasesig: trusted key is not a base64 Ed25519 public key: " + s)
		}
		keys = append(keys, ed25519.PublicKey(raw))
	}
	return keys
}
