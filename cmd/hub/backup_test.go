package main

import (
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/s3"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/testwait"
)

func TestServeBackupUploadsAndDeliversRecovery(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	fail := true
	objects := map[string][]byte{}
	receiver, bodies := alertReceiver(t)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		key := strings.TrimPrefix(r.URL.Path, "/backups/")
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
			t.Error("S3 request not signed")
		}
		switch r.Method {
		case http.MethodPut:
			if fail && strings.Contains(key, "config/") {
				w.WriteHeader(503)
				return
			}
			data, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			objects[key] = data
		case http.MethodGet:
			var entries []s3.Object
			for k := range objects {
				if strings.HasPrefix(k, r.URL.Query().Get("prefix")) {
					entries = append(entries, s3.Object{Key: k})
				}
			}
			if err := xml.NewEncoder(w).Encode(struct {
				XMLName  xml.Name    `xml:"ListBucketResult"`
				Contents []s3.Object `xml:"Contents"`
			}{Contents: entries}); err != nil {
				t.Error(err)
			}
		case http.MethodDelete:
			delete(objects, key)
		default:
			t.Errorf("unexpected S3 method=%s", r.Method)
			w.WriteHeader(400)
		}
	}))
	defer remote.Close()
	clk := clock.NewFake(time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC))
	client, events, stop := startAlertHub(t, clk, func(st *store.Store) {
		config, _ := json.Marshal(map[string]string{"url": receiver, "method": "POST"})
		channel, err := st.SaveNotifyChannel(t.Context(), store.NotifyChannel{Name: "backup", Kind: store.ChannelWebhook, Config: string(config)})
		if err != nil {
			t.Fatal(err)
		}
		secret := "secret"
		_, err = st.SaveSettings(t.Context(), store.SettingsUpdate{Backup: &store.BackupSettingsUpdate{Endpoint: remote.URL, Bucket: "backups", Region: "auto", AccessKey: "key", Secret: &secret, Prefix: "hub", Channels: &[]int64{channel.ID}}})
		if err != nil {
			t.Fatal(err)
		}
	})
	defer stop()
	waitStatus := func(recovered bool) {
		t.Helper()
		deadline := time.NewTimer(testwait.Bound)
		defer deadline.Stop()
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			resp, err := client.GetBackupStatus(t.Context(), connect.NewRequest(&heronv1.GetBackupStatusRequest{}))
			if err != nil {
				t.Fatal(err)
			}
			s := resp.Msg
			if s.Enabled && s.Metrics.LastSuccessAt != nil {
				if recovered && s.Config.Failure == nil && s.Config.GetLastSuccessAt() == clk.Now().Unix() {
					return
				}
				if !recovered && s.Config.Failure != nil && s.Config.Failure.Category == "upload/http_status" && s.Config.LastSuccessAt == nil {
					return
				}
			}
			select {
			case <-deadline.C:
				for len(events) > 0 {
					t.Logf("hub event: %s", <-events)
				}
				t.Fatalf("serve backup state did not converge: recovered=%v status=%v", recovered, s)
			case <-ticker.C:
			}
		}
	}
	waitStatus(false)
	awaitDelivered(t, client, bodies, "backup_failed", testwait.Bound)
	mu.Lock()
	fail = false
	mu.Unlock()
	clk.Advance(5 * time.Minute)
	waitStatus(true)
	select {
	case body := <-bodies:
		if !strings.Contains(body, `"transition":"backup_recovered"`) || !strings.Contains(body, `"kind":"backup"`) || !strings.Contains(body, "已恢复") {
			t.Fatalf("recovery webhook=%s", body)
		}
	case <-time.After(testwait.Bound):
		t.Fatal("backup recovery webhook not delivered")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, layer := range []string{"config", "metrics"} {
		found := false
		for k, b := range objects {
			if strings.Contains(k, "/"+layer+"/") && strings.HasPrefix(string(b), "SQLite format 3") {
				found = true
			}
		}
		if !found {
			t.Errorf("S3 missing SQLite %s snapshot", layer)
		}
	}
}
