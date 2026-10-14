// Package theme 校验公开页主题包：一个 zip，展开后是只调 PublicService 的静态前端产物。
//
// 包由面板安装、存进库、在同域沙箱中托管。文件处理是需要长期维护的攻击面，所以每一面各配一条显式守卫，而不是靠
// "没人上传恶意主题"维持；任一守卫不满足即拒绝整包，不跳过个别条目——跳过会让"装上了"与"装对了"不可分辨。
// Parse 只做纯校验；GitHubClient 单独限制公开发行版的下载目标。入库与执行准入由调用方负责。
package theme

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"path"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// SDKVersion 是主题与可信容器之间的公开数据协议版本；零值表示仅可归档的旧包。
	SDKVersion = 1
	// MaxPackageBytes 是上传的 zip 本身的上限。包经单个 Connect unary 请求以 bytes 送达，UploadTheme 的解码预算
	// 由它推出（api 的 maxThemeBody）。
	MaxPackageBytes = 8 << 20
	// 以下三项给展开设界，与上传体积独立：deflate 的压缩比可达约千倍，8 MiB 的包能展开出数 GiB，压缩比藏在上传
	// 体积上限后面，所以展开总量必须有自己的界。
	MaxEntries    = 2000
	MaxTotalBytes = 64 << 20
	MaxFileBytes  = 16 << 20
	// MaxThemes 是库里主题身份数的上限；同一 id 的多个产物共用一个名额，版本数由存储层另行限制。
	MaxThemes = 20

	// ManifestPath 是清单在包根的路径。
	ManifestPath = "theme.json"
	// IndexPath 是沙箱文档入口；缺了它，启用后公开页是一张错误页。
	IndexPath = "index.html"
	// BuiltinID 是内置公开页的标识，主题不得自报。
	BuiltinID = "builtin"
)

// 清单里 name 与 version 的上限：它们回显在面板的主题列表里，不应让一个 16 MiB 的字段撑满列表响应。
const (
	maxNameRunes    = 64
	maxVersionRunes = 64
)

var idPattern = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// ValidID 是可安装主题的标识口径；内置公开页不对应可上传或可删除的主题包。
func ValidID(id string) bool { return id != BuiltinID && idPattern.MatchString(id) }

// Error 是一次拒绝：Field 指出违反约束的位置（package、条目路径、清单字段），Reason 说明约束与实际取值。
type Error struct {
	Field  string
	Reason string
}

func (e *Error) Error() string { return e.Field + ": " + e.Reason }

func reject(field, format string, args ...any) error {
	return &Error{Field: field, Reason: fmt.Sprintf(format, args...)}
}

// Manifest 是包根 theme.json 的内容。Preview 为空表示没有预览图。
type Manifest struct {
	SDK     int    `json:"sdk,omitempty"`
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version string `json:"version"`
	Preview string `json:"preview,omitempty"`
}

// CheckExecutable 是所有执行入口共用的准入，不把旧包能解析、能恢复误当成可以运行。
func CheckExecutable(sdk int) error {
	if sdk != SDKVersion {
		return reject("theme.json sdk", "主题需要适配 SDK %d（当前 %d）；旧包仅可归档", SDKVersion, sdk)
	}
	return nil
}

// File 是包里的一个普通文件；Path 是规范形态的包内路径，也是它入库后的键。
type File struct {
	Path    string
	Content []byte
}

// Package 是校验通过的主题包：清单与全部普通文件（按包内出现顺序，目录条目不在其中）。
type Package struct {
	Manifest Manifest
	Files    []File
}

// Parse 校验 pkg 并展开。任一守卫不满足即返回 *Error，不返回部分结果。
//
// 顺序有意为之：先只读中央目录判出展开的上界（条目数、每条与总计的声明大小、路径、条目类型）与读入的压缩字节总量的
// 上界，全部通过后才读任何条目的内容；读内容时再按实际展开的字节逐条核对，不信任中央目录的声明。
//
// 压缩字节总量的界：expand 读一条的压缩数据以该条声明的压缩大小为界（OpenRaw 按它截取），解压的 CPU 随读入的压缩
// 字节增长，而展开的界（declared 与 expand 的逐条核对）只约束产出。格式正确的包里各条的压缩数据是包内互不相交的
// 片段，合计必小于包长；合计超过包长，只能是多条记录指向同一段数据（重叠）或声明了包里没有的字节。重叠时读者每打开
// 一条都把同一段从头解一遍：一段只解出 0 字节的 deflate 流不占展开额度，MaxEntries 条记录就把它解 MaxEntries 遍。
// 要求合计不超过包长，Parse 读入的压缩字节总量就不超过包本身。
func Parse(pkg []byte) (*Package, error) {
	if len(pkg) > MaxPackageBytes {
		return nil, reject("package", "%d bytes; at most %d (8 MiB)", len(pkg), MaxPackageBytes)
	}
	zr, err := zip.NewReader(bytes.NewReader(pkg), int64(len(pkg)))
	// ErrInsecurePath（GODEBUG=zipinsecurepath=0 时）随一个可用的 Reader 返回；路径由下面的 checkPath 逐条裁决，
	// 错误信息点名条目，所以这里不把它当作格式错误。
	if err != nil && !errors.Is(err, zip.ErrInsecurePath) {
		return nil, reject("package", "not a readable zip archive: %v", err)
	}
	if n := len(zr.File); n > MaxEntries {
		return nil, reject("package", "%d entries; at most %d", n, MaxEntries)
	}

	var declared, compressed uint64
	seen := make(map[string]bool, len(zr.File))
	var files []*zip.File
	var paths []string
	for _, f := range zr.File {
		p, dir, err := checkPath(f.Name)
		if err != nil {
			return nil, err
		}
		if err := checkKind(f, p, dir); err != nil {
			return nil, err
		}
		if seen[p] {
			return nil, reject(entryField(p), "appears more than once in the archive; which copy wins would depend on the reader")
		}
		seen[p] = true
		// 单条先与包长比：zip.NewReader 不核对压缩大小（Go 1.27.1 实测，接近 2^64 的声明也照收）。每条界在包长
		// （≤ MaxPackageBytes）以内、至多 MaxEntries 条，合计才不会回绕 uint64。目录条目也计入：它们的压缩数据同样
		// 是包内互不相交的片段。
		if f.CompressedSize64 > uint64(len(pkg)) {
			return nil, reject(entryField(p), "central directory declares %d compressed bytes but the archive is only %d bytes", f.CompressedSize64, len(pkg))
		}
		compressed += f.CompressedSize64
		if dir {
			// 目录条目不入库、不读内容；声明了内容的目录条目是自相矛盾的包。
			if f.UncompressedSize64 != 0 {
				return nil, reject(entryField(p), "directory entry declares %d bytes of content", f.UncompressedSize64)
			}
			continue
		}
		if f.UncompressedSize64 > MaxFileBytes {
			return nil, reject(entryField(p), "central directory declares %d bytes uncompressed; at most %d (16 MiB) per file", f.UncompressedSize64, MaxFileBytes)
		}
		// 每项不超过 MaxFileBytes、至多 MaxEntries 项，累加不会溢出 uint64。
		declared += f.UncompressedSize64
		files = append(files, f)
		paths = append(paths, p)
	}
	if declared > MaxTotalBytes {
		return nil, reject("package", "central directory declares %d bytes uncompressed in total; at most %d (64 MiB)", declared, MaxTotalBytes)
	}
	if compressed > uint64(len(pkg)) {
		return nil, reject("package", "central directory declares %d compressed bytes in total but the archive is only %d bytes; entries overlap or claim data the archive does not hold", compressed, len(pkg))
	}

	out := &Package{Files: make([]File, 0, len(files))}
	var expanded uint64
	for i, f := range files {
		content, err := expand(f, paths[i], MaxTotalBytes-expanded)
		if err != nil {
			return nil, err
		}
		expanded += uint64(len(content))
		out.Files = append(out.Files, File{Path: paths[i], Content: content})
	}

	byPath := make(map[string][]byte, len(out.Files))
	for _, f := range out.Files {
		byPath[f.Path] = f.Content
	}
	if _, ok := byPath[IndexPath]; !ok {
		return nil, reject("package", "no %s at the package root; the theme origin falls back to it for every page, so without it the enabled theme serves only errors", IndexPath)
	}
	m, err := parseManifest(byPath)
	if err != nil {
		return nil, err
	}
	out.Manifest = m
	return out, nil
}

func entryField(p string) string { return fmt.Sprintf("entry %q", p) }

// checkPath 把条目名规范化成包内路径并判定是否是目录条目（名字以 / 结尾）。只接受已经是规范形态的名字：入库后
// 路径就是 theme_file 的包内键，也是摘要目录下的相对 URL；非规范的名字（..、绝对路径、空段）在规范化时会落到包外
// 或另一个键上——`../` 在键空间里仍能构造出对其他主题键的遮蔽。反斜杠拒绝：Windows 工具把它当分隔符，
// 同一个名字在不同读者那里是不同的路径。以 . 开头的段与静态服务同一口径（web.hidden）：它们永远不会被服务，
// 包里带着它们多半是 .git、.env 这类误打进来的东西，拒绝比静默存一份永远读不到的内容更早暴露问题。
// 控制字符与非法 UTF-8 拒绝：路径会出现在面板、日志与 URL 里。
func checkPath(name string) (p string, dir bool, err error) {
	field := fmt.Sprintf("entry %q", name)
	if !utf8.ValidString(name) {
		return "", false, reject(field, "name is not valid UTF-8")
	}
	if strings.ContainsFunc(name, unicode.IsControl) {
		return "", false, reject(field, "name contains control characters")
	}
	if strings.Contains(name, `\`) {
		return "", false, reject(field, `name contains a backslash; zip entry names separate directories with "/"`)
	}
	if strings.HasPrefix(name, "/") {
		return "", false, reject(field, "absolute path; entry names must be relative to the package root")
	}
	p, dir = strings.CutSuffix(name, "/")
	if p == "" {
		return "", false, reject(field, "empty path")
	}
	for _, seg := range strings.Split(p, "/") {
		switch {
		case seg == "":
			return "", false, reject(field, `empty path segment ("//")`)
		case seg == "..":
			return "", false, reject(field, `".." segment; entries must stay inside the package`)
		case strings.HasPrefix(seg, "."):
			return "", false, reject(field, "segment %q starts with a dot; dot-files are never served (.git, .env, .DS_Store, __MACOSX/._*), remove them from the package", seg)
		}
	}
	return p, dir, nil
}

// zip 外部属性的两种口径：高 16 位是 Unix 的 st_mode，低 16 位是 MS-DOS 属性。两部分都可能是 0（archive/zip 不经
// SetMode 时整体为 0），为 0 时不说明条目类型，按名字判。
const (
	unixTypeMask = 0o170000
	unixRegular  = 0o100000
	unixDir      = 0o040000
	unixSymlink  = 0o120000

	dosDirectory    = 0x10
	dosDevice       = 0x40
	dosReparsePoint = 0x400 // Windows 的符号链接与挂载点
)

// checkKind 只接受普通文件与目录。路径检查看的是条目名，看不见链接指向：符号链接条目的内容是目标路径，被解包工具
// 还原时可以指向包外；设备与 FIFO 条目同理不是"一段内容"。按白名单判定：Unix 类型位只认普通文件、目录与"未记录"
// （0，按名字判），其余一律拒绝；archive/zip 的 FileHeader.Mode 把不认识的类型位（如 0160000）读成普通文件
// （Go 1.27.1 实测），所以不用它。zip 的外部属性没有硬链接这一类型：Info-ZIP 3.0 把硬链接存成带内容的普通文件
// （实测），这样的条目就是普通文件。
// Unix 类型位对每个条目都判，不看创建者：解包工具对哪些创建者按 Unix 类型位还原各不相同，macOS 自带的 Info-ZIP
// unzip 6.00 对创建者 2、3、5、16、30 的 0120777 条目都还原出真符号链接，对 0、10、19 还原成普通文件（实测）；
// 只在某几个创建者上判，就会放进另一些创建者的链接条目。不写外部属性的条目（archive/zip 不经 SetMode 时，文件与
// 目录条目的外部属性整体为 0，Go 1.27.1 实测）高 16 位为 0，类型位落在"未记录"，按名字判。
// 名字与属性必须一致：名字以 / 结尾而属性说是普通文件（或反过来），不同的解包工具会还原出不同的东西。
func checkKind(f *zip.File, p string, dir bool) error {
	field := entryField(p)
	dos := f.ExternalAttrs & 0xffff
	if dos&dosReparsePoint != 0 {
		return reject(field, "MS-DOS attributes %#x mark a symbolic link or reparse point; only regular files and directories are accepted", dos)
	}
	if dos&dosDevice != 0 {
		return reject(field, "MS-DOS attributes %#x mark a device; only regular files and directories are accepted", dos)
	}
	if dos&dosDirectory != 0 && !dir {
		return reject(field, "MS-DOS attributes mark a directory but the name does not end in /")
	}
	mode := f.ExternalAttrs >> 16
	switch mode & unixTypeMask {
	case 0:
	case unixRegular:
		if dir {
			return reject(field, "name ends in / but the Unix mode %#o marks a regular file", mode)
		}
	case unixDir:
		if !dir {
			return reject(field, "Unix mode %#o marks a directory but the name does not end in /", mode)
		}
	case unixSymlink:
		return reject(field, "Unix mode %#o marks a symbolic link; only regular files and directories are accepted", mode)
	default:
		return reject(field, "Unix mode %#o marks a special file (device, FIFO or socket); only regular files and directories are accepted", mode)
	}
	return nil
}

// expand 读出一个普通文件条目的内容。不经 zip.File.Open：它按中央目录的声明截断与核对，展开的界就成了"信任声明"
// 的推论；这里经 OpenRaw 自己解压，读取量以 min(声明大小, 剩余总额度) + 1 为界，逐条核对实际字节数与声明相等、
// 累计不超过 MaxTotalBytes，并核对 CRC——中央目录撒谎（声明 1 KiB、实际展开出几十 MiB）时最多多读一个字节就停下。
func expand(f *zip.File, p string, remaining uint64) ([]byte, error) {
	field := entryField(p)
	raw, err := f.OpenRaw()
	if err != nil {
		return nil, reject(field, "unreadable local header: %v", err)
	}
	var src io.Reader
	switch f.Method {
	case zip.Store:
		src = raw
	case zip.Deflate:
		fr := flate.NewReader(raw)
		defer fr.Close()
		src = fr
	default:
		return nil, reject(field, "compression method %d is not supported; use store (0) or deflate (8)", f.Method)
	}
	limit := min(f.UncompressedSize64, remaining)
	content, err := io.ReadAll(io.LimitReader(src, int64(limit)+1))
	if err != nil {
		return nil, reject(field, "corrupt compressed data: %v", err)
	}
	n := uint64(len(content))
	if n > remaining {
		return nil, reject("package", "expands past %d bytes (64 MiB) in total at entry %q", MaxTotalBytes, p)
	}
	if n != f.UncompressedSize64 {
		if n > f.UncompressedSize64 {
			return nil, reject(field, "expands past the %d bytes the central directory declares", f.UncompressedSize64)
		}
		return nil, reject(field, "expands to %d bytes but the central directory declares %d", n, f.UncompressedSize64)
	}
	if sum := crc32.ChecksumIEEE(content); sum != f.CRC32 {
		return nil, reject(field, "CRC-32 %08x does not match the declared %08x; the archive is corrupt", sum, f.CRC32)
	}
	return content, nil
}

// previewTypes 是预览图允许的扩展名与它必须嗅探出的内容类型。GetThemePreview 按它回报 content type；内容也必须
// 真是那种图片，否则面板按图片类型拿到的是别的东西（HTML、SVG 里可以带脚本）。
var previewTypes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".webp": "image/webp",
}

// PreviewContentType 是清单 preview 路径对应的内容类型；经 Parse 核对过的 preview 路径都在表内，表外的得到空串。
func PreviewContentType(p string) string { return previewTypes[path.Ext(p)] }

func parseManifest(files map[string][]byte) (Manifest, error) {
	raw, ok := files[ManifestPath]
	if !ok {
		return Manifest{}, reject("package", "no %s at the package root", ManifestPath)
	}
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(raw))
	// 未知字段拒绝：主题不声明配置项（外观由 GetSite 下发），写了 "config" 之类却被静默忽略，作者会以为它生效了。
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, reject(ManifestPath, "not a JSON object with string fields id, name, version and optional preview: %v", err)
	}
	if dec.More() {
		return Manifest{}, reject(ManifestPath, "trailing data after the JSON object")
	}
	if m.ID == BuiltinID {
		return Manifest{}, reject(ManifestPath+" id", "%q is reserved for the built-in public page", BuiltinID)
	}
	if !ValidID(m.ID) {
		return Manifest{}, reject(ManifestPath+" id", "must match [a-z0-9-]{1,32}; got %q", m.ID)
	}
	if err := checkLabel(ManifestPath+" name", m.Name, maxNameRunes); err != nil {
		return Manifest{}, err
	}
	if err := checkLabel(ManifestPath+" version", m.Version, maxVersionRunes); err != nil {
		return Manifest{}, err
	}
	if m.Preview != "" {
		field := ManifestPath + " preview"
		p, dir, err := checkPath(m.Preview)
		if err != nil || dir || p != m.Preview {
			return Manifest{}, reject(field, "must be the path of a file inside the package, relative to its root; got %q", m.Preview)
		}
		want, ok := previewTypes[path.Ext(p)]
		if !ok {
			return Manifest{}, reject(field, "must be a .png, .jpg, .jpeg or .webp file; got %q", m.Preview)
		}
		content, ok := files[p]
		if !ok {
			return Manifest{}, reject(field, "%q is not a file in the package", m.Preview)
		}
		if got := http.DetectContentType(content); got != want {
			return Manifest{}, reject(field, "%q content is %s, not %s", m.Preview, got, want)
		}
	}
	return m, nil
}

// checkLabel：去掉首尾空白后非空、不超过 max 个字符、不含控制字符。原样保存（不修剪）：清单是作者写的文件，
// 存下的与作者写的不同会让人无从对照。
func checkLabel(field, v string, max int) error {
	if strings.TrimSpace(v) == "" {
		return reject(field, "required; got %q", v)
	}
	if strings.ContainsFunc(v, unicode.IsControl) {
		return reject(field, "must not contain control characters; got %q", v)
	}
	if n := utf8.RuneCountInString(v); n > max {
		return reject(field, "at most %d characters; got %d", max, n)
	}
	return nil
}
