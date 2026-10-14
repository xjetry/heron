// checkimage 核对一个镜像组（deploy/image.mk）的根文件系统。输入是 buildx 以 --output type=tar 导出的多平台结果：
// 每个平台一个 linux_<arch>/ 目录，里面是该平台镜像的完整文件树，不含容器运行时注入的 /dev、/proc 等。
//
//	checkimage -manifest <组> <rootfs.tar> <arch>...
//
// 镜像只含静态二进制、CA 证书与非 root 用户（hub 另有数据目录与临时目录，agent 另有配置目录）。这里
// 按组的清单逐条目核对类型、权限、属主与扩展属性，缺少或多出任何条目、任何扩展属性都拒绝：往镜像里加东西必须
// 同时改清单，清单与对应的 Dockerfile 一起审阅。清单名就是组名，scripts/docker-image.sh 经 MANIFEST 传入。
package main

import (
	"archive/tar"
	"bytes"
	"debug/elf"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
)

// runUID 是两个镜像共同的运行用户与主组，与各自 Dockerfile 的 USER、rootfs 阶段写进 /etc/passwd 的数同一个。
// 这里只能核对 passwd/group 与目录的属主；USER 在镜像配置里，导出的 tar 不含它，由冒烟核对（hub 在新卷里写出的
// 库文件属主、agent 在新卷里写出的配置）。
const runUID = 65532

// minCACerts：Alpine 3.21 的证书包在 2026-09-26 有 121 张根证书。远少于此说明拿到的不是完整的
// 系统证书包，出站 HTTPS（hub 的通知、agent 连 hub）会对许多对端校验失败。
const minCACerts = 100

const (
	passwdPath = "etc/passwd"
	groupPath  = "etc/group"
	caPath     = "etc/ssl/certs/ca-certificates.crt"
)

type entry struct {
	typ      byte  // tar 条目类型（tar.TypeDir、tar.TypeReg）
	perm     int64 // 权限位连同 setuid、setgid、sticky（Mode & 07777）
	uid, gid int
	// xattrs 是条目的全部扩展属性（名字 → 原始值），nil 表示一个也不许有。文件能力（security.capability）就是
	// 扩展属性，它与 setuid 位一样决定进程拿到的权限，所以与权限位同等对待：多出、缺少、值不同都拒绝。
	xattrs map[string]string
}

// xattrPrefix：buildx 的 tar 输出与 OCI 层都把扩展属性写成这个前缀的 PAX 记录（GNU tar、Go archive/tar 同一写法），
// 值是原始字节。libarchive 的另一种写法（LIBARCHIVE.xattr.，名字与值都另行编码）不解析，出现即拒绝，免得
// 换了写法的扩展属性绕过核对。
const (
	xattrPrefix      = "SCHILY.xattr."
	otherXattrPrefix = "LIBARCHIVE.xattr."
)

// netRawFileCap 是 deploy/Dockerfile.agent 里 setcap cap_net_raw+ep 写出的 security.capability：
// linux/capability.h 的 struct vfs_cap_data，版本 2，小端的五个 32 位字——magic_etc = VFS_CAP_REVISION_2
// (0x02000000) | VFS_CAP_FLAGS_EFFECTIVE (0x1)；permitted 低 32 位只有 CAP_NET_RAW（第 13 位，0x2000）；
// inheritable 与两个高 32 位为 0。版本 3 会多带一个 rootid，在用户命名空间里 setcap 才写它，那样的值在这里不相等。
const netRawFileCap = "\x01\x00\x00\x02" + "\x00\x20\x00\x00" + "\x00\x00\x00\x00" + "\x00\x00\x00\x00" + "\x00\x00\x00\x00"

// manifest 是一个镜像组根文件系统的全部期望。entries 是完整清单（多出的条目即失败）；absent 是另外点名的、必须
// 不存在的路径与理由——它们本来也会作为多出的条目被拒绝，单独点名是为了失败时直接说出后果。
type manifest struct {
	uid        int    // passwd 与 group 里唯一账户的 uid / gid
	minCACerts int    // CA 证书包至少要有的证书数
	bin        string // 静态二进制的路径，核对 ELF 机器类型
	entries    map[string]entry
	absent     map[string]string
}

// 两个镜像共有的条目：账户文件、CA 证书与二进制所在的目录。
func baseEntries(bin string) map[string]entry {
	return map[string]entry{
		"etc":           {tar.TypeDir, 0o755, 0, 0, nil},
		groupPath:       {tar.TypeReg, 0o644, 0, 0, nil},
		passwdPath:      {tar.TypeReg, 0o644, 0, 0, nil},
		"etc/ssl":       {tar.TypeDir, 0o755, 0, 0, nil},
		"etc/ssl/certs": {tar.TypeDir, 0o755, 0, 0, nil},
		caPath:          {tar.TypeReg, 0o644, 0, 0, nil},
		"usr":           {tar.TypeDir, 0o755, 0, 0, nil},
		"usr/local":     {tar.TypeDir, 0o755, 0, 0, nil},
		"usr/local/bin": {tar.TypeDir, 0o755, 0, 0, nil},
		bin:             {tar.TypeReg, 0o755, 0, 0, nil},
	}
}

func withEntries(m map[string]entry, extra map[string]entry) map[string]entry {
	maps.Copy(m, extra)
	return m
}

var manifests = map[string]manifest{
	// hub 镜像（根目录 Dockerfile）。
	"hub": {
		uid:        runUID,
		minCACerts: minCACerts,
		bin:        "usr/local/bin/heron-hub",
		entries: withEntries(baseEntries("usr/local/bin/heron-hub"), map[string]entry{
			// 空卷挂到 /data 时 Docker 沿用镜像里这个目录的属主，hub 以 runUID 写库。
			"data": {tar.TypeDir, 0o755, runUID, runUID, nil},
			// SQLite 的排序、临时表与建索引在页缓存装不下时写临时文件；镜像里没有可写的临时目录时报
			// disk I/O error (6410)，小查询不触发。1777：root 属主的 1777 目录对任何 uid 可写，不依赖 USER
			// 取值；sticky 位让各 uid 只能删改自己的文件。
			"tmp": {tar.TypeDir, 0o1777, 0, 0, nil},
		}),
	},
	// agent 镜像（deploy/Dockerfile.agent）。
	"agent": {
		uid:        runUID,
		minCACerts: minCACerts,
		bin:        "usr/local/bin/heron-agent",
		entries: withEntries(baseEntries("usr/local/bin/heron-agent"), map[string]entry{
			// register 以 runUID 在这里写 0600 的配置（含节点 token）；空的命名卷挂上时 Docker 沿用镜像里这个目录的
			// 属主与权限，属主不对时注册写不进去。只许运行用户进入。
			"etc/heron-agent": {tar.TypeDir, 0o700, runUID, runUID, nil},
			// 以 runUID 运行时 ICMP 退到 raw socket 要 CAP_NET_RAW（宿主的 ping_group_range 不放行它时），由文件能力
			// 给出；值的由来见 netRawFileCap。
			"usr/local/bin/heron-agent": {tar.TypeReg, 0o755, 0, 0, map[string]string{"security.capability": netRawFileCap}},
		}),
		absent: map[string]string{
			// agent 的系统名读被观测主机的 os-release；镜像自带一份时，读不到宿主的那一刻就会把镜像的系统名报成节点的。
			"etc/os-release":     "the agent would report the image's OS as the node's",
			"usr/lib/os-release": "the agent would report the image's OS as the node's",
		},
	},
}

var typeNames = map[byte]string{
	tar.TypeReg:     "regular file",
	tar.TypeLink:    "hard link",
	tar.TypeSymlink: "symlink",
	tar.TypeChar:    "char device",
	tar.TypeBlock:   "block device",
	tar.TypeDir:     "directory",
	tar.TypeFifo:    "fifo",
}

func typeName(t byte) string {
	if n, ok := typeNames[t]; ok {
		return n
	}
	return fmt.Sprintf("typeflag %q", t)
}

// machines：镜像索引按 --platform 给每个平台打标签，Dockerfile 按同一平台的 TARGETARCH 从
// build/image/linux/<arch>/ 取二进制。目录里放了别的架构的二进制时，能经 binfmt 执行其他架构的宿主
// 照样能把容器跑起来，冒烟照不到；这里读 ELF 头的机器类型核对。
var machines = map[string]elf.Machine{
	"amd64": elf.EM_X86_64,
	"arm64": elf.EM_AARCH64,
}

func check(r io.Reader, m manifest, arches []string) []string {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	expected := map[string]string{} // 平台目录 → GOARCH
	for _, a := range arches {
		if _, ok := machines[a]; !ok {
			add("%s: no ELF machine known for this architecture; add it to machines", a)
			continue
		}
		expected["linux_"+a] = a
	}

	headers := map[string]map[string]*tar.Header{}
	bodies := map[string][]byte{}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			add("read tar: %v", err)
			return problems
		}
		name := strings.TrimSuffix(hdr.Name, "/")
		platform, rel, _ := strings.Cut(name, "/")
		if headers[platform] == nil {
			headers[platform] = map[string]*tar.Header{}
		}
		if rel == "" {
			continue
		}
		headers[platform][rel] = hdr
		switch rel {
		case passwdPath, groupPath, caPath, m.bin:
			b, err := io.ReadAll(tr)
			if err != nil {
				add("%s: read: %v", name, err)
				continue
			}
			bodies[name] = b
		}
	}

	for _, platform := range slices.Sorted(maps.Keys(headers)) {
		if _, ok := expected[platform]; !ok {
			add("%s: unexpected platform", platform)
		}
	}
	for _, platform := range slices.Sorted(maps.Keys(expected)) {
		got, ok := headers[platform]
		if !ok {
			add("%s: missing platform", platform)
			continue
		}
		for _, rel := range slices.Sorted(maps.Keys(m.absent)) {
			if _, ok := got[rel]; ok {
				add("%s/%s: present, must be absent: %s", platform, rel, m.absent[rel])
			}
		}
		for _, rel := range slices.Sorted(maps.Keys(m.entries)) {
			w := m.entries[rel]
			hdr, ok := got[rel]
			if !ok {
				add("%s/%s: missing", platform, rel)
				continue
			}
			if hdr.Typeflag != w.typ {
				add("%s/%s: type %s, want %s", platform, rel, typeName(hdr.Typeflag), typeName(w.typ))
			}
			if perm := hdr.Mode & 0o7777; perm != w.perm {
				add("%s/%s: mode %04o, want %04o", platform, rel, perm, w.perm)
			}
			if hdr.Uid != w.uid || hdr.Gid != w.gid {
				add("%s/%s: owner %d:%d, want %d:%d", platform, rel, hdr.Uid, hdr.Gid, w.uid, w.gid)
			}
			for _, p := range xattrProblems(hdr, w.xattrs) {
				add("%s/%s: %s", platform, rel, p)
			}
		}
		for _, rel := range slices.Sorted(maps.Keys(got)) {
			if _, ok := m.entries[rel]; !ok {
				add("%s/%s: unexpected entry", platform, rel)
			}
		}
		if b, ok := bodies[platform+"/"+passwdPath]; ok {
			if err := checkAccount(b, 7, m.uid); err != nil {
				add("%s/%s: %v", platform, passwdPath, err)
			}
		}
		if b, ok := bodies[platform+"/"+groupPath]; ok {
			if err := checkAccount(b, 4, m.uid); err != nil {
				add("%s/%s: %v", platform, groupPath, err)
			}
		}
		if b, ok := bodies[platform+"/"+caPath]; ok {
			if n := countCerts(b); n < m.minCACerts {
				add("%s/%s: %d certificates, want at least %d", platform, caPath, n, m.minCACerts)
			}
		}
		if b, ok := bodies[platform+"/"+m.bin]; ok {
			machine := machines[expected[platform]]
			f, err := elf.NewFile(bytes.NewReader(b))
			switch {
			case err != nil:
				add("%s/%s: not an ELF file: %v", platform, m.bin, err)
			case f.Machine != machine:
				add("%s/%s: ELF machine %v, want %v", platform, m.bin, f.Machine, machine)
			}
		}
	}
	return problems
}

// xattrProblems 比较条目的扩展属性与期望的全集，每处不符一条，按名字排序。
func xattrProblems(hdr *tar.Header, want map[string]string) []string {
	got := map[string]string{}
	var problems []string
	for _, k := range slices.Sorted(maps.Keys(hdr.PAXRecords)) {
		switch {
		case strings.HasPrefix(k, xattrPrefix):
			got[strings.TrimPrefix(k, xattrPrefix)] = hdr.PAXRecords[k]
		case strings.HasPrefix(k, otherXattrPrefix):
			problems = append(problems, fmt.Sprintf("extended attribute record %s is in a form this check does not read", k))
		}
	}
	names := slices.Sorted(maps.Keys(got))
	for n := range want {
		if _, ok := got[n]; !ok {
			names = append(names, n)
		}
	}
	slices.Sort(names)
	for _, n := range names {
		g, inGot := got[n]
		w, inWant := want[n]
		switch {
		case !inWant:
			problems = append(problems, fmt.Sprintf("unexpected extended attribute %s (%x)", n, g))
		case !inGot:
			problems = append(problems, fmt.Sprintf("extended attribute %s missing, want %x", n, w))
		case g != w:
			problems = append(problems, fmt.Sprintf("extended attribute %s is %x, want %x", n, g, w))
		}
	}
	return problems
}

// checkAccount 要求文件里只列运行用户一个账户：passwd 七列、group 四列，第三列（uid 或 gid）为 uid，
// passwd 的第四列（主组）也为 uid。
func checkAccount(b []byte, fields, uid int) error {
	line := strings.TrimSuffix(string(b), "\n")
	if line == "" || strings.Contains(line, "\n") {
		return fmt.Errorf("want exactly one account, got %q", line)
	}
	f := strings.Split(line, ":")
	if len(f) != fields {
		return fmt.Errorf("malformed line %q", line)
	}
	id := strconv.Itoa(uid)
	if f[2] != id || (fields == 7 && f[3] != id) {
		return fmt.Errorf("account %q is not %s:%s", line, id, id)
	}
	return nil
}

// countCerts 数 PEM 里的 CERTIFICATE 块；块之间的注释行由 pem.Decode 跳过。
func countCerts(b []byte) int {
	n := 0
	for {
		var block *pem.Block
		block, b = pem.Decode(b)
		if block == nil {
			return n
		}
		if block.Type == "CERTIFICATE" {
			n++
		}
	}
}

// run 是可测的 CLI 入口。缺清单名、清单名未知或缺架构参数是调用方式错误，以 2 退出并打印用法，与核对不通过的 1
// 区分。清单为空时 check 本身也不放行：清单之外的平台目录都报 unexpected platform，非空的 tar 照样失败。
func run(args []string, stdout, stderr io.Writer) int {
	const usage = "usage: checkimage -manifest <hub|agent> <rootfs.tar> <arch>..."
	fs := flag.NewFlagSet("checkimage", flag.ContinueOnError)
	fs.SetOutput(stderr)
	name := fs.String("manifest", "", "image group whose manifest the root filesystems must match (hub or agent)")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	m, ok := manifests[*name]
	if !ok {
		fmt.Fprintf(stderr, "checkimage: unknown manifest %q\n%s\n", *name, usage)
		return 2
	}
	if fs.NArg() < 2 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	f, err := os.Open(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer f.Close()
	problems := check(f, m, fs.Args()[1:])
	for _, p := range problems {
		fmt.Fprintln(stdout, p)
	}
	if len(problems) > 0 {
		return 1
	}
	return 0
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
