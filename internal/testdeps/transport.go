package testdeps

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
)

// 为什么测试的 HTTP 客户端必须带自己的 Transport：标准库 httptest.Server.Close 会对进程共享的 http.DefaultTransport
// 调 CloseIdleConnections（net/http/httptest 的 Server.Close）；Go 1.27.2 的 Transport.CloseIdleConnections 关掉池里
// 全部空闲连接，并在下一个请求来取空闲连接之前，把新变空闲的连接直接关掉（net/http 的 transport.go，closeIdle）。
// 用例并行时，别的用例收尾关掉自己的 httptest 服务，本用例经 DefaultTransport 正要复用的连接就可能被关，请求报
// "HTTP/1.x transport connection broken: http: CloseIdleConnections called"。所以并行的包里，测试发出的每个客户端
// 都用夹具拥有的 Transport：对 httptest 服务用 srv.Client().Transport（随该服务的 Close 一起关），对真实监听的 hub
// 用 OwnedTransport；RequireOwnedTransports 在源码上守着这一条。

// OwnedTransport 返回一个只属于 t 的 Transport：设置克隆自 http.DefaultTransport（代理取自环境、拨号与空闲超时相同），
// 连接池独立，别的用例对 DefaultTransport 的 CloseIdleConnections 碰不到它；用例结束时关掉它的空闲连接。
func OwnedTransport(t testing.TB) *http.Transport {
	t.Helper()
	tr := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(tr.CloseIdleConnections)
	return tr
}

// RequireOwnedTransports 扫描 dir 下的全部 *_test.go，对经进程共享 Transport 发请求的写法逐处报错（文件:行）：
//   - 引用 http.DefaultClient；
//   - 调用包级的 http.Get、http.Head、http.Post、http.PostForm（它们经 http.DefaultClient）；
//   - 引用 http.DefaultTransport，作为 Clone() 接收者的除外（克隆出的 Transport 有自己的连接池）；
//   - 没有 Transport 字段的 http.Client 复合字面量、new(http.Client) 与没有初值的 http.Client 变量（零值的 Transport
//     就是 DefaultTransport）。
//
// 按 import 的实际名字识别 net/http，别名导入同样受查；没有导入 net/http 的文件不受影响。
func RequireOwnedTransports(t *testing.T, dir string) {
	t.Helper()
	findings, err := SharedTransportUses(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		t.Errorf("%s; give the client a Transport the test owns (srv.Client().Transport or testdeps.OwnedTransport)", f)
	}
}

// SharedTransportUses 是 RequireOwnedTransports 的检查本体，返回按位置排序的 "文件:行: 说明"。
func SharedTransportUses(dir string) ([]string, error) {
	names, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no *_test.go files in %s", dir)
	}
	fset := token.NewFileSet()
	var findings []finding
	for _, name := range names {
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			return nil, err
		}
		findings = append(findings, sharedTransportUses(fset, file)...)
	}
	sort.Slice(findings, func(i, j int) bool {
		a, b := findings[i].pos, findings[j].pos
		if a.Filename != b.Filename {
			return a.Filename < b.Filename
		}
		return a.Line < b.Line
	})
	out := make([]string, len(findings))
	for i, f := range findings {
		out[i] = fmt.Sprintf("%s:%d: %s", filepath.Base(f.pos.Filename), f.pos.Line, f.what)
	}
	return out, nil
}

type finding struct {
	pos  token.Position
	what string
}

func sharedTransportUses(fset *token.FileSet, file *ast.File) []finding {
	httpName := ""
	for _, imp := range file.Imports {
		if path, err := strconv.Unquote(imp.Path.Value); err == nil && path == "net/http" {
			httpName = "http"
			if imp.Name != nil {
				httpName = imp.Name.Name
			}
		}
	}
	if httpName == "" || httpName == "_" || httpName == "." {
		return nil
	}
	isHTTP := func(e ast.Expr, sel string) bool {
		s, ok := e.(*ast.SelectorExpr)
		if !ok || s.Sel.Name != sel {
			return false
		}
		id, ok := s.X.(*ast.Ident)
		return ok && id.Name == httpName && id.Obj == nil
	}
	// cloned 收集作为 Clone() 接收者出现的 http.DefaultTransport（可以隔一层类型断言）。
	cloned := map[ast.Expr]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Clone" {
			return true
		}
		recv := sel.X
		if ta, ok := recv.(*ast.TypeAssertExpr); ok {
			recv = ta.X
		}
		if isHTTP(recv, "DefaultTransport") {
			cloned[recv] = true
		}
		return true
	})
	var out []finding
	add := func(n ast.Node, what string) { out = append(out, finding{fset.Position(n.Pos()), what}) }
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			switch {
			case isHTTP(n, "DefaultClient"):
				add(n, "http.DefaultClient sends through the shared http.DefaultTransport")
			case isHTTP(n, "DefaultTransport") && !cloned[n]:
				add(n, "http.DefaultTransport is shared by the whole test process")
			}
		case *ast.CallExpr:
			for _, fn := range []string{"Get", "Head", "Post", "PostForm"} {
				if isHTTP(n.Fun, fn) {
					add(n, "http."+fn+" sends through http.DefaultClient")
				}
			}
			if id, ok := n.Fun.(*ast.Ident); ok && id.Name == "new" && id.Obj == nil && len(n.Args) == 1 && isHTTP(n.Args[0], "Client") {
				add(n, "new(http.Client) has no Transport and falls back to http.DefaultTransport")
			}
		case *ast.CompositeLit:
			if isHTTP(n.Type, "Client") && !hasTransport(n) {
				add(n, "http.Client literal without Transport falls back to http.DefaultTransport")
			}
		case *ast.ValueSpec:
			if n.Type != nil && isHTTP(n.Type, "Client") && len(n.Values) == 0 {
				add(n, "zero http.Client has no Transport and falls back to http.DefaultTransport")
			}
		}
		return true
	})
	return out
}

// hasTransport 判断 http.Client 字面量是否给了 Transport：键值写法看有没有 Transport 键；按位置写法的第一个字段就是
// Transport。
func hasTransport(lit *ast.CompositeLit) bool {
	for i, e := range lit.Elts {
		kv, ok := e.(*ast.KeyValueExpr)
		if !ok {
			return i == 0
		}
		if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "Transport" {
			return true
		}
	}
	return false
}
