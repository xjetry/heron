package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCleanName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, raw, want, message string }{
		{"trim-and-controls", " \x01 a\u202e b \x7f\t", "a b", ""},
		{"one-rune", "名", "名", ""},
		{"unicode-limit", strings.Repeat("名", 64), strings.Repeat("名", 64), ""},
		{"empty", " \t\n ", "", "name must be 1–64 characters after trimming whitespace and control characters; got 0"},
		{"too-long", strings.Repeat("名", 65), "", "name must be 1–64 characters after trimming whitespace and control characters; got 65"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CleanName(tc.raw)
			if got != tc.want {
				t.Errorf("CleanName = %q, want %q", got, tc.want)
			}
			if tc.message == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				var nameError NameError
				if !errors.As(err, &nameError) || err.Error() != tc.message {
					t.Fatalf("CleanName error = %v, want NameError %q", err, tc.message)
				}
			}
		})
	}
}

func TestAgentNodeName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, raw, want string }{
		{"unchanged", "kept", "kept"},
		{"clean-before-truncate", strings.Repeat("\x01", 70) + strings.Repeat("名", 65), strings.Repeat("名", 64)},
		{"trim-after-truncate", strings.Repeat("界", 63) + " x", strings.Repeat("界", 63)},
		{"unicode-whitespace", "\u3000node\u3000", "node"},
		{"empty", " \t\n ", "node"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := AgentNodeName(tc.raw)
			if got != tc.want {
				t.Errorf("AgentNodeName = %q, want %q", got, tc.want)
			}
			clean, err := CleanName(got)
			if err != nil || clean != got {
				t.Fatalf("AgentNodeName result %q is not canonical: CleanName = %q, %v", got, clean, err)
			}
		})
	}
}

// uncleanNameCase 是写入口准入（checkNodeName）的判定用例：每个 raw 都不等于自己的 CleanName 结果。
// want 是拒绝文案原文（NameError/UncleanNameError 的 Error()）——api 层逐字透传为 InvalidArgument，CLI 原样输出。
var uncleanNameCases = []struct {
	name        string
	raw         string
	want        string
	wantNameErr bool // true 期望 NameError（长度），false 期望 UncleanNameError（可清洗的非规范形）
}{
	{"too-long", strings.Repeat("名", MaxNameRunes+1), "name must be 1–64 characters after trimming whitespace and control characters; got 65", true},
	{"blank", " \t\n ", "name must be 1–64 characters after trimming whitespace and control characters; got 0", true},
	{"padded", " padded ", `name must not have leading or trailing whitespace or control characters; got " padded "`, false},
	{"control", "a\x01b", `name must not have leading or trailing whitespace or control characters; got "a\x01b"`, false},
	{"bidi", "\u2066x\u2069", `name must not have leading or trailing whitespace or control characters; got "\u2066x\u2069"`, false},
}

func checkUncleanNameErr(t *testing.T, tc struct {
	name        string
	raw         string
	want        string
	wantNameErr bool
}, err error) {
	t.Helper()
	if err == nil || err.Error() != tc.want {
		t.Fatalf("%s: err=%v, want %q", tc.name, err, tc.want)
	}
	var nameErr NameError
	var unclean UncleanNameError
	if tc.wantNameErr {
		if !errors.As(err, &nameErr) {
			t.Fatalf("%s: %T is not a NameError", tc.name, err)
		}
	} else if !errors.As(err, &unclean) {
		t.Fatalf("%s: %T is not an UncleanNameError", tc.name, err)
	}
}

func nodeCount(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.r.QueryRow("SELECT count(*) FROM node").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCreateNodeRejectsUncleanNames(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	for _, tc := range uncleanNameCases {
		_, _, err := s.CreateNode(t.Context(), NewNode{Name: tc.raw}, hash(1))
		checkUncleanNameErr(t, tc, err)
	}
	if got := nodeCount(t, s); got != 0 {
		t.Fatalf("rejected names still created %d nodes", got)
	}
}

func TestUpdateNodeRejectsUncleanNames(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	id, _, err := s.CreateNode(t.Context(), NewNode{Name: "keep"}, hash(1))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range uncleanNameCases {
		_, err := s.UpdateNode(t.Context(), id, NodeEdit{Name: tc.raw})
		checkUncleanNameErr(t, tc, err)
	}
	n, err := s.GetNode(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if n.Name != "keep" {
		t.Fatalf("rejected updates still renamed the node to %q", n.Name)
	}
}

func TestRegisterNodeRejectsUncleanNames(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	for i, tc := range uncleanNameCases {
		keyHash := hash(byte(10 + i))
		if err := s.SetRegisterWindow(t.Context(), keyHash, clk.Now().Add(time.Hour), 1); err != nil {
			t.Fatal(err)
		}
		_, _, err := s.RegisterNode(t.Context(), keyHash, tc.raw, hash(byte(60+i)))
		checkUncleanNameErr(t, tc, err)
	}
	if got := nodeCount(t, s); got != 0 {
		t.Fatalf("rejected names still registered %d nodes", got)
	}
	// 准入只拒绝未清洗的值：agent 口径（AgentNodeName）规范化后的注册照常成功，超长已截到 64 个字符。
	keyHash := hash(99)
	if err := s.SetRegisterWindow(t.Context(), keyHash, clk.Now().Add(time.Hour), 1); err != nil {
		t.Fatal(err)
	}
	id, _, err := s.RegisterNode(t.Context(), keyHash, AgentNodeName(strings.Repeat("名", MaxNameRunes+1)), hash(98))
	if err != nil {
		t.Fatal(err)
	}
	n, err := s.GetNode(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.Repeat("名", MaxNameRunes); n.Name != want {
		t.Fatalf("registered name = %q, want %q", n.Name, want)
	}
}
