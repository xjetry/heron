package main

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xjetry/heron/internal/hub/store"
)

func seedTokens(t *testing.T, db string, names ...string) []store.APIToken {
	t.Helper()
	st, a, err := openOffline(db, true)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var out []store.APIToken
	for _, n := range names {
		tok, _, err := a.CreateAPIToken(context.Background(), n, nil)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, tok)
	}
	return out
}

func TestTokenListAndRevoke(t *testing.T) {
	t.Parallel()
	db := filepath.Join(t.TempDir(), "hub.db")
	toks := seedTokens(t, db, "ci", "laptop")
	var out, errOut bytes.Buffer
	if err := runTokenWith([]string{"list", "--db", db}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"ID", "NAME", "LAST USED", "ci", "laptop", "never"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("list output lacks %q:\n%s", s, out.String())
		}
	}
	out.Reset()
	if err := runTokenWith([]string{"revoke", "--db", db, "--id", fmt.Sprint(toks[0].ID)}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if err := runTokenWith([]string{"revoke", "--db", db, "--id", fmt.Sprint(toks[0].ID)}, &out, &errOut); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("revoking twice: %v", err)
	}
	if err := runTokenWith([]string{"revoke", "--db", db, "--all"}, &out, &errOut); err != nil || !strings.Contains(out.String(), "revoked 1") {
		t.Fatalf("revoke --all: %v %q", err, out.String())
	}
	for _, bad := range [][]string{{"revoke", "--db", db}, {"revoke", "--db", db, "--all", "--id", "1"}, {"frobnicate", "--db", db}, {}} {
		if err := runTokenWith(bad, &out, &errOut); err == nil {
			t.Errorf("%v: no error", bad)
		}
	}
}

func TestReviewAPITokensAfterPasswordChange(t *testing.T) {
	t.Parallel()
	db := filepath.Join(t.TempDir(), "hub.db")
	seedTokens(t, db, "ci")
	st, _, err := openOffline(db, false)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	var w bytes.Buffer
	// 非终端（管道、容器初始化）：只列出与提示，不提问、不吊销。
	if err := reviewAPITokens(ctx, st, &w, nil, db); err != nil {
		t.Fatal(err)
	}
	if list, _ := st.ListAPITokens(ctx); len(list) != 1 || !strings.Contains(w.String(), "ci") || !strings.Contains(w.String(), "heron-hub token revoke --all") {
		t.Fatalf("non-interactive review: %d tokens left, output %q", len(list), w.String())
	}
	w.Reset()
	if err := reviewAPITokens(ctx, st, &w, func() (bool, error) { return false, nil }, db); err != nil {
		t.Fatal(err)
	}
	if list, _ := st.ListAPITokens(ctx); len(list) != 1 {
		t.Fatal("answering no revoked tokens")
	}
	if err := reviewAPITokens(ctx, st, &w, func() (bool, error) { return true, nil }, db); err != nil {
		t.Fatal(err)
	}
	if list, _ := st.ListAPITokens(ctx); len(list) != 0 {
		t.Fatal("answering yes kept tokens")
	}
	w.Reset()
	asked := false
	if err := reviewAPITokens(ctx, st, &w, func() (bool, error) { asked = true; return true, nil }, db); err != nil || asked || w.Len() != 0 {
		t.Fatalf("no tokens: asked=%v output %q err %v", asked, w.String(), err)
	}
}

func TestReviewAPITokensQuotesDBPathForShell(t *testing.T) {
	t.Parallel()
	db := filepath.Join(t.TempDir(), "hub.db")
	seedTokens(t, db, "ci")
	st, _, err := openOffline(db, false)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var w bytes.Buffer
	// 提示按字面粘贴。空格与单引号都必须留在单引号引用里，不能靠 Go 的 %q。
	path := "/var/lib/o'brien hub/heron.db"
	if err := reviewAPITokens(context.Background(), st, &w, nil, path); err != nil {
		t.Fatal(err)
	}
	want := "to revoke them: heron-hub token revoke --all --db '/var/lib/o'\\''brien hub/heron.db'\n"
	if !strings.Contains(w.String(), want) {
		t.Fatalf("revoke hint %q, want substring %q", w.String(), want)
	}
}
