package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/gen/heron/v1/heronv1connect"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/auth"
	"github.com/xjetry/heron/internal/hub/probe"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/testdeps"
)

func TestServePublicSwitchBothSources(t *testing.T) {
	t.Parallel()
	for _, custom := range []bool{false, true} {
		t.Run(map[bool]string{false: "embedded", true: "directory"}[custom], func(t *testing.T) {
			db := filepath.Join(t.TempDir(), "hub.db")
			clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
			st, err := store.Open(db, clk, slog.Default(), store.MigrateSchema)
			if err != nil {
				t.Fatal(err)
			}
			if err := auth.New(st, probe.New(st, slog.Default()), nil, clk, time.UTC, slog.Default()).SetPassword(t.Context(), "public-switch-password"); err != nil {
				t.Fatal(err)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			var flags []string
			if custom {
				dir := t.TempDir()
				for name, content := range map[string]string{"index.html": "custom public page", "theme.js": "secret theme script"} {
					if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Mkdir(filepath.Join(dir, "assets"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "assets", "app.js"), []byte("public script"), 0600); err != nil {
					t.Fatal(err)
				}
				flags = []string{"--public-dir", dir}
			}
			base, _, _ := startTestHub(t, db, clk, flags...)
			jar, _ := cookiejar.New(nil)
			client := &http.Client{Transport: testdeps.OwnedTransport(t), Jar: jar}
			admin := heronv1connect.NewAdminServiceClient(client, base)
			if _, err := admin.Login(t.Context(), connect.NewRequest(&heronv1.LoginRequest{Password: "public-switch-password"})); err != nil {
				t.Fatal(err)
			}
			fetch := func(path string) (int, string, http.Header) {
				t.Helper()
				r, err := client.Get(base + path)
				if err != nil {
					t.Fatal(err)
				}
				defer r.Body.Close()
				b, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				return r.StatusCode, string(b), r.Header
			}
			rootCode, rootBody, _ := fetch("/")
			adminCode, adminBody, adminHeaders := fetch("/admin/")
			// 面板经 builtinHeaders 下发内置策略，拿它作说明页的对照：--public-dir 的来源只带 frame-ancestors，
			// 说明页若随来源取头，目录模式下就与面板不同。
			builtinCSP := adminHeaders.Get("Content-Security-Policy")
			if !strings.Contains(builtinCSP, "default-src 'self'") {
				t.Fatalf("admin CSP %q is not the built-in policy", builtinCSP)
			}
			checkBuiltinHeaders := func(path string, h http.Header) {
				t.Helper()
				if h.Get("Content-Security-Policy") != builtinCSP || h.Get("X-Content-Type-Options") != "nosniff" ||
					h.Get("Referrer-Policy") != adminHeaders.Get("Referrer-Policy") || h.Get("Cache-Control") != "no-store" {
					t.Errorf("closed %s: headers %v, want the built-in CSP %q, nosniff, the panel's Referrer-Policy and no-store", path, h, builtinCSP)
				}
			}
			for _, enabled := range []bool{false, true} {
				if _, err := admin.UpdateSettings(t.Context(), connect.NewRequest(&heronv1.UpdateSettingsRequest{Settings: &heronv1.Settings{Theme: "auto", PublicEnabled: &enabled}})); err != nil {
					t.Fatal(err)
				}
				code, body, _ := fetch("/")
				if !enabled {
					// 公开页的前端路由与 assets/ 之外的文件路径都得到说明页（文件内容不外泄），assets/ 下 404。
					for _, path := range []string{"/", "/nodes/1", "/theme.js", "/index.html"} {
						code, body, headers := fetch(path)
						if code != 200 || !strings.Contains(body, "公开页已关闭") || headers.Get("Content-Type") != "text/html; charset=utf-8" {
							t.Errorf("closed %s: %d %s %v, want the closed page", path, code, body, headers)
						}
						checkBuiltinHeaders(path, headers)
					}
					code, body, headers := fetch("/assets/app.js")
					if code != 404 || strings.Contains(body, "public script") {
						t.Errorf("closed /assets/app.js: %d %s, want 404", code, body)
					}
					checkBuiltinHeaders("/assets/app.js", headers)
				} else if code != rootCode || body != rootBody {
					t.Fatalf("reopened root differs: %d %s", code, body)
				}
				if code, body, _ := fetch("/admin/"); code != adminCode || body != adminBody {
					t.Fatalf("admin affected: %d %s", code, body)
				}
				if _, err := admin.GetSettings(t.Context(), connect.NewRequest(&heronv1.GetSettingsRequest{})); err != nil {
					t.Fatalf("admin RPC affected: %v", err)
				}
			}
		})
	}
}
