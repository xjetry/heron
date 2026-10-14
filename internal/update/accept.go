package update

import (
	"crypto/ed25519"
	"crypto/sha256"
	"errors"

	"github.com/xjetry/heron/internal/releasesig"
)

// Artifacts 是来源取回的三份原始字节。来源只负责取回，接受规则见 Accept。
type Artifacts struct {
	Sums      []byte
	Signature []byte
	Archive   []byte
}

// Accept 是在线更新的共同接受规则：hub 与 agent 两种角色、GitHub 与 hub 两种来源都经过它；来源只取回三份原始字节，
// 不参与版本、签名、摘要或归档结构判定。顺序：版本合法 → 以调用方持有的版本验签 → 归档摘要在已验签
// 清单里 → 归档结构检查。version 必须是调用方自己的任务版本：签名绑定版本号，来源声称的版本不参与判定。
// "新于本机当前版本"不在这里：Accept 不知道本机版本，它由 Engine.submit 在接收任务时裁决。
// hub 的 Relay 取回时调用同一判定提前暴露坏产物，agent 更新器下载后仍自己调用一次，不信任中转的结论。
func Accept(keys []ed25519.PublicKey, role, arch, version string, a Artifacts) ([]byte, error) {
	asset, err := archiveName(role, arch)
	if err != nil {
		return nil, err
	}
	if !ValidVersion(version) {
		return nil, errors.New("invalid stable version")
	}
	if len(a.Sums) == 0 || len(a.Sums) > releasesig.MaxSums || len(a.Archive) == 0 || len(a.Archive) > maxArchive {
		return nil, errors.New("release artifacts are empty or exceed size limits")
	}
	if err := releasesig.Verify(keys, version, a.Sums, a.Signature); err != nil {
		return nil, err
	}
	want, err := checksum(a.Sums, asset)
	if err != nil {
		return nil, err
	}
	if sha256.Sum256(a.Archive) != want {
		return nil, errors.New("release asset SHA-256 mismatch")
	}
	return extractBinary(a.Archive, role)
}

// AgentArch 判定 arch 是否在 agent 产物矩阵里；与拼资产名共用 archiveName 那一张表。
func AgentArch(arch string) bool {
	_, err := archiveName("agent", arch)
	return err == nil
}
