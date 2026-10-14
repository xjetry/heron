package api

import (
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"google.golang.org/protobuf/proto"
)

func fullBackup() *heronv1.BackupSettings {
	return &heronv1.BackupSettings{Endpoint: "https://account.r2.cloudflarestorage.com", Bucket: "private-backups", Region: "auto", AccessKey: "access", Secret: proto.String("never-echo-this"), Prefix: "hub", ConfigIntervalS: proto.Uint32(600), MetricsIntervalS: proto.Uint32(7200), ConfigKeep: proto.Uint32(24), MetricsKeep: proto.Uint32(7)}
}

// echoOf 是 in 保存之后的回显：secret 不回显、换成 has_secret，notify 恒给出。
func echoOf(in *heronv1.BackupSettings, hasSecret bool, channels ...int64) *heronv1.BackupSettings {
	want := proto.Clone(in).(*heronv1.BackupSettings)
	want.Secret = nil
	want.HasSecret = hasSecret
	want.Notify = &heronv1.BackupNotify{ChannelIds: channels}
	return want
}

func backupChannels(s *heronv1.Settings) []int64 { return s.GetBackup().GetNotify().GetChannelIds() }

func TestBackupSettingsDefaultsWriteOnlyAndOmission(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	defaults := currentSettings(t, h).GetBackup()
	if defaults == nil || defaults.GetConfigIntervalS() != 300 || defaults.GetMetricsIntervalS() != 86400 || defaults.GetConfigKeep() != 48 || defaults.GetMetricsKeep() != 14 || defaults.GetHasSecret() || defaults.GetNotify() == nil {
		t.Fatalf("backup defaults = %v", defaults)
	}
	in := validSettings()
	in.Backup = fullBackup()
	got := saveSettings(t, h, in)
	want := echoOf(in.Backup, true)
	if !proto.Equal(got.Backup, want) {
		t.Errorf("saved backup mismatch or secret echoed: %v", got.Backup)
	}
	if read := currentSettings(t, h); !proto.Equal(read.Backup, want) {
		t.Errorf("read backup mismatch or secret echoed: %v", read.Backup)
	}
	// 缺席 backup 的现有外观消费者不得改备份配置。
	if read := saveSettings(t, h, validSettings()); !proto.Equal(read.Backup, want) {
		t.Fatalf("appearance update changed backup: %v", read.Backup)
	}
	in.Backup.Secret = nil
	in.Backup.ConfigIntervalS, in.Backup.MetricsIntervalS, in.Backup.ConfigKeep, in.Backup.MetricsKeep = nil, nil, nil, nil
	if read := saveSettings(t, h, in); !proto.Equal(read.Backup, want) {
		t.Fatalf("omitted numeric fields changed backup: %v", read.Backup)
	}
	in.Backup.ConfigIntervalS, in.Backup.MetricsIntervalS, in.Backup.ConfigKeep, in.Backup.MetricsKeep = proto.Uint32(300), proto.Uint32(86400), proto.Uint32(48), proto.Uint32(14)
	got = saveSettings(t, h, in)
	if got.Backup.GetConfigIntervalS() != 300 || got.Backup.GetMetricsIntervalS() != 86400 || got.Backup.GetConfigKeep() != 48 || got.Backup.GetMetricsKeep() != 14 {
		t.Fatalf("explicit defaults not saved: %v", got.Backup)
	}
}

// has_secret 由存储值是否非空推出：两个方向都要钉住，恒为 true 与恒为 false 都会让读侧说错"是否已配置"。
func TestBackupHasSecretFollowsStoredSecret(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	if currentSettings(t, h).GetBackup().GetHasSecret() {
		t.Fatal("has_secret is true before any secret was saved")
	}
	in := validSettings()
	in.Backup = fullBackup()
	if got := saveSettings(t, h, in).GetBackup(); !got.GetHasSecret() || got.Secret != nil {
		t.Fatalf("after saving a secret: has_secret=%v secret=%v", got.GetHasSecret(), got.Secret)
	}
	if !currentSettings(t, h).GetBackup().GetHasSecret() {
		t.Fatal("GetSettings does not report the saved secret")
	}
	in.Backup.Secret = nil
	if !saveSettings(t, h, in).GetBackup().GetHasSecret() {
		t.Fatal("omitting secret dropped it")
	}
	in.Backup.Secret = proto.String("")
	if saveSettings(t, h, in).GetBackup().GetHasSecret() || currentSettings(t, h).GetBackup().GetHasSecret() {
		t.Fatal("has_secret is still true after the secret was cleared")
	}
	in.Backup.Secret, in.Backup.HasSecret = nil, true
	if saveSettings(t, h, in).GetBackup().GetHasSecret() {
		t.Fatal("has_secret from the request was trusted")
	}
}

// notify 缺席不变、显式空集合关闭、给出即替换；三个方向缺一个，只改周期的脚本就会顺手清空备份失败告警的渠道。
func TestBackupNotifyPresence(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	a := saveChannel(t, h, webhook("https://hooks.example/a")).Id
	b := saveChannel(t, h, webhook("https://hooks.example/b")).Id
	in := validSettings()
	in.Backup = fullBackup()
	in.Backup.Notify = &heronv1.BackupNotify{ChannelIds: []int64{b, a, b}}
	if got := backupChannels(saveSettings(t, h, in)); !slices.Equal(got, []int64{a, b}) {
		t.Fatalf("given channels saved as %v, want %v (sorted, duplicates merged)", got, []int64{a, b})
	}
	in.Backup.Notify, in.Backup.Secret, in.Backup.ConfigIntervalS = nil, nil, proto.Uint32(900)
	got := saveSettings(t, h, in)
	if !slices.Equal(backupChannels(got), []int64{a, b}) || got.GetBackup().GetConfigIntervalS() != 900 {
		t.Fatalf("interval-only update without notify changed channels: %v", got.GetBackup())
	}
	in.Backup.Notify = &heronv1.BackupNotify{ChannelIds: []int64{b}}
	if got := backupChannels(saveSettings(t, h, in)); !slices.Equal(got, []int64{b}) {
		t.Fatalf("given channels did not replace the old ones: %v", got)
	}
	in.Backup.Notify = &heronv1.BackupNotify{}
	if got := backupChannels(saveSettings(t, h, in)); len(got) != 0 {
		t.Fatalf("explicit empty notify did not turn notifications off: %v", got)
	}
	if read := currentSettings(t, h); read.GetBackup().GetNotify() == nil || len(backupChannels(read)) != 0 {
		t.Fatalf("read after explicit empty notify: %v", read.GetBackup())
	}
	// JSON 客户端的写法：{} 是给出的空集合，省略才是缺席。
	in.Backup.Notify = &heronv1.BackupNotify{ChannelIds: []int64{a}}
	saveSettings(t, h, in)
	postBackupJSON(t, h, `{"endpoint":"https://s3.example","bucket":"private-backups","accessKey":"access"}`)
	if got := backupChannels(currentSettings(t, h)); !slices.Equal(got, []int64{a}) {
		t.Fatalf("JSON update omitting notify changed channels: %v", got)
	}
	postBackupJSON(t, h, `{"endpoint":"https://s3.example","bucket":"private-backups","accessKey":"access","notify":{}}`)
	if got := backupChannels(currentSettings(t, h)); len(got) != 0 {
		t.Fatalf("JSON update with notify {} kept channels: %v", got)
	}
}

func postBackupJSON(t *testing.T, h *harness, backup string) {
	t.Helper()
	body := `{"settings":{"theme":"auto","backup":` + backup + `}}`
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+"/heron.v1.AdminService/UpdateSettings", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("UpdateSettings %s: %d %s", body, resp.StatusCode, b)
	}
}

func TestBackupSettingsRanges(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		field    string
		min, max uint32
		set      func(*heronv1.BackupSettings, *uint32)
	}{
		{"config_interval_s", 60, 86400, func(b *heronv1.BackupSettings, v *uint32) { b.ConfigIntervalS = v }},
		{"metrics_interval_s", 3600, 604800, func(b *heronv1.BackupSettings, v *uint32) { b.MetricsIntervalS = v }},
		{"config_keep", 1, 1000, func(b *heronv1.BackupSettings, v *uint32) { b.ConfigKeep = v }},
		{"metrics_keep", 1, 1000, func(b *heronv1.BackupSettings, v *uint32) { b.MetricsKeep = v }},
	} {
		t.Run(tc.field, func(t *testing.T) {
			h := newHarness(t, "")
			h.login(t)
			in := validSettings()
			in.Backup = fullBackup()
			before := saveSettings(t, h, in)
			for _, bad := range []uint32{0, tc.min - 1, tc.max + 1, ^uint32(0)} {
				saveSettings(t, h, before)
				tc.set(in.Backup, &bad)
				in.Title = "must-not-be-saved"
				_, err := h.admin.UpdateSettings(t.Context(), connect.NewRequest(&heronv1.UpdateSettingsRequest{Settings: in}))
				want := fmt.Sprintf("backup.%s must be in [%d, %d]; got %d", tc.field, tc.min, tc.max, bad)
				if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), want) {
					t.Errorf("%s=%d: want InvalidArgument %q, got %v", tc.field, bad, want, err)
				}
				if got := currentSettings(t, h); !proto.Equal(got, before) {
					t.Errorf("invalid backup update changed settings: %v", got)
				}
			}
			for _, good := range []uint32{tc.min, tc.max} {
				tc.set(in.Backup, &good)
				saveSettings(t, h, in)
			}
		})
	}
}

func TestBackupTargetValidation(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	in := validSettings()
	in.Backup = fullBackup()
	before := saveSettings(t, h, in)
	for _, tc := range []struct {
		want   string
		change func(*heronv1.BackupSettings)
	}{
		{"endpoint", func(b *heronv1.BackupSettings) { b.Endpoint = "file:///tmp/backup" }},
		{"endpoint", func(b *heronv1.BackupSettings) { b.Endpoint = "https://user:secret@host" }},
		{"bucket", func(b *heronv1.BackupSettings) { b.Bucket = "../escape" }},
		{"region", func(b *heronv1.BackupSettings) { b.Region = "region/escape" }},
		{"access_key", func(b *heronv1.BackupSettings) { b.AccessKey = "key\nheader" }},
		{"secret", func(b *heronv1.BackupSettings) { b.Secret = proto.String(strings.Repeat("s", 4097)) }},
		{"prefix", func(b *heronv1.BackupSettings) { b.Prefix = strings.Repeat("p", 513) }},
		{"endpoint host must be ASCII", func(b *heronv1.BackupSettings) { b.Endpoint = "https://例子.example" }},
		{"endpoint host must be ASCII", func(b *heronv1.BackupSettings) { b.Endpoint = "http://[fe80::1%25en0]:9000" }},
		{"prefix must not start or end with /", func(b *heronv1.BackupSettings) { b.Prefix = "/hub" }},
		{"prefix must not start or end with /", func(b *heronv1.BackupSettings) { b.Prefix = "hub/" }},
		{"prefix must not start or end with / or contain control characters", func(b *heronv1.BackupSettings) { b.Prefix = "hub\x7fbackups" }},
		{"notify.channel_ids must list at most 16 channel IDs, duplicates included; got 17", func(b *heronv1.BackupSettings) {
			ids := make([]int64, maxNotifyChannels+1)
			for i := range ids {
				ids[i] = int64(i + 1)
			}
			b.Notify = &heronv1.BackupNotify{ChannelIds: ids}
		}},
	} {
		t.Run(tc.want, func(t *testing.T) {
			in.Backup = fullBackup()
			tc.change(in.Backup)
			rejected(t, h, in, "backup."+tc.want, before)
		})
	}
	for _, change := range []func(*heronv1.BackupSettings){
		func(b *heronv1.BackupSettings) { b.Endpoint = "https://xn--fsqu00a.example:9000/base" },
		func(b *heronv1.BackupSettings) { b.Prefix = "hub/nested backups" },
		func(b *heronv1.BackupSettings) { b.Prefix = "" },
	} {
		in.Backup = fullBackup()
		change(in.Backup)
		saveSettings(t, h, in)
	}
}

// 渠道必须存在，错误点名不存在的 ID，整次更新不写入。
func TestBackupNotifyChannelMustExist(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	a := saveChannel(t, h, webhook("https://hooks.example/a")).Id
	in := validSettings()
	in.Backup = fullBackup()
	in.Backup.Notify = &heronv1.BackupNotify{ChannelIds: []int64{a}}
	before := saveSettings(t, h, in)
	in.Backup.Notify = &heronv1.BackupNotify{ChannelIds: []int64{a, 999}}
	in.Title = "must-not-be-saved"
	rejected(t, h, in, "backup.notify.channel_ids: channel 999 does not exist", before)
}

// 解码预算装得下满额设置的最坏请求（worstCaseSettings），两个渠道列表的 ID 都取满额个数的 19 位正数，每个都占满预算里
// 的 maxChannelIDJSONBytes。这些 ID 不存在，请求因而在写事务里以 InvalidArgument 点名 ID 结束——能走到那一步，说明解码
// 没有超出预算。SaveSettings 先存备份再存登录通知，点名的是备份列表里排序后第一个不存在的 ID，即请求里最后、最小的
// 那个，所以它也说明整个列表都被解码了。
func TestUpdateSettingsBudgetFitsFullBackupWithWorstCaseEscaping(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	ids := boundaryChannelIDs()
	status, b := postUpdateSettings(t, h, worstCaseSettings(t, ids))
	want := fmt.Sprintf("backup.notify.channel_ids: channel %s does not exist", ids[len(ids)-1])
	if status != http.StatusBadRequest || !strings.Contains(b, `"invalid_argument"`) || !strings.Contains(b, want) {
		t.Fatalf("worst-case settings request: %d %s; want invalid_argument %q", status, b, want)
	}
}
