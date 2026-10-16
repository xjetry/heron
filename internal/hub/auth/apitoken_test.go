package auth

import (
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/testwait"
)

func TestNewAPITokenFormat(t *testing.T) {
	plain, hash := NewAPIToken()
	if !strings.HasPrefix(plain, APITokenPrefix) || len(plain) != len(APITokenPrefix)+64 {
		t.Fatalf("token %q: want %q + 64 hex digits", plain, APITokenPrefix)
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(plain, APITokenPrefix)); err != nil {
		t.Fatalf("suffix is not hex: %v", err)
	}
	if hash != HashToken(plain) {
		t.Fatal("hash must cover the whole string including the prefix")
	}
}

func TestAuthenticateAPIToken(t *testing.T) {
	a, st, _ := setup(t)
	ctx := t.Context()
	tok, plain, err := a.CreateAPIToken(ctx, "ci", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name  string
		plain string
		want  bool
	}{
		{"valid", plain, true},
		{"unknown", APITokenPrefix + strings.Repeat("0", 64), false},
		{"no prefix", strings.TrimPrefix(plain, APITokenPrefix), false},
		{"empty", "", false},
	} {
		if _, state, err := a.AuthenticateAPIToken(ctx, c.plain); err != nil || (state == store.TokenLive) != c.want {
			t.Errorf("%s: state=%v err=%v, want live=%v", c.name, state, err, c.want)
		}
	}
	if _, err := st.DeleteAPIToken(ctx, tok.ID); err != nil {
		t.Fatal(err)
	}
	if _, state, err := a.AuthenticateAPIToken(ctx, plain); err != nil || state != store.TokenMissing {
		t.Fatalf("revoked token accepted: %v %v", state, err)
	}
}

func TestCreateAPITokenEnforcesLimit(t *testing.T) {
	a, _, _ := setup(t)
	for i := 0; i < MaxAPITokens; i++ {
		if _, _, err := a.CreateAPIToken(t.Context(), "n", nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := a.CreateAPIToken(t.Context(), "n", nil); !errors.Is(err, store.ErrAPITokenLimit) {
		t.Fatalf("token %d: %v, want ErrAPITokenLimit", MaxAPITokens+1, err)
	}
}

// 与会话同一口径：从未使用或距已落库值满一分钟才刷新。
func TestAPITokenUseIsRecordedAtMostOncePerMinute(t *testing.T) {
	a, st, clk := setup(t)
	ctx := t.Context()
	_, plain, _ := a.CreateAPIToken(ctx, "ci", nil)
	lastUsed := func() time.Time {
		t.Helper()
		got, _, err := st.APITokenByHash(ctx, HashToken(plain))
		if err != nil {
			t.Fatal(err)
		}
		return got.LastUsedAt
	}
	waitLastUsed := func(want time.Time) {
		t.Helper()
		var got time.Time
		testwait.Until(t, 5*time.Millisecond, func() bool {
			got = lastUsed()
			return got.Equal(want)
		}, "last used %v, want %v", &got, want)
	}
	first := clk.Now().Truncate(time.Second)
	a.AuthenticateAPIToken(ctx, plain)
	waitLastUsed(first)
	clk.Advance(59 * time.Second)
	a.AuthenticateAPIToken(ctx, plain)
	// 未满一分钟不投递刷新；用一次同步写把可能的异步写排到它之后，再断言值未变。
	if _, err := st.DeleteAPIToken(ctx, -1); err != nil {
		t.Fatal(err)
	}
	if got := lastUsed(); !got.Equal(first) {
		t.Fatalf("touched within a minute: %v", got)
	}
	clk.Advance(time.Second)
	a.AuthenticateAPIToken(ctx, plain)
	waitLastUsed(clk.Now().Truncate(time.Second))
}
