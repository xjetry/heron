package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/clock"
)

// /healthz 从真实 serve 的入口测：状态、正文、Cache-Control 一并钉住，且不带任何凭据就能拿到。
// body 只允许是 "ok\n"——多出任何版本、节点数或配置都是信息泄漏。
func TestHealthzServedByReadyHub(t *testing.T) {
	t.Parallel()
	url, _, _ := startTestHub(t, filepath.Join(t.TempDir(), "hub.db"), clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))
	resp, err := ownedClient(t).Get(url + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || string(body) != "ok\n" {
		t.Fatalf("GET /healthz: %d %q, want 200 \"ok\\n\"", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/plain; charset=utf-8", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

// health 必须与 Serve 同生命周期：候选 hub 在打开库之前不建立监听，更新门确认之前不开始 Serve。
// 占住目标端口，让"先绑定后开库"的实现以 address already in use 失败；实际错误必须来自打不开的库，
// 证明 store.Open 排在 net.Listen 之前，因而 /healthz 不会在库就绪前应答。
func TestHealthzListenerOpensOnlyAfterTheDatabaseDoes(t *testing.T) {
	t.Parallel()
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	db := filepath.Join(t.TempDir(), "missing", "hub.db")
	err = runServeWith(context.Background(), []string{"--db", db, "--listen", blocker.Addr().String()},
		clock.NewFake(time.Now()), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil || !strings.Contains(err.Error(), "open database "+db) {
		t.Fatalf("err = %v, want an open-database error naming %s; a bind error would mean the listener opened first", err, db)
	}
}

// health 子命令对 /healthz 的应答：2xx 打印 ok 并以 nil 返回；非 2xx 或传输错误返回带原因的错误。
// 重定向不跟随：3xx 必须是失败，探针看的是候选 hub 自己的状态码。
func TestHealthCommandReportsReadinessAndFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		wantErr string
	}{
		{"ready", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK); io.WriteString(w, "ok\n") }, ""},
		{"server error", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) }, "status 500"},
		{"not found", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) }, "status 404"},
		{"redirect is not followed", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/elsewhere", http.StatusFound) }, "status 302"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			t.Cleanup(srv.Close)
			var out strings.Builder
			err := runHealthWith([]string{"--url", srv.URL}, &out)
			if tc.wantErr == "" {
				if err != nil || out.String() != "ok\n" {
					t.Fatalf("healthy hub: err=%v out=%q, want nil and \"ok\\n\"", err, out.String())
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || out.Len() != 0 {
				t.Fatalf("err=%v out=%q, want an error containing %q and no stdout", err, out.String(), tc.wantErr)
			}
		})
	}

	t.Run("nothing listening", func(t *testing.T) {
		dead := httptest.NewServer(http.NotFoundHandler())
		dead.Close()
		var out strings.Builder
		err := runHealthWith([]string{"--url", dead.URL}, &out)
		if err == nil || out.Len() != 0 {
			t.Fatalf("err=%v out=%q, want a transport error and no stdout", err, out.String())
		}
	})
}
