package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// writeCerts 生成测试用的三级证书：根、中间、叶。反代发送叶加中间两张（与公共 CA 签发的常见形状相同：
// 根在客户端信任库里，不随握手发送），叶的 SAN 是 names。密钥都是 ECDSA P-256：Caddy 经 ACME 申请证书的
// 默认密钥类型。证书链的 DER 字节数直接计入服务端握手字节（TLS 1.3 的 Certificate 消息逐张携带 DER，
// Go 与 nginx 默认都不做证书压缩），所以把它们写进 chain-sizes.txt，换成真实证书时据此折算。
func writeCerts(dir string, names []string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	now := time.Now()
	rootKey, rootCert, err := newCA("wirebench test root", nil, nil, now)
	if err != nil {
		return err
	}
	midKey, midCert, err := newCA("wirebench test intermediate", rootCert, rootKey, now)
	if err != nil {
		return err
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	leaf := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: names[0]},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(90 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, n := range names {
		if ip := net.ParseIP(n); ip != nil {
			leaf.IPAddresses = append(leaf.IPAddresses, ip)
		} else {
			leaf.DNSNames = append(leaf.DNSNames, n)
		}
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, midCert, &leafKey.PublicKey, midKey)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		return err
	}
	files := []struct {
		name  string
		mode  os.FileMode
		block []*pem.Block
	}{
		{"ca.pem", 0o644, []*pem.Block{{Type: "CERTIFICATE", Bytes: rootCert.Raw}}},
		{"chain.pem", 0o644, []*pem.Block{{Type: "CERTIFICATE", Bytes: leafDER}, {Type: "CERTIFICATE", Bytes: midCert.Raw}}},
		// 反代容器以非 root 用户读它；这是每轮现生成的测试密钥，只在被 .gitignore 覆盖的目录里。
		{"key.pem", 0o644, []*pem.Block{{Type: "PRIVATE KEY", Bytes: keyDER}}},
	}
	for _, f := range files {
		var b []byte
		for _, blk := range f.block {
			b = append(b, pem.EncodeToMemory(blk)...)
		}
		if err := os.WriteFile(filepath.Join(dir, f.name), b, f.mode); err != nil {
			return err
		}
	}
	sizes := fmt.Sprintf("leaf_der=%d intermediate_der=%d root_der=%d sent_chain_der=%d\n", len(leafDER), len(midCert.Raw), len(rootCert.Raw), len(leafDER)+len(midCert.Raw))
	return os.WriteFile(filepath.Join(dir, "chain-sizes.txt"), []byte(sizes), 0o644)
}

func newCA(name string, parent *x509.Certificate, parentKey *ecdsa.PrivateKey, now time.Time) (*ecdsa.PrivateKey, *x509.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	signer, signerKey := tmpl, key
	if parent != nil {
		signer, signerKey = parent, parentKey
		tmpl.MaxPathLenZero = true
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signer, &key.PublicKey, signerKey)
	if err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	return key, cert, err
}

func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		panic(err)
	}
	return n
}
