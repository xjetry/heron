package api

import (
	"testing"
	"time"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/store"
)

// 面板与公开页按 hub 时区切日历时段（晚高峰、日界），流量周期与到期日也按它算：GetSettings、GetSite 与 GetTraffic
// 下发的必须是同一个名字。选一个有夏令时、偏移不是整小时也无妨的真实 IANA 区，名字原样回显，不经访客时区换算。
func TestSiteTimezoneIsTheHubZoneOnEverySurface(t *testing.T) {
	t.Parallel()
	loc, err := time.LoadLocation("America/Santiago")
	if err != nil {
		t.Fatal(err)
	}
	h := newZonedHarness(t, "", loc, store.DefaultRetention)
	h.login(t)
	settings, err := h.admin.GetSettings(t.Context(), connect.NewRequest(&heronv1.GetSettingsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	site, err := h.publicClient().GetSite(t.Context(), connect.NewRequest(&heronv1.GetSiteRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	traffic, err := h.admin.GetTraffic(t.Context(), connect.NewRequest(&heronv1.GetTrafficRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]string{
		"GetSettings.timezone": settings.Msg.GetTimezone(),
		"GetSite.timezone":     site.Msg.GetTimezone(),
		"GetTraffic.timezone":  traffic.Msg.GetTimezone(),
	} {
		if got != "America/Santiago" {
			t.Errorf("%s = %q, want America/Santiago", name, got)
		}
	}
}

// 同一进程里 days_left、静默窗口与下发的时区名读 Config.Location，流量周期读账本的时区；两者装配成不同的区，
// 面板就会按一个时区画日历、按另一个时区算周期。构造期拒绝，而不是在某个读侧露出来。
func TestConstructorsRejectLocationOtherThanTrafficZone(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	other := time.FixedZone("UTC+8", 8*3600)
	cfg := h.svc.cfg
	cfg.Location = other
	pubCfg := h.pub.cfg
	pubCfg.Location = other
	for _, c := range []struct {
		want string
		call func()
	}{
		{"api.Config.Location must be the traffic book's zone", func() { New(cfg, h.deps()) }},
		{"api.PublicConfig.Location must be the traffic book's zone", func() { NewPublic(pubCfg, h.publicDeps()) }},
	} {
		func() {
			defer func() {
				if r := recover(); r != c.want {
					t.Errorf("panic = %v, want %s", r, c.want)
				}
			}()
			c.call()
		}()
	}
}
