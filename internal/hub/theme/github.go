package theme

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/xjetry/heron/internal/githubtransport"
)

const (
	githubMaxMetadataBytes = 4 << 20
	githubReleaseLimit     = 30
	githubTimeout          = 30 * time.Second
)

// GitHubError 区分可修正的输入错误与上游网络、限流或响应错误。
type GitHubError struct {
	Kind       string
	Message    string
	StatusCode int
}

func (e *GitHubError) Error() string { return e.Message }

func githubError(kind, format string, args ...any) *GitHubError {
	return &GitHubError{Kind: kind, Message: fmt.Sprintf(format, args...)}
}

// GitHubAsset 是发行版显式上传的 ZIP 资产，不包括 GitHub 自动生成的源码归档。
type GitHubAsset struct {
	ID   int64
	Name string
	Size int64
}

// GitHubRelease 是可选择的已发布版本；Assets 为空时没有可安装的 ZIP 资产。
type GitHubRelease struct {
	Name       string
	Tag        string
	Prerelease bool
	Assets     []GitHubAsset
}

// GitHubSource 记录下载时重新核实的仓库、版本标签与资产文件名。
type GitHubSource struct {
	Repository string
	Release    string
	Asset      string
}

// GitHubClient 只访问公开发行版，不接受访问令牌或任意下载 URL。
type GitHubClient struct {
	http *http.Client
}

// NewGitHubClient 创建有界的公开发行版客户端；环境代理不能绕过目的地址检查。
func NewGitHubClient() *GitHubClient {
	return &GitHubClient{http: githubtransport.NewClient(githubTimeout)}
}

func newGitHubClient(transport http.RoundTripper) *GitHubClient {
	return &GitHubClient{http: githubtransport.WithTransport(transport, githubTimeout)}
}

// Releases 对仓库返回最新至多 30 个发行版，对 Release 链接返回指定版本。
// 只读这一页且不自动选择资产，调用方须展示预发布标记、文件名与大小供管理员选择。
func (c *GitHubClient) Releases(ctx context.Context, repositoryOrReleaseURL string) ([]GitHubRelease, error) {
	repository, tag, err := parseGitHubRepository(repositoryOrReleaseURL)
	if err != nil {
		return nil, err
	}
	if tag != "" {
		release, err := c.release(ctx, repository, tag)
		if err != nil {
			return nil, err
		}
		return []GitHubRelease{release}, nil
	}
	var raw []githubRelease
	endpoint := githubRepositoryAPI(repository) + "/releases?per_page=30&page=1"
	if err := c.json(ctx, endpoint, &raw); err != nil {
		return nil, err
	}
	if len(raw) > githubReleaseLimit {
		raw = raw[:githubReleaseLimit]
	}
	out := make([]GitHubRelease, 0, len(raw))
	for _, r := range raw {
		if r.Draft {
			continue
		}
		parsed, err := r.public()
		if err != nil {
			return nil, err
		}
		out = append(out, parsed)
	}
	return out, nil
}

// Download 按仓库、标签和资产 ID 重查归属后下载，返回值仍须经过 Parse 的包结构校验。
func (c *GitHubClient) Download(ctx context.Context, repository, tag string, assetID int64) ([]byte, GitHubSource, error) {
	repository, urlTag, err := parseGitHubRepository(repository)
	if err != nil {
		return nil, GitHubSource{}, err
	}
	if !validGitHubTag(tag) || assetID <= 0 || urlTag != "" && urlTag != tag {
		return nil, GitHubSource{}, githubError("invalid_argument", "select a valid release tag and asset ID belonging to the repository")
	}
	ctx, cancel := context.WithTimeout(ctx, githubTimeout)
	defer cancel()
	release, err := c.release(ctx, repository, tag)
	if err != nil {
		return nil, GitHubSource{}, err
	}
	var selected *GitHubAsset
	for i := range release.Assets {
		if release.Assets[i].ID == assetID {
			selected = &release.Assets[i]
			break
		}
	}
	if selected == nil {
		return nil, GitHubSource{}, githubError("invalid_argument", "selected ZIP asset is not part of this published release; source archives cannot be installed")
	}
	if selected.Size > MaxPackageBytes {
		return nil, GitHubSource{}, githubError("invalid_argument", "selected asset exceeds the 8 MiB package limit")
	}
	// 下载入口由仓库与资产 ID 生成，不使用响应中的 browser_download_url；资产归属由上面的版本查询保证。
	endpoint := githubRepositoryAPI(repository) + "/releases/assets/" + strconv.FormatInt(assetID, 10)
	body, err := c.get(ctx, endpoint, "application/octet-stream", MaxPackageBytes, "asset exceeds the 8 MiB package limit")
	if err != nil {
		return nil, GitHubSource{}, err
	}
	return body, GitHubSource{Repository: repository, Release: tag, Asset: selected.Name}, nil
}

type githubRelease struct {
	Name       string        `json:"name"`
	Tag        string        `json:"tag_name"`
	Draft      bool          `json:"draft"`
	Prerelease bool          `json:"prerelease"`
	Assets     []GitHubAsset `json:"assets"`
}

func (r githubRelease) public() (GitHubRelease, error) {
	if !validGitHubTag(r.Tag) {
		return GitHubRelease{}, githubError("unavailable", "GitHub returned an invalid release tag")
	}
	out := GitHubRelease{Name: r.Name, Tag: r.Tag, Prerelease: r.Prerelease, Assets: make([]GitHubAsset, 0)}
	seen := make(map[int64]bool, len(r.Assets))
	for _, asset := range r.Assets {
		if asset.ID <= 0 || asset.Size < 0 || asset.Name == "" || !utf8.ValidString(asset.Name) || strings.ContainsFunc(asset.Name, unicode.IsControl) {
			return GitHubRelease{}, githubError("unavailable", "GitHub returned invalid asset metadata")
		}
		if seen[asset.ID] {
			return GitHubRelease{}, githubError("unavailable", "GitHub returned a duplicate asset ID")
		}
		seen[asset.ID] = true
		if len(asset.Name) > 4 && strings.HasSuffix(strings.ToLower(asset.Name), ".zip") {
			out.Assets = append(out.Assets, asset)
		}
	}
	return out, nil
}

func (c *GitHubClient) release(ctx context.Context, repository, tag string) (GitHubRelease, error) {
	var raw githubRelease
	if err := c.json(ctx, githubRepositoryAPI(repository)+"/releases/tags/"+url.PathEscape(tag), &raw); err != nil {
		return GitHubRelease{}, err
	}
	if raw.Tag != tag {
		return GitHubRelease{}, githubError("unavailable", "GitHub returned a release tag different from the selected tag")
	}
	if raw.Draft {
		return GitHubRelease{}, githubError("invalid_argument", "the selected release is not published")
	}
	return raw.public()
}

func githubRepositoryAPI(repository string) string {
	return "https://api.github.com/repos/" + repository
}

func (c *GitHubClient) json(ctx context.Context, endpoint string, dst any) error {
	body, err := c.get(ctx, endpoint, "application/vnd.github+json", githubMaxMetadataBytes, "release metadata exceeds the size limit")
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return githubError("unavailable", "GitHub returned invalid JSON: %v", err)
	}
	return nil
}

func (c *GitHubClient) get(ctx context.Context, endpoint, accept string, limit int64, oversize string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, githubError("unavailable", "cannot create GitHub request: %v", err)
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "heron-hub-theme-installer")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, githubError("unavailable", "GitHub request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		failure := githubError("unavailable", "GitHub returned HTTP %d", resp.StatusCode)
		failure.StatusCode = resp.StatusCode
		switch resp.StatusCode {
		case http.StatusNotFound:
			failure.Kind = "invalid_argument"
			failure.Message = "GitHub repository, release or asset was not found; only public releases are supported"
		case http.StatusForbidden, http.StatusTooManyRequests:
			failure.Message = "GitHub refused the request or its rate limit was reached; retry later"
		}
		return nil, failure
	}
	if resp.ContentLength > limit {
		return nil, githubError("unavailable", "GitHub %s", oversize)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, githubError("unavailable", "cannot read GitHub response: %v", err)
	}
	if int64(len(body)) > limit {
		return nil, githubError("unavailable", "GitHub %s", oversize)
	}
	return body, nil
}

var (
	githubOwnerPattern = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,37}[a-zA-Z0-9])?$`)
	githubRepoPattern  = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,100}$`)
)

func parseGitHubRepository(input string) (repository, tag string, err error) {
	reject := func() (string, string, error) {
		return "", "", githubError("invalid_argument", "repository must be owner/repo, an HTTPS github.com repository URL, or a published Release URL without credentials, port, query or fragment")
	}
	path := input
	if strings.Contains(input, "://") {
		u, parseErr := url.Parse(input)
		if parseErr != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(input, "#") {
			return reject()
		}
		path = strings.TrimPrefix(u.EscapedPath(), "/")
		parts := strings.Split(path, "/")
		if len(parts) == 3 && parts[2] == "" {
			parts = parts[:2]
			path = strings.Join(parts, "/")
		}
		if len(parts) > 2 {
			if len(parts) < 5 || parts[2] != "releases" || parts[3] != "tag" {
				return reject()
			}
			tag, err = url.PathUnescape(strings.Join(parts[4:], "/"))
			if err != nil || !validGitHubTag(tag) {
				return reject()
			}
			path = strings.Join(parts[:2], "/")
		}
	}
	parts := strings.Split(path, "/")
	if len(parts) != 2 || !githubOwnerPattern.MatchString(parts[0]) || !githubRepoPattern.MatchString(parts[1]) || parts[1] == "." || parts[1] == ".." {
		return reject()
	}
	return strings.ToLower(path), tag, nil
}

func validGitHubTag(tag string) bool {
	if tag == "" || len(tag) > 1024 || !utf8.ValidString(tag) || tag == "@" || strings.ContainsFunc(tag, unicode.IsControl) || strings.ContainsAny(tag, " ~^:?*[\\") || strings.Contains(tag, "..") || strings.Contains(tag, "@{") || strings.HasSuffix(tag, ".") {
		return false
	}
	for _, segment := range strings.Split(tag, "/") {
		if segment == "" || strings.HasPrefix(segment, ".") || strings.HasSuffix(segment, ".lock") {
			return false
		}
	}
	return true
}
