package main

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/xjetry/heron/internal/releasesig/sigtest"
)

func pemKey(t *testing.T, priv ed25519.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func distDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "SHA256SUMS"), []byte("00  heron-agent_linux_amd64.tar.gz\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return d
}

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestSignThenVerify(t *testing.T) {
	pub, priv := sigtest.Key()
	d := distDir(t)
	if err := run([]string{"sign", "-version", "v1.2.3", "-dir", d}, env(map[string]string{"HERON_RELEASE_SIGNING_KEY": pemKey(t, priv)}), []ed25519.PublicKey{pub}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"verify", "-version", "v1.2.3", "-dir", d}, env(nil), []ed25519.PublicKey{pub}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"verify", "-version", "v1.2.4", "-dir", d}, env(nil), []ed25519.PublicKey{pub}); err == nil {
		t.Fatal("signature verified for a different version")
	}
}

func TestSignRefusesMissingKey(t *testing.T) {
	pub, _ := sigtest.Key()
	d := distDir(t)
	err := run([]string{"sign", "-version", "v1.2.3", "-dir", d}, env(nil), []ed25519.PublicKey{pub})
	if err == nil || !strings.Contains(err.Error(), "HERON_RELEASE_SIGNING_KEY") {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(d, "SHA256SUMS.sig")); !os.IsNotExist(statErr) {
		t.Fatal("a signature file was written without a key")
	}
}

func TestSignRefusesKeyOutsideTrustedSet(t *testing.T) {
	_, priv := sigtest.Key()
	other := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)).Public().(ed25519.PublicKey)
	d := distDir(t)
	err := run([]string{"sign", "-version", "v1.2.3", "-dir", d}, env(map[string]string{"HERON_RELEASE_SIGNING_KEY": pemKey(t, priv)}), []ed25519.PublicKey{other})
	if err == nil || !strings.Contains(err.Error(), "does not match a trusted public key") {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(d, "SHA256SUMS.sig")); !os.IsNotExist(statErr) {
		t.Fatal("a signature no updater accepts was written")
	}
}

// 持有私钥的 job 只能编译标准库与 internal/releasesig。列表必须含本包自己，
// 否则 go list 出错或输出为空时这条检查会空过。
func TestSigningToolLinksOnlyStdlibAndReleasesig(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	got := strings.Fields(string(out))
	slices.Sort(got)
	want := []string{"github.com/xjetry/heron/internal/releasesig", "github.com/xjetry/heron/scripts/releasesign"}
	if !slices.Equal(got, want) {
		t.Fatalf("non-standard dependencies = %q, want %q", got, want)
	}
}
