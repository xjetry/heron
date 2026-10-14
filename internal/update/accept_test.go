package update

import (
	"archive/tar"
	"crypto/ed25519"
	"strings"
	"testing"

	"github.com/xjetry/heron/internal/releasesig/sigtest"
)

func signedArtifacts(role, arch, version string, archive []byte) Artifacts {
	asset, err := archiveName(role, arch)
	if err != nil {
		panic(err)
	}
	sums := sigtest.Sums(asset, archive)
	return Artifacts{Sums: sums, Signature: sigtest.Sign(version, sums), Archive: archive}
}

func testKeys() []ed25519.PublicKey { pub, _ := sigtest.Key(); return []ed25519.PublicKey{pub} }

func TestAccept(t *testing.T) {
	archive := sigtest.Archive("agent", []byte("binary"))
	good := signedArtifacts("agent", "amd64", "v1.2.3", archive)
	otherVersion := signedArtifacts("agent", "amd64", "v1.2.2", archive)
	otherAsset := signedArtifacts("agent", "arm64", "v1.2.3", archive)
	tamperedArchive := good
	tamperedArchive.Archive = sigtest.Archive("agent", []byte("evil"))
	extraEntry := makeArchive(t, []*tar.Header{
		{Name: "heron-agent", Mode: 0o755, Size: 6, Typeflag: tar.TypeReg},
		{Name: "extra", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg},
	}, []string{"binary", "x"})
	badStructure := signedArtifacts("agent", "amd64", "v1.2.3", extraEntry)
	other := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)).Public().(ed25519.PublicKey)
	for _, tc := range []struct {
		name    string
		keys    []ed25519.PublicKey
		role    string
		arch    string
		version string
		a       Artifacts
		want    string
	}{
		{"valid", testKeys(), "agent", "amd64", "v1.2.3", good, ""},
		{"untrusted_key", []ed25519.PublicKey{other}, "agent", "amd64", "v1.2.3", good, "does not verify"},
		{"no_trusted_keys", nil, "agent", "amd64", "v1.2.3", good, "does not verify"},
		{"signed_for_other_version", testKeys(), "agent", "amd64", "v1.2.3", otherVersion, "does not verify"},
		{"tampered_archive", testKeys(), "agent", "amd64", "v1.2.3", tamperedArchive, "SHA-256 mismatch"},
		{"sums_lack_asset", testKeys(), "agent", "amd64", "v1.2.3", otherAsset, "missing target asset"},
		{"archive_structure", testKeys(), "agent", "amd64", "v1.2.3", badStructure, "unexpected"},
		{"prerelease_version", testKeys(), "agent", "amd64", "v1.2.3-rc.1", good, "invalid stable version"},
		{"unknown_arch", testKeys(), "agent", "mips", "v1.2.3", good, "unsupported agent architecture"},
		{"empty_archive", testKeys(), "agent", "amd64", "v1.2.3", Artifacts{Sums: good.Sums, Signature: good.Signature}, "size limits"},
	} {
		bin, err := Accept(tc.keys, tc.role, tc.arch, tc.version, tc.a)
		if tc.want == "" {
			if err != nil || string(bin) != "binary" {
				t.Errorf("%s: bin=%q err=%v", tc.name, bin, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err=%v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestAgentArchMatchesArchiveMatrix(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64", "armv7", "386", "riscv64"} {
		if !AgentArch(arch) {
			t.Errorf("%s rejected", arch)
		}
	}
	for _, arch := range []string{"", "arm", "mips", "amd64/../x"} {
		if AgentArch(arch) {
			t.Errorf("%q accepted", arch)
		}
	}
}
