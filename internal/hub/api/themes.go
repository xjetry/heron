package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/hub/theme"
)

func themeProto(t store.Theme) *heronv1.Theme {
	return &heronv1.Theme{Id: t.ID, Name: t.Name, Version: t.Version, UploadedAt: t.UploadedAt.Unix(), Enabled: t.Enabled, HasPreview: t.Preview != "", Digest: t.Digest, Sdk: uint32(t.SDK), Previous: t.Previous, Published: t.Published, Repository: t.Repository, Release: t.Release, Asset: t.Asset}
}

func themeError(err error) error {
	var bad *theme.Error
	var github *theme.GitHubError
	switch {
	case errors.As(err, &bad):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.As(err, &github):
		if github.Kind == "invalid_argument" {
			return invalid("%s", err)
		}
		return connect.NewError(connect.CodeUnavailable, err)
	case errors.Is(err, store.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, errors.New("主题版本不存在"))
	case errors.Is(err, store.ErrThemeLimit):
		return connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("最多安装 %d 个主题，请先删除不使用的主题", theme.MaxThemes))
	case errors.Is(err, store.ErrThemeVersionLimit):
		return connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("每个主题最多保留 %d 个版本，请先清理非当前、非回滚版本后重试", store.MaxThemeVersions))
	case errors.Is(err, store.ErrThemeInUse), errors.Is(err, store.ErrThemeContentMissing):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	default:
		return internalError("主题操作失败")
	}
}

func (s *Service) installTheme(ctx context.Context, content []byte, expect string, source theme.GitHubSource) (store.Theme, error) {
	pkg, err := theme.Parse(content)
	if err != nil {
		return store.Theme{}, invalid("%s", err)
	}
	if err := theme.CheckExecutable(pkg.Manifest.SDK); err != nil {
		return store.Theme{}, themeError(err)
	}
	if expect != "" && expect != pkg.Manifest.ID {
		return store.Theme{}, invalid("expect_id: %q does not match the package's theme.json id %q", expect, pkg.Manifest.ID)
	}
	files := make([]store.ThemeFile, len(pkg.Files))
	for i, f := range pkg.Files {
		files[i] = store.ThemeFile{Path: f.Path, Content: f.Content}
	}
	meta := store.Theme{ID: pkg.Manifest.ID, Name: pkg.Manifest.Name, Version: pkg.Manifest.Version, Preview: pkg.Manifest.Preview, SDK: pkg.Manifest.SDK, UploadedAt: s.clk.Now(), Repository: source.Repository, Release: source.Release, Asset: source.Asset}
	got, err := s.store.PutTheme(ctx, meta, files, content, expect != "", theme.MaxThemes)
	if err != nil {
		s.log.Error("installing theme failed", "err", err)
		return store.Theme{}, themeError(err)
	}
	return got, nil
}

func (s *Service) acquireThemeInstall() error {
	select {
	case s.uploading <- struct{}{}:
		return nil
	default:
		return connect.NewError(connect.CodeResourceExhausted, errors.New("另一个主题正在安装，请完成后重试"))
	}
}

func (s *Service) UploadTheme(ctx context.Context, req *connect.Request[heronv1.UploadThemeRequest]) (*connect.Response[heronv1.UploadThemeResponse], error) {
	if err := s.acquireThemeInstall(); err != nil {
		return nil, err
	}
	defer func() { <-s.uploading }()
	installed, err := s.installTheme(ctx, req.Msg.GetPackage(), req.Msg.GetExpectId(), theme.GitHubSource{})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&heronv1.UploadThemeResponse{Theme: themeProto(installed)}), nil
}

func (s *Service) ListThemes(ctx context.Context, _ *connect.Request[heronv1.ListThemesRequest]) (*connect.Response[heronv1.ListThemesResponse], error) {
	list, err := s.store.ListThemes(ctx)
	if err != nil {
		return nil, themeError(err)
	}
	out := &heronv1.ListThemesResponse{PublicDir: s.cfg.PublicDir}
	for _, t := range list {
		out.Themes = append(out.Themes, themeProto(t))
	}
	return connect.NewResponse(out), nil
}

func (s *Service) EnableTheme(ctx context.Context, req *connect.Request[heronv1.EnableThemeRequest]) (*connect.Response[heronv1.EnableThemeResponse], error) {
	if s.cfg.PublicDir && req.Msg.GetId() != "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("--public-dir 正在接管公开页，请移除该启动参数后启用主题"))
	}
	if err := s.store.EnableTheme(ctx, req.Msg.GetId(), req.Msg.GetDigest()); err != nil {
		return nil, themeError(err)
	}
	return connect.NewResponse(&heronv1.EnableThemeResponse{}), nil
}

func (s *Service) DeleteTheme(ctx context.Context, req *connect.Request[heronv1.DeleteThemeRequest]) (*connect.Response[heronv1.DeleteThemeResponse], error) {
	if err := s.store.DeleteTheme(ctx, req.Msg.GetId()); err != nil {
		return nil, themeError(err)
	}
	return connect.NewResponse(&heronv1.DeleteThemeResponse{}), nil
}

func (s *Service) DeleteThemeVersion(ctx context.Context, req *connect.Request[heronv1.DeleteThemeVersionRequest]) (*connect.Response[heronv1.DeleteThemeVersionResponse], error) {
	if err := s.store.DeleteThemeVersion(ctx, req.Msg.GetId(), req.Msg.GetDigest()); err != nil {
		return nil, themeError(err)
	}
	return connect.NewResponse(&heronv1.DeleteThemeVersionResponse{}), nil
}

func (s *Service) GetThemePreview(ctx context.Context, req *connect.Request[heronv1.GetThemePreviewRequest]) (*connect.Response[heronv1.GetThemePreviewResponse], error) {
	t, content, err := s.store.ThemePreview(ctx, req.Msg.GetId(), req.Msg.GetDigest())
	if err != nil {
		return nil, themeError(err)
	}
	if t.Preview == "" {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("主题没有预览图"))
	}
	return connect.NewResponse(&heronv1.GetThemePreviewResponse{Content: content, ContentType: theme.PreviewContentType(t.Preview)}), nil
}

func (s *Service) GetThemePackage(ctx context.Context, req *connect.Request[heronv1.GetThemePackageRequest]) (*connect.Response[heronv1.GetThemePackageResponse], error) {
	content, err := s.store.ThemePackage(ctx, req.Msg.GetId(), req.Msg.GetDigest())
	if err != nil {
		return nil, themeError(err)
	}
	return connect.NewResponse(&heronv1.GetThemePackageResponse{Package: content}), nil
}

func (s *Service) ListThemeReleases(ctx context.Context, req *connect.Request[heronv1.ListThemeReleasesRequest]) (*connect.Response[heronv1.ListThemeReleasesResponse], error) {
	releases, err := s.github.Releases(ctx, req.Msg.GetRepository())
	if err != nil {
		return nil, themeError(err)
	}
	out := &heronv1.ListThemeReleasesResponse{}
	for _, r := range releases {
		item := &heronv1.ThemeRelease{Name: r.Name, Tag: r.Tag, Prerelease: r.Prerelease}
		for _, a := range r.Assets {
			item.Assets = append(item.Assets, &heronv1.ThemeReleaseAsset{Id: a.ID, Name: a.Name, Size: a.Size})
		}
		out.Releases = append(out.Releases, item)
	}
	return connect.NewResponse(out), nil
}

func (s *Service) InstallThemeRelease(ctx context.Context, req *connect.Request[heronv1.InstallThemeReleaseRequest]) (*connect.Response[heronv1.InstallThemeReleaseResponse], error) {
	if err := s.acquireThemeInstall(); err != nil {
		return nil, err
	}
	defer func() { <-s.uploading }()
	content, source, err := s.github.Download(ctx, req.Msg.GetRepository(), req.Msg.GetTag(), req.Msg.GetAssetId())
	if err != nil {
		return nil, themeError(err)
	}
	installed, err := s.installTheme(ctx, content, req.Msg.GetExpectId(), source)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&heronv1.InstallThemeReleaseResponse{Theme: themeProto(installed)}), nil
}

type themePreviewGrant struct {
	id, digest, session string
	expires             time.Time
	generation          uint64
}

func (s *Service) PreviewTheme(ctx context.Context, req *connect.Request[heronv1.PreviewThemeRequest]) (*connect.Response[heronv1.PreviewThemeResponse], error) {
	if !s.store.PublicEnabled() {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("公开页已关闭，交互预览不可用"))
	}
	gen := s.store.ThemeGeneration()
	t, err := s.store.ThemeVersion(ctx, req.Msg.GetId(), req.Msg.GetDigest())
	if err != nil {
		return nil, themeError(err)
	}
	if err := theme.CheckExecutable(t.SDK); err != nil {
		return nil, themeError(err)
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, internalError("生成预览能力失败")
	}
	token := hex.EncodeToString(b[:])
	session := ctx.Value(sessionKey{}).(string)
	s.previewMu.Lock()
	defer s.previewMu.Unlock()
	for key, g := range s.previews {
		if g.session == session || !s.clk.Now().Before(g.expires) || g.generation != gen {
			delete(s.previews, key)
		}
	}
	if len(s.previews) >= 64 {
		return nil, connect.NewError(connect.CodeResourceExhausted, errors.New("预览过多，请稍后重试"))
	}
	s.previews[token] = themePreviewGrant{id: t.ID, digest: t.Digest, session: session, expires: s.clk.Now().Add(5 * time.Minute), generation: gen}
	return connect.NewResponse(&heronv1.PreviewThemeResponse{Url: "/_heron/preview/" + token + "/"}), nil
}

func (s *Service) ThemePreviewAccess(ctx context.Context, token string) (string, string, bool) {
	s.previewMu.Lock()
	g, ok := s.previews[token]
	if ok && (!s.clk.Now().Before(g.expires) || g.generation != s.store.ThemeGeneration() || !s.store.PublicEnabled()) {
		delete(s.previews, token)
		ok = false
	}
	s.previewMu.Unlock()
	if !ok {
		return "", "", false
	}
	_, alive, err := s.auth.AuthenticateSession(ctx, []string{g.session})
	return g.id, g.digest, err == nil && alive
}
