// stampinstall 是发布目标（release-full / release-hub-only）调用发布写入的入口：把 -dir 下全部 tar 包的 SHA-256 与 -version 写进每个源码
// 安装脚本，输出到 -dir。写入的实现在 deploy/releasestamp，deploy 的脚本测试调用的是同一个函数。
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/xjetry/heron/deploy/releasestamp"
)

func run(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("stampinstall", flag.ContinueOnError)
	fs.SetOutput(stderr)
	version := fs.String("version", "", "release version to embed")
	dir := fs.String("dir", "", "directory holding the release tar packages; stamped scripts are written here")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *version == "" || *dir == "" || fs.NArg() == 0 {
		fmt.Fprintln(stderr, "usage: stampinstall -version VERSION -dir DIR SCRIPT...")
		return 2
	}
	if err := releasestamp.WriteDir(*version, *dir, fs.Args()...); err != nil {
		fmt.Fprintln(stderr, "stampinstall:", err)
		return 1
	}
	return 0
}

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}
