package store

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
)

// 同一枚 token 依次处于"不过期 / 恰到失效时刻 / 复活"，四个读者对它的放行必须一致：入口与事务内重读直接问，
// 注册窗口认领与更新下发用它开的窗口与排队的更新去试——后两者不经过该 token 的请求，入口判过一次替代不了它们。
func TestTokenExpiryAppliesToEveryReader(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	h := sha256.Sum256([]byte("agent"))
	grant := &TokenGrant{AllNodes: true, Permissions: []Permission{PermissionRegister, PermissionUpdate}}
	tok, err := s.CreateAPIToken(t.Context(), "agent", h, clk.Now(), 100, grant)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithPrincipal(t.Context(), tok)
	if err := s.SetRegisterWindow(ctx, hash(2), clk.Now().Add(24*time.Hour), 100); err != nil {
		t.Fatal(err)
	}
	probes := 0
	readers := []struct {
		name    string
		allowed func() bool
	}{
		{"request entry", func() bool {
			_, state, err := s.APITokenByHash(t.Context(), h)
			if err != nil {
				t.Fatal(err)
			}
			return state == TokenLive
		}},
		{"change reader", func() bool {
			err := s.write(t.Context(), func(tx *sql.Tx) error {
				_, err := s.changeReader(tx).APIToken(tok.ID)
				return err
			})
			if err != nil && !errors.Is(err, ErrNotFound) {
				t.Fatal(err)
			}
			return err == nil
		}},
		{"register window", func() bool {
			probes++
			_, _, err := s.RegisterNode(t.Context(), hash(2), fmt.Sprintf("probe-%d", probes), hash(byte(10+probes)))
			if err != nil && !errors.Is(err, ErrNoWindow) {
				t.Fatal(err)
			}
			return err == nil
		}},
		{"update dispatch", func() bool {
			probes++
			node, _, err := s.CreateNode(t.Context(), NewNode{Name: fmt.Sprintf("update-%d", probes)}, hash(byte(100+probes)))
			if err != nil {
				t.Fatal(err)
			}
			c := &Change{Operation: Operation{OwnerID: tok.ID, RequestID: fmt.Sprintf("update-%d", probes), RequestHash: "x", ResourceID: node}, Action: ActionStartUpdate, Policy: permitPolicy{}}
			if c.ExpectedVersion, err = s.ChangeVersion(ctx, c); err != nil {
				t.Fatal(err)
			}
			status := &heronv1.UpdateStatus{Supported: true, Task: &heronv1.UpdateTask{Id: fmt.Sprintf("task-%d", probes), State: "queued"}}
			if err := s.StartNodeUpdate(WithChange(ctx, c), node, status); err != nil {
				t.Fatal(err)
			}
			status.Task.State = "dispatched"
			if err := s.SaveNodeUpdate(t.Context(), node, status); err != nil {
				t.Fatal(err)
			}
			return status.Task.State == "dispatched"
		}},
	}
	for _, step := range []struct {
		name    string
		expires func() time.Time
		advance time.Duration
		want    bool
	}{
		{"never expires", func() time.Time { return time.Time{} }, 0, true},
		{"at the expiry instant", func() time.Time { return clk.Now().Add(time.Minute) }, time.Minute, false},
		{"revived", func() time.Time { return clk.Now().Add(time.Hour) }, 0, true},
	} {
		if _, err := s.UpdateAPIToken(t.Context(), tok.ID, tok.Name, grant, step.expires()); err != nil {
			t.Fatal(err)
		}
		clk.Advance(step.advance)
		for _, r := range readers {
			if got := r.allowed(); got != step.want {
				t.Errorf("%s / %s: allowed = %v, want %v", step.name, r.name, got, step.want)
			}
		}
	}
}

// 零值时刻会让每一枚 token 都显得未过期；按错误拒绝，不放行。
func TestLookupAPITokenRejectsZeroTime(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	tok, err := s.CreateAPIToken(t.Context(), "agent", sha256.Sum256([]byte("agent")), clk.Now(), 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := lookupAPIToken(s.r.QueryRow, "id = ?", tok.ID, time.Time{}); err == nil {
		t.Fatal("zero time accepted")
	}
	if _, err := (txReader{}).APIToken(tok.ID); err == nil {
		t.Fatal("txReader without a clock reading accepted")
	}
}

// lookupAPIToken 是 token 有效性的唯一判定：scanAPIToken 只能出现在它与 ListAPITokens（列表要展示过期的 token）里，
// 别处直接扫行就绕开了过期判定。先确认两处合法用法都被扫到，再判别处没有——扫描本身没跑成时不能当作通过。
func TestScanAPITokenOnlyInLookupAndList(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"lookupAPIToken": true, "ListAPITokens": true, "scanAPIToken": true}
	seen := map[string]int{}
	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && id.Name == "scanAPIToken" && id.Pos() != fn.Name.Pos() {
					seen[fn.Name.Name]++
					if !allowed[fn.Name.Name] {
						t.Errorf("%s: %s reads api_token rows with scanAPIToken; go through lookupAPIToken", fset.Position(id.Pos()), fn.Name.Name)
					}
				}
				return true
			})
		}
	}
	if seen["lookupAPIToken"] == 0 || seen["ListAPITokens"] == 0 {
		t.Fatalf("scan did not find the legitimate uses (%v); it is not looking at the store sources", seen)
	}
}
