package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hub 的 TCP 监听必须是普通 TCP：Go 在 Linux 上默认给监听请求 MPTCP，连接层的写侧上界只在普通 TCP 上实测过。
func TestListenConfigDisablesMultipathTCP(t *testing.T) {
	t.Parallel()
	if listenConfig().MultipathTCP() {
		t.Fatal("hub listen config requests MPTCP")
	}
}

// 每个 TCP 监听都经 listenTCP，新加的监听不能绕开它直接调 net.Listen 或零值 ListenConfig 而回到 Go 的默认。
func TestTCPListenersGoThroughListenTCP(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	scanned := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		scanned++
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "net" {
				return true
			}
			if sel.Sel.Name == "Listen" || sel.Sel.Name == "ListenTCP" || sel.Sel.Name == "ListenConfig" {
				if fn := enclosingFunc(f, sel.Pos()); fn != "listenConfig" {
					t.Errorf("%s: net.%s used in %s; TCP listeners must go through listenTCP", fset.Position(sel.Pos()), sel.Sel.Name, fn)
				}
			}
			return true
		})
	}
	if scanned == 0 {
		t.Fatal("scanned no source files")
	}
}

func enclosingFunc(f *ast.File, pos token.Pos) string {
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Pos() <= pos && pos < fd.End() {
			return fd.Name.Name
		}
	}
	return "<package scope>"
}
