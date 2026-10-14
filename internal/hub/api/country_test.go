package api

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/metric"
	"github.com/xjetry/heron/internal/hub/store"
)

// 库层的三种来源与协议枚举的全部取值一一对应。
func TestCountrySourcesMapEveryValue(t *testing.T) {
	t.Parallel()
	values := heronv1.CountrySource(0).Descriptor().Values()
	var got []heronv1.CountrySource
	for _, s := range []store.CountrySource{store.CountryNone, store.CountryManual, store.CountryLookup} {
		v, ok := countrySources[s]
		if !ok {
			t.Fatalf("store source %d has no protocol value", s)
		}
		got = append(got, v)
	}
	var want []heronv1.CountrySource
	for i := 0; i < values.Len(); i++ {
		want = append(want, heronv1.CountrySource(values.Get(i).Number()))
	}
	if len(countrySources) != values.Len() || !slices.Equal(got, want) {
		t.Fatalf("protocol values %v, want %v", got, want)
	}
}

func updateCountryPin(t *testing.T, h *harness, id int64, pin string) (*heronv1.Node, error) {
	t.Helper()
	req := &heronv1.UpdateNodeRequest{Id: id, Name: "n", Public: true, TrafficResetDay: 1, OfflineGraceS: proto.Uint32(0), CountryPin: pin}
	resp, err := h.admin.UpdateNode(t.Context(), connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetNode(), nil
}

func listedNode(t *testing.T, h *harness) *heronv1.Node {
	t.Helper()
	resp, err := h.admin.ListNodes(t.Context(), connect.NewRequest(&heronv1.ListNodesRequest{}))
	if err != nil || len(resp.Msg.GetNodes()) != 1 {
		t.Fatalf("ListNodes = %v %v", resp, err)
	}
	return resp.Msg.GetNodes()[0]
}

// 面板看到显示值、来源、查得值与它所属的地址、手动值；pin 优先，查得值在手动指定时照常回显，清空 pin 回落到它。
// 公开快照只带显示值：原文里没有地址，也没有来源、查得值与手动值字段。
func TestNodeCountryPinWinsAndClearingFallsBack(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, _ := h.createNode(t, "n")
	h.setPublic(t, id, "n", true)
	row := metric.Row{NodeID: id, TS: 600, CoverageStart: 600, Bucket: metric.NewBucket(), LastSeen: h.clk.Now(), Source: "8.8.8.8"}
	if _, err := h.store.WriteMinuteBatch(t.Context(), metric.Batch{Rows: []metric.Row{row}}); err != nil {
		t.Fatal(err)
	}
	if set, err := h.store.SetLookupCountry(t.Context(), id, "8.8.8.8", "US"); err != nil || !set {
		t.Fatalf("SetLookupCountry = %v %v", set, err)
	}
	check := func(stage string, n *heronv1.Node, country string, source heronv1.CountrySource, pin string) {
		t.Helper()
		if n.GetCountry() != country || n.GetCountrySource() != source || n.GetCountryLookup() != "US" || n.GetCountryIp() != "8.8.8.8" || n.GetCountryPin() != pin {
			t.Fatalf("%s: node = %v, want country %q source %s country_lookup US country_ip 8.8.8.8 pin %q", stage, n, country, source, pin)
		}
		h.clk.Advance(snapshotTTL)
		snap := pubGet(t, h, "GetSnapshot", jsonQuery("{}"), nil)
		if !bytes.Contains(snap.body, []byte(`"country":"`+country+`"`)) || bytes.Contains(snap.body, []byte("8.8.8.8")) ||
			bytes.Contains(snap.body, []byte("countryIp")) || bytes.Contains(snap.body, []byte("countryPin")) || bytes.Contains(snap.body, []byte("countrySource")) ||
			bytes.Contains(snap.body, []byte("countryLookup")) {
			t.Fatalf("%s: public snapshot must carry only the display country %q: %s", stage, country, snap.body)
		}
	}
	check("lookup only", listedNode(t, h), "US", heronv1.CountrySource_COUNTRY_SOURCE_LOOKUP, "")
	n, err := updateCountryPin(t, h, id, "JP")
	if err != nil {
		t.Fatal(err)
	}
	check("pinned (response)", n, "JP", heronv1.CountrySource_COUNTRY_SOURCE_MANUAL, "JP")
	check("pinned", listedNode(t, h), "JP", heronv1.CountrySource_COUNTRY_SOURCE_MANUAL, "JP")
	if n, err = updateCountryPin(t, h, id, ""); err != nil {
		t.Fatal(err)
	}
	check("pin cleared", n, "US", heronv1.CountrySource_COUNTRY_SOURCE_LOOKUP, "")
}

// 没有国家时来源为未指定（none），公开快照的 country 为空串（JSON 里省略）。
func TestNodeWithoutCountry(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, _ := h.createNode(t, "n")
	h.setPublic(t, id, "n", true)
	if n := listedNode(t, h); n.GetCountry() != "" || n.GetCountrySource() != heronv1.CountrySource_COUNTRY_SOURCE_UNSPECIFIED || n.GetCountryLookup() != "" || n.GetCountryIp() != "" {
		t.Fatalf("node = %v", n)
	}
	if snap := pubGet(t, h, "GetSnapshot", jsonQuery("{}"), nil); !bytes.Contains(snap.body, []byte(`"name":"n"`)) || bytes.Contains(snap.body, []byte("country")) {
		t.Fatalf("snapshot = %s", snap.body)
	}
}

// 手动值只接受两个大写字母或空串；被拒的更新什么都不写。
func TestUpdateNodeValidatesCountryPin(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	id, _ := h.createNode(t, "n")
	if _, err := updateCountryPin(t, h, id, "DE"); err != nil {
		t.Fatal(err)
	}
	for _, pin := range []string{"de", "DEU", "D", "D1", " DE", "ÄE"} {
		_, err := updateCountryPin(t, h, id, pin)
		want := "country_pin: must be empty or two uppercase letters (ISO 3166-1 alpha-2), e.g. US; got "
		if codeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), want) {
			t.Fatalf("pin %q: err = %v, want InvalidArgument containing %q", pin, err, want)
		}
		if n := listedNode(t, h); n.GetCountryPin() != "DE" {
			t.Fatalf("rejected pin %q changed the node to %v", pin, n)
		}
	}
}

// 国家查询的两项：从未保存过为开与默认服务地址；提交了就保存并回显；只改外观的请求不带它们时不改。保存的开关取与
// 默认相反的关、地址取非默认值，"不改"才与"回落到默认"分得开。
func TestUpdateSettingsGeoFieldsAbsentMeansUnchanged(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	if got := currentSettings(t, h); got.GeoEnabled == nil || !got.GetGeoEnabled() || got.GetGeoUrl() != "https://ipinfo.io/{ip}/country" {
		t.Fatalf("never saved: %v", got)
	}
	in := withSettings(func(s *heronv1.Settings) {
		s.GeoEnabled, s.GeoUrl = proto.Bool(false), proto.String("http://geo.example:8080/lookup?addr={ip}")
	})
	want := proto.Clone(in).(*heronv1.Settings)
	want.Backup = defaultBackup()
	want.GeoBackend = heronv1.GeoBackend_GEO_BACKEND_HTTP
	want.LoginNotify = &heronv1.LoginNotify{}
	want.TrafficReport = &heronv1.TrafficReport{}
	if got := saveSettings(t, h, in); !proto.Equal(got, want) {
		t.Fatalf("echo = %v, want %v", got, want)
	}
	got := saveSettings(t, h, withSettings(func(s *heronv1.Settings) { s.Title = "只改外观" }))
	if got.GetGeoEnabled() || got.GetGeoUrl() != "http://geo.example:8080/lookup?addr={ip}" || got.GetTitle() != "只改外观" {
		t.Fatalf("appearance-only update: %v", got)
	}
	if got := currentSettings(t, h); got.GetGeoEnabled() || got.GetGeoUrl() != "http://geo.example:8080/lookup?addr={ip}" {
		t.Fatalf("stored after appearance-only update: %v", got)
	}
}

func TestUpdateSettingsValidatesGeoURL(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	before := saveSettings(t, h, withSettings(func(s *heronv1.Settings) {
		s.GeoEnabled, s.GeoUrl = proto.Bool(true), proto.String("https://geo.example/{ip}")
	}))
	withURL := func(u string) *heronv1.Settings {
		return withSettings(func(s *heronv1.Settings) { s.GeoUrl = proto.String(u) })
	}
	for _, c := range []struct {
		name string
		in   *heronv1.Settings
		want string
	}{
		{"no placeholder", withURL("https://geo.example/lookup"), `settings.geo_url must contain the {ip} placeholder for the node address; got "https://geo.example/lookup"`},
		{"empty", withURL(""), `settings.geo_url must contain the {ip} placeholder`},
		{"ftp", withURL("ftp://geo.example/{ip}"), `settings.geo_url must be an absolute http:// or https:// URL once {ip} is filled in, with {ip} outside the host and port; got "ftp://geo.example/{ip}"`},
		{"relative", withURL("/{ip}/country"), `settings.geo_url must be an absolute http:// or https:// URL`},
		{"no host", withURL("https:///{ip}"), `settings.geo_url must be an absolute http:// or https:// URL`},
		{"control character", withURL("https://geo.example/{ip}\n"), `settings.geo_url must be an absolute http:// or https:// URL`},
		{"user information", withURL("https://user:secret@geo.example/{ip}"), `settings.geo_url must not contain user information`},
		// {ip} 不得在主机或端口位置：主机与端口随节点地址变化时，hub 连到哪里读不出来。能按两族样例分辨的写法报点名的
		// 那条；裸写在端口位置时两族都不是合法 URL，报通用的那条，它同样写明 {ip} 不能在主机与端口。
		{"placeholder as host", withURL("https://{ip}/country"), `settings.geo_url must not put {ip} in the host or port, including the [{ip}] form: the server the lookup goes to must not depend on the node address; got "https://{ip}/country"`},
		{"placeholder in a host label", withURL("https://{ip}.geo.example/country"), `settings.geo_url must not put {ip} in the host or port, including the [{ip}] form`},
		{"bracketed placeholder", withURL("https://[{ip}]/country"), `settings.geo_url must not put {ip} in the host or port, including the [{ip}] form: the server the lookup goes to must not depend on the node address; got "https://[{ip}]/country"`},
		{"bracketed placeholder with port", withURL("https://[{ip}]:8443/country"), `settings.geo_url must not put {ip} in the host or port, including the [{ip}] form: the server the lookup goes to must not depend on the node address; got "https://[{ip}]:8443/country"`},
		// 两族样例都能解析、只有 host 不同的写法：{ip} 在 IPv6 字面量的区域标识里。
		{"placeholder in the zone of a literal host", withURL("https://[fe80::1%25{ip}]/country"), `settings.geo_url must not put {ip} in the host or port, including the [{ip}] form`},
		// 片段不随请求发出：两族样例填入后请求行相同，地址没有进请求。
		{"placeholder only in the fragment", withURL("https://geo.example/country#{ip}"), `settings.geo_url must put {ip} in the path or query so the request carries the node address; a fragment (#...) is never sent; got "https://geo.example/country#{ip}"`},
		{"placeholder as port", withURL("https://geo.example:{ip}/country"), `settings.geo_url must be an absolute http:// or https:// URL once {ip} is filled in, with {ip} outside the host and port; got "https://geo.example:{ip}/country"`},
		{"too long", withURL("https://geo.example/{ip}?" + strings.Repeat("a", maxGeoURLBytes)), `settings.geo_url must be at most 2048 bytes; got 2073`},
	} {
		t.Run(c.name, func(t *testing.T) { rejected(t, h, c.in, c.want, before) })
	}
}
