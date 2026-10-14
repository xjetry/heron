package main

import (
	"archive/tar"
	"bytes"
	"debug/elf"
	"encoding/binary"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type file struct {
	name     string
	typ      byte
	mode     int64
	uid, gid int
	body     []byte
	pax      map[string]string // 原样写进 PAX 头的记录（扩展属性是 SCHILY.xattr.<名字>）
}

// fakeELF 只有 64 字节的 ELF 文件头：checkimage 只看机器类型，合成夹具不依赖交叉工具链。
func fakeELF(m elf.Machine) []byte {
	var buf bytes.Buffer
	h := elf.Header64{
		Ident:     [elf.EI_NIDENT]byte{0x7f, 'E', 'L', 'F', byte(elf.ELFCLASS64), byte(elf.ELFDATA2LSB), byte(elf.EV_CURRENT)},
		Type:      uint16(elf.ET_EXEC),
		Machine:   uint16(m),
		Version:   uint32(elf.EV_CURRENT),
		Ehsize:    64,
		Phentsize: 56,
		Shentsize: 64,
	}
	if err := binary.Write(&buf, binary.LittleEndian, h); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// caBundle 由 n 个 CERTIFICATE 块组成；checkimage 只数块，不解析证书内容。
func caBundle(n int) []byte {
	return bytes.Repeat(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not parsed")}), n)
}

func dir(name string, mode int64, owner int) file {
	return file{name: name, typ: tar.TypeDir, mode: mode, uid: owner, gid: owner}
}

func reg(name string, mode int64, body []byte) file {
	return file{name: name, typ: tar.TypeReg, mode: mode, body: body}
}

// expectedLayout 与对应组的 Dockerfile 产出的根文件系统一致（hub：根目录 Dockerfile；agent：deploy/Dockerfile.agent），
// 按 buildx 的 tar 输出分平台目录。
func expectedLayout(group string, arches ...string) []file {
	var files []file
	for _, a := range arches {
		p := "linux_" + a + "/"
		files = append(files,
			dir(p, 0o755, 0),
			dir(p+"etc/", 0o755, 0),
			dir(p+"etc/ssl/", 0o755, 0),
			dir(p+"etc/ssl/certs/", 0o755, 0),
			reg(p+"etc/ssl/certs/ca-certificates.crt", 0o644, caBundle(minCACerts)),
			dir(p+"usr/", 0o755, 0),
			dir(p+"usr/local/", 0o755, 0),
			dir(p+"usr/local/bin/", 0o755, 0),
		)
		switch group {
		case "hub":
			files = append(files,
				dir(p+"data/", 0o755, runUID),
				reg(p+"etc/group", 0o644, []byte("heron-hub:x:65532:\n")),
				reg(p+"etc/passwd", 0o644, []byte("heron-hub:x:65532:65532:heron-hub:/nonexistent:/sbin/nologin\n")),
				dir(p+"tmp/", 0o1777, 0),
				reg(p+"usr/local/bin/heron-hub", 0o755, fakeELF(machines[a])),
			)
		case "agent":
			files = append(files,
				dir(p+"etc/heron-agent/", 0o700, runUID),
				reg(p+"etc/group", 0o644, []byte("heron-agent:x:65532:\n")),
				reg(p+"etc/passwd", 0o644, []byte("heron-agent:x:65532:65532:heron-agent:/nonexistent:/sbin/nologin\n")),
				withCap(reg(p+"usr/local/bin/heron-agent", 0o755, fakeELF(machines[a])), netRawFileCap),
			)
		default:
			panic("no layout for group " + group)
		}
	}
	return files
}

// withCap 给文件加上 security.capability 扩展属性，写法与 buildx 的 tar 输出相同。
func withCap(f file, value string) file {
	f.pax = map[string]string{"SCHILY.xattr.security.capability": value}
	return f
}

func tarOf(t *testing.T, files []file) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range files {
		hdr := &tar.Header{Name: f.name, Typeflag: f.typ, Mode: f.mode, Uid: f.uid, Gid: f.gid, Size: int64(len(f.body)), PAXRecords: f.pax, Format: tar.FormatPAX}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(f.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

func edit(name string, change func(*file)) func([]file) []file {
	return func(files []file) []file {
		for i := range files {
			if files[i].name == name {
				change(&files[i])
			}
		}
		return files
	}
}

func without(prefix string) func([]file) []file {
	return func(files []file) []file {
		var kept []file
		for _, f := range files {
			if !strings.HasPrefix(f.name, prefix) {
				kept = append(kept, f)
			}
		}
		return kept
	}
}

func with(extra file) func([]file) []file {
	return func(files []file) []file { return append(files, extra) }
}

func TestAcceptsTheExpectedLayout(t *testing.T) {
	for _, group := range []string{"hub", "agent"} {
		if problems := check(tarOf(t, expectedLayout(group, "amd64", "arm64")), manifests[group], []string{"amd64", "arm64"}); len(problems) != 0 {
			t.Errorf("%s: problems on the expected layout: %q", group, problems)
		}
	}
}

func TestRejectsEachDeviation(t *testing.T) {
	for _, tc := range []struct {
		group  string
		name   string
		mutate func([]file) []file
		want   string
	}{
		{"hub", "tmp without the sticky bit", edit("linux_amd64/tmp/", func(f *file) { f.mode = 0o777 }), "linux_amd64/tmp: mode 0777, want 1777"},
		{"hub", "no tmp", without("linux_arm64/tmp/"), "linux_arm64/tmp: missing"},
		{"hub", "data owned by root", edit("linux_amd64/data/", func(f *file) { f.uid, f.gid = 0, 0 }), "linux_amd64/data: owner 0:0, want 65532:65532"},
		{"hub", "binary not executable", edit("linux_amd64/usr/local/bin/heron-hub", func(f *file) { f.mode = 0o644 }), "linux_amd64/usr/local/bin/heron-hub: mode 0644, want 0755"},
		{"hub", "a shell", with(reg("linux_amd64/bin/sh", 0o755, nil)), "linux_amd64/bin/sh: unexpected entry"},
		{"hub", "binary for the other architecture", edit("linux_arm64/usr/local/bin/heron-hub", func(f *file) { f.body = fakeELF(elf.EM_X86_64) }), "linux_arm64/usr/local/bin/heron-hub: ELF machine EM_X86_64, want EM_AARCH64"},
		{"hub", "binary that is not ELF", edit("linux_amd64/usr/local/bin/heron-hub", func(f *file) { f.body = []byte("#!/bin/sh\n") }), "linux_amd64/usr/local/bin/heron-hub: not an ELF file"},
		{"hub", "missing platform", without("linux_arm64/"), "linux_arm64: missing platform"},
		{"hub", "extra platform", with(dir("linux_386/", 0o755, 0)), "linux_386: unexpected platform"},
		{"hub", "short CA bundle", edit("linux_amd64/etc/ssl/certs/ca-certificates.crt", func(f *file) { f.body = caBundle(1) }), "linux_amd64/etc/ssl/certs/ca-certificates.crt: 1 certificates, want at least 100"},
		{"hub", "root account", edit("linux_amd64/etc/passwd", func(f *file) { f.body = []byte("root:x:0:0:root:/root:/bin/sh\n") }), "linux_amd64/etc/passwd: account"},
		{"hub", "a second account", edit("linux_arm64/etc/passwd", func(f *file) { f.body = append(f.body, "root:x:0:0:root:/root:/bin/sh\n"...) }), "linux_arm64/etc/passwd: want exactly one account"},
		{"hub", "root group", edit("linux_amd64/etc/group", func(f *file) { f.body = []byte("root:x:0:\n") }), "linux_amd64/etc/group: account"},
		{"hub", "passwd with root as the primary group", edit("linux_amd64/etc/passwd", func(f *file) { f.body = []byte("heron-hub:x:65532:0:heron-hub:/nonexistent:/sbin/nologin\n") }), "linux_amd64/etc/passwd: account"},
		{"hub", "passwd with an extra column", edit("linux_amd64/etc/passwd", func(f *file) { f.body = []byte("heron-hub:x:65532:65532:heron-hub:/nonexistent:/sbin/nologin:x\n") }), "linux_amd64/etc/passwd: malformed line"},
		{"hub", "tmp as a regular file", edit("linux_amd64/tmp/", func(f *file) { f.name, f.typ = "linux_amd64/tmp", tar.TypeReg }), "linux_amd64/tmp: type regular file, want directory"},
		{"hub", "passwd as a hard link", edit("linux_arm64/etc/passwd", func(f *file) { f.typ, f.body = tar.TypeLink, nil }), "linux_arm64/etc/passwd: type hard link, want regular file"},
		{"agent", "config directory owned by root", edit("linux_amd64/etc/heron-agent/", func(f *file) { f.uid, f.gid = 0, 0 }), "linux_amd64/etc/heron-agent: owner 0:0, want 65532:65532"},
		{"agent", "config directory open to others", edit("linux_arm64/etc/heron-agent/", func(f *file) { f.mode = 0o755 }), "linux_arm64/etc/heron-agent: mode 0755, want 0700"},
		{"agent", "no config directory", without("linux_amd64/etc/heron-agent/"), "linux_amd64/etc/heron-agent: missing"},
		{"agent", "an os-release", with(reg("linux_amd64/etc/os-release", 0o644, []byte("NAME=Alpine\n"))), "linux_amd64/etc/os-release: present, must be absent"},
		{"agent", "an os-release under usr/lib", with(reg("linux_arm64/usr/lib/os-release", 0o644, []byte("NAME=Alpine\n"))), "linux_arm64/usr/lib/os-release: present, must be absent"},
		{"agent", "the hub's data directory", with(dir("linux_amd64/data/", 0o755, runUID)), "linux_amd64/data: unexpected entry"},
		{"agent", "binary for the other architecture", edit("linux_amd64/usr/local/bin/heron-agent", func(f *file) { f.body = fakeELF(elf.EM_AARCH64) }), "linux_amd64/usr/local/bin/heron-agent: ELF machine EM_AARCH64, want EM_X86_64"},
		{"agent", "the hub binary in its place", edit("linux_amd64/usr/local/bin/heron-agent", func(f *file) { f.name = "linux_amd64/usr/local/bin/heron-hub" }), "linux_amd64/usr/local/bin/heron-agent: missing"},
		{"agent", "short CA bundle", edit("linux_arm64/etc/ssl/certs/ca-certificates.crt", func(f *file) { f.body = caBundle(1) }), "linux_arm64/etc/ssl/certs/ca-certificates.crt: 1 certificates, want at least 100"},
		{"agent", "root account", edit("linux_amd64/etc/passwd", func(f *file) { f.body = []byte("root:x:0:0:root:/root:/bin/sh\n") }), "linux_amd64/etc/passwd: account"},
		// 文件能力：二进制必须恰好带 cap_net_raw+ep；没有、换了能力、去掉 effective 位、多出别的扩展属性都拒绝。
		{"agent", "binary without the file capability", edit("linux_arm64/usr/local/bin/heron-agent", func(f *file) { f.pax = nil }), "linux_arm64/usr/local/bin/heron-agent: extended attribute security.capability missing, want 0100000200200000000000000000000000000000"},
		{"agent", "cap_net_admin instead of cap_net_raw", edit("linux_amd64/usr/local/bin/heron-agent", func(f *file) {
			*f = withCap(*f, "\x01\x00\x00\x02"+"\x00\x10\x00\x00"+strings.Repeat("\x00", 12))
		}), "linux_amd64/usr/local/bin/heron-agent: extended attribute security.capability is 0100000200100000000000000000000000000000, want 0100000200200000000000000000000000000000"},
		{"agent", "capability without the effective bit", edit("linux_amd64/usr/local/bin/heron-agent", func(f *file) {
			*f = withCap(*f, "\x00\x00\x00\x02"+"\x00\x20\x00\x00"+strings.Repeat("\x00", 12))
		}), "linux_amd64/usr/local/bin/heron-agent: extended attribute security.capability is 0000000200200000000000000000000000000000"},
		{"agent", "a second extended attribute on the binary", edit("linux_amd64/usr/local/bin/heron-agent", func(f *file) {
			f.pax = map[string]string{"SCHILY.xattr.security.capability": netRawFileCap, "SCHILY.xattr.user.note": "x"}
		}), "linux_amd64/usr/local/bin/heron-agent: unexpected extended attribute user.note (78)"},
		{"agent", "a capability on the config directory", edit("linux_amd64/etc/heron-agent/", func(f *file) { *f = withCap(*f, netRawFileCap) }), "linux_amd64/etc/heron-agent: unexpected extended attribute security.capability"},
		{"agent", "extended attributes in libarchive form", edit("linux_arm64/usr/local/bin/heron-agent", func(f *file) {
			f.pax["LIBARCHIVE.xattr.security.capability"] = "AQAAAgAgAAAAAAAAAAAAAAAAAAA="
		}), "linux_arm64/usr/local/bin/heron-agent: extended attribute record LIBARCHIVE.xattr.security.capability is in a form this check does not read"},
		{"hub", "a file capability on the hub binary", edit("linux_amd64/usr/local/bin/heron-hub", func(f *file) { *f = withCap(*f, netRawFileCap) }), "linux_amd64/usr/local/bin/heron-hub: unexpected extended attribute security.capability"},
	} {
		t.Run(tc.group+"/"+tc.name, func(t *testing.T) {
			problems := check(tarOf(t, tc.mutate(expectedLayout(tc.group, "amd64", "arm64"))), manifests[tc.group], []string{"amd64", "arm64"})
			for _, p := range problems {
				if strings.Contains(p, tc.want) {
					return
				}
			}
			t.Fatalf("no problem contains %q; got %q", tc.want, problems)
		})
	}
}

// 两份清单互不通用：hub 的根文件系统按 agent 清单核对、或反过来，都必须失败。清单若退化成同一份（或核对没看
// 清单里组特有的条目），这一条就红。
func TestManifestsAreNotInterchangeable(t *testing.T) {
	for _, pair := range [][2]string{{"hub", "agent"}, {"agent", "hub"}} {
		layout, against := pair[0], pair[1]
		if problems := check(tarOf(t, expectedLayout(layout, "amd64")), manifests[against], []string{"amd64"}); len(problems) == 0 {
			t.Errorf("the %s layout passed the %s manifest", layout, against)
		}
	}
}

// 读 tar 出错时 check 提前返回、跳过其余核对，这一条问题是损坏输入唯一的失败信号。
func TestReportsAnUnreadableArchive(t *testing.T) {
	problems := check(bytes.NewReader(bytes.Repeat([]byte("not a tar archive\n"), 64)), manifests["hub"], []string{"amd64"})
	for _, p := range problems {
		if strings.HasPrefix(p, "read tar: ") {
			return
		}
	}
	t.Fatalf("no problem starts with \"read tar: \"; got %q", problems)
}

// 调用方式错误以 2 退出，与核对不通过的 1 区分：缺架构、缺清单名、清单名未知都不能被当成"核对失败"或"核对通过"。
func TestRunRejectsBadInvocations(t *testing.T) {
	for _, args := range [][]string{
		{"-manifest", "hub", "rootfs.tar"},
		{"rootfs.tar", "amd64"},
		{"-manifest", "", "rootfs.tar", "amd64"},
		{"-manifest", "updater", "rootfs.tar", "amd64"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 2 {
			t.Errorf("run(%q) = %d, want 2; stderr %q", args, code, stderr.String())
		}
	}
}

func TestRunExitCodes(t *testing.T) {
	for _, group := range []string{"hub", "agent"} {
		path := filepath.Join(t.TempDir(), "rootfs.tar")
		if err := os.WriteFile(path, tarOf(t, expectedLayout(group, "amd64")).Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		if code := run([]string{"-manifest", group, path, "amd64"}, &stdout, &stderr); code != 0 || stdout.Len() != 0 {
			t.Fatalf("%s: expected layout: exit %d, stdout %q, stderr %q", group, code, stdout.String(), stderr.String())
		}
		stdout.Reset()
		if code := run([]string{"-manifest", group, path, "amd64", "riscv64"}, &stdout, &stderr); code != 1 || !strings.Contains(stdout.String(), "riscv64: no ELF machine known") {
			t.Fatalf("%s: unknown architecture: exit %d, stdout %q", group, code, stdout.String())
		}
	}
}

// release.yml 里唯一持推送权限的 image job 要编译 checkimage 来回读镜像，那个 job 不能编译任何第三方代码：
// checkimage 连同全部传递依赖只能是标准库与本仓库的包。列表必须含本包自己，否则 go list 出错或
// 输出为空时这条检查会空过。
func TestLinksOnlyStdlibAndThisModule(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	const self = "github.com/xjetry/heron/scripts/checkimage"
	deps := strings.Fields(string(out))
	if !slices.Contains(deps, self) {
		t.Fatalf("go list -deps did not list %s itself: %q", self, deps)
	}
	for _, d := range deps {
		if !strings.HasPrefix(d, "github.com/xjetry/heron/") {
			t.Errorf("checkimage depends on %s, outside the standard library and this module", d)
		}
	}
}
