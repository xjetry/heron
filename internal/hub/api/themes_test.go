package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/theme"
	. "github.com/xjetry/heron/internal/hub/theme/themetest"
)

func newThemeHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, "")
	h.login(t)
	return h
}

func TestThemeWithoutContentCannotEnable(t *testing.T) {
	t.Parallel()
	h := newThemeHarness(t)
	if _, err := h.upload(t, Minimal(t, "a"), ""); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", h.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("DELETE FROM theme_file; DELETE FROM theme_package"); err != nil {
		t.Fatal(err)
	}
	_, err = h.admin.EnableTheme(t.Context(), connect.NewRequest(&heronv1.EnableThemeRequest{Id: "a", Digest: h.digest(t, "a")}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "upload") {
		t.Fatalf("empty theme enable error=%v", err)
	}
	status, err := h.admin.GetBackupStatus(t.Context(), connect.NewRequest(&heronv1.GetBackupStatusRequest{}))
	if err != nil || !slices.Equal(status.Msg.ThemesWithoutPackage, []string{"a"}) {
		t.Fatalf("missing packages not exposed: %v %v", status, err)
	}
}

func TestLegacyThemeCannotExecute(t *testing.T) {
	t.Parallel()
	h := newThemeHarness(t)
	pkg := Minimal(t, "legacy")
	meta, err := h.upload(t, pkg, "")
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", h.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("UPDATE theme_version SET sdk=0"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.admin.EnableTheme(t.Context(), connect.NewRequest(&heronv1.EnableThemeRequest{Id: meta.Id, Digest: meta.Digest})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("legacy enable: %v", err)
	}
	if _, err := h.admin.PreviewTheme(t.Context(), connect.NewRequest(&heronv1.PreviewThemeRequest{Id: meta.Id, Digest: meta.Digest})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("legacy preview: %v", err)
	}
	archive, err := h.admin.GetThemePackage(t.Context(), connect.NewRequest(&heronv1.GetThemePackageRequest{Id: meta.Id, Digest: meta.Digest}))
	if err != nil || !bytes.Equal(archive.Msg.Package, pkg) {
		t.Fatalf("legacy archive download: %v %v", archive, err)
	}
}

func (h *harness) digest(t *testing.T, id string) string {
	t.Helper()
	if id == "" {
		return ""
	}
	for _, th := range h.themes(t) {
		if th.Id == id {
			return th.Digest
		}
	}
	return "missing"
}

func (h *harness) upload(t *testing.T, pkg []byte, expect string) (*heronv1.Theme, error) {
	t.Helper()
	resp, err := h.admin.UploadTheme(t.Context(), connect.NewRequest(&heronv1.UploadThemeRequest{Package: pkg, ExpectId: expect}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetTheme(), nil
}

func (h *harness) themes(t *testing.T) []*heronv1.Theme {
	t.Helper()
	resp, err := h.admin.ListThemes(t.Context(), connect.NewRequest(&heronv1.ListThemesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg.GetThemes()
}

func (h *harness) enabledThemes(t *testing.T) []string {
	t.Helper()
	out := []string{}
	for _, th := range h.themes(t) {
		if th.GetEnabled() {
			out = append(out, th.GetId())
		}
	}
	return out
}

func (h *harness) themeRows(t *testing.T) [2]int64 {
	t.Helper()
	rows := rowCounts(t, h.dbPath)
	return [2]int64{rows["theme"], rows["theme_file"]}
}

// 上传 → 列出 → 启用 → 预览 → 删除 → 回落：删掉启用中的主题后没有启用的主题，库里不留它的任何行。
func TestThemeRoundTrip(t *testing.T) {
	t.Parallel()
	h := newThemeHarness(t)
	ctx := t.Context()
	h.clk.Advance(time.Hour)
	pkg := Zip(t,
		Manifest(t, "night", "Night", "1.2.0", "preview.webp"),
		File("index.html", "<!doctype html>night"),
		Entry{Name: "assets/", Mode: fs.ModeDir | 0o755},
		File("assets/app.js", "fetch('/heron.v1.PublicService/GetSnapshot')"),
		File("assets/empty.css", ""),
		Entry{Name: "preview.webp", Content: []byte("RIFF\x00\x00\x00\x00WEBPVP8 ")},
	)
	got, err := h.upload(t, pkg, "")
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", h.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var original []byte
	err = db.QueryRow("SELECT content FROM theme_package WHERE theme_id = 'night'").Scan(&original)
	if err != nil || !bytes.Equal(original, pkg) {
		t.Fatalf("UploadTheme did not preserve original zip: %v", err)
	}
	want := &heronv1.Theme{Id: "night", Name: "Night", Version: "1.2.0", UploadedAt: h.clk.Now().Unix(), HasPreview: true, Digest: got.Digest, Sdk: 1}
	if !proto.Equal(got, want) {
		t.Fatalf("UploadTheme = %v, want %v", got, want)
	}
	if list := h.themes(t); len(list) != 1 || !proto.Equal(list[0], want) {
		t.Fatalf("ListThemes = %v, want [%v]", list, want)
	}
	archive, err := h.admin.GetThemePackage(ctx, connect.NewRequest(&heronv1.GetThemePackageRequest{Id: got.Id, Digest: got.Digest}))
	if err != nil || !bytes.Equal(archive.Msg.Package, pkg) {
		t.Fatalf("archive download: %v %v", archive, err)
	}
	if rows := h.themeRows(t); rows != [2]int64{1, 5} {
		t.Fatalf("theme, theme_file rows = %v, want [1 5] (directories are not stored)", rows)
	}
	if _, err := h.admin.EnableTheme(ctx, connect.NewRequest(&heronv1.EnableThemeRequest{Id: "night", Digest: h.digest(t, "night")})); err != nil {
		t.Fatal(err)
	}
	if got := h.enabledThemes(t); !slices.Equal(got, []string{"night"}) {
		t.Fatalf("enabled = %v, want [night]", got)
	}
	preview, err := h.admin.GetThemePreview(ctx, connect.NewRequest(&heronv1.GetThemePreviewRequest{Id: "night", Digest: h.digest(t, "night")}))
	if err != nil {
		t.Fatal(err)
	}
	if preview.Msg.GetContentType() != "image/webp" || !bytes.HasPrefix(preview.Msg.GetContent(), []byte("RIFF")) {
		t.Fatalf("GetThemePreview = %q %q", preview.Msg.GetContentType(), preview.Msg.GetContent())
	}
	if _, err := h.admin.DeleteTheme(ctx, connect.NewRequest(&heronv1.DeleteThemeRequest{Id: "night"})); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT content FROM theme_package WHERE theme_id = 'night'").Scan(&original); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("DeleteTheme left original package: %v", err)
	}
	if list := h.themes(t); len(list) != 0 {
		t.Fatalf("ListThemes after delete = %v", list)
	}
	if rows := h.themeRows(t); rows != [2]int64{0, 0} {
		t.Fatalf("theme, theme_file rows after delete = %v, want none", rows)
	}
	for _, c := range []struct {
		name string
		call func() error
	}{
		{"preview", func() error {
			_, err := h.admin.GetThemePreview(ctx, connect.NewRequest(&heronv1.GetThemePreviewRequest{Id: "night", Digest: h.digest(t, "night")}))
			return err
		}},
		{"enable", func() error {
			_, err := h.admin.EnableTheme(ctx, connect.NewRequest(&heronv1.EnableThemeRequest{Id: "night", Digest: h.digest(t, "night")}))
			return err
		}},
		{"delete", func() error {
			_, err := h.admin.DeleteTheme(ctx, connect.NewRequest(&heronv1.DeleteThemeRequest{Id: "night"}))
			return err
		}},
	} {
		if err := c.call(); codeOf(err) != connect.CodeNotFound {
			t.Errorf("%s of a deleted theme: %v, want NotFound naming it", c.name, err)
		}
	}
	if _, err := h.upload(t, Minimal(t, "plain"), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.admin.GetThemePreview(ctx, connect.NewRequest(&heronv1.GetThemePreviewRequest{Id: "plain", Digest: h.digest(t, "plain")})); codeOf(err) != connect.CodeNotFound {
		t.Fatalf("preview of a theme without one: %v, want NotFound saying it has no preview", err)
	}
}

// 每种被拒的包：InvalidArgument，错误写明位置与原因，库里的主题与文件行数不变（校验先于写入，写入是单事务）。
func TestUploadThemeRejectsBadPackagesWithoutResidue(t *testing.T) {
	t.Parallel()
	h := newThemeHarness(t)
	if _, err := h.upload(t, Minimal(t, "kept", File("kept.js", "1")), ""); err != nil {
		t.Fatal(err)
	}
	before := h.themeRows(t)
	zeros := func(n int) []byte { return make([]byte, n) }
	base := func(extra ...Entry) []byte {
		return Zip(t, append([]Entry{Manifest(t, "bad", "Bad", "1", ""), File("index.html", "x")}, extra...)...)
	}
	many := []Entry{Manifest(t, "bad", "Bad", "1", ""), File("index.html", "x")}
	for i := len(many); i <= theme.MaxEntries; i++ {
		many = append(many, File(fmt.Sprintf("f%04d", i), "x"))
	}
	for _, c := range []struct {
		name, want string
		pkg        []byte
	}{
		{"symlink", `entry "link": Unix mode 0120777 marks a symbolic link`, base(Entry{Name: "link", Content: []byte("../kept/index.html"), Mode: fs.ModeSymlink | 0o777})},
		{"parent", `entry "../x": ".." segment`, base(File("../x", "x"))},
		{"absolute", `entry "/x": absolute path`, base(File("/x", "x"))},
		{"2001 entries", "package: 2001 entries; at most 2000", Zip(t, many...)},
		{"declared total", "package: central directory declares", base(
			Entry{Name: "a", Content: zeros(13 << 20)}, Entry{Name: "b", Content: zeros(13 << 20)}, Entry{Name: "c", Content: zeros(13 << 20)},
			Entry{Name: "d", Content: zeros(13 << 20)}, Entry{Name: "e", Content: zeros(13 << 20)})},
		{"lying central directory", `entry "a": expands past the 1024 bytes the central directory declares`, base(
			Entry{Name: "a", Content: zeros(15 << 20), Declared: 1 << 10}, Entry{Name: "b", Content: zeros(15 << 20), Declared: 1 << 10},
			Entry{Name: "c", Content: zeros(15 << 20), Declared: 1 << 10}, Entry{Name: "d", Content: zeros(15 << 20), Declared: 1 << 10},
			Entry{Name: "e", Content: zeros(15 << 20), Declared: 1 << 10})},
		{"single file", `entry "big": central directory declares 16777217 bytes uncompressed; at most 16777216`, base(Entry{Name: "big", Content: zeros(theme.MaxFileBytes + 1)})},
		{"builtin", `theme.json id: "builtin" is reserved`, Zip(t, Manifest(t, "builtin", "B", "1", ""), File("index.html", "x"))},
		{"no manifest", "package: no theme.json", Zip(t, File("index.html", "x"))},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := h.upload(t, c.pkg, "")
			if codeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("UploadTheme = %v, want InvalidArgument containing %q", err, c.want)
			}
			if rows := h.themeRows(t); rows != before {
				t.Fatalf("theme, theme_file rows = %v, want %v unchanged", rows, before)
			}
		})
	}
}

// 重叠的中央目录记录经 UploadTheme 同样在读任何内容之前被拒（theme.Parse 的注释写了推导）：InvalidArgument 写明
// 声明的压缩字节总量与包长，库里不留行。2 秒的上限只用来区分"没有读内容"与"读了"（后者要把 1 MiB 的流解 2000 遍）。
func TestUploadThemeRejectsOverlappingEntriesBeforeReadingThem(t *testing.T) {
	t.Parallel()
	h := newThemeHarness(t)
	before := h.themeRows(t)
	pkg := Overlapping(theme.MaxEntries, 1<<20)
	start := time.Now()
	_, err := h.upload(t, pkg, "")
	elapsed := time.Since(start)
	want := fmt.Sprintf("compressed bytes in total but the archive is only %d bytes", len(pkg))
	if codeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "package: central directory declares") || !strings.Contains(err.Error(), want) {
		t.Fatalf("UploadTheme = %v, want InvalidArgument containing %q", err, want)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("UploadTheme took %v to reject; the bound is checked before any content is read", elapsed)
	}
	if rows := h.themeRows(t); rows != before {
		t.Fatalf("theme, theme_file rows = %v, want %v unchanged", rows, before)
	}
}

// 贴着每条上限的合法包（展开 64 MiB、最大文件 16 MiB、包接近 8 MiB 且压缩字节之和接近包长）经 UploadTheme 装得上，
// 每个普通文件一行。
func TestUploadThemeAcceptsAPackageAtEveryLimit(t *testing.T) {
	t.Parallel()
	h := newThemeHarness(t)
	pkg := AtLimits(t)
	parsed, err := theme.Parse(pkg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := h.upload(t, pkg, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.GetId() != "limits" {
		t.Fatalf("UploadTheme = %v, want theme limits", got)
	}
	if rows, want := h.themeRows(t), [2]int64{1, int64(len(parsed.Files))}; rows != want {
		t.Fatalf("theme, theme_file rows = %v, want %v", rows, want)
	}
}

// 同 ID 的新产物独立保留，安装与启用不可耦合。
func TestUploadThemeExpectIDAndVersions(t *testing.T) {
	t.Parallel()
	h := newThemeHarness(t)
	first, err := h.upload(t, Minimal(t, "a", File("old.js", "old")), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.admin.EnableTheme(t.Context(), connect.NewRequest(&heronv1.EnableThemeRequest{Id: first.Id, Digest: first.Digest})); err != nil {
		t.Fatal(err)
	}
	before := h.themeRows(t)
	if _, err := h.upload(t, Minimal(t, "b"), "a"); codeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("mismatched ID: %v", err)
	}
	if _, err := h.upload(t, Minimal(t, "c"), "c"); codeOf(err) != connect.CodeNotFound {
		t.Fatalf("missing target: %v", err)
	}
	if got := h.themeRows(t); got != before {
		t.Fatalf("rejected upload changed rows: %v", got)
	}
	second, err := h.upload(t, Minimal(t, "a", File("new.js", "new")), "a")
	if err != nil {
		t.Fatal(err)
	}
	if second.Enabled || second.Digest == first.Digest || len(h.themes(t)) != 2 {
		t.Fatalf("installation replaced current: %v", second)
	}
	cur, _, err := h.store.ThemeSelection(t.Context())
	if err != nil || cur.Digest != first.Digest {
		t.Fatalf("selection changed: %v %v", cur, err)
	}
	if _, err := h.admin.EnableTheme(t.Context(), connect.NewRequest(&heronv1.EnableThemeRequest{Id: second.Id, Digest: second.Digest})); err != nil {
		t.Fatal(err)
	}
	cur, prev, err := h.store.ThemeSelection(t.Context())
	if err != nil || cur.Digest != second.Digest || prev.Digest != first.Digest {
		t.Fatalf("switch: %v %v %v", cur, prev, err)
	}
	for _, digest := range []string{cur.Digest, prev.Digest} {
		if _, err := h.admin.DeleteThemeVersion(t.Context(), connect.NewRequest(&heronv1.DeleteThemeVersionRequest{Id: "a", Digest: digest})); codeOf(err) != connect.CodeFailedPrecondition {
			t.Fatalf("delete protected: %v", err)
		}
	}
}

// 同一时刻至多一个上传在校验与入库：一个上传停在入库时，另一个上传立即得到 ResourceExhausted 并说明有上传正在进行；
// 前一个完成后名额归还，被拒的上传同样归还。让第一个上传停在入库：store 的写入由单个写协程按序执行，
// TouchSessionAsync 的回调就在写协程里运行，回调阻塞期间 PutTheme 排在它后面等。探测用的上传带一个不是 zip 的包：
// 拿到名额就在校验时被拒（InvalidArgument），不写库，也就不会自己排进被阻塞的写协程。
// 先等第一个上传占住名额再探测：探测若赶在它前面，会短暂占住名额，让第一个上传反被拒绝。探测带 5 秒期限：名额
// 被占时若改成排队，探测会一直停在信号量上，期限让用例当场以断言失败，而不是拖到 go test 的全局超时。
func TestUploadThemeAdmitsOneAtATime(t *testing.T) {
	t.Parallel()
	h := newThemeHarness(t)
	parked, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unpark := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unpark) // 先于 harness 注册的 store.Close 运行：写协程停着，Close 会一直等它。
	h.store.TouchSessionAsync([32]byte{}, h.clk.Now(), func(error) {
		close(parked)
		<-release
	})
	<-parked
	pkg := Minimal(t, "first")
	first := make(chan error, 1)
	go func() {
		_, err := h.upload(t, pkg, "")
		first <- err
	}()
	for deadline := time.Now().Add(10 * time.Second); len(h.svc.uploading) == 0; time.Sleep(time.Millisecond) {
		select {
		case err := <-first:
			t.Fatalf("first upload returned before taking the upload slot: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("first upload never took the upload slot")
		}
	}
	probe := func() error {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		_, err := h.admin.UploadTheme(ctx, connect.NewRequest(&heronv1.UploadThemeRequest{Package: []byte("not a zip")}))
		return err
	}
	if err := probe(); codeOf(err) != connect.CodeResourceExhausted || !strings.Contains(err.Error(), "另一个主题正在安装") {
		t.Fatalf("upload while another is storing: %v, want ResourceExhausted saying another upload is in progress", err)
	}
	select {
	case err := <-first:
		t.Fatalf("first upload returned while the store writer was parked: %v", err)
	default:
	}
	unpark()
	if err := <-first; err != nil {
		t.Fatalf("first upload: %v", err)
	}
	if err := probe(); codeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("upload after the first finished: %v, want the package judged (InvalidArgument)", err)
	}
	if _, err := h.upload(t, Minimal(t, "second"), ""); err != nil {
		t.Fatalf("upload after a rejected one: %v", err)
	}
}

// 至多 20 个主题；第 21 个 id 被拒且不留行，已装的 id 仍可替换。
func TestUploadThemeLimit(t *testing.T) {
	t.Parallel()
	h := newThemeHarness(t)
	for i := range theme.MaxThemes {
		if _, err := h.upload(t, Minimal(t, fmt.Sprintf("t%02d", i)), ""); err != nil {
			t.Fatal(err)
		}
	}
	before := h.themeRows(t)
	if _, err := h.upload(t, Minimal(t, "t20"), ""); codeOf(err) != connect.CodeResourceExhausted || !strings.Contains(err.Error(), "最多安装 20 个主题") {
		t.Fatalf("21st theme: %v, want ResourceExhausted", err)
	}
	if rows := h.themeRows(t); rows != before {
		t.Fatalf("rejected 21st theme changed rows: %v, want %v", rows, before)
	}
	if _, err := h.upload(t, Minimal(t, "t07"), "t07"); err != nil {
		t.Fatalf("replacing at the limit: %v", err)
	}
}

// 启用至多一个：启用 B 之后 A 不再启用；空 id 不启用任何主题；不存在的 id 被拒且不改状态。
func TestEnableThemeIsExclusive(t *testing.T) {
	t.Parallel()
	h := newThemeHarness(t)
	ctx := t.Context()
	for _, id := range []string{"a", "b"} {
		if _, err := h.upload(t, Minimal(t, id), ""); err != nil {
			t.Fatal(err)
		}
	}
	enable := func(id string) error {
		_, err := h.admin.EnableTheme(ctx, connect.NewRequest(&heronv1.EnableThemeRequest{Id: id, Digest: h.digest(t, id)}))
		return err
	}
	if err := enable("a"); err != nil {
		t.Fatal(err)
	}
	if err := enable("b"); err != nil {
		t.Fatal(err)
	}
	if got := h.enabledThemes(t); !slices.Equal(got, []string{"b"}) {
		t.Fatalf("enabled after enabling b = %v, want [b]", got)
	}
	if err := enable("zz"); codeOf(err) != connect.CodeNotFound {
		t.Fatalf("enabling a missing theme: %v", err)
	}
	if got := h.enabledThemes(t); !slices.Equal(got, []string{"b"}) {
		t.Fatalf("enabled after a failed enable = %v, want [b]", got)
	}
	if err := enable(""); err != nil {
		t.Fatal(err)
	}
	if got := h.enabledThemes(t); len(got) != 0 {
		t.Fatalf("enabled after enabling none = %v", got)
	}
}

// 解码预算装得下满额的包在 JSON 里的 base64 与最坏转义的 expect_id：请求到得了方法体（包的校验给出 InvalidArgument），
// 而不是在解码时被 ResourceExhausted 截下。多一个字节的包同样解码得到，由包的上限拒绝并写明字节数。
func TestUploadThemeBudgetFitsAFullPackage(t *testing.T) {
	t.Parallel()
	h := newThemeHarness(t)
	cookie := map[string][]string{"Cookie": {sessionCookieHeader(t, h)}}
	for _, c := range []struct {
		size int
		want string
	}{
		{theme.MaxPackageBytes, "package: not a readable zip archive"},
		{theme.MaxPackageBytes + 1, "package: 8388609 bytes; at most 8388608"},
	} {
		body, err := json.Marshal(map[string]string{
			"package":   base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xff}, c.size)),
			"expect_id": strings.Repeat("\x01", 32),
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(body) < 4*((c.size+2)/3)+6*32 {
			t.Fatalf("request is %d bytes; the worst case was not constructed", len(body))
		}
		t.Logf("request %d bytes, budget %d", len(body), maxThemeBody)
		got := rawCall(t, h, "UploadTheme", string(body), cookie)
		if got.code != "invalid_argument" || !strings.Contains(got.message, c.want) {
			t.Fatalf("%d-byte package: %+v, want invalid_argument containing %q", c.size, got, c.want)
		}
	}
}

// 解码预算按过程分（settings_budget.go 的 settingsBudget/maxSettingsBody 与 service.go 的 maxThemeBody）。解码先于鉴权，预算也就是匿名请求被读到的上限：
// 按描述符枚举 AdminService 的每个过程，匿名发一个恰比 maxSettingsBody 多一字节的请求——UploadTheme 把它读完、由拦截器
// 给出 unauthenticated，其余过程在解码时就以 resource_exhausted（HTTP 429）拒绝并写明 maxSettingsBody；UploadTheme
// 自己的上限是 maxThemeBody，多一字节同样被拒。1 MiB 的匿名 ListNodes 是这条界要挡的场景：借一个未知字段把请求撑大。
func TestDecodeBudgetIsPerProcedure(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	// body 恰为 n 字节：{"package":"AAAA…"} 对 UploadTheme 是合法的 bytes 字段（A 解出零字节），对其余过程是未知字段；
	// base64 取 4 的倍数个字符，余数用 JSON 允许的前导空白补齐。
	body := func(n int) string {
		const frame = len(`{"package":""}`)
		chars := (n - frame) / 4 * 4
		return strings.Repeat(" ", n-frame-chars) + `{"package":"` + strings.Repeat("A", chars) + `"}`
	}
	settingsOver := body(maxSettingsBody + 1)
	if len(settingsOver) != maxSettingsBody+1 {
		t.Fatalf("request is %d bytes, want %d", len(settingsOver), maxSettingsBody+1)
	}
	refused := func(method string, size int, budget int) {
		t.Helper()
		got := rawCall(t, h, method, body(size), nil)
		if got.status != http.StatusTooManyRequests || got.code != "resource_exhausted" || !strings.Contains(got.message, fmt.Sprintf("larger than configured max %d", budget)) {
			t.Errorf("anonymous %d-byte %s: %+v, want 429 resource_exhausted naming the %d-byte budget", size, method, got, budget)
		}
	}
	svc := adminService()
	var sawUpload bool
	for i := 0; i < svc.Methods().Len(); i++ {
		name := string(svc.Methods().Get(i).Name())
		if name != "UploadTheme" {
			refused(name, maxSettingsBody+1, maxSettingsBody)
			continue
		}
		sawUpload = true
		if got := rawCall(t, h, name, settingsOver, nil); got.status != http.StatusUnauthorized || got.code != "unauthenticated" {
			t.Errorf("anonymous %d-byte UploadTheme: %+v, want it read in full and refused by the interceptor (401 unauthenticated)", len(settingsOver), got)
		}
		refused(name, maxThemeBody+1, maxThemeBody)
	}
	if !sawUpload {
		t.Fatal("AdminService has no UploadTheme method; the enumeration no longer covers the large budget")
	}
	refused("ListNodes", 1<<20, maxSettingsBody)
}
