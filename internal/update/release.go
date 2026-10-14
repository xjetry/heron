package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/xjetry/heron/internal/githubtransport"
	"github.com/xjetry/heron/internal/releasesig"
)

const (
	officialAPI       = "https://api.github.com/repos/xjetry/heron/releases"
	officialDownloads = "https://github.com/xjetry/heron/releases/download/"
	maxMetadata       = 4 << 20
	// maxServiceFile 是归档里单个服务文件的大小上限，与清单文件的 releasesig.MaxSums 数值相同但语义不同。
	maxServiceFile = 1 << 20
	maxArchive     = 128 << 20
	maxBinary      = 256 << 20
	// maxBoundAgentFile 是 AGENT_VERSION 资产的大小上限。契约内容是一个正式版本加换行，ValidVersion 限每段不超过
	// uint32，合法内容至多 1+3*10+2+1 = 34 字节，超过上限的必然不合契约。
	maxBoundAgentFile = 64
)

var (
	// ErrBoundAgentMissing：目标 release 的下载目录里没有 AGENT_VERSION（下载地址回 404）。该资产随 release 发布
	// 之前的所有版本都属此类。
	ErrBoundAgentMissing = errors.New("release has no AGENT_VERSION asset")
	// ErrBoundAgentInvalid：资产取到了，但不是"一个正式版本加一个换行"。
	ErrBoundAgentInvalid = errors.New("release AGENT_VERSION asset is not one stable version followed by a newline")

	errTooLarge = errors.New("official response exceeds size limit")
)

// officialStatusError 是官方地址回了 200 以外的状态。BoundAgentVersion 靠它把"资产不存在"（404）与其余失败分开：
// GitHub 对不存在的 release 资产直接回 404，存在的资产回 302 转到存储地址。
type officialStatusError struct{ code int }

func (e officialStatusError) Error() string {
	return fmt.Sprintf("official release returned HTTP %d", e.code)
}

// OfficialSource 只从固定官方仓库的下载目录取回产物字节，不做任何接受判定（见 Accept），
// 不接收外部下载地址或摘要。
type OfficialSource struct {
	http *http.Client
	// stall 是读正文的停滞期限，正式构造固定为 stallTimeout；只有包内测试会把它缩短。
	stall time.Duration
}

// NewOfficialSource 的客户端不设总时限，只有 githubtransport 的建连、握手与等响应头时限；总上限由调用方的 ctx
// 给出（更新器与 hub 中转都是 DownloadLimit，后台查最新版本 15 秒），get 拒绝没有期限的 ctx（见 source），
// 停滞由 get 读正文时判定（见 transfer.go）。
func NewOfficialSource() *OfficialSource {
	return &OfficialSource{http: githubtransport.NewClient(0), stall: stallTimeout}
}

type officialRelease struct {
	Tag        string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Assets     []struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	} `json:"assets"`
}

func (s *OfficialSource) release(ctx context.Context, suffix string) (officialRelease, error) {
	var release officialRelease
	budget, err := timeLimit(ctx)
	if err != nil {
		return release, err
	}
	body, err := s.get(ctx, budget, officialAPI+suffix, "application/vnd.github+json", maxMetadata)
	if err != nil {
		return release, err
	}
	if err := json.Unmarshal(body, &release); err != nil {
		return release, fmt.Errorf("invalid official release metadata: %w", err)
	}
	if !ValidVersion(release.Tag) || release.Draft || release.Prerelease {
		return release, fmt.Errorf("official release is not a published stable version")
	}
	return release, nil
}

// Latest 返回 GitHub 标记为最新的正式版，不把预发布标签转换成正式版本。
func (s *OfficialSource) Latest(ctx context.Context) (string, error) {
	release, err := s.release(ctx, "/latest")
	if err != nil {
		return "", err
	}
	return release.Tag, nil
}

// Fetch 从固定官方下载目录取回某版本的清单、签名与归档，不做任何接受判定（见 Accept）。
// 地址只由固定仓库、版本与本地产物矩阵拼出，不调 releases API：草稿 release 没有公开下载地址，
// 预发布 tag 过不了 ValidVersion，资产是否存在由下载结果本身回答。task 只用到版本。
func (s *OfficialSource) Fetch(ctx context.Context, task Request, role, arch string) (Artifacts, error) {
	asset, err := archiveName(role, arch)
	if err != nil {
		return Artifacts{}, err
	}
	if !ValidVersion(task.Version) {
		return Artifacts{}, errors.New("invalid stable version")
	}
	budget, err := timeLimit(ctx)
	if err != nil {
		return Artifacts{}, err
	}
	base := officialDownloads + task.Version + "/"
	var a Artifacts
	if a.Sums, err = s.get(ctx, budget, base+"SHA256SUMS", "application/octet-stream", releasesig.MaxSums); err != nil {
		return Artifacts{}, err
	}
	if a.Signature, err = s.get(ctx, budget, base+"SHA256SUMS.sig", "application/octet-stream", releasesig.MaxFile); err != nil {
		return Artifacts{}, err
	}
	if a.Archive, err = s.get(ctx, budget, base+asset, "application/octet-stream", maxArchive); err != nil {
		return Artifacts{}, err
	}
	return a, nil
}

// BoundAgentVersion 读正式版 tag 的 AGENT_VERSION 资产：那次构建注入 hub 的绑定 agent 版本，地址与 Fetch 取
// SHA256SUMS 同一拼法。内容按原始字节核对，必须恰好是一个正式版本加一个换行；多余的行、空白、CR 都不合契约。
// 正式版 release 的绑定值总是正式版：完整发布时它等于 tag，只发 hub 时 releasekind 只允许绑定正式版。
//
// 这里不验签：结果只用来在面板发起 hub 更新前提示绑定是否会变化，不参与任何接受判定。更新后真正生效的绑定由
// 已验签的 hub 二进制自带，与这份资产无关。
//
// 错误分三类，调用方据此分开记日志：ErrBoundAgentMissing（404）、ErrBoundAgentInvalid（取到但不合契约）、
// 其余（传输失败、其他 HTTP 状态、超时）。
func (s *OfficialSource) BoundAgentVersion(ctx context.Context, tag string) (string, error) {
	if !ValidVersion(tag) {
		return "", errors.New("invalid stable version")
	}
	budget, err := timeLimit(ctx)
	if err != nil {
		return "", err
	}
	body, err := s.get(ctx, budget, officialDownloads+tag+"/AGENT_VERSION", "application/octet-stream", maxBoundAgentFile)
	var status officialStatusError
	switch {
	case errors.As(err, &status) && status.code == http.StatusNotFound:
		return "", ErrBoundAgentMissing
	case errors.Is(err, errTooLarge):
		return "", fmt.Errorf("%w: larger than %d bytes", ErrBoundAgentInvalid, maxBoundAgentFile)
	case err != nil:
		return "", err
	}
	version, ok := strings.CutSuffix(string(body), "\n")
	if !ok || !ValidVersion(version) {
		return "", fmt.Errorf("%w: got %q", ErrBoundAgentInvalid, body)
	}
	return version, nil
}

func archiveName(role, arch string) (string, error) {
	switch role {
	case "hub":
		if arch != "amd64" && arch != "arm64" {
			return "", fmt.Errorf("unsupported hub architecture")
		}
	case "agent":
		switch arch {
		case "amd64", "arm64", "armv7", "386", "riscv64":
		default:
			return "", fmt.Errorf("unsupported agent architecture")
		}
	default:
		return "", fmt.Errorf("unsupported update role")
	}
	return "heron-" + role + "_linux_" + arch + ".tar.gz", nil
}

// get 取回一个地址的正文。budget 是调用方给整次取回的总时长，只用于文案，期限本身在 ctx 上；停滞计时从收到响应头
// 开始，覆盖读正文的全程。
func (s *OfficialSource) get(ctx context.Context, budget time.Duration, endpoint, accept string, limit int64) ([]byte, error) {
	if _, err := timeLimit(ctx); err != nil {
		return nil, err
	}
	// 零值的停滞期限会让计时器立刻到点、任何正文都判停滞；它只可能来自绕开 NewOfficialSource 的构造，显式拒绝。
	if s.stall <= 0 {
		return nil, errors.New("official source has no stall timeout")
	}
	reqCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "heron-official-updater")
	start := time.Now()
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download official release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, officialStatusError{resp.StatusCode}
	}
	if resp.ContentLength > limit {
		return nil, errTooLarge
	}
	body := &watchedBody{r: resp.Body, stall: s.stall, timer: time.AfterFunc(s.stall, func() { cancel(errStalled) })}
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	body.timer.Stop()
	if err != nil {
		return nil, readError(err, context.Cause(reqCtx), s.stall, budget, body.n, resp.ContentLength, time.Since(start))
	}
	if int64(len(data)) > limit {
		return nil, errTooLarge
	}
	return data, nil
}

func checksum(body []byte, asset string) ([32]byte, error) {
	var sum [32]byte
	found := false
	seen := make(map[string]bool)
	for _, line := range strings.Split(strings.TrimSuffix(string(body), "\n"), "\n") {
		if len(line) < 67 || line[64] != ' ' || line[65] != ' ' && line[65] != '*' {
			return sum, fmt.Errorf("invalid official checksum line")
		}
		raw, err := hex.DecodeString(line[:64])
		name := line[66:]
		if err != nil || name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\r\x00") || seen[name] {
			return sum, fmt.Errorf("invalid or duplicate official checksum entry")
		}
		seen[name] = true
		if name == asset {
			copy(sum[:], raw)
			found = true
		}
	}
	if !found {
		return sum, fmt.Errorf("official checksum missing target asset")
	}
	return sum, nil
}

func extractBinary(archive []byte, role string) ([]byte, error) {
	compressed := bytes.NewReader(archive)
	gz, err := gzip.NewReader(compressed)
	if err != nil {
		return nil, fmt.Errorf("open official archive: %w", err)
	}
	defer gz.Close()
	gz.Multistream(false)
	// 二进制及两个已知服务文件之外只允许 tar 零填充；全流限额也覆盖扩展头与尾部。
	limited := &io.LimitedReader{R: gz, N: maxBinary + 3*maxServiceFile + 1}
	tr := tar.NewReader(limited)
	binaryName := "heron-" + role
	seen := make(map[string]bool)
	var binary []byte
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read official archive: %w", err)
		}
		allowed := h.Name == binaryName || h.Name == binaryName+".service" || role == "agent" && h.Name == binaryName+".openrc"
		if !allowed || seen[h.Name] || h.Typeflag != tar.TypeReg || h.Linkname != "" || len(h.PAXRecords) != 0 {
			return nil, fmt.Errorf("unexpected, duplicate or non-regular official archive entry %q", h.Name)
		}
		seen[h.Name] = true
		limit := int64(maxServiceFile)
		if h.Name == binaryName {
			limit = maxBinary
		}
		if h.Size < 0 || h.Size > limit || h.Name == binaryName && h.Size == 0 {
			return nil, fmt.Errorf("invalid official archive entry size")
		}
		if h.Name == binaryName {
			binary, err = io.ReadAll(tr)
			if err != nil {
				return nil, fmt.Errorf("read official binary: %w", err)
			}
		}
	}
	// tar 的结束标记不保证 gzip 校验和已被读取，继续读完才能拒绝截断和损坏的压缩流。
	buffer := make([]byte, 32<<10)
	for {
		n, readErr := limited.Read(buffer)
		for _, b := range buffer[:n] {
			if b != 0 {
				return nil, fmt.Errorf("unexpected data after official tar archive")
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("finish official archive: %w", readErr)
		}
	}
	if limited.N <= 0 || compressed.Len() != 0 {
		return nil, fmt.Errorf("official archive exceeds limits or contains trailing data")
	}
	if len(binary) == 0 {
		return nil, fmt.Errorf("official archive has no binary")
	}
	return binary, nil
}
