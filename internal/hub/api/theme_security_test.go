package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/hub/theme/themetest"
)

func TestAdminHTTPRejectsOpaqueAndCrossOriginBeforeSideEffects(t *testing.T) {
	t.Parallel()
	h := newThemeHarness(t)
	for _, origin := range []string{"null", "https://elsewhere.test", h.srv.URL + ".evil", ""} {
		headers := map[string][]string{"Cookie": {sessionCookieHeader(t, h)}}
		if origin != "" {
			headers["Origin"] = []string{origin}
		}
		got := rawCall(t, h, "CreateNode", `{"name":"origin-test"}`, headers)
		want := http.StatusForbidden
		if origin == "" {
			want = http.StatusOK
		}
		if got.status != want {
			t.Fatalf("origin %q: %+v", origin, got)
		}
	}
	list, err := h.store.ListNodes(t.Context())
	if err != nil || len(list) != 1 {
		t.Fatalf("cross-origin side effects: %v %v", list, err)
	}
	got := rawCall(t, h, "ListNodes", "{}", map[string][]string{"Cookie": {sessionCookieHeader(t, h)}, "Origin": {h.srv.URL}})
	if got.status != 200 {
		t.Fatalf("same origin blocked: %+v", got)
	}
}

func TestThemePreviewCapabilitiesExpireRevokeAndDoNotPublish(t *testing.T) {
	t.Parallel()
	h := newThemeHarness(t)
	a, err := h.upload(t, themetest.Minimal(t, "a"), "")
	if err != nil {
		t.Fatal(err)
	}
	preview := func() string {
		r, err := h.admin.PreviewTheme(t.Context(), connect.NewRequest(&heronv1.PreviewThemeRequest{Id: a.Id, Digest: a.Digest}))
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSuffix(strings.TrimPrefix(r.Msg.Url, "/_heron/preview/"), "/")
	}
	check := func(token string, want bool) {
		t.Helper()
		id, digest, ok := h.svc.ThemePreviewAccess(t.Context(), token)
		if ok != want || (ok && (id != a.Id || digest != a.Digest)) {
			t.Fatalf("preview grant: %s %s %v; want %v", id, digest, ok, want)
		}
	}
	first := preview()
	check(first, true)
	check("invalid", false)
	second := preview()
	check(first, false)
	check(second, true)
	if list := h.themes(t); list[0].Published || list[0].Enabled {
		t.Fatal("preview published package")
	}
	h.clk.Advance(5 * time.Minute)
	check(second, false)
	third := preview()
	if _, err := h.admin.EnableTheme(t.Context(), connect.NewRequest(&heronv1.EnableThemeRequest{Id: a.Id, Digest: a.Digest})); err != nil {
		t.Fatal(err)
	}
	check(third, false)
	fourth := preview()
	if _, err := h.admin.Logout(t.Context(), connect.NewRequest(&heronv1.LogoutRequest{})); err != nil {
		t.Fatal(err)
	}
	check(fourth, false)
}

func TestPublicDirectoryCannotPretendToEnableHostedTheme(t *testing.T) {
	t.Parallel()
	h := newZonedHarness(t, "", time.UTC, store.DefaultRetention, withConfig(func(c *Config) { c.PublicDir = true }))
	h.login(t)
	a, err := h.upload(t, themetest.Minimal(t, "a"), "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.admin.EnableTheme(t.Context(), connect.NewRequest(&heronv1.EnableThemeRequest{Id: a.Id, Digest: a.Digest}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("public dir enable: %v", err)
	}
	if current, _, err := h.store.ThemeSelection(t.Context()); err != nil || current.ID != "" {
		t.Fatalf("selection changed: %v %v", current, err)
	}
}

func TestSecurityUsesTrustedCurrentHTTPSWithoutFlag(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "127.0.0.1/32")
	h.login(t)
	r, err := http.NewRequest("POST", h.srv.URL+"/heron.v1.AdminService/GetSecurity", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	r.Host = "panel.test"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Cookie", sessionCookieHeader(t, h))
	r.Header.Set("Origin", "https://panel.test")
	r.Header.Set("X-Forwarded-Proto", "https")
	resp, err := h.http.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var result struct {
		PasskeyAvailable bool   `json:"passkeyAvailable"`
		CurrentOrigin    string `json:"currentOrigin"`
		Origin           string `json:"origin"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || !result.PasskeyAvailable || result.CurrentOrigin != "https://panel.test" || result.Origin != "" {
		t.Fatalf("auto origin: %d %+v", resp.StatusCode, result)
	}
}
