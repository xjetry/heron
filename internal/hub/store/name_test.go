package store

import (
	"errors"
	"strings"
	"testing"
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
