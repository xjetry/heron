package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/metric"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/testwait"
)

func TestServeRejectsBadMMDBBeforeOpeningDatabase(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"missing", "text", "empty"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "country.mmdb")
			if kind == "empty" {
				path = ""
			}
			if kind == "text" {
				if err := os.WriteFile(path, []byte("not an mmdb database"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			db := filepath.Join(t.TempDir(), "hub.db")
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			err := runServeWith(ctx, []string{"--db", db, "--listen", "127.0.0.1:0", "--geo-mmdb", path},
				clock.NewFake(time.Now()), slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "--geo-mmdb") {
				t.Errorf("bad mmdb startup error = %v, want --geo-mmdb, path %q and reason", err, path)
			}
			if _, err := os.Stat(db); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("bad mmdb touched database: %v", err)
			}
		})
	}
}

// --geo-mmdb 指向没有写者的命名管道：serve 在打开数据库之前报错退出，不挂住。打开这样的管道会一直等写者，等不到
// 返回就说明 serve 去打开了它。等待的上界只为阻塞时能以失败结束；超时后以写端打开一次，放走被阻塞的那次打开。
func TestServeRejectsANamedPipeWithoutBlocking(t *testing.T) {
	t.Parallel()
	fifo := filepath.Join(t.TempDir(), "country.mmdb")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(t.TempDir(), "hub.db")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	done := make(chan error, 1)
	go func() {
		done <- runServeWith(ctx, []string{"--db", db, "--listen", "127.0.0.1:0", "--geo-mmdb", fifo},
			clock.NewFake(time.Now()), slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), fifo) || !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("named pipe startup error = %v, want path %q and \"not a regular file\"", err, fifo)
		}
	case <-time.After(testwait.Bound):
		t.Errorf("serve still blocked on the named pipe after %v, want it to exit before opening the database", testwait.Bound)
		if w, err := os.OpenFile(fifo, os.O_WRONLY, 0); err == nil {
			w.Close()
		}
		<-done
	}
	if _, err := os.Stat(db); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("named pipe touched database: %v", err)
	}
}

func TestServeMMDBTakesPriorityAndEchoesBackend(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		io.WriteString(w, "DE")
	}))
	defer srv.Close()
	clk := clock.NewFake(time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC))
	path := "../../internal/hub/geo/testdata/country.mmdb"
	client, _, stop := startAlertHub(t, clk, func(st *store.Store) {
		if _, err := st.SaveSettings(t.Context(), store.SettingsUpdate{Geo: store.GeoUpdate{Enabled: proto.Bool(true), URL: proto.String(srv.URL + "/{ip}")}}); err != nil {
			t.Fatal(err)
		}
		id, _, err := st.CreateNode(t.Context(), store.NewNode{Name: "public"}, []byte("node"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.WriteMinuteBatch(t.Context(), metric.Batch{Rows: []metric.Row{{NodeID: id, TS: clk.Now().Unix(), CoverageStart: clk.Now().Unix(), Bucket: metric.NewBucket(), LastSeen: clk.Now(), Source: "8.8.8.8"}}}); err != nil {
			t.Fatal(err)
		}
	}, "--geo-mmdb", path)
	settings, err := client.GetSettings(t.Context(), connect.NewRequest(&heronv1.GetSettingsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if s := settings.Msg.Settings; s.GeoBackend != heronv1.GeoBackend_GEO_BACKEND_MMDB || s.GeoMmdbPath != path {
		t.Errorf("serve backend = %v path = %q, want MMDB %q", s.GeoBackend, s.GeoMmdbPath, path)
	}
	var country string
	testwait.Until(t, 10*time.Millisecond, func() bool {
		nodes, err := client.ListNodes(t.Context(), connect.NewRequest(&heronv1.ListNodesRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		country = nodes.Msg.Nodes[0].Country
		return country != ""
	}, "serve did not resolve the public address")
	stop()
	if country != "US" {
		t.Errorf("serve country = %q, want local US, not HTTP DE", country)
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("HTTP requests with mmdb configured = %d, want 0", n)
	}
}

// startupLine 按 serve 的装配启动一次，取到 "hub listening" 那条记录后停下。serve 经 runInBackground 跑：
// 用例从任何路径返回，t.TempDir 删库目录之前 serve 都已退出。
func startupLine(t *testing.T, flags ...string) map[string]json.RawMessage {
	t.Helper()
	events := make(serveEvents, 128)
	args := append([]string{"--db", filepath.Join(t.TempDir(), "hub.db"), "--listen", "127.0.0.1:0"}, flags...)
	serve := runInBackground(t, func(ctx context.Context) error {
		return runServeWith(ctx, args, clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)), slog.New(slog.NewJSONHandler(events, nil)))
	})
	timer := time.NewTimer(testwait.Bound)
	defer timer.Stop()
	var line map[string]json.RawMessage
	for line == nil {
		select {
		case e := <-events:
			if string(e["msg"]) == `"hub listening"` {
				line = e
			}
		case <-serve.finished:
			t.Fatalf("serve stopped before listening: %v", serve.result)
		case <-timer.C:
			t.Fatal("serve did not bind a listener")
		}
	}
	serve.stop(t)
	return line
}

// 启动行写明选定的国家查询后端；本地库另写路径、数据库类型与构建时间（UTC 的 RFC 3339），运维据此确认不出网、加载的
// 是哪一版库。夹具的类型是 Probe-Test-Country、构建时间是 Unix 秒 1。HTTP 后端不写本地库的三项。
func TestServeStartupLineStatesTheGeoBackend(t *testing.T) {
	t.Parallel()
	path := "../../internal/hub/geo/testdata/country.mmdb"
	for _, c := range []struct {
		name  string
		flags []string
		want  map[string]string
	}{
		{"http", nil, map[string]string{"geo_backend": "http"}},
		{"mmdb", []string{"--geo-mmdb", path}, map[string]string{
			"geo_backend": "mmdb", "geo_mmdb": path, "geo_mmdb_type": "Probe-Test-Country", "geo_mmdb_built": "1970-01-01T00:00:01Z",
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			line := startupLine(t, c.flags...)
			for _, key := range []string{"geo_backend", "geo_mmdb", "geo_mmdb_type", "geo_mmdb_built"} {
				raw, present := line[key]
				want, wanted := c.want[key]
				var got string
				if present {
					if err := json.Unmarshal(raw, &got); err != nil {
						t.Errorf("startup line %s = %s, want a string: %v", key, raw, err)
					}
				}
				if present != wanted || got != want {
					t.Errorf("startup line %s = %s (present %v), want %q (present %v)", key, raw, present, want, wanted)
				}
			}
		})
	}
}
