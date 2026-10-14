package api

import "testing"

func TestRegexpBudgetSample(t *testing.T) {
	t.Parallel()
	t.Run("accent", func(t *testing.T) {
		sample := regexpBudgetSample(accentRE.String())
		if !accentRE.MatchString(sample) {
			t.Errorf("accent budget sample %q does not match %s", sample, accentRE)
		}
		walkSettings(func(field settingsBudgetField) {
			if field.path == "accent_color" {
				want, _ := field.entry.boundary(field.fd)
				if got := jsonStringBytes(sample); got != want {
					t.Errorf("accent regexp sample JSON bytes=%d, registered budget=%d", got, want)
				}
			}
		})
	})
	t.Run("finite", func(t *testing.T) {
		for _, tc := range []struct{ pattern, want string }{
			{`^(a|bcd)?x{2}$`, "bcdxx"},
		} {
			if got := regexpBudgetSample(tc.pattern); got != tc.want {
				t.Errorf("regexp %q sample=%q, want %q", tc.pattern, got, tc.want)
			}
		}
	})
	t.Run("unbounded", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Error("unbounded regexp a* accepted without a budget model")
			}
		}()
		regexpBudgetSample(`a*`)
	})
}
