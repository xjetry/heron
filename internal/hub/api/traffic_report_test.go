package api

import (
	"fmt"
	"slices"
	"testing"

	"google.golang.org/protobuf/proto"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
)

// traffic_report 必须是带 presence 的 message，字段号为 14。UpdateSettings 用 presence 区分缺席与整组替换；号或 presence 变了，旧请求会改到别的字段，或被当成没给出。
func TestTrafficReportFieldNumberAndPresence(t *testing.T) {
	t.Parallel()
	field := (&heronv1.Settings{}).ProtoReflect().Descriptor().Fields().ByName("traffic_report")
	if field == nil || field.Number() != 14 || !field.HasPresence() || field.Message() == nil {
		t.Fatalf("settings.traffic_report must be a message with presence at field 14: %v", field)
	}
}

// 缺席即不变，给出即整组替换；响应总带它，读到的整份设置原样写回不改变它。
func TestTrafficReportSettingsPresenceAndEcho(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	if got := currentSettings(t, h).TrafficReport; got == nil || !proto.Equal(got, &heronv1.TrafficReport{}) {
		t.Fatalf("default traffic_report = %v, want an empty message", got)
	}
	a := saveChannel(t, h, webhook("https://hooks.example/a")).Id
	b := saveChannel(t, h, webhook("https://hooks.example/b")).Id
	want := &heronv1.TrafficReport{Enabled: true, Daily: true, Monthly: true, Hour: 8, ChannelIds: []int64{a, b}}
	saved := saveSettings(t, h, &heronv1.Settings{TrafficReport: &heronv1.TrafficReport{Enabled: true, Daily: true, Monthly: true, Hour: 8, ChannelIds: []int64{b, a, b}}})
	if !proto.Equal(saved.TrafficReport, want) || !proto.Equal(currentSettings(t, h).TrafficReport, want) {
		t.Fatalf("saved %v / read %v, want %v", saved.TrafficReport, currentSettings(t, h).TrafficReport, want)
	}
	saveSettings(t, h, &heronv1.Settings{Theme: "dark"})
	if got := currentSettings(t, h).TrafficReport; !proto.Equal(got, want) {
		t.Fatalf("saving appearance changed traffic_report to %v", got)
	}
	whole := currentSettings(t, h)
	if got := saveSettings(t, h, whole).TrafficReport; !proto.Equal(got, want) {
		t.Fatalf("writing back the settings read changed traffic_report to %v", got)
	}
	saveSettings(t, h, &heronv1.Settings{TrafficReport: &heronv1.TrafficReport{Weekly: true}})
	if got := currentSettings(t, h).TrafficReport; !proto.Equal(got, &heronv1.TrafficReport{Weekly: true}) {
		t.Fatalf("whole-group replacement left %v", got)
	}
}

// 每条约束都点名字段，被拒时什么都不写。
func TestTrafficReportSettingsRejections(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	a := saveChannel(t, h, webhook("https://hooks.example/a")).Id
	before := saveSettings(t, h, &heronv1.Settings{TrafficReport: &heronv1.TrafficReport{Enabled: true, Daily: true, Hour: 6, ChannelIds: []int64{a}}})
	full := make([]int64, maxNotifyChannels+1)
	for i := range full {
		full[i] = a
	}
	for _, c := range []struct {
		in   *heronv1.TrafficReport
		want string
	}{
		{&heronv1.TrafficReport{Enabled: true, Hour: 6}, "settings.traffic_report must select at least one of daily, weekly or monthly when enabled"},
		{&heronv1.TrafficReport{Daily: true, Hour: 24}, "settings.traffic_report.hour must be in [0, 23]; got 24"},
		{&heronv1.TrafficReport{Daily: true, ChannelIds: full}, fmt.Sprintf("settings.traffic_report.channel_ids must list at most %d channel IDs, duplicates included; got %d", maxNotifyChannels, maxNotifyChannels+1)},
		{&heronv1.TrafficReport{Daily: true, ChannelIds: []int64{a, a + 100}}, fmt.Sprintf("settings.traffic_report.channel_ids: channel %d does not exist", a+100)},
	} {
		rejected(t, h, &heronv1.Settings{Title: "改了", Theme: "light", TrafficReport: c.in}, c.want, before)
	}
	if ids := currentSettings(t, h).GetTrafficReport().GetChannelIds(); !slices.Equal(ids, []int64{a}) {
		t.Fatalf("channels after rejections = %v", ids)
	}
}
