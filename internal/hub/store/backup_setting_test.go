package store

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/clock"
)

func TestBackupSecretAndDisabled(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	secret := "secret"
	u := &BackupSettingsUpdate{Endpoint: "https://s3.example", Bucket: "backups", Region: "auto", AccessKey: "access", Secret: &secret}
	got, err := s.SaveSettings(t.Context(), SettingsUpdate{Backup: u})
	if b := got.Backup; err != nil || !b.Target.Enabled() || b.Target.Secret != secret {
		t.Fatalf("backup not enabled or secret lost: %+v, %v", b, err)
	}
	u.Secret = nil
	got, err = s.SaveSettings(t.Context(), SettingsUpdate{Backup: u})
	if b := got.Backup; err != nil || b.Target.Secret != secret {
		t.Fatalf("omitted secret overwritten: %+v, %v", b, err)
	}
	for _, field := range []string{"endpoint", "bucket", "access_key", "secret"} {
		t.Run(field, func(t *testing.T) {
			copy := *u
			switch field {
			case "endpoint":
				copy.Endpoint = ""
			case "bucket":
				copy.Bucket = ""
			case "access_key":
				copy.AccessKey = ""
			case "secret":
				empty := ""
				copy.Secret = &empty
			}
			_, err := s.SaveSettings(t.Context(), SettingsUpdate{Backup: &copy})
			if err != nil {
				t.Fatal(err)
			}
			b, err := s.BackupSettings(t.Context())
			if err != nil || b.Target.Enabled() {
				t.Fatalf("stored backup enabled without %s: %+v %v", field, b, err)
			}
		})
	}
}

func TestBackupSettingsAtomicAndChannels(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	channel, err := s.SaveNotifyChannel(ctx, NotifyChannel{Name: "backup", Kind: ChannelTelegram, Config: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	secret := "preserve"
	keep := uint32(24)
	u := &BackupSettingsUpdate{Secret: &secret, ConfigKeep: &keep, Channels: &[]int64{channel.ID}}
	saved, err := s.SaveSettings(ctx, SettingsUpdate{Appearance: &SiteAppearance{Theme: "auto", Title: "old"}, Backup: u})
	if err != nil {
		t.Fatal(err)
	}
	site, before := saved.Site, saved.Backup
	if len(before.Channels) != 1 || before.Channels[0] != channel.ID {
		t.Fatalf("channels not saved: %+v", before)
	}
	if err := s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec("CREATE TRIGGER reject_backup BEFORE INSERT ON setting WHEN NEW.key = 'backup.config_keep' BEGIN SELECT RAISE(ABORT, 'reject backup'); END")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	newSecret := "must-not-write"
	u.Secret = &newSecret
	if _, err := s.SaveSettings(ctx, SettingsUpdate{Appearance: &SiteAppearance{Theme: "dark", Title: "new"}, Backup: u}); err == nil {
		t.Fatal("injected storage failure not returned")
	}
	got, err := s.Settings(ctx)
	if err != nil || got.Site != site || !reflect.DeepEqual(got.Backup, before) {
		t.Fatalf("failed transaction changed settings: %+v %+v %v", got.Site, got.Backup, err)
	}
	if err := s.DeleteNotifyChannel(ctx, channel.ID); err != nil {
		t.Fatal(err)
	}
	b, err := s.BackupSettings(ctx)
	if err != nil || len(b.Channels) != 0 {
		t.Fatalf("deleted channel remains selected: %+v %v", b, err)
	}
	if got := storedSetting(t, s, string(BackupNotifyList)); got != "[]" {
		t.Fatalf("channel list after its last channel was deleted = %q, want []", got)
	}
	var missing NotFoundError
	if _, err := s.SaveSettings(ctx, SettingsUpdate{Backup: &BackupSettingsUpdate{Channels: &[]int64{channel.ID}}}); !errors.As(err, &missing) || missing.Kind != ObjectNotifyChannel || missing.ID != channel.ID {
		t.Fatalf("nonexistent backup channel: %v", err)
	}
}

func storedSetting(t *testing.T, s *Store, key string) string {
	t.Helper()
	var v string
	if err := s.r.QueryRowContext(t.Context(), "SELECT value FROM setting WHERE key = ?", key).Scan(&v); err != nil {
		t.Fatalf("reading %s: %v", key, err)
	}
	return v
}

// 渠道列表的存储形态：缺席不写，显式空写 []（不是 JSON null），给出的列表排序去重。
func TestBackupChannelsStoredForm(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	var ids []int64
	for _, name := range []string{"a", "b"} {
		c, err := s.SaveNotifyChannel(ctx, NotifyChannel{Name: name, Kind: ChannelTelegram, Config: `{}`})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, c.ID)
	}
	if _, err := s.SaveSettings(ctx, SettingsUpdate{Backup: &BackupSettingsUpdate{Channels: &[]int64{ids[1], ids[0], ids[1]}}}); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("[%d,%d]", ids[0], ids[1])
	if got := storedSetting(t, s, string(BackupNotifyList)); got != want {
		t.Fatalf("given channels stored as %q, want %q", got, want)
	}
	if _, err := s.SaveSettings(ctx, SettingsUpdate{Backup: &BackupSettingsUpdate{}}); err != nil {
		t.Fatal(err)
	}
	if got := storedSetting(t, s, string(BackupNotifyList)); got != want {
		t.Fatalf("absent channels rewrote the list to %q", got)
	}
	var none []int64
	if _, err := s.SaveSettings(ctx, SettingsUpdate{Backup: &BackupSettingsUpdate{Channels: &none}}); err != nil {
		t.Fatal(err)
	}
	if got := storedSetting(t, s, string(BackupNotifyList)); got != "[]" {
		t.Fatalf("explicit empty channels stored as %q, want []", got)
	}
}

// 数值范围只有一张表：写侧出范围返回 BackupRangeError 且不写入；读侧遇到库里的坏值报错，但不是 BackupRangeError，
// api 不会把库的问题当作请求的 InvalidArgument。Open 读一次全部设置，这样的库（连同解析不了的渠道列表）打开时就被拒绝。
func TestBackupNumbersRangeOnBothSides(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	keep := uint32(24)
	if _, err := s.SaveSettings(ctx, SettingsUpdate{Backup: &BackupSettingsUpdate{ConfigKeep: &keep}}); err != nil {
		t.Fatal(err)
	}
	zero := uint32(0)
	var rangeErr BackupRangeError
	_, err := s.SaveSettings(ctx, SettingsUpdate{Appearance: &SiteAppearance{Theme: "dark"}, Backup: &BackupSettingsUpdate{Bucket: "changed", ConfigKeep: &zero}})
	if !errors.As(err, &rangeErr) || rangeErr.Error() != "backup.config_keep must be in [1, 1000]; got 0" {
		t.Fatalf("out-of-range write: %v", err)
	}
	got, err := s.Settings(ctx)
	if err != nil || got.Site.Theme != "auto" || got.Backup.ConfigKeep != 24 || got.Backup.Target.Bucket != "" {
		t.Fatalf("rejected write changed settings: %+v %+v %v", got.Site, got.Backup, err)
	}
	for key, value := range map[string]string{"backup.config_keep": "0", "backup.metrics_keep": "1001", "backup.config_interval_s": "59", "backup.metrics_interval_s": "604801", string(BackupNotifyList): "[1,"} {
		t.Run(key, func(t *testing.T) {
			s, path := openAt(t)
			if err := s.write(ctx, func(tx *sql.Tx) error { return putSetting(tx, key, value) }); err != nil {
				t.Fatal(err)
			}
			got, err := s.Settings(ctx)
			if err == nil || errors.As(err, &rangeErr) || !strings.Contains(err.Error(), "invalid stored "+key) {
				t.Fatalf("stored %s=%s read as %+v, %v", key, value, got.Backup, err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(path, clock.NewFake(time.Now()), slog.Default(), RequireCurrentSchema)
			if reopened != nil {
				reopened.Close()
			}
			if err == nil || !strings.Contains(err.Error(), "invalid stored "+key) {
				t.Fatalf("reopen with %s=%s: %v, want an error containing %q", key, value, err, "invalid stored "+key)
			}
		})
	}
}
