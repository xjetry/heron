package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"debug/buildinfo"
	"debug/elf"
	"runtime/debug"
)

func buildHello(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "hello")
	cmd := exec.Command("go", "build", "-o", out, "./testdata/hello")
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	log, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build testdata/hello: %v\n%s", err, log)
	}
	return out
}

// 合成夹具不依赖宿主的 C 交叉工具链，便于在任何平台上独立验证 PT_INTERP 判定。
func writeInterpELF(t *testing.T) string {
	t.Helper()
	interp := []byte("/lib/ld-musl-x86_64.so.1\x00")

	var buf bytes.Buffer
	ident := make([]byte, 16)
	ident[0], ident[1], ident[2], ident[3] = 0x7f, 'E', 'L', 'F'
	ident[4] = 2 // ELFCLASS64
	ident[5] = 1 // ELFDATA2LSB
	ident[6] = 1 // EV_CURRENT
	buf.Write(ident)

	le := binary.Write
	le(&buf, binary.LittleEndian, uint16(2))  // e_type = ET_EXEC
	le(&buf, binary.LittleEndian, uint16(62)) // e_machine = EM_X86_64
	le(&buf, binary.LittleEndian, uint32(1))  // e_version
	le(&buf, binary.LittleEndian, uint64(0))  // e_entry
	le(&buf, binary.LittleEndian, uint64(64)) // e_phoff，紧跟文件头
	le(&buf, binary.LittleEndian, uint64(0))  // e_shoff
	le(&buf, binary.LittleEndian, uint32(0))  // e_flags
	le(&buf, binary.LittleEndian, uint16(64)) // e_ehsize
	le(&buf, binary.LittleEndian, uint16(56)) // e_phentsize
	le(&buf, binary.LittleEndian, uint16(1))  // e_phnum
	le(&buf, binary.LittleEndian, uint16(64)) // e_shentsize
	le(&buf, binary.LittleEndian, uint16(0))  // e_shnum
	le(&buf, binary.LittleEndian, uint16(0))  // e_shstrndx

	off := uint64(64 + 56)
	le(&buf, binary.LittleEndian, uint32(3))           // p_type = PT_INTERP
	le(&buf, binary.LittleEndian, uint32(4))           // p_flags = PF_R
	le(&buf, binary.LittleEndian, off)                 // p_offset
	le(&buf, binary.LittleEndian, off)                 // p_vaddr
	le(&buf, binary.LittleEndian, off)                 // p_paddr
	le(&buf, binary.LittleEndian, uint64(len(interp))) // p_filesz
	le(&buf, binary.LittleEndian, uint64(len(interp))) // p_memsz
	le(&buf, binary.LittleEndian, uint64(1))           // p_align
	buf.Write(interp)

	path := filepath.Join(t.TempDir(), "interp.elf")
	if err := os.WriteFile(path, buf.Bytes(), 0o755); err != nil {
		t.Fatalf("write interp elf: %v", err)
	}
	return path
}

// 以真实静态产物为底做节级手术：程序段与 buildinfo 原样保留，动态节（及可选的
// 字符串表）追加在文件尾，节头表整体搬到新末尾。strtab 为 nil 时动态节的字符串表
// link 指向节索引 0（SHT_NULL），模拟动态依赖读不出的产物。
func withDynamicSection(t *testing.T, strtab []byte) string {
	t.Helper()
	base, err := os.ReadFile(buildHello(t))
	if err != nil {
		t.Fatalf("read hello: %v", err)
	}
	e_shoff := binary.LittleEndian.Uint64(base[40:48])
	e_shentsize := binary.LittleEndian.Uint16(base[58:60])
	e_shnum := binary.LittleEndian.Uint16(base[60:62])

	data := append([]byte{}, base...)
	pad8 := func() {
		for len(data)%8 != 0 {
			data = append(data, 0)
		}
	}

	type shdr struct {
		shType  uint32
		offset  uint64
		size    uint64
		link    uint32
		entsize uint64
	}
	var extra []shdr

	strtabLink := uint32(0)
	if strtab != nil {
		pad8()
		strtabLink = uint32(e_shnum)
		extra = append(extra, shdr{uint32(elf.SHT_STRTAB), uint64(len(data)), uint64(len(strtab)), 0, 0})
		data = append(data, strtab...)
	}

	pad8()
	dynOff := uint64(len(data))
	var dyn bytes.Buffer
	binary.Write(&dyn, binary.LittleEndian, uint64(1)) // DT_NEEDED，指向字符串表偏移 1
	binary.Write(&dyn, binary.LittleEndian, uint64(1))
	binary.Write(&dyn, binary.LittleEndian, uint64(0)) // DT_NULL
	binary.Write(&dyn, binary.LittleEndian, uint64(0))
	extra = append(extra, shdr{uint32(elf.SHT_DYNAMIC), dynOff, uint64(dyn.Len()), strtabLink, 16})
	data = append(data, dyn.Bytes()...)

	pad8()
	newShoff := uint64(len(data))
	data = append(data, base[e_shoff:e_shoff+uint64(e_shentsize)*uint64(e_shnum)]...)
	for _, s := range extra {
		var sh bytes.Buffer
		binary.Write(&sh, binary.LittleEndian, uint32(0)) // sh_name
		binary.Write(&sh, binary.LittleEndian, s.shType)
		binary.Write(&sh, binary.LittleEndian, uint64(0)) // sh_flags
		binary.Write(&sh, binary.LittleEndian, uint64(0)) // sh_addr
		binary.Write(&sh, binary.LittleEndian, s.offset)
		binary.Write(&sh, binary.LittleEndian, s.size)
		binary.Write(&sh, binary.LittleEndian, s.link)
		binary.Write(&sh, binary.LittleEndian, uint32(0)) // sh_info
		binary.Write(&sh, binary.LittleEndian, uint64(8)) // sh_addralign
		binary.Write(&sh, binary.LittleEndian, s.entsize)
		data = append(data, sh.Bytes()...)
	}
	binary.LittleEndian.PutUint64(data[40:48], newShoff)
	binary.LittleEndian.PutUint16(data[60:62], e_shnum+uint16(len(extra)))

	path := filepath.Join(t.TempDir(), "with-dynamic")
	if err := os.WriteFile(path, data, 0o755); err != nil {
		t.Fatalf("write elf with dynamic section: %v", err)
	}
	return path
}

func TestCheckStaticBinaryHasNoReasons(t *testing.T) {
	if reasons := check(buildHello(t)); len(reasons) != 0 {
		t.Fatalf("static linux build reported reasons: %v", reasons)
	}
}

func TestCheckInterpELF(t *testing.T) {
	reasons := strings.Join(check(writeInterpELF(t)), "; ")
	if !strings.Contains(reasons, "has PT_INTERP") {
		t.Fatalf("want has PT_INTERP, got %q", reasons)
	}
	if !strings.Contains(reasons, "/lib/ld-musl-x86_64.so.1") {
		t.Fatalf("want loader path in reason, got %q", reasons)
	}
}

// 没有读到动态依赖不等于已经证明没有依赖，读不出时必须给出原因而不是放行。
func TestCheckUnreadableDynamic(t *testing.T) {
	reasons := strings.Join(check(withDynamicSection(t, nil)), "; ")
	if !strings.Contains(reasons, "cannot read dynamic dependencies: section has invalid string table link") {
		t.Fatalf("want cannot read dynamic dependencies with the underlying error, got %q", reasons)
	}
}

func TestCheckNeededLib(t *testing.T) {
	got := check(withDynamicSection(t, []byte("\x00libaudit.so\x00")))
	if len(got) != 1 || got[0] != "has DT_NEEDED libaudit.so" {
		// 程序段与 buildinfo 都保持有效：动态库是唯一该报的原因。
		t.Fatalf("want exactly has DT_NEEDED libaudit.so, got %q", got)
	}
}

func TestCgoReason(t *testing.T) {
	cgoOn := &buildinfo.BuildInfo{Settings: []debug.BuildSetting{{Key: "CGO_ENABLED", Value: "1"}}}
	if got := strings.Join(cgoReason(cgoOn), "; "); !strings.Contains(got, "built with CGO_ENABLED=1") {
		t.Fatalf("want built with CGO_ENABLED=1, got %q", got)
	}
	// 构建设置缺失不等于 0，不能放行。
	noSetting := &buildinfo.BuildInfo{}
	if got := strings.Join(cgoReason(noSetting), "; "); !strings.Contains(got, "missing CGO_ENABLED build setting") {
		t.Fatalf("want missing CGO_ENABLED build setting, got %q", got)
	}
}

func TestCheckNotELF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-an-elf")
	if err := os.WriteFile(path, []byte("just some text\n"), 0o644); err != nil {
		t.Fatalf("write text file: %v", err)
	}
	reasons := strings.Join(check(path), "; ")
	if !strings.Contains(reasons, "not an ELF file") {
		t.Fatalf("want not an ELF file, got %q", reasons)
	}
}

// 空文件清单等价于"全部通过"会把忘传文件变成绿灯，入口必须拒绝。
func TestRunRequiresFiles(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(nil, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("no files must not exit 0")
	}
	if !strings.Contains(stderr.String(), "usage") {
		t.Fatalf("want usage on stderr, got %q", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{buildHello(t)}, &stdout, &stderr); code != 0 || stdout.Len() != 0 {
		t.Fatalf("static build: want exit 0 and no output, got code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}
