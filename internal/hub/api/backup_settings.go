package api

import (
	"strings"
	"unicode"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/s3"
	"github.com/xjetry/heron/internal/hub/store"
	"google.golang.org/protobuf/proto"
)

// 备份字符串项的字节上限（渠道个数上限见 maxNotifyChannels）。settingsBudget 由这些常量登记解码预算，改这里即改预算。
const (
	maxEndpointBytes  = 2048
	maxBucketBytes    = 63
	maxRegionBytes    = 64
	maxAccessKeyBytes = 128
	maxSecretBytes    = 4096
	maxPrefixBytes    = 512
)

// cleanBackup 校验协议层的约束并构造存储更新；nil 表示请求里没有 backup，存储不动任何备份键。
// 四个数值的范围与渠道是否存在由 store.SaveSettings 在写事务里裁决（范围表只在 store 一处）。
func cleanBackup(in *heronv1.BackupSettings) (*store.BackupSettingsUpdate, error) {
	if in == nil {
		return nil, nil
	}
	for _, f := range []struct {
		name, value string
		max         int
	}{
		{"endpoint", in.Endpoint, maxEndpointBytes}, {"bucket", in.Bucket, maxBucketBytes}, {"region", in.Region, maxRegionBytes},
		{"access_key", in.AccessKey, maxAccessKeyBytes}, {"secret", in.GetSecret(), maxSecretBytes}, {"prefix", in.Prefix, maxPrefixBytes},
	} {
		if len(f.value) > f.max {
			return nil, invalid("backup.%s must be at most %d bytes", f.name, f.max)
		}
	}
	if in.Endpoint != "" {
		if err := s3.ValidateEndpoint(in.Endpoint); err != nil {
			return nil, invalid("%s", err)
		}
	}
	if in.Bucket != "" {
		if err := s3.ValidateBucket(in.Bucket); err != nil {
			return nil, invalid("%s", err)
		}
	}
	for _, f := range []struct{ name, value string }{{"region", in.Region}, {"access_key", in.AccessKey}} {
		if err := s3.ValidateSigningIdentifier(f.name, f.value); err != nil {
			return nil, invalid("%s", err)
		}
	}
	// 对象键是 <prefix>/<层>/…。允许首尾的 / 会让同一个意图有两种写法（hub 与 hub/ 拼出 hub/config/… 与
	// hub//config/…）：改了写法，旧对象就落在另一组键下，按当前前缀的列举与保留删除不再覆盖它们。写侧只收一种写法。
	if strings.HasPrefix(in.Prefix, "/") || strings.HasSuffix(in.Prefix, "/") || strings.ContainsFunc(in.Prefix, unicode.IsControl) {
		return nil, invalid("backup.prefix must not start or end with / or contain control characters")
	}
	region := in.Region
	if region == "" {
		region = "auto"
	}
	update := &store.BackupSettingsUpdate{Endpoint: in.Endpoint, Bucket: in.Bucket, Region: region, AccessKey: in.AccessKey, Secret: in.Secret, Prefix: in.Prefix, ConfigIntervalS: in.ConfigIntervalS, MetricsIntervalS: in.MetricsIntervalS, ConfigKeep: in.ConfigKeep, MetricsKeep: in.MetricsKeep}
	if n := in.GetNotify(); n != nil {
		var err error
		if update.Channels, err = cleanChannelIDs(store.BackupNotifyList, n.ChannelIds); err != nil {
			return nil, err
		}
	}
	return update, nil
}

// 读侧允许列表：secret 不在其中，只回显由它推出的 has_secret。GetSettings 与 UpdateSettings 的回显共用此处。
func backupProto(b store.BackupSettings) *heronv1.BackupSettings {
	return &heronv1.BackupSettings{
		Endpoint: b.Target.Endpoint, Bucket: b.Target.Bucket, Region: b.Target.Region, AccessKey: b.Target.AccessKey, Prefix: b.Prefix,
		ConfigIntervalS: proto.Uint32(b.ConfigIntervalS), MetricsIntervalS: proto.Uint32(b.MetricsIntervalS), ConfigKeep: proto.Uint32(b.ConfigKeep), MetricsKeep: proto.Uint32(b.MetricsKeep),
		Notify: &heronv1.BackupNotify{ChannelIds: b.Channels}, HasSecret: b.Target.Secret != "",
	}
}
