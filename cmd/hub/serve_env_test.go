package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/clock"
)

// envOf 是 lookupEnv 的替身：只有 vars 里的键存在，值可以是空串。
func envOf(vars map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := vars[name]
		return v, ok
	}
}

func TestFlagEnvName(t *testing.T) {
	t.Parallel()
	for flagName, want := range map[string]string{
		"db": "HERON_DB", "offline-after": "HERON_OFFLINE_AFTER", "retention-alert-events": "HERON_RETENTION_ALERT_EVENTS", "geo-mmdb": "HERON_GEO_MMDB",
	} {
		if got := flagEnvName(flagName); got != want {
			t.Errorf("flagEnvName(%q) = %q, want %q", flagName, got, want)
		}
	}
}

func TestApplyFlagEnv(t *testing.T) {
	t.Parallel()
	newSet := func() (*flag.FlagSet, *string, *string, *int) {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		return fs, fs.String("name", "default-name", ""), fs.String("blank", "default-blank", ""), fs.Int("count", 1, "")
	}
	t.Run("explicit flag wins, present empty fills, absent keeps default", func(t *testing.T) {
		fs, name, blank, count := newSet()
		if err := fs.Parse([]string{"--name", "from-flag"}); err != nil {
			t.Fatal(err)
		}
		if err := applyFlagEnv(fs, envOf(map[string]string{"HERON_NAME": "from-env", "HERON_BLANK": ""})); err != nil {
			t.Fatal(err)
		}
		if *name != "from-flag" || *blank != "" || *count != 1 {
			t.Fatalf("name=%q blank=%q count=%d; want from-flag, empty, 1", *name, *blank, *count)
		}
		// 回填的 flag 与显式给出的一样算作已给出，"是否给出"的判定对两种来源一致。
		given := map[string]bool{}
		fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
		if !given["name"] || !given["blank"] || given["count"] {
			t.Fatalf("flags counted as given = %v; want name and blank only", given)
		}
	})
	t.Run("explicit empty flag wins over env", func(t *testing.T) {
		fs, name, _, _ := newSet()
		if err := fs.Parse([]string{"--name="}); err != nil {
			t.Fatal(err)
		}
		if err := applyFlagEnv(fs, envOf(map[string]string{"HERON_NAME": "from-env"})); err != nil || *name != "" {
			t.Fatalf("name=%q err=%v; want the explicit empty value", *name, err)
		}
	})
	t.Run("invalid value names variable and flag", func(t *testing.T) {
		fs, _, _, _ := newSet()
		if err := fs.Parse(nil); err != nil {
			t.Fatal(err)
		}
		err := applyFlagEnv(fs, envOf(map[string]string{"HERON_COUNT": "many"}))
		if err == nil || !strings.Contains(err.Error(), "HERON_COUNT") || !strings.Contains(err.Error(), "--count") || !strings.Contains(err.Error(), `"many"`) {
			t.Fatalf("err = %v; want it to name HERON_COUNT, --count and the value", err)
		}
	})
}

func TestParseServeOptionsTakesEveryFlagFromTheEnvironment(t *testing.T) {
	t.Parallel()
	db := filepath.Join(t.TempDir(), "hub.db")
	for _, tc := range []struct {
		name  string
		args  []string
		env   map[string]string
		check func(serveOptions) bool
		want  string
	}{
		{"env fills an absent flag", nil, map[string]string{"HERON_LISTEN": "127.0.0.1:9001"},
			func(o serveOptions) bool { return o.listen == "127.0.0.1:9001" }, "listen from HERON_LISTEN"},
		{"explicit flag beats env", []string{"--listen", "127.0.0.1:9002"}, map[string]string{"HERON_LISTEN": "127.0.0.1:9001"},
			func(o serveOptions) bool { return o.listen == "127.0.0.1:9002" }, "listen from --listen"},
		{"typed flags parse from env", nil, map[string]string{"HERON_RETENTION_1M": "12h"},
			func(o serveOptions) bool { return o.retention.M1 == 12*time.Hour }, "retention-1m 12h"},
		{"offline-after from env", nil, map[string]string{"HERON_OFFLINE_AFTER": "12s"},
			func(o serveOptions) bool { return o.ttl == 12*time.Second }, "ttl 12s"},
		{"offline-after flag beats env", []string{"--offline-after", "20s"}, map[string]string{"HERON_OFFLINE_AFTER": "12s"},
			func(o serveOptions) bool { return o.ttl == 20*time.Second }, "ttl 20s"},
		{"offline-after absent takes the default", nil, nil,
			func(o serveOptions) bool { return o.ttl == defaultTTL }, "default ttl"},
		{"offline-after env set empty takes the default", nil, map[string]string{"HERON_OFFLINE_AFTER": ""},
			func(o serveOptions) bool { return o.ttl == defaultTTL }, "default ttl"},
		{"offline-after flag given empty takes the default", []string{"--offline-after="}, map[string]string{"HERON_OFFLINE_AFTER": "12s"},
			func(o serveOptions) bool { return o.ttl == defaultTTL }, "default ttl"},
		{"geo-mmdb env set empty counts as given", nil, map[string]string{"HERON_GEO_MMDB": ""},
			func(o serveOptions) bool { return o.geoMMDBSet && o.geoMMDB == "" }, "an explicit empty mmdb path"},
		{"geo-mmdb absent selects HTTP", nil, nil,
			func(o serveOptions) bool { return !o.geoMMDBSet }, "no mmdb"},
		{"admin-origin from env is normalized", nil, map[string]string{"HERON_ADMIN_ORIGIN": "https://ADMIN.example.:443"},
			func(o serveOptions) bool { return o.adminOrigin == "https://admin.example" }, "the normalized legacy origin"},
		{"admin-origin flag beats env", []string{"--admin-origin", "https://flag.example"}, map[string]string{"HERON_ADMIN_ORIGIN": "http://invalid.example"},
			func(o serveOptions) bool { return o.adminOrigin == "https://flag.example" }, "the legacy origin from --admin-origin"},
		{"admin-origin env set empty is no legacy origin", nil, map[string]string{"HERON_ADMIN_ORIGIN": ""},
			func(o serveOptions) bool { return o.adminOrigin == "" }, "no legacy origin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts, err := parseServeOptions(append([]string{"--db", db, "--timezone", "UTC"}, tc.args...), envOf(tc.env))
			if err != nil || !tc.check(opts) {
				t.Fatalf("opts = %+v, err = %v; want %s", opts, err, tc.want)
			}
		})
	}
}

func TestParseServeOptionsRejectsInvalidEnvironmentValues(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		env  map[string]string
		want []string
	}{
		{map[string]string{"HERON_RETENTION_1M": "40d"}, []string{"HERON_RETENTION_1M", "--retention-1m", `"40d"`}},
		{map[string]string{"HERON_OFFLINE_AFTER": "9s"}, []string{"HERON_OFFLINE_AFTER", "--offline-after", "below the minimum 10s"}},
		{map[string]string{"HERON_OFFLINE_AFTER": "banana"}, []string{"HERON_OFFLINE_AFTER", "--offline-after"}},
		{map[string]string{"HERON_TIMEZONE": "Mars/Olympus"}, []string{"--timezone"}},
		{map[string]string{"HERON_TRUSTED_PROXIES": "not-a-cidr"}, []string{"not-a-cidr"}},
		{map[string]string{"HERON_ADMIN_ORIGIN": "http://admin.example"}, []string{"HERON_ADMIN_ORIGIN", "--admin-origin", "requires HTTPS"}},
	} {
		_, err := parseServeOptions([]string{"--db", filepath.Join(t.TempDir(), "hub.db")}, envOf(tc.env))
		for _, w := range tc.want {
			if err == nil || !strings.Contains(err.Error(), w) {
				t.Errorf("env %v: err = %v, want it to contain %q", tc.env, err, w)
			}
		}
	}
}

// 经真实入口：HERON_* 给出的非法配置与显式 flag 一样在打开数据库之前拒绝，不建库、不监听；--geo-mmdb 由环境变量给成
// 空串或坏路径时报错，不回退到出网的 HTTP 后端。
//
// 不并行：用 t.Setenv，环境变量是进程级的，并行的进程内 hub 会读到它。
func TestServeRejectsInvalidEnvironmentBeforeOpeningTheDatabase(t *testing.T) {
	for _, tc := range []struct {
		name, key, value, want string
	}{
		{"empty geo-mmdb", "HERON_GEO_MMDB", "", "--geo-mmdb"},
		{"missing geo-mmdb", "HERON_GEO_MMDB", filepath.Join(t.TempDir(), "missing.mmdb"), "--geo-mmdb"},
		{"unknown timezone", "HERON_TIMEZONE", "Mars/Olympus", "--timezone"},
		{"offline-after below minimum", "HERON_OFFLINE_AFTER", "9s", "--offline-after"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)
			db := filepath.Join(t.TempDir(), "hub.db")
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			err := runServeWith(ctx, []string{"--db", db, "--listen", "127.0.0.1:0"},
				clock.NewFake(time.Now()), slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want a %s error", err, tc.want)
			}
			if _, err := os.Stat(db); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid environment touched the database: %v", err)
			}
		})
	}
}

// 经真实入口：非法的 --admin-origin 与 HERON_ADMIN_ORIGIN 与其它 flag 一样在打开数据库之前拒绝，不建库、不监听，错误
// 同时点名 flag 与环境变量。
//
// 不并行：用 t.Setenv，环境变量是进程级的，并行的进程内 hub 会读到它。
func TestServeRejectsInvalidAdminOriginBeforeOpeningTheDatabase(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags []string
		env   string
	}{
		{"flag", []string{"--admin-origin", "http://admin.example"}, ""},
		{"environment", nil, "http://admin.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env != "" {
				t.Setenv("HERON_ADMIN_ORIGIN", tc.env)
			}
			db := filepath.Join(t.TempDir(), "hub.db")
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			err := runServeWith(ctx, append([]string{"--db", db, "--listen", "127.0.0.1:0"}, tc.flags...),
				clock.NewFake(time.Now()), discardLog())
			if _, statErr := os.Stat(db); !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("an invalid --admin-origin touched the database: %v", statErr)
			}
			if err == nil || !strings.Contains(err.Error(), "--admin-origin (HERON_ADMIN_ORIGIN)") {
				t.Errorf("err = %v, want an error naming --admin-origin and HERON_ADMIN_ORIGIN", err)
			}
		})
	}
}

// 库里已有持久绑定时，非法的 --admin-origin 照样拒绝启动：ConfigureWebAuthn 在有绑定时不看旧参数，所以格式错误要在
// 启动层不看库就拦下，否则它被库的状态掩盖，等到换库或 security-reset 清掉绑定后的那次启动才暴露。
func TestServeRejectsInvalidAdminOriginDespiteAPersistentBinding(t *testing.T) {
	t.Parallel()
	db := filepath.Join(t.TempDir(), "hub.db")
	seedAdminSecurity(t, db, `{"origin":"https://admin.example","rp_id":"admin.example"}`)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := runServeWith(ctx, []string{"--db", db, "--listen", "127.0.0.1:0", "--timezone", "UTC", "--admin-origin", "not an origin"},
		clock.NewFake(time.Now()), discardLog())
	if err == nil || !strings.Contains(err.Error(), "--admin-origin (HERON_ADMIN_ORIGIN)") {
		t.Fatalf("err = %v, want the invalid --admin-origin rejected despite the binding", err)
	}
}

// 启动行的 ttl 经真实入口取自 --offline-after 与 HERON_OFFLINE_AFTER：环境变量单独给出时生效（scripts/e2e.sh 就这样
// 设 12s），两者同时给出时显式 flag 优先。
//
// 不并行：用 t.Setenv，环境变量是进程级的，并行的进程内 hub 会读到它。
func TestServeStartupLineTakesTheTTLFromFlagOrEnvironment(t *testing.T) {
	t.Setenv("HERON_OFFLINE_AFTER", "12s")
	for _, tc := range []struct {
		flags []string
		want  time.Duration
	}{
		{nil, 12 * time.Second},
		{[]string{"--offline-after", "20s"}, 20 * time.Second},
	} {
		line := startupLine(t, tc.flags...)
		var ttl time.Duration
		if err := json.Unmarshal(line["ttl"], &ttl); err != nil || ttl != tc.want {
			t.Errorf("flags %v: startup ttl = %s (%v), want %v", tc.flags, line["ttl"], err, tc.want)
		}
	}
}
