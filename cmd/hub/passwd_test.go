package main

import (
	"context"
	"errors"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xjetry/heron/internal/hub/auth"
)

func pipeWith(t *testing.T, content string) *os.File {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() { io.WriteString(w, content); w.Close() }()
	t.Cleanup(func() { r.Close() })
	return r
}

func TestPasswdReadsOneLineFromNonTerminalStdin(t *testing.T) {
	t.Parallel()
	db := filepath.Join(t.TempDir(), "t.db")
	if err := runPasswdWith([]string{"--db", db}, pipeWith(t, "a sufficiently long password\n"), io.Discard); err != nil {
		t.Fatal(err)
	}
	st, a, err := openOffline(db, false)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	phc, found, err := st.AdminPasswordHash(context.Background())
	if err != nil || !found || !strings.HasPrefix(phc, "$argon2id$") {
		t.Fatalf("stored password is not argon2id: found=%v err=%v", found, err)
	}
	if _, err := a.Login(context.Background(), "a sufficiently long password", netip.MustParseAddr("127.0.0.1")); err != nil {
		t.Fatalf("login with the password just set: %v", err)
	}
}

func TestPasswdListsTokensWithoutPromptingOnAPipe(t *testing.T) {
	t.Parallel()
	db := filepath.Join(t.TempDir(), "t.db")
	st, a, err := openOffline(db, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.CreateAPIToken(context.Background(), "ci", nil); err != nil {
		t.Fatal(err)
	}
	st.Close()
	var prompt strings.Builder
	if err := runPasswdWith([]string{"--db", db}, pipeWith(t, "a sufficiently long password\n"), &prompt); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(prompt.String(), "[y/N]") || !strings.Contains(prompt.String(), "API tokens are not revoked") {
		t.Fatalf("pipe review: %q", prompt.String())
	}
	st, _, err = openOffline(db, false)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	list, err := st.ListAPITokens(context.Background())
	if err != nil || len(list) != 1 {
		t.Fatalf("token after passwd: %+v %v", list, err)
	}
}

func TestPasswdRejectsShortPassword(t *testing.T) {
	t.Parallel()
	db := filepath.Join(t.TempDir(), "t.db")
	err := runPasswdWith([]string{"--db", db}, pipeWith(t, "short\n"), io.Discard)
	if !errors.Is(err, auth.ErrWeakPassword) {
		t.Fatalf("err = %v, want ErrWeakPassword", err)
	}
}

func TestReadPasswordLineBoundaries(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, input, want string }{
		{"one-line", "first line\nignored line\n", "first line"},
		{"CRLF-and-spaces", "  前后保留空白的密码  \r\n", "  前后保留空白的密码  "},
		{"EOF", "no final newline", "no final newline"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readPassword(pipeWith(t, tc.input), io.Discard)
			if err != nil || got != tc.want {
				t.Fatalf("password line=%q err=%v want=%q", got, err, tc.want)
			}
		})
	}
}

// docker exec 不带 -i 时容器里的 stdin 就是 /dev/null：读到立即 EOF，这不是"密码太短"。
func TestReadPasswordFromDevNullIsMissingInput(t *testing.T) {
	t.Parallel()
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	if _, err := readPassword(devnull, io.Discard); !errors.Is(err, errNoPasswordInput) {
		t.Fatalf("stdin /dev/null: err = %v, want errNoPasswordInput", err)
	}
}

// 空行是输入了一个空密码，由 SetPassword 按长度拒绝，不归入"没有输入"。
func TestReadPasswordEmptyLineIsAnEmptyPassword(t *testing.T) {
	t.Parallel()
	got, err := readPassword(pipeWith(t, "\n"), io.Discard)
	if err != nil || got != "" {
		t.Fatalf("empty line: password=%q err=%v, want an empty password and no error", got, err)
	}
}

func TestPasswdWithoutInputLeavesNoDatabase(t *testing.T) {
	t.Parallel()
	db := filepath.Join(t.TempDir(), "t.db")
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	if err := runPasswdWith([]string{"--db", db}, devnull, io.Discard); !errors.Is(err, errNoPasswordInput) {
		t.Fatalf("err = %v, want errNoPasswordInput", err)
	}
	if _, err := os.Stat(db); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("passwd without input touched %s (stat err = %v)", db, err)
	}
}
