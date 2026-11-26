package api

import (
	"testing"
	"time"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/store"
	"google.golang.org/protobuf/proto"
)

// 面板与公开页按 hub 时区切日历时段（晚高峰、日界）：GetSettings 与 GetSite 下发的必须是同一个名字。没设计费时区的
// 节点的流量周期也按它，GetTraffic 逐节点下发的周期时区就是它；设了计费时区的节点下发自己的。选一个有夏令时、偏移不是
// 整小时也无妨的真实 IANA 区，名字原样回显，不经访客时区换算。
func TestSiteTimezoneIsTheHubZoneOnEverySurface(t *testing.T) {
	t.Parallel()
	loc, err := time.LoadLocation("America/Santiago")
	if err != nil {
		t.Fatal(err)
	}
	h := newZonedHarness(t, "", loc, store.DefaultRetention)
	h.login(t)
	hubNode, _ := h.createNode(t, "hub-zone")
	ownNode, _ := h.createNode(t, "own-zone")
	if _, err := h.admin.UpdateNode(t.Context(), connect.NewRequest(&heronv1.UpdateNodeRequest{Id: ownNode, Name: "own-zone", TrafficResetDay: 1, OfflineGraceS: proto.Uint32(0), BillingTimezone: "Asia/Kolkata"})); err != nil {
		t.Fatal(err)
	}
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
	zones := map[int64]string{}
	for _, n := range traffic.Msg.GetNodes() {
		zones[n.GetNodeId()] = n.GetTraffic().GetTimezone()
	}
	for name, c := range map[string]struct{ got, want string }{
		"GetSettings.timezone":                   {settings.Msg.GetTimezone(), "America/Santiago"},
		"GetSite.timezone":                       {site.Msg.GetTimezone(), "America/Santiago"},
		"GetTraffic node without a billing zone": {zones[hubNode], "America/Santiago"},
		"GetTraffic node with a billing zone":    {zones[ownNode], "Asia/Kolkata"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", name, c.got, c.want)
		}
	}
}

// 没设计费时区的节点，days_left 回落到 Config.Location、流量周期回落到账本的 hub 时区；两者装配成不同的区，同一节点的
// 到期日界与周期就按两个时区各算各的。构造期拒绝，而不是在某个读侧露出来。
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
		{"api.Config.Location must be the traffic book's hub zone", func() { New(cfg, h.deps()) }},
		{"api.PublicConfig.Location must be the traffic book's hub zone", func() { NewPublic(pubCfg, h.publicDeps()) }},
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
