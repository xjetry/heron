package api

import (
	"testing"
	"unicode/utf8"
)

// logo 的预算从 checkLogo 的接受集推出，成立的前提是 checkLogo 只经 logoPrefix 与 isBase64Char 接受值。
// 前缀集或字母表放宽时上界自动变大；这里另把"当前上界等于上限加引号"钉住，让放宽在用例里可见。
func TestLogoBudgetEncodingContract(t *testing.T) {
	t.Parallel()
	t.Run("derived_budget", func(t *testing.T) {
		got, sample := logoBoundary(maxLogoBytes)
		if want := maxLogoBytes + 2; got != want {
			t.Errorf("logo budget derived from checkLogo = %d, want %d (prefixes and alphabet need no JSON escaping)", got, want)
		}
		if s, ok := sample.(string); !ok || len(s) != maxLogoBytes || jsonStringBytes(s) != got {
			t.Errorf("logo boundary sample: %d bytes, JSON %d, want %d bytes encoding to %d", len(sample.(string)), jsonStringBytes(sample.(string)), maxLogoBytes, got)
		}
	})
	t.Run("alphabet", func(t *testing.T) {
		for r := rune(0); r <= utf8.MaxRune; r++ {
			if !isBase64Char(r) {
				continue
			}
			value := string(r)
			if got, want := jsonStringBytes(value), len(value)+2; got != want {
				t.Errorf("logo alphabet U+%04X JSON bytes=%d, want unescaped bytes=%d", r, got, want)
			}
		}
	})
	// 接受集不得越出前缀集 × 字母表：对每个前缀，字母表外的每个字节都必须被拒；不经 ;base64, 的写法也必须被拒。
	// 正向对照确认前缀集本身被接受，用例不是空转。
	t.Run("accept_set", func(t *testing.T) {
		for _, mediaType := range logoTypes {
			prefix := logoPrefix(mediaType)
			if err := checkLogo(prefix + "AAAA"); err != nil {
				t.Fatalf("checkLogo rejected %q: %v", prefix+"AAAA", err)
			}
			for b := 0; b < 256; b++ {
				if isBase64Char(rune(b)) {
					continue
				}
				if err := checkLogo(prefix + "AAA" + string([]byte{byte(b)})); err == nil {
					t.Errorf("checkLogo accepted byte 0x%02X outside the base64 alphabet after %q", b, prefix)
				}
			}
			for _, raw := range []string{"data:" + mediaType + ",", "data:" + mediaType + ";utf8,", "data:" + mediaType + ";charset=utf-8,"} {
				if err := checkLogo(raw + "<svg/>"); err == nil {
					t.Errorf("checkLogo accepted %q, a payload outside the base64 grammar", raw+"<svg/>")
				}
			}
		}
	})
}
