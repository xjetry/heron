// Package sigtest 为测试提供固定的发行签名密钥与产物。只供 _test.go 引用；正式二进制不链接它，
// cmd 的依赖检查守着这一条（update 包的 TestProductionBinariesDoNotLinkSigtest）。
package sigtest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"

	"github.com/xjetry/heron/internal/releasesig"
)

// Key 返回固定种子派生的测试密钥对，跨包、跨进程（隔离验收的 hub 与 agent 两端）都相同。
func Key() (ed25519.PublicKey, ed25519.PrivateKey) {
	priv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))
	return priv.Public().(ed25519.PublicKey), priv
}

// Archive 返回只含 heron-<role> 一个普通文件的 tar.gz，满足更新器的归档结构检查。
func Archive(role string, binary []byte) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "heron-" + role, Mode: 0o755, Size: int64(len(binary)), Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}); err != nil {
		panic(err)
	}
	if _, err := tw.Write(binary); err != nil {
		panic(err)
	}
	if err := tw.Close(); err != nil {
		panic(err)
	}
	if err := gz.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// Sums 返回只含 asset 一行的 SHA256SUMS。
func Sums(asset string, archive []byte) []byte {
	sum := sha256.Sum256(archive)
	return []byte(hex.EncodeToString(sum[:]) + "  " + asset + "\n")
}

// Sign 用测试私钥为 (version, sums) 签名。
func Sign(version string, sums []byte) []byte {
	_, priv := Key()
	sig, err := releasesig.Sign(priv, version, sums)
	if err != nil {
		panic(err)
	}
	return sig
}
