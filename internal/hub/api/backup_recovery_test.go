package api

import (
	"database/sql"
	"testing"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"google.golang.org/protobuf/proto"
)

func TestCorruptBackupNumbersRecoverThroughSettingsAPI(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	in := validSettings()
	in.Backup = fullBackup()
	before := saveSettings(t, h, in)
	db, err := sql.Open("sqlite", h.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, key := range []string{"backup.config_interval_s", "backup.metrics_interval_s", "backup.config_keep", "backup.metrics_keep"} {
		if _, err := db.Exec("UPDATE setting SET value = '0' WHERE key = ?", key); err != nil {
			t.Fatal(err)
		}
	}
	var bad int
	if err := db.QueryRow("SELECT count(*) FROM setting WHERE key IN ('backup.config_interval_s','backup.metrics_interval_s','backup.config_keep','backup.metrics_keep') AND value = '0'").Scan(&bad); err != nil || bad != 4 {
		t.Fatalf("corrupt fixture not installed: count=%d err=%v", bad, err)
	}
	if _, err := h.admin.GetSettings(t.Context(), connect.NewRequest(&heronv1.GetSettingsRequest{})); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("corrupt GetSettings: %v, want Internal", err)
	}
	in.Title = "must-not-commit"
	in.PublicEnabled = proto.Bool(false)
	in.Backup.ConfigKeep = nil
	if _, err := h.admin.UpdateSettings(t.Context(), connect.NewRequest(&heronv1.UpdateSettingsRequest{Settings: in})); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("incomplete repair: %v, want Internal", err)
	}
	var title, enabled string
	if err := db.QueryRow("SELECT (SELECT value FROM setting WHERE key='site.title'), (SELECT value FROM setting WHERE key='site.public_enabled')").Scan(&title, &enabled); err != nil {
		t.Fatal(err)
	}
	if title != before.Title || enabled != "1" || !h.store.PublicEnabled() {
		t.Fatalf("failed repair committed appearance or public switch: title=%q enabled=%q memory=%v", title, enabled, h.store.PublicEnabled())
	}
	in = proto.Clone(before).(*heronv1.Settings)
	in.Backup.ConfigIntervalS, in.Backup.MetricsIntervalS = proto.Uint32(300), proto.Uint32(86400)
	in.Backup.ConfigKeep, in.Backup.MetricsKeep = proto.Uint32(48), proto.Uint32(14)
	got := saveSettings(t, h, in)
	if !proto.Equal(got, in) || !proto.Equal(currentSettings(t, h), in) {
		t.Fatalf("complete API repair did not restore readable settings: got=%v want=%v", got, in)
	}
}
