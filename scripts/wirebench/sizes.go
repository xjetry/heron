package main

import (
	"bufio"
	"bytes"
	"debug/buildinfo"
	"debug/elf"
	"debug/macho"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
)

// sizes 把一个 Go 二进制的符号按模块归并，用来看发布体积里 connect 与 protobuf runtime 占多少（linux/amd64、链接 -s -w：agent 约 8.5 MB，常驻更新器约 7.8 MB）。
//
// 方法：符号与大小取自 `go tool nm -size`（Go 自己的符号表读法，ELF 与 Mach-O 同一口径），符号名里的包路径
// 按 debug/buildinfo 记录的模块表做最长前缀匹配；本仓库的 gen/heron/v1（protoc 生成的消息与 connect 存根）
// 单列。只计文件里占字节的符号（nm 类型 T/t 代码、R/r 只读数据、D/d 已初始化数据），不计 B/b（bss 不占文件）。
//
// 符号表照不到的部分：DWARF 调试段、pclntab（函数表与行号表，按函数数增长、不分包）、符号表与字符串表本身、
// 段对齐的填充。所以按模块的合计小于文件大小，差额在输出里按段列出，不摊给任何模块。
//
// 发布产物以 -ldflags "-s -w" 链接（deploy/agent.mk 的 RELEASE_LDFLAGS），没有符号表：ELF 没有 .symtab，go tool nm
// 报错退出；Mach-O 只剩动态链接要的未定义符号，nm 照常退出 0，逐行却都是 U 类型，按上面的规则一条也不计，表会是空的。
// 所以先看文件里有没有已定义的符号：没有就明说，只给段大小，不出按模块的表。-s -w 只去掉不在加载段里的符号表与
// DWARF，加载段逐段相同，要按模块的表就对同一提交与同一 Go 版本、去掉这两个参数的构建运行本命令。
// 泛型实例化的符号按名字里方括号之前的包路径归属，类型参数里出现的包不参与归属；链接器合成的符号
// （以 go: 开头，如函数元数据 go:func.*，按函数数增长、不分包）单列，不摊给模块；标准库里 vendor 的
// golang.org/x/net 等（net/http 内置的 HTTP/2、idna）归 std。
func runSizes(args []string, w io.Writer) error {
	fs := flag.NewFlagSet("sizes", flag.ContinueOnError)
	bin := fs.String("bin", "", "Go binary")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *bin == "" {
		return errors.New("-bin is required")
	}
	info, err := buildinfo.ReadFile(*bin)
	if err != nil {
		return err
	}
	st, err := os.Stat(*bin)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "binary %s: %d bytes, %s %s/%s\n", *bin, st.Size(), info.GoVersion, setting(info, "GOOS"), setting(info, "GOARCH"))
	defined, err := definedSymbols(*bin)
	if err != nil {
		return err
	}
	if defined == 0 {
		fmt.Fprintln(w, "no symbol table (linked with -s): per-module attribution needs a build of the same commit and Go version without -s -w, whose loaded sections are the same")
		return writeSections(w, *bin)
	}
	out, err := exec.Command("go", "tool", "nm", "-size", *bin).Output()
	if err != nil {
		return fmt.Errorf("go tool nm: %w", err)
	}
	mods := []string{info.Main.Path}
	versions := map[string]string{info.Main.Path: info.Main.Version}
	for _, d := range info.Deps {
		mods = append(mods, d.Path)
		versions[d.Path] = d.Version
	}
	totals := map[string]*[3]int64{} // 代码、只读数据、数据
	var symTotal int64
	var unattributed []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		// addr size type name...；未定义符号没有地址，只有 size type name。
		if len(f) < 4 {
			continue
		}
		size, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			continue
		}
		var slot int
		switch f[2] {
		case "T", "t":
			slot = 0
		case "R", "r":
			slot = 1
		case "D", "d":
			slot = 2
		default:
			continue
		}
		name := strings.Join(f[3:], " ")
		owner := symbolOwner(name, info.Main.Path, mods)
		if owner == noPackage {
			unattributed = append(unattributed, fmt.Sprintf("%d %s", size, name))
		}
		if totals[owner] == nil {
			totals[owner] = new([3]int64)
		}
		totals[owner][slot] += size
		symTotal += size
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if symTotal == 0 {
		// 文件里有已定义的符号，nm 却一条带大小的都没列出：读法与上面的类型规则对不上了，空表比报错更误导。
		return fmt.Errorf("%s has %d defined symbols but go tool nm listed none with a size", *bin, defined)
	}
	fmt.Fprintln(w, "| owner | version | text | rodata | data | total | share of attributed |")
	fmt.Fprintln(w, "|---|---|---:|---:|---:|---:|---:|")
	owners := make([]string, 0, len(totals))
	for o := range totals {
		owners = append(owners, o)
	}
	slices.SortFunc(owners, func(a, b string) int {
		ta, tb := totals[a][0]+totals[a][1]+totals[a][2], totals[b][0]+totals[b][1]+totals[b][2]
		if ta != tb {
			if ta > tb {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	})
	for _, o := range owners {
		t := totals[o]
		sum := t[0] + t[1] + t[2]
		fmt.Fprintf(w, "| %s | %s | %d | %d | %d | %d | %.1f%% |\n", o, versions[o], t[0], t[1], t[2], sum, 100*float64(sum)/float64(symTotal))
	}
	fmt.Fprintf(w, "symbols with a size: %d bytes of %d (%.1f%%)\n", symTotal, st.Size(), 100*float64(symTotal)/float64(st.Size()))
	if len(unattributed) > 0 {
		fmt.Fprintf(w, "symbols without a package path (%d), first few: %q\n", len(unattributed), unattributed[:min(5, len(unattributed))])
	}
	return writeSections(w, *bin)
}

func setting(info *buildinfo.BuildInfo, key string) string {
	for _, s := range info.Settings {
		if s.Key == key {
			return s.Value
		}
	}
	return ""
}

// symbolOwner 给出符号的归属：模块路径、"std"（标准库与运行时）、本仓库的生成代码或链接器合成。
func symbolOwner(name, main string, mods []string) string {
	for _, p := range []string{"type:", "go:itab."} {
		name = strings.TrimPrefix(name, p)
	}
	if strings.HasPrefix(name, "go:") {
		return "(linker-synthesized go:*)"
	}
	name = strings.TrimLeft(name, "*[]")
	name = strings.TrimPrefix(name, "map[")
	pkg := symbolPackage(name)
	if pkg == "" {
		// 汇编实现的运行时入口（_rt0_arm64、cmpbody）与编译器生成的类型相等/哈希函数（type:.eq.*）不带包路径。
		return noPackage
	}
	if pkg == "main" {
		return main + " (cmd)"
	}
	best := ""
	for _, m := range mods {
		if (pkg == m || strings.HasPrefix(pkg, m+"/")) && len(m) > len(best) {
			best = m
		}
	}
	if best == main && (pkg == main+"/gen/heron/v1" || strings.HasPrefix(pkg, main+"/gen/")) {
		return main + "/gen (generated protobuf + connect stubs)"
	}
	if best != "" {
		return best
	}
	if first, _, _ := strings.Cut(pkg, "/"); !strings.Contains(first, ".") {
		return "std"
	}
	return noPackage
}

const noPackage = "(no package path: assembly, type:.eq/.hash helpers)"

// symbolPackage 取符号名里的包路径：在第一个 '('、'['、','（itab 名是"具体类型,接口"，归具体类型）或空格之前，
// 最后一个 '/' 之后的第一个 '.' 为止。
func symbolPackage(name string) string {
	head := name
	if i := strings.IndexAny(head, "([, "); i >= 0 {
		head = head[:i]
	}
	slash := strings.LastIndex(head, "/")
	dot := strings.Index(head[slash+1:], ".")
	if dot < 0 {
		return ""
	}
	return head[:slash+1+dot]
}

// definedSymbols 数文件符号表里已定义的符号。ELF 以 .symtab 为准（-s 去掉的就是它，.dynsym 只有动态链接的产物
// 才有）；Mach-O 的符号表在 -s 之后仍在，只剩 N_UNDF 的动态导入，按类型位数已定义的。
func definedSymbols(path string) (int, error) {
	if f, err := elf.Open(path); err == nil {
		defer f.Close()
		syms, err := f.Symbols()
		if errors.Is(err, elf.ErrNoSymbols) {
			return 0, nil
		}
		if err != nil {
			return 0, err
		}
		n := 0
		for _, s := range syms {
			if s.Section != elf.SHN_UNDEF {
				n++
			}
		}
		return n, nil
	}
	f, err := macho.Open(path)
	if err != nil {
		return 0, errors.New("neither ELF nor Mach-O")
	}
	defer f.Close()
	if f.Symtab == nil {
		return 0, nil
	}
	n := 0
	for _, s := range f.Symtab.Syms {
		if s.Type&machoTypeMask != machoUndefined {
			n++
		}
	}
	return n, nil
}

// Mach-O nlist 的 n_type 里 N_TYPE 位（mach-o/nlist.h：N_TYPE 0x0e，N_UNDF 0x0）；debug/macho 没有导出这两个常量。
const (
	machoTypeMask  = 0x0e
	machoUndefined = 0x0
)

type section struct {
	name string
	size uint64
}

// fileSections 给出每个段在文件里占的字节。ELF 的 DWARF 段由 Go 链接器压缩，debug/elf 的 Size 是解压后的大小，
// 文件里占的是 FileSize；Mach-O 的 DWARF 段本身就以压缩形式（__zdebug_*）存放，Size 即文件里的字节。
// 零填充段（ELF 的 NOBITS、Mach-O 的 S_ZEROFILL：bss）不占文件，不列。
func fileSections(path string) ([]section, error) {
	var secs []section
	if f, err := elf.Open(path); err == nil {
		defer f.Close()
		for _, s := range f.Sections {
			if s.Type != elf.SHT_NOBITS && s.FileSize > 0 {
				secs = append(secs, section{s.Name, s.FileSize})
			}
		}
	} else if f, err := macho.Open(path); err == nil {
		defer f.Close()
		for _, s := range f.Sections {
			if s.Flags&0xff != 0x1 && s.Size > 0 {
				secs = append(secs, section{s.Seg + "/" + s.Name, s.Size})
			}
		}
	} else {
		return nil, errors.New("neither ELF nor Mach-O")
	}
	return secs, nil
}

func writeSections(w io.Writer, path string) error {
	secs, err := fileSections(path)
	if err != nil {
		return err
	}
	slices.SortFunc(secs, func(a, b section) int {
		if a.size != b.size {
			if a.size > b.size {
				return -1
			}
			return 1
		}
		return strings.Compare(a.name, b.name)
	})
	fmt.Fprintln(w, "sections, bytes in the file (compressed DWARF counted as stored):")
	var debug uint64
	for _, s := range secs {
		fmt.Fprintf(w, "  %-28s %d\n", s.name, s.size)
		if strings.Contains(s.name, "debug") || strings.Contains(s.name, "zdebug") {
			debug += s.size
		}
	}
	fmt.Fprintf(w, "debug sections total: %d\n", debug)
	return nil
}
