package releasesig

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"
)

func key(seed byte) (ed25519.PublicKey, ed25519.PrivateKey) {
	priv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seed}, ed25519.SeedSize))
	return priv.Public().(ed25519.PublicKey), priv
}

func TestMessageLayout(t *testing.T) {
	got := string(Message("v1.2.3", []byte("abc  x.tar.gz\n")))
	if want := "heron-release-v1\nv1.2.3\nabc  x.tar.gz\n"; got != want {
		t.Fatalf("Message = %q, want %q", got, want)
	}
}

func TestVerify(t *testing.T) {
	pub, priv := key(1)
	other, _ := key(2)
	sums := []byte("00  heron-agent_linux_amd64.tar.gz\n")
	good, err := Sign(priv, "v1.2.3", sums)
	if err != nil {
		t.Fatal(err)
	}
	clip := func(b []byte) []byte { return b[:len(b):len(b)] }
	for _, tc := range []struct {
		name    string
		keys    []ed25519.PublicKey
		version string
		sums    []byte
		sig     []byte
		want    string
	}{
		{"valid", []ed25519.PublicKey{pub}, "v1.2.3", sums, good, ""},
		{"second_trusted_key", []ed25519.PublicKey{other, pub}, "v1.2.3", sums, good, ""},
		{"untrusted_key", []ed25519.PublicKey{other}, "v1.2.3", sums, good, "does not verify"},
		{"no_trusted_keys", nil, "v1.2.3", sums, good, "does not verify"},
		{"other_version", []ed25519.PublicKey{pub}, "v1.2.4", sums, good, "does not verify"},
		{"tampered_sums", []ed25519.PublicKey{pub}, "v1.2.3", append(clip(sums), 'x'), good, "does not verify"},
		{"short_public_key", []ed25519.PublicKey{pub[:31]}, "v1.2.3", sums, good, "does not verify"},
		{"missing_newline", []ed25519.PublicKey{pub}, "v1.2.3", sums, good[:len(good)-1], "one base64 line"},
		{"crlf", []ed25519.PublicKey{pub}, "v1.2.3", sums, append(clip(good[:len(good)-1]), '\r', '\n'), "one base64 line"},
		{"truncated", []ed25519.PublicKey{pub}, "v1.2.3", sums, append(clip(good[:20]), '\n'), "not a base64 Ed25519 signature"},
		{"oversized", []ed25519.PublicKey{pub}, "v1.2.3", sums, bytes.Repeat([]byte("A"), MaxFile+1), "exceeds size limit"},
		{"newline_in_version", []ed25519.PublicKey{pub}, "v1\nx", sums, good, "version"},
	} {
		err := Verify(tc.keys, tc.version, tc.sums, tc.sig)
		if tc.want == "" && err != nil {
			t.Errorf("%s rejected: %v", tc.name, err)
		}
		if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: err=%v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestSignRejectsNewlineInVersion(t *testing.T) {
	_, priv := key(1)
	if _, err := Sign(priv, "v1\nx", nil); err == nil {
		t.Fatal("version with a newline makes the signed message ambiguous and must be refused")
	}
}

func TestParsePrivateKey(t *testing.T) {
	_, priv := key(3)
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	block := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	got, err := ParsePrivateKey(block)
	if err != nil || !got.Equal(priv) {
		t.Fatalf("round trip: %v", err)
	}
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecDER, err := x509.MarshalPKCS8PrivateKey(ec)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		"ecdsa":      pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecDER}),
		"two_blocks": append(append([]byte{}, block...), block...),
		"wrong_type": pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}),
		"not_pem":    []byte("seed"),
	} {
		if _, err := ParsePrivateKey(data); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestTrustedKeysAreWellFormed(t *testing.T) {
	for i, k := range Trusted() {
		if len(k) != ed25519.PublicKeySize {
			t.Fatalf("trusted key %d has %d bytes", i, len(k))
		}
	}
}

// 受信列表是信任根：调用方拿到的副本无论替换元素还是改写某把公钥的字节，都不能改到包内的列表。
// 正式列表可能为空，这里临时放进一把测试公钥，让断言总有对象。
func TestTrustedReturnsDeepCopy(t *testing.T) {
	pub, _ := key(9)
	saved := trusted
	t.Cleanup(func() { trusted = saved })
	trusted = []ed25519.PublicKey{append(ed25519.PublicKey(nil), pub...)}
	keys := Trusted()
	keys[0][0] ^= 0xff
	if !Trusted()[0].Equal(pub) {
		t.Fatal("rewriting a returned key's bytes changed the trusted list")
	}
	keys[0] = nil
	if Trusted()[0] == nil {
		t.Fatal("replacing a returned element changed the trusted list")
	}
}

// 正式构建必须带至少一把受信公钥：空列表下更新器拒绝一切产物，release 流水线的签名步骤也会失败。
func TestTrustedKeysConfigured(t *testing.T) {
	if len(Trusted()) == 0 {
		t.Fatal("no trusted release key is configured")
	}
}
