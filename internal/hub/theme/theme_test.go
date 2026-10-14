package theme_test

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"hash/crc32"
	"io/fs"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/hub/theme"
	. "github.com/xjetry/heron/internal/hub/theme/themetest"
)

func TestParseAcceptsAMinimalPackageWithDirectoriesAndPreview(t *testing.T) {
	pkg := Zip(t,
		Manifest(t, "night-sky", "Night Sky", "2.1.0", "shots/preview.png"),
		File("index.html", "<!doctype html>"),
		Entry{Name: "assets/", Mode: fs.ModeDir | 0o755},
		File("assets/app.js", "console.log(1)"),
		Entry{Name: "shots/preview.png", Content: PNG},
	)
	got, err := theme.Parse(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if want := (theme.Manifest{SDK: theme.SDKVersion, ID: "night-sky", Name: "Night Sky", Version: "2.1.0", Preview: "shots/preview.png"}); got.Manifest != want {
		t.Fatalf("manifest = %+v, want %+v", got.Manifest, want)
	}
	var paths []string
	for _, f := range got.Files {
		paths = append(paths, f.Path)
	}
	if want := "theme.json index.html assets/app.js shots/preview.png"; strings.Join(paths, " ") != want {
		t.Fatalf("files = %v, want %s (directories are not files)", paths, want)
	}
	if string(got.Files[2].Content) != "console.log(1)" {
		t.Fatalf("assets/app.js content = %q", got.Files[2].Content)
	}
	if ct := theme.PreviewContentType(got.Manifest.Preview); ct != "image/png" {
		t.Fatalf("preview content type = %q", ct)
	}
}

// 贴着每条上限的合法包照收：展开合计恰为 64 MiB、最大的文件恰为 16 MiB，各条的压缩字节之和接近包长——压缩字节
// 总量的界是"合计不超过包长"，合法包的合计总在包长以内。
func TestParseAcceptsAPackageAtEveryLimit(t *testing.T) {
	pkg := AtLimits(t)
	zr, err := zip.NewReader(bytes.NewReader(pkg), int64(len(pkg)))
	if err != nil {
		t.Fatal(err)
	}
	var compressed uint64
	for _, f := range zr.File {
		compressed += f.CompressedSize64
	}
	t.Logf("package %d bytes, entries declare %d compressed bytes", len(pkg), compressed)
	if compressed < uint64(len(pkg))-4<<10 {
		t.Fatalf("the fixture's entries declare %d compressed bytes, not within 4 KiB of the %d-byte package; it no longer sits at the limit", compressed, len(pkg))
	}
	got, err := theme.Parse(pkg)
	if err != nil {
		t.Fatal(err)
	}
	var total, largest int
	for _, f := range got.Files {
		total += len(f.Content)
		largest = max(largest, len(f.Content))
	}
	if total != theme.MaxTotalBytes || largest != theme.MaxFileBytes {
		t.Fatalf("expanded %d bytes, largest file %d; want exactly %d and %d", total, largest, theme.MaxTotalBytes, theme.MaxFileBytes)
	}
}

// 读入的压缩字节总量以包长为界（Parse 的注释写了推导）。2000 条记录重叠地指向同一段 1 MiB、解出 0 字节的 deflate
// 流：展开总量的守卫全部放行，没有这条界时 Parse 要把这段流解 2000 遍才发现缺 index.html。这条界在读任何内容之前
// 判完，所以拒绝几乎不花时间；1 秒的上限只用来区分"没有读内容"与"读了"，不是性能要求。
func TestParseRejectsOverlappingEntriesBeforeReadingThem(t *testing.T) {
	pkg := Overlapping(theme.MaxEntries, 1<<20)
	zr, err := zip.NewReader(bytes.NewReader(pkg), int64(len(pkg)))
	if err != nil {
		t.Fatal(err)
	}
	var compressed, declared uint64
	for _, f := range zr.File {
		compressed += f.CompressedSize64
		declared += f.UncompressedSize64
	}
	if len(zr.File) != theme.MaxEntries || declared != 0 {
		t.Fatalf("fixture has %d entries declaring %d bytes uncompressed; want %d declaring 0", len(zr.File), declared, theme.MaxEntries)
	}
	start := time.Now()
	_, err = theme.Parse(pkg)
	elapsed := time.Since(start)
	want := fmt.Sprintf("package: central directory declares %d compressed bytes in total but the archive is only %d bytes", compressed, len(pkg))
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Parse error = %v, want it to contain %q", err, want)
	}
	if elapsed > time.Second {
		t.Fatalf("Parse took %v to reject; the bound is checked before any content is read", elapsed)
	}
}

// 单条的压缩大小先与包长比，合计才不会回绕：两条分别声明 2^64-16 与 32 字节，直接相加回绕成 16，看起来远小于包长。
func TestParseRejectsACompressedSizeLargerThanThePackage(t *testing.T) {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, e := range []Entry{Manifest(t, "ok", "Ok", "1", ""), File("index.html", "x")} {
		fw, _ := w.Create(e.Name)
		fw.Write(e.Content)
	}
	for _, h := range []zip.FileHeader{
		{Name: "a", Method: zip.Store, CRC32: crc32.ChecksumIEEE([]byte("hello")), CompressedSize64: math.MaxUint64 - 15, UncompressedSize64: 5},
		{Name: "b", Method: zip.Store, CRC32: crc32.ChecksumIEEE([]byte("hello")), CompressedSize64: 32, UncompressedSize64: 5},
	} {
		raw, err := w.CreateRaw(&h)
		if err != nil {
			t.Fatal(err)
		}
		raw.Write([]byte("hello"))
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf(`entry "a": central directory declares %d compressed bytes but the archive is only %d bytes`, uint64(math.MaxUint64-15), buf.Len())
	if _, err := theme.Parse(buf.Bytes()); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Parse error = %v, want it to contain %q", err, want)
	}
}

// 每条拒绝都要点名字段（或条目）与原因；wantField 与 wantReason 是错误里必须出现的两段。
func TestParseRejects(t *testing.T) {
	zeros := func(n int) []byte { return make([]byte, n) }
	many := func(n int) []Entry {
		out := []Entry{Manifest(t, "many", "Many", "1", ""), File("index.html", "x")}
		for i := len(out); i < n; i++ {
			out = append(out, File(fmt.Sprintf("f%04d.txt", i), "x"))
		}
		return out
	}
	base := func(extra ...Entry) []Entry {
		return append([]Entry{Manifest(t, "ok", "Ok", "1", ""), File("index.html", "x")}, extra...)
	}
	for _, c := range []struct {
		name       string
		pkg        []byte
		wantField  string
		wantReason string
	}{
		{"symlink", Zip(t, base(Entry{Name: "link", Content: []byte("/etc/passwd"), Mode: fs.ModeSymlink | 0o777})...), `entry "link"`, "symbolic link"},
		{"device", Zip(t, base(Entry{Name: "dev", Mode: fs.ModeDevice | 0o600})...), `entry "dev"`, "special file"},
		{"fifo", Zip(t, base(Entry{Name: "pipe", Mode: fs.ModeNamedPipe | 0o600})...), `entry "pipe"`, "special file"},
		{"parent", Zip(t, base(File("../x", "x"))...), `entry "../x"`, `".." segment`},
		{"nested parent", Zip(t, base(File("a/../../x", "x"))...), `entry "a/../../x"`, `".." segment`},
		{"absolute", Zip(t, base(File("/etc/x", "x"))...), `entry "/etc/x"`, "absolute path"},
		{"backslash", Zip(t, base(File(`a\..\x`, "x"))...), `entry "a\\..\\x"`, "backslash"},
		{"empty segment", Zip(t, base(File("a//x", "x"))...), `entry "a//x"`, "empty path segment"},
		{"dot segment", Zip(t, base(File("__MACOSX/._index.html", "x"))...), `entry "__MACOSX/._index.html"`, "starts with a dot"},
		{"leading ./", Zip(t, base(File("./x", "x"))...), `entry "./x"`, "starts with a dot"},
		{"duplicate", Zip(t, base(File("index.html", "again"))...), `entry "index.html"`, "more than once"},
		{"entries 2001", Zip(t, many(theme.MaxEntries+1)...), "package", "2001 entries; at most 2000"},
		{"declared total", Zip(t, base(
			Entry{Name: "a", Content: zeros(13 << 20)}, Entry{Name: "b", Content: zeros(13 << 20)}, Entry{Name: "c", Content: zeros(13 << 20)},
			Entry{Name: "d", Content: zeros(13 << 20)}, Entry{Name: "e", Content: zeros(13 << 20)})...),
			"package", "central directory declares 68157"},
		{"declared file", Zip(t, base(Entry{Name: "big", Content: zeros(theme.MaxFileBytes + 1)})...), `entry "big"`, "declares 16777217 bytes uncompressed"},
		// 中央目录每条只声明 1 KiB，实际每条展开 15 MiB、合计 75 MiB：只信声明就会展开超过 64 MiB。
		{"lying central directory", Zip(t, base(
			Entry{Name: "a", Content: zeros(15 << 20), Declared: 1 << 10}, Entry{Name: "b", Content: zeros(15 << 20), Declared: 1 << 10},
			Entry{Name: "c", Content: zeros(15 << 20), Declared: 1 << 10}, Entry{Name: "d", Content: zeros(15 << 20), Declared: 1 << 10},
			Entry{Name: "e", Content: zeros(15 << 20), Declared: 1 << 10})...),
			`entry "a"`, "expands past the 1024 bytes the central directory declares"},
		{"no manifest", Zip(t, File("index.html", "x")), "package", "no theme.json"},
		{"no index", Zip(t, Manifest(t, "ok", "Ok", "1", "")), "package", "no index.html"},
		{"manifest only in a subdirectory", Zip(t, File("index.html", "x"), File("sub/theme.json", `{"id":"ok","name":"Ok","version":"1"}`)), "package", "no theme.json"},
		{"builtin", Zip(t, Manifest(t, "builtin", "B", "1", ""), File("index.html", "x")), "theme.json id", `"builtin" is reserved`},
		{"bad id", Zip(t, Manifest(t, "Night_Sky", "B", "1", ""), File("index.html", "x")), "theme.json id", "must match [a-z0-9-]{1,32}"},
		{"long id", Zip(t, Manifest(t, strings.Repeat("a", 33), "B", "1", ""), File("index.html", "x")), "theme.json id", "must match"},
		{"empty name", Zip(t, Manifest(t, "ok", " ", "1", ""), File("index.html", "x")), "theme.json name", "required"},
		{"empty version", Zip(t, Manifest(t, "ok", "Ok", "", ""), File("index.html", "x")), "theme.json version", "required"},
		{"unknown field", Zip(t, File("theme.json", `{"id":"ok","name":"Ok","version":"1","config":{}}`), File("index.html", "x")), "theme.json", `unknown field "config"`},
		{"preview missing", Zip(t, Manifest(t, "ok", "Ok", "1", "p.png"), File("index.html", "x")), "theme.json preview", `"p.png" is not a file in the package`},
		{"preview type", Zip(t, Manifest(t, "ok", "Ok", "1", "p.svg"), File("index.html", "x"), File("p.svg", "<svg/>")), "theme.json preview", "must be a .png"},
		{"preview content", Zip(t, Manifest(t, "ok", "Ok", "1", "p.png"), File("index.html", "x"), File("p.png", "<html>")), "theme.json preview", "not image/png"},
		{"preview outside", Zip(t, Manifest(t, "ok", "Ok", "1", "../p.png"), File("index.html", "x")), "theme.json preview", "inside the package"},
		{"too large", bytes.Repeat([]byte{0}, theme.MaxPackageBytes+1), "package", "at most 8388608"},
		{"not a zip", []byte("hello"), "package", "not a readable zip archive"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := theme.Parse(c.pkg)
			var te *theme.Error
			if !errors.As(err, &te) {
				t.Fatalf("Parse = %+v, %v; want a *theme.Error", got, err)
			}
			if te.Field != c.wantField || !strings.Contains(te.Reason, c.wantReason) {
				t.Fatalf("error = %q; want field %q and reason containing %q", err, c.wantField, c.wantReason)
			}
		})
	}
}

// 按外部属性拒绝的条目：名字与属性不一致（不同的解包工具会还原出不同的东西）、Windows 的链接与设备位、
// archive/zip 的 Mode 会读成普通文件的未知 Unix 类型位。
func TestParseRejectsByEntryAttributes(t *testing.T) {
	for _, c := range []struct {
		name  string
		entry func(*zip.FileHeader)
		want  string
	}{
		{"dir mode without slash", func(h *zip.FileHeader) { h.Name = "d"; h.SetMode(fs.ModeDir | 0o755) }, "a directory but the name does not end in /"},
		{"unix dir mode without slash", func(h *zip.FileHeader) { h.Name = "d"; h.CreatorVersion = 3 << 8; h.ExternalAttrs = 0o040755 << 16 }, "Unix mode 040755 marks a directory"},
		{"file mode with slash", func(h *zip.FileHeader) { h.Name = "d/"; h.CreatorVersion = 3 << 8; h.ExternalAttrs = 0o100644 << 16 }, "marks a regular file"},
		{"windows reparse point", func(h *zip.FileHeader) { h.Name = "l"; h.ExternalAttrs = 0x400 }, "reparse point"},
		{"windows device", func(h *zip.FileHeader) { h.Name = "l"; h.ExternalAttrs = 0x40 }, "device"},
		{"unknown unix type", func(h *zip.FileHeader) { h.Name = "w"; h.CreatorVersion = 3 << 8; h.ExternalAttrs = 0o160644 << 16 }, "Unix mode 0160644 marks a special file"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			w := zip.NewWriter(&buf)
			for _, e := range []Entry{Manifest(t, "ok", "Ok", "1", ""), File("index.html", "x")} {
				fw, _ := w.Create(e.Name)
				fw.Write(e.Content)
			}
			h := &zip.FileHeader{}
			c.entry(h)
			if _, err := w.CreateHeader(h); err != nil {
				t.Fatal(err)
			}
			w.Close()
			_, err := theme.Parse(buf.Bytes())
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Parse error = %v, want it to mention %q", err, c.want)
			}
		})
	}
}

// Unix 类型位不看创建者（checkKind 的注释写了为什么）：每个创建者上，0120777 都按符号链接拒绝，0100644 的普通文件与
// 高 16 位为 0 的条目都照收。创建者覆盖 FAT（0）、实测 unzip 会还原出符号链接的 2、3、5、16、30 与不会的 10、19。
func TestParseJudgesUnixTypeBitsWhateverTheCreator(t *testing.T) {
	for _, creator := range []uint16{0, 2, 3, 5, 10, 16, 19, 30} {
		for _, c := range []struct {
			name  string
			attrs uint32
			want  string // 空串表示照收
		}{
			{"symlink", 0o120777 << 16, `entry "e": Unix mode 0120777 marks a symbolic link`},
			{"regular", 0o100644 << 16, ""},
			{"no unix mode", 0, ""},
		} {
			t.Run(fmt.Sprintf("creator %d %s", creator, c.name), func(t *testing.T) {
				var buf bytes.Buffer
				w := zip.NewWriter(&buf)
				for _, e := range []Entry{Manifest(t, "ok", "Ok", "1", ""), File("index.html", "x")} {
					fw, _ := w.Create(e.Name)
					fw.Write(e.Content)
				}
				fw, err := w.CreateHeader(&zip.FileHeader{Name: "e", CreatorVersion: creator << 8, ExternalAttrs: c.attrs})
				if err != nil {
					t.Fatal(err)
				}
				fw.Write([]byte("/etc/passwd"))
				if err := w.Close(); err != nil {
					t.Fatal(err)
				}
				zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
				if err != nil {
					t.Fatal(err)
				}
				if f := zr.File[2]; f.CreatorVersion>>8 != creator || f.ExternalAttrs != c.attrs {
					t.Fatalf("fixture wrote creator %d, attributes %#o; want %d, %#o", f.CreatorVersion>>8, f.ExternalAttrs, creator, c.attrs)
				}
				_, err = theme.Parse(buf.Bytes())
				switch {
				case c.want == "" && err != nil:
					t.Fatalf("Parse error = %v, want the entry accepted", err)
				case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
					t.Fatalf("Parse error = %v, want it to contain %q", err, c.want)
				}
			})
		}
	}
}

// 条目内容与中央目录的其余声明不符：CRC、压缩方式、展开得比声明少、目录条目声明了内容。
func TestParseRejectsEntriesThatContradictTheCentralDirectory(t *testing.T) {
	for _, c := range []struct {
		name   string
		header zip.FileHeader
		raw    []byte
		want   string
	}{
		{"crc", zip.FileHeader{Name: "a.txt", Method: zip.Store, CRC32: 1, CompressedSize64: 5, UncompressedSize64: 5}, []byte("hello"), `entry "a.txt": CRC-32`},
		{"method", zip.FileHeader{Name: "a.txt", Method: 12, CompressedSize64: 5, UncompressedSize64: 5}, []byte("hello"), `entry "a.txt": compression method 12 is not supported`},
		{"short", zip.FileHeader{Name: "a.txt", Method: zip.Store, CompressedSize64: 5, UncompressedSize64: 9}, []byte("hello"), `entry "a.txt": expands to 5 bytes but the central directory declares 9`},
		{"directory content", zip.FileHeader{Name: "d/", Method: zip.Store, CompressedSize64: 5, UncompressedSize64: 5}, []byte("hello"), `entry "d": directory entry declares 5 bytes`},
	} {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			w := zip.NewWriter(&buf)
			for _, e := range []Entry{Manifest(t, "ok", "Ok", "1", ""), File("index.html", "x")} {
				fw, _ := w.Create(e.Name)
				fw.Write(e.Content)
			}
			h := c.header
			if c.header.CRC32 == 0 {
				h.CRC32 = crc32.ChecksumIEEE(c.raw)
			}
			raw, err := w.CreateRaw(&h)
			if err != nil {
				t.Fatal(err)
			}
			raw.Write(c.raw)
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			_, err = theme.Parse(buf.Bytes())
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Parse error = %v, want it to contain %q", err, c.want)
			}
		})
	}
}

// GODEBUG=zipinsecurepath=0 时 zip.NewReader 对含 .. 的名字随可用的 Reader 返回 ErrInsecurePath；拒绝仍由 checkPath 给出，
// 错误点名条目，而不是一句"不是可读的 zip"。
func TestParseNamesTheEntryUnderZipInsecurePath(t *testing.T) {
	t.Setenv("GODEBUG", "zipinsecurepath=0")
	_, err := theme.Parse(Zip(t, Manifest(t, "ok", "Ok", "1", ""), File("index.html", "x"), File("../x", "x")))
	if err == nil || !strings.Contains(err.Error(), `entry "../x": ".." segment`) {
		t.Fatalf("Parse error = %v, want the entry named", err)
	}
}
