// Package themetest 用 Go 生成主题包夹具，供 theme 与 api 的测试共用：夹具随代码生成，不入库二进制文件，
// 每个用例需要的畸形（链接条目、撒谎的中央目录）都能在这里精确构造。
package themetest

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"math/rand/v2"
	"testing"

	"github.com/xjetry/heron/internal/hub/theme"
)

// PNG 以 PNG 签名开头，http.DetectContentType 把它嗅探为 image/png。
var PNG = append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...)

// Entry 是一个 zip 条目。Mode 非零时经 SetMode 写入外部属性（符号链接、设备等）；Declared 非零时按原始数据写入，
// 中央目录声明的未压缩大小取 Declared 而不是内容的真实长度——撒谎的中央目录。
type Entry struct {
	Name     string
	Content  []byte
	Mode     fs.FileMode
	Declared uint64
	// Method 为 zip.Store 时不压缩；默认 deflate。
	Method uint16
}

// File 是一个带内容的普通文件条目。
func File(name, content string) Entry { return Entry{Name: name, Content: []byte(content)} }

// Manifest 生成 theme.json 条目；preview 为空时不写该字段。
func Manifest(t testing.TB, id, name, version, preview string) Entry {
	t.Helper()
	m := map[string]any{"id": id, "name": name, "version": version, "sdk": theme.SDKVersion}
	if preview != "" {
		m["preview"] = preview
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return Entry{Name: "theme.json", Content: b}
}

// Minimal 是最小的合法主题：清单加 index.html，extra 追加在后面。
func Minimal(t testing.TB, id string, extra ...Entry) []byte {
	t.Helper()
	return Zip(t, append([]Entry{Manifest(t, id, "Theme "+id, "1.0.0", ""), File("index.html", "<!doctype html>"+id)}, extra...)...)
}

// Zip 按顺序写出条目。
func Zip(t testing.TB, entries ...Entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	// 最快档：大夹具是几十 MiB 的零字节，任何档位都压得很小，高档位只多花时间。
	w.RegisterCompressor(zip.Deflate, func(out io.Writer) (io.WriteCloser, error) { return flate.NewWriter(out, flate.BestSpeed) })
	for _, e := range entries {
		method := e.Method
		if method == 0 && e.Content != nil {
			method = zip.Deflate
		}
		if e.Declared != 0 {
			var comp bytes.Buffer
			fw, err := flate.NewWriter(&comp, flate.BestSpeed)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := fw.Write(e.Content); err != nil {
				t.Fatal(err)
			}
			if err := fw.Close(); err != nil {
				t.Fatal(err)
			}
			h := &zip.FileHeader{Name: e.Name, Method: zip.Deflate, CRC32: crc32.ChecksumIEEE(e.Content),
				CompressedSize64: uint64(comp.Len()), UncompressedSize64: e.Declared}
			raw, err := w.CreateRaw(h)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := raw.Write(comp.Bytes()); err != nil {
				t.Fatal(err)
			}
			continue
		}
		h := &zip.FileHeader{Name: e.Name, Method: method}
		if e.Mode != 0 {
			h.SetMode(e.Mode)
		}
		fw, err := w.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write(e.Content); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// AtLimits 是一个合法的主题包，同时贴着各条上限：展开合计恰为 theme.MaxTotalBytes，最大的文件恰为 theme.MaxFileBytes，
// 包本身接近 theme.MaxPackageBytes 且大半是按 store 写入的不可压缩内容——各条声明的压缩字节之和因此接近包长。
// 用它钉住守卫没有收紧到拒绝合法的满额包。
func AtLimits(t testing.TB) []byte {
	t.Helper()
	// noise 之外留给清单、index.html、零字节文件的压缩数据与全部头部；留得不够时下面的核对会指出。
	const slack = 128 << 10
	entries := []Entry{Manifest(t, "limits", "Limits", "1.0.0", ""), File("index.html", "<!doctype html>limits")}
	noise := make([]byte, theme.MaxPackageBytes-slack)
	rand.NewChaCha8([32]byte{}).Read(noise)
	entries = append(entries, Entry{Name: "noise.bin", Content: noise, Method: zip.Store})
	rest := theme.MaxTotalBytes
	for _, e := range entries {
		rest -= len(e.Content)
	}
	for i := 0; rest > 0; i++ {
		n := min(rest, theme.MaxFileBytes)
		entries = append(entries, Entry{Name: fmt.Sprintf("zeros%d.bin", i), Content: make([]byte, n)})
		rest -= n
	}
	pkg := Zip(t, entries...)
	if len(pkg) > theme.MaxPackageBytes {
		t.Fatalf("AtLimits is %d bytes, over the %d-byte package limit; raise slack", len(pkg), theme.MaxPackageBytes)
	}
	return pkg
}

// Overlapping 生成中央目录记录互相重叠的包：包里只有一段压缩数据——payload 字节左右的 deflate 流，全由空的 stored
// 块组成、解出 0 字节——中央目录却有 entries 条名字各异的记录，全部指向它的本地头。每条声明未压缩 0 字节、CRC 0，
// 条目数、路径、类型与展开总量的守卫都放行；读者每打开一条都要把同一段流从头解一遍。archive/zip 的 Writer 写不出
// 重叠的包，这里按 APPNOTE 的记录格式逐字节拼出。
func Overlapping(entries, payload int) []byte {
	// 空的非末尾 stored 块：BFINAL=0、BTYPE=00，补齐到字节，LEN=0、NLEN=0xffff；末尾块只把 BFINAL 置 1。
	var stream []byte
	for len(stream)+10 <= payload {
		stream = append(stream, 0x00, 0x00, 0x00, 0xff, 0xff)
	}
	stream = append(stream, 0x01, 0x00, 0x00, 0xff, 0xff)
	le16, le32 := binary.LittleEndian.AppendUint16, binary.LittleEndian.AppendUint32
	const local = "payload"
	b := le32(nil, 0x04034b50)                 // 本地文件头
	b = le16(b, 20)                            // 解压所需版本
	b = le16(b, 0)                             // 标志
	b = le16(b, zip.Deflate)                   // 压缩方式
	b = le32(b, 0)                             // 修改时间与日期
	b = le32(b, 0)                             // CRC-32（空内容）
	b = le32(b, uint32(len(stream)))           // 压缩大小
	b = le32(b, 0)                             // 未压缩大小
	b = le16(b, uint16(len(local)))            // 名字长度
	b = le16(b, 0)                             // 扩展字段长度
	b = append(append(b, local...), stream...) // 名字与数据
	dirStart := len(b)
	for i := range entries {
		name := fmt.Sprintf("f%04d", i)
		b = le32(b, 0x02014b50)          // 中央目录记录
		b = le16(b, 20)                  // 创建者版本（创建者 0）
		b = le16(b, 20)                  // 解压所需版本
		b = le16(b, 0)                   // 标志
		b = le16(b, zip.Deflate)         // 压缩方式
		b = le32(b, 0)                   // 修改时间与日期
		b = le32(b, 0)                   // CRC-32
		b = le32(b, uint32(len(stream))) // 压缩大小
		b = le32(b, 0)                   // 未压缩大小
		b = le16(b, uint16(len(name)))   // 名字长度
		b = le16(b, 0)                   // 扩展字段长度
		b = le16(b, 0)                   // 注释长度
		b = le16(b, 0)                   // 起始磁盘
		b = le16(b, 0)                   // 内部属性
		b = le32(b, 0)                   // 外部属性
		b = le32(b, 0)                   // 本地头偏移：都指向同一个本地头
		b = append(b, name...)
	}
	dirLen := len(b) - dirStart
	b = le32(b, 0x06054b50) // 中央目录结束记录
	b = le16(b, 0)
	b = le16(b, 0)
	b = le16(b, uint16(entries))
	b = le16(b, uint16(entries))
	b = le32(b, uint32(dirLen))
	b = le32(b, uint32(dirStart))
	return le16(b, 0)
}
