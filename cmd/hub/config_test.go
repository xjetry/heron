package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/hub/store"
)

func TestParseTTL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want time.Duration
		err  bool
	}{
		{"", 30 * time.Second, false},
		{"45s", 45 * time.Second, false},
		{"10s", 10 * time.Second, false},
		{"180s", 180 * time.Second, false},
		{"181s", 0, true},
		{"9s", 0, true},
		{"banana", 0, true},
	}
	for _, c := range cases {
		got, err := parseTTL(c.in)
		if (err != nil) != c.err || got != c.want {
			t.Fatalf("parseTTL(%q) = %v, %v; want %v, err=%v", c.in, got, err, c.want, c.err)
		}
	}
}

func TestServeUsageShowsRetentionMinima(t *testing.T) {
	t.Parallel()
	output, _ := hubCommand(t, "serve", "--help").CombinedOutput()
	for _, tc := range []struct {
		flag    string
		minimum time.Duration
		want    string
	}{
		{"retention-1m", store.MinRetentionM1, "6h0m0s"},
		{"retention-5m", store.MinRetentionM5, "168h0m0s"},
		{"retention-1h", store.MinRetentionH1, "168h0m0s"},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			_, section, _ := strings.Cut(string(output), "-"+tc.flag+" duration\n")
			section, _, _ = strings.Cut(section, "\n  -")
			if !strings.Contains(section, "minimum "+tc.minimum.String()+")") {
				t.Errorf("usage differs from Validate minimum %v: %q", tc.minimum, section)
			}
			// 独立的对外取值防止常量和帮助一起漂移后自证正确。
			if !strings.Contains(section, "minimum "+tc.want+")") {
				t.Errorf("usage minimum changed: got %q, want minimum %s", section, tc.want)
			}
		})
	}
}

func TestIsLoopback(t *testing.T) {
	t.Parallel()
	if !isLoopback("127.0.0.1:8080") || !isLoopback("[::1]:8080") || !isLoopback("localhost:8080") {
		t.Fatal("loopback addresses misclassified")
	}
	if isLoopback("0.0.0.0:8080") || isLoopback(":8080") || isLoopback("10.0.0.1:8080") {
		t.Fatal("non-loopback addresses misclassified")
	}
}

// 不并行：用 t.Setenv，环境变量是进程级的，并行的进程内 hub 会读到它。
func TestServeRejectsInvalidRetention(t *testing.T) {
	t.Setenv("HERON_OFFLINE_AFTER", "30s")
	for _, tc := range []struct{ flag, value, want string }{
		{"--retention-1m", "5h59m59s", "minimum is 6h"},
		{"--retention-1m", "40d", `invalid value "40d"`},
		{"--retention-5m", "167h59m59s", "minimum is 168h"},
		{"--retention-1h", "167h", "minimum is 168h"},
		{"--retention-1m", "800h", "must not shrink"},
		{"--retention-5m", "9000h", "must not shrink"},
	} {
		t.Run(tc.flag+"/"+tc.value, func(t *testing.T) {
			db := filepath.Join(t.TempDir(), "t.db")
			err := runServe([]string{"--db", db, "--listen", "127.0.0.1:65536", tc.flag, tc.value})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("retention validation: err=%v want=%q", err, tc.want)
			}
			if _, err := os.Stat(db); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("invalid retention touched database: %v", err)
			}
		})
	}
}

// 不并行：用 t.Setenv，环境变量是进程级的，并行的进程内 hub 会读到它。
func TestLoadZone(t *testing.T) {
	t.Setenv("TZ", "Asia/Tokyo")
	if loc, fallback, err := loadZone(""); err != nil || fallback || loc.String() != "Asia/Tokyo" {
		t.Fatalf("TZ must select Asia/Tokyo without fallback: %v %v %v", loc, fallback, err)
	}
	if loc, _, err := loadZone("Asia/Shanghai"); err != nil || loc.String() != "Asia/Shanghai" {
		t.Fatalf("Asia/Shanghai: %v %v", loc, err)
	}
	if _, _, err := loadZone("Mars/Olympus"); err == nil || !strings.Contains(err.Error(), "--timezone") {
		t.Fatalf("unknown zone: %v, want an error naming the flag", err)
	}
}

// 不并行：用 t.Setenv，环境变量是进程级的，并行的进程内 hub 会读到它。
func TestLoadZoneFallsBackPastInvalidTZ(t *testing.T) {
	t.Setenv("TZ", "Mars/Olympus")
	loc, _, err := loadZone("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := time.LoadLocation(loc.String()); err != nil || loc.String() == "Local" {
		t.Fatalf("unusable resolved zone %q: %v", loc, err)
	}
}

// 不并行：用 t.Setenv 并改包级的 localtimePath，二者都是进程级状态，并行的进程内 hub 解析时区时会读到它们。
func TestLoadZoneResolutionSources(t *testing.T) {
	for _, tc := range []struct {
		name, explicit, tz, target, want string
		fallback, twoHops                bool
	}{
		{name: "environment wins", tz: "Asia/Tokyo", target: "UTC", want: "Asia/Tokyo"},
		{name: "invalid environment uses link", tz: "Mars/Olympus", target: "Asia/Shanghai", want: "Asia/Shanghai"},
		{name: "local environment uses link", tz: "Local", target: "Asia/Shanghai", want: "Asia/Shanghai"},
		{name: "missing sources", want: "UTC", fallback: true},
		{name: "invalid link", target: "Mars/Olympus", want: "UTC", fallback: true},
		{name: "explicit right UTC", explicit: "right/UTC", want: "UTC"},
		{name: "posix environment", tz: "posix/Asia/Tokyo", want: "Asia/Tokyo"},
		{name: "right link", target: "right/UTC", want: "UTC"},
		{name: "two hops", target: "Asia/Tokyo", want: "Asia/Tokyo", twoHops: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TZ", tc.tz)
			old := localtimePath
			localtimePath = filepath.Join(t.TempDir(), "localtime")
			t.Cleanup(func() { localtimePath = old })
			if tc.target != "" {
				target := filepath.Join(filepath.Dir(localtimePath), "zoneinfo", tc.target)
				if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(target, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				if tc.twoHops {
					mid := filepath.Join(filepath.Dir(localtimePath), "mid")
					if err := os.Symlink(target, mid); err != nil {
						t.Fatal(err)
					}
					target = mid
				}
				if err := os.Symlink(target, localtimePath); err != nil {
					t.Fatal(err)
				}
			}
			loc, fallback, err := loadZone(tc.explicit)
			if err != nil || fallback != tc.fallback || loc.String() != tc.want {
				t.Fatalf("zone=%v fallback=%v err=%v, want %s/%v", loc, fallback, err, tc.want, tc.fallback)
			}
		})
	}
}

func TestLoadZoneRejectsExplicitLocal(t *testing.T) {
	t.Parallel()
	if _, _, err := loadZone("Local"); err == nil || !strings.Contains(err.Error(), "--timezone") {
		t.Fatalf("Local must be rejected with a flag error: %v", err)
	}
}

// 已移除的配置在打开数据库之前拒绝，避免升级后静默改变托管语义。
//
// 不并行：用 t.Setenv，环境变量是进程级的，并行的进程内 hub 会读到它。
func TestServeRejectsRemovedThemeOriginBeforeOpeningTheDatabase(t *testing.T) {
	t.Setenv("HERON_OFFLINE_AFTER", "30s")
	for _, origin := range []string{"https://status.example.com", "https://example.com/themes"} {
		db := filepath.Join(t.TempDir(), "t.db")
		err := runServe([]string{"--db", db, "--listen", "127.0.0.1:65536", "--theme-origin", origin})
		if err == nil || !strings.Contains(err.Error(), "theme-origin") || !strings.Contains(err.Error(), "flag provided but not defined") {
			t.Fatalf("err = %v, want removed theme-origin rejection", err)
		}
		if _, err := os.Stat(db); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("removed theme-origin touched the database: %v", err)
		}
	}
}
