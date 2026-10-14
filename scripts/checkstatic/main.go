// 未显式关闭 cgo 且 C 工具链可用时，包含 net 的原生构建可能引入系统 C 库依赖；
// Alpine 基础系统不能假定提供 glibc 的动态加载器，所以在产物上验证，而不只依赖构建命令的约定。
package main

import (
	"bytes"
	"debug/buildinfo"
	"debug/elf"
	"fmt"
	"io"
	"os"
)

func check(path string) []string {
	var reasons []string
	f, err := elf.Open(path)
	if err != nil {
		return append(reasons, fmt.Sprintf("not an ELF file: %v", err))
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type != elf.PT_INTERP {
			continue
		}
		data := make([]byte, p.Filesz)
		if _, err := p.ReadAt(data, 0); err != nil {
			return append(reasons, fmt.Sprintf("has PT_INTERP (unreadable: %v)", err))
		}
		reasons = append(reasons, fmt.Sprintf("has PT_INTERP (dynamic loader %s)", string(bytes.TrimRight(data, "\x00"))))
	}
	if libs, err := f.ImportedLibraries(); err != nil {
		// 没有读到依赖不等于已经证明没有依赖，读不出本身就是拒绝的理由。
		reasons = append(reasons, fmt.Sprintf("cannot read dynamic dependencies: %v", err))
	} else {
		for _, lib := range libs {
			reasons = append(reasons, fmt.Sprintf("has DT_NEEDED %s", lib))
		}
	}
	bi, err := buildinfo.ReadFile(path)
	if err != nil {
		// 非 Go 产物或读不出构建设置：无法证明 CGO_ENABLED=0，按缺失处理。
		return append(reasons, "missing CGO_ENABLED build setting")
	}
	return append(reasons, cgoReason(bi)...)
}

func cgoReason(bi *buildinfo.BuildInfo) []string {
	if bi == nil {
		return []string{"missing CGO_ENABLED build setting"}
	}
	for _, s := range bi.Settings {
		if s.Key != "CGO_ENABLED" {
			continue
		}
		if s.Value != "0" {
			return []string{fmt.Sprintf("built with CGO_ENABLED=%s", s.Value)}
		}
		return nil
	}
	// 设置缺失不等于 0，不能放行。
	return []string{"missing CGO_ENABLED build setting"}
}

// run 是可测的 CLI 入口；空文件清单等价于"全部通过"，必须在入口拒绝。
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: checkstatic <file>...")
		return 2
	}
	status := 0
	for _, path := range args {
		for _, reason := range check(path) {
			fmt.Fprintf(stdout, "%s: %s\n", path, reason)
			status = 1
		}
	}
	return status
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
