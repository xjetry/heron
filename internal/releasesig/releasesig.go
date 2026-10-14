// Package releasesig 定义发行签名：被签的字节、签名文件的格式与受信公钥。
// 只依赖标准库：release 流水线在持有私钥的 job 里编译并运行它（scripts/releasesign），那个 job 不编译也不执行
// 任何第三方代码，私钥才不会落到依赖手里。scripts/releasesign 的依赖检查守着这一条。
package releasesig

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
)

// prefix 把发行签名与同一把私钥可能签的其他内容区分开；改它等于作废全部已发布的签名。
const prefix = "heron-release-v1\n"

const (
	// MaxSums 是 SHA256SUMS 的大小上限，更新器、hub 中转与签名工具读它时共用。
	MaxSums = 1 << 20
	// MaxFile 是签名文件的大小上限。合法文件是 64 字节签名的 base64（88 字节）加换行。
	MaxFile = 1 << 10
)

// Message 返回签名覆盖的字节。Accept 将自己持有的任务版本交给 Verify，签名才绑定到任务版本而不是来源声称的版本。
// Sign 与 Verify 都先由 checkVersion 拒绝版本号里的换行：否则 (v, sums) 的不同切分会拼出同一串被签字节，
// 一份签名就可能被用于不同版本与清单的组合。
func Message(version string, sums []byte) []byte {
	msg := make([]byte, 0, len(prefix)+len(version)+1+len(sums))
	msg = append(msg, prefix...)
	msg = append(msg, version...)
	msg = append(msg, '\n')
	return append(msg, sums...)
}

func checkVersion(version string) error {
	if version == "" || strings.ContainsAny(version, "\r\n") {
		return errors.New("release version must be a non-empty single line")
	}
	return nil
}

// Sign 返回 SHA256SUMS.sig 的完整内容。
func Sign(key ed25519.PrivateKey, version string, sums []byte) ([]byte, error) {
	if err := checkVersion(version); err != nil {
		return nil, err
	}
	sig := ed25519.Sign(key, Message(version, sums))
	return []byte(base64.StdEncoding.EncodeToString(sig) + "\n"), nil
}

// Verify 在 keys 中任一公钥验过 sigFile 时返回 nil。keys 为空时一律失败：没有受信公钥意味着不接受任何产物，
// 而不是不验。
func Verify(keys []ed25519.PublicKey, version string, sums, sigFile []byte) error {
	if err := checkVersion(version); err != nil {
		return err
	}
	sig, err := decode(sigFile)
	if err != nil {
		return err
	}
	msg := Message(version, sums)
	for _, k := range keys {
		// ed25519.Verify 遇到长度不对的公钥会 panic；受信列表由 mustParse 构造，这里仍按不可信输入处理。
		if len(k) == ed25519.PublicKeySize && ed25519.Verify(k, msg, sig) {
			return nil
		}
	}
	return fmt.Errorf("release signature does not verify for %s with any trusted key", version)
}

func decode(file []byte) ([]byte, error) {
	if len(file) > MaxFile {
		return nil, errors.New("release signature file exceeds size limit")
	}
	line, ok := bytes.CutSuffix(file, []byte("\n"))
	if !ok || bytes.ContainsAny(line, "\r\n") {
		return nil, errors.New("release signature file must be one base64 line ending in a newline")
	}
	sig, err := base64.StdEncoding.Strict().DecodeString(string(line))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, errors.New("release signature is not a base64 Ed25519 signature")
	}
	return sig, nil
}

// ParsePrivateKey 解析 `openssl genpkey -algorithm ed25519` 输出的 PKCS#8 PEM。
func ParsePrivateKey(pemData []byte) (ed25519.PrivateKey, error) {
	block, rest := pem.Decode(pemData)
	if block == nil || block.Type != "PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("signing key must be a single PKCS#8 PEM block")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse signing key: %w", err)
	}
	ed, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("signing key is not Ed25519")
	}
	return ed, nil
}
