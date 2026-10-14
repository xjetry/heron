package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSymbolOwner 钉住符号名到模块的归属规则，覆盖泛型实例（类型参数里的包路径不能抢走归属）、
// 生成代码单列、标准库里 vendor 的 x/net 与独立依赖的 x/net 区分、没有包路径的汇编与类型辅助函数。
func TestSymbolOwner(t *testing.T) {
	const main = "github.com/xjetry/heron"
	mods := []string{main, "connectrpc.com/connect", "google.golang.org/protobuf", "golang.org/x/net", "golang.org/x/sys"}
	gen := main + "/gen (generated protobuf + connect stubs)"
	cases := map[string]string{
		"connectrpc.com/connect.(*Client[go.shape.struct {}]).CallUnary":            "connectrpc.com/connect",
		"google.golang.org/protobuf/internal/impl.fieldCoder":                       "google.golang.org/protobuf",
		"github.com/xjetry/heron/gen/heron/v1.init":                                 gen,
		"github.com/xjetry/heron/gen/heron/v1/heronv1connect.NewAgentServiceClient": gen,
		"github.com/xjetry/heron/internal/agent/client.(*Runner).Run":               main,
		"main.main": main + " (cmd)",
		"vendor/golang.org/x/net/idna.idnaValues":                                                               "std",
		"net/http.(*Transport).dialConn":                                                                        "std",
		"golang.org/x/net/http2.(*Framer).ReadFrame":                                                            "golang.org/x/net",
		"sync/atomic.(*Pointer[go.shape.struct { connectrpc.com/connect.x int }]).Load":                         "std",
		"internal/sync..dict.HashTrieMap[*internal/abi.Type,interface {}]":                                      "std",
		"unique..dict.canonMap[connectrpc.com/connect.key]":                                                     "std",
		"type:*connectrpc.com/connect.Error":                                                                    "connectrpc.com/connect",
		"go:itab.*connectrpc.com/connect.errorTranslatingClientConn,connectrpc.com/connect.StreamingClientConn": "connectrpc.com/connect",
		"type:.eq.SIISIISIISIIM16":                                                                              noPackage,
		"_rt0_arm64_linux":                                                                                      noPackage,
		"go:func.*":                                                                                             "(linker-synthesized go:*)",
	}
	for name, want := range cases {
		if got := symbolOwner(name, main, mods); got != want {
			t.Errorf("symbolOwner(%q) = %q, want %q", name, got, want)
		}
	}
}

// TestFileSectionsFitInTheFile：段在文件里占的字节加起来不能超过文件本身。Go 链接器把 ELF 的 DWARF 段压缩存放，
// 取成解压后的大小就会超出。被测的是交叉编译出来的真实 linux 二进制（gofmt，只依赖标准库，不需要网络）。
func TestFileSectionsFitInTheFile(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "gofmt")
	cmd := exec.Command("go", "build", "-o", bin, "cmd/gofmt")
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	st, err := os.Stat(bin)
	if err != nil {
		t.Fatal(err)
	}
	secs, err := fileSections(bin)
	if err != nil {
		t.Fatal(err)
	}
	var total uint64
	debug := false
	for _, s := range secs {
		total += s.size
		debug = debug || strings.HasPrefix(s.name, ".debug_")
	}
	if !debug {
		t.Fatal("the test binary has no DWARF sections; the check below would prove nothing about them")
	}
	if total > uint64(st.Size()) {
		t.Fatalf("sections add up to %d bytes, the file has %d", total, st.Size())
	}
}

// TestSizesWithoutSymbolTable：发布产物以 -s -w 链接，没有符号表。sizes 对它必须明说没有符号表、只给段大小，
// 不能给出一张空的或残缺的按模块表；同一程序保留符号时照常给出表。ELF 与 Mach-O 各测一次：Mach-O 去符号后
// go tool nm 照常退出 0，只列动态导入，最容易静默地出空表。
func TestSizesWithoutSymbolTable(t *testing.T) {
	for _, platform := range [][2]string{{"linux", "amd64"}, {"darwin", "arm64"}} {
		for _, stripped := range []bool{false, true} {
			bin := filepath.Join(t.TempDir(), "gofmt")
			args := []string{"build", "-trimpath", "-o", bin}
			if stripped {
				args = append(args, "-ldflags=-s -w")
			}
			cmd := exec.Command("go", append(args, "cmd/gofmt")...)
			cmd.Env = append(os.Environ(), "GOOS="+platform[0], "GOARCH="+platform[1], "CGO_ENABLED=0")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("go build %v: %v\n%s", platform, err, out)
			}
			var out bytes.Buffer
			if err := runSizes([]string{"-bin", bin}, &out); err != nil {
				t.Fatalf("%v stripped=%v: %v\n%s", platform, stripped, err, out.String())
			}
			got := out.String()
			table := strings.Contains(got, "| owner |")
			missing := strings.Contains(got, "no symbol table")
			if !strings.Contains(got, "sections, bytes in the file") {
				t.Errorf("%v stripped=%v: no section sizes:\n%s", platform, stripped, got)
			}
			if stripped && (table || !missing) {
				t.Errorf("%v stripped: want the missing symbol table stated and no module table:\n%s", platform, got)
			}
			if !stripped && (!table || missing || !strings.Contains(got, "| std |")) {
				t.Errorf("%v with symbols: want a module table with a std row:\n%s", platform, got)
			}
		}
	}
}
