package api

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/store"
)

func validSettings() *heronv1.Settings {
	return &heronv1.Settings{Title: "状态", Theme: "dark", AccentColor: "#112233", Logo: "data:image/png;base64,iVBORw0KGgo=", CustomCss: "body { color: red }", PublicEnabled: proto.Bool(true)}
}

// defaultBackup 是从未保存过备份时的回显：GetSettings 与 UpdateSettings 的响应总带 backup。
func defaultBackup() *heronv1.BackupSettings {
	return &heronv1.BackupSettings{Region: "auto", ConfigIntervalS: proto.Uint32(300), MetricsIntervalS: proto.Uint32(86400), ConfigKeep: proto.Uint32(48), MetricsKeep: proto.Uint32(14), Notify: &heronv1.BackupNotify{}}
}

func withSettings(change func(*heronv1.Settings)) *heronv1.Settings {
	s := validSettings()
	change(s)
	return s
}

func saveSettings(t *testing.T, h *harness, in *heronv1.Settings) *heronv1.Settings {
	t.Helper()
	resp, err := h.admin.UpdateSettings(t.Context(), connect.NewRequest(&heronv1.UpdateSettingsRequest{Settings: in}))
	if err != nil {
		t.Fatalf("UpdateSettings(%v): %v", in, err)
	}
	return resp.Msg.GetSettings()
}

func currentSettings(t *testing.T, h *harness) *heronv1.Settings {
	t.Helper()
	resp, err := h.admin.GetSettings(t.Context(), connect.NewRequest(&heronv1.GetSettingsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg.GetSettings()
}

// rejected 断言更新被拒、错误含 want，且库里的外观仍是 before：一项不合约束，整次更新什么都不写。
// 先把库复位到 before，每个子用例都从同一状态开始：前一个子用例被错误接受时，红只落在它自己身上。
func rejected(t *testing.T, h *harness, in *heronv1.Settings, want string, before *heronv1.Settings) {
	t.Helper()
	saveSettings(t, h, before)
	_, err := h.admin.UpdateSettings(t.Context(), connect.NewRequest(&heronv1.UpdateSettingsRequest{Settings: in}))
	if codeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want InvalidArgument containing %q", err, want)
	}
	if got := currentSettings(t, h); !proto.Equal(got, before) {
		t.Fatalf("rejected update changed settings to %v", got)
	}
}

func TestUpdateSettingsValidatesTitleThemeAndAccent(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	before := saveSettings(t, h, validSettings())
	for _, c := range []struct {
		name string
		in   *heronv1.Settings
		want string
	}{
		{"title", withSettings(func(s *heronv1.Settings) { s.Title = strings.Repeat("字", 65) }), "settings.title must be at most 64 characters after removing control characters and surrounding whitespace; got 65"},
		{"title raw bytes", withSettings(func(s *heronv1.Settings) { s.Title = strings.Repeat(" ", maxTitleBytes) + "a" }), "settings.title must be at most 1024 bytes before cleaning; got 1025"},
		{"theme empty", withSettings(func(s *heronv1.Settings) { s.Theme = "" }), `settings.theme must be one of auto, light, dark; got ""`},
		{"theme case", withSettings(func(s *heronv1.Settings) { s.Theme = "Dark" }), `settings.theme must be one of auto, light, dark; got "Dark"`},
		{"accent short", withSettings(func(s *heronv1.Settings) { s.AccentColor = "#12345" }), `settings.accent_color must be empty (the default color) or #rrggbb with six hex digits; got "#12345"`},
		{"accent long", withSettings(func(s *heronv1.Settings) { s.AccentColor = "#1234567" }), `got "#1234567"`},
		{"accent without hash", withSettings(func(s *heronv1.Settings) { s.AccentColor = "123456" }), `got "123456"`},
		{"accent not hex", withSettings(func(s *heronv1.Settings) { s.AccentColor = "#gggggg" }), `got "#gggggg"`},
	} {
		t.Run(c.name, func(t *testing.T) { rejected(t, h, c.in, c.want, before) })
	}
}

func TestUpdateSettingsCleansTitleAndAccentAndEchoes(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	// 总闸、国家查询两项、backup 与 login_notify 没有提交，回显的是从未保存过时的值（login_notify 关闭即空 message）；
	// 后端回显夹具装配的 HTTP 后端。
	want := &heronv1.Settings{Title: "运行状态", Theme: "light", AccentColor: "#abcdef", PublicEnabled: proto.Bool(true), GeoEnabled: proto.Bool(true), GeoUrl: proto.String("https://ipinfo.io/{ip}/country"),
		GeoBackend: heronv1.GeoBackend_GEO_BACKEND_HTTP, LoginNotify: &heronv1.LoginNotify{}, TrafficReport: &heronv1.TrafficReport{}}
	want.Backup = defaultBackup()
	if got := saveSettings(t, h, &heronv1.Settings{Title: " \u202e\x07运行状态 \t", Theme: "light", AccentColor: "#AbCdEf"}); !proto.Equal(got, want) {
		t.Fatalf("echo = %v, want %v", got, want)
	}
	if got := currentSettings(t, h); !proto.Equal(got, want) {
		t.Fatalf("stored = %v, want %v", got, want)
	}
	// 控制字符不计入 64 个字符；清洗前的字节上限恰好用满也照常保存。
	saveSettings(t, h, &heronv1.Settings{Title: strings.Repeat("字", 64) + "\x00\x01", Theme: "auto"})
	if got := saveSettings(t, h, &heronv1.Settings{Title: strings.Repeat(" ", maxTitleBytes-1) + "a", Theme: "auto"}); got.GetTitle() != "a" {
		t.Fatalf("title at the raw byte limit: echo %q, want \"a\"", got.GetTitle())
	}
}

// 标题与节点名是同一种显示文字，清洗结果必须一致；任何一边换成自己的写法，这里就会分叉。
func TestTitleAndNodeNameCleanAlike(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{" \x01 状态\u202e 页 \x7f\t", "\u0085a\u200cb", "\u2066x\u2069 "} {
		name, err := cleanName(raw)
		if err != nil {
			t.Fatal(err)
		}
		a, err := cleanAppearance(&heronv1.Settings{Title: raw, Theme: "auto"})
		if err != nil {
			t.Fatal(err)
		}
		if a.Title != name {
			t.Fatalf("title %q, node name %q from %q", a.Title, name, raw)
		}
	}
}

const noGroup = "settings must give at least one group: the appearance (title, theme, accent_color, logo, custom_css; given when any of them is non-empty), public_enabled, the country lookup (geo_enabled, geo_url), backup, login_notify, heartbeat, or traffic_report"

// UpdateSettings 按组判定、各组彼此独立：外观五项任一非空即算给出并整体校验，所以只带 title 的请求报 theme 的错；
// 只带国家查询两项之一、只带总闸或只带备份（部分项或全部项）的请求照常保存，其余各组原样保留；一组都没给出（含整个 settings 缺失）的请求被拒并
// 点名各组。
func TestUpdateSettingsGroupsAreIndependent(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	before := saveSettings(t, h, validSettings())
	rejected(t, h, &heronv1.Settings{Title: "只改标题"}, `settings.theme must be one of auto, light, dark; got ""`, before)
	rejected(t, h, &heronv1.Settings{}, noGroup, before)
	rejected(t, h, nil, noGroup, before)
	want := proto.Clone(before).(*heronv1.Settings)
	for _, c := range []struct {
		name  string
		in    *heronv1.Settings
		apply func(*heronv1.Settings)
	}{
		{"geo only", &heronv1.Settings{GeoEnabled: proto.Bool(true)}, func(s *heronv1.Settings) { s.GeoEnabled = proto.Bool(true) }},
		{"geo_url only", &heronv1.Settings{GeoUrl: proto.String("https://geo.example/{ip}")}, func(s *heronv1.Settings) { s.GeoUrl = proto.String("https://geo.example/{ip}") }},
		{"public_enabled only", &heronv1.Settings{PublicEnabled: proto.Bool(false)}, func(s *heronv1.Settings) { s.PublicEnabled = proto.Bool(false) }},
		{"backup only", &heronv1.Settings{Backup: &heronv1.BackupSettings{ConfigKeep: proto.Uint32(30)}}, func(s *heronv1.Settings) { s.Backup.ConfigKeep = proto.Uint32(30) }},
		{"full backup only", &heronv1.Settings{Backup: fullBackup()}, func(s *heronv1.Settings) { s.Backup = echoOf(fullBackup(), true) }},
	} {
		c.apply(want)
		if got := saveSettings(t, h, c.in); !proto.Equal(got, want) {
			t.Fatalf("%s: echo = %v, want %v", c.name, got, want)
		}
		if got := currentSettings(t, h); !proto.Equal(got, want) {
			t.Fatalf("%s: stored = %v, want %v", c.name, got, want)
		}
	}
}

// Settings 的每个字段都必须归进 appearanceFields、readOnlySettingsFields 或"有 presence 的独立一组"之一，并按所属的类
// 生效。按 proto 描述枚举全部字段，逐个单独给出：
//   - 外观字段：单给报 theme 的错（外观按组整体校验，不被当作一组都没给）；配上 theme 给出其合法样例值，回显的外观恰是
//     theme 与这一项、其余外观项清空，钉住 cleanAppearance 逐项转抄与整组替换。
//   - 只读回显字段：单给等于一组都没给。
//   - 有 presence 的字段：单给照常保存，回显里只有这一项被写入，外观与其余各组原样保留。
//   - 其余字段直接失败：新增字段的人必须表态它属哪一类。
//
// 样例值取清洗后的形式，回显与输入逐字相同。新增的外观字符串字段没有样例时按 "x" 试，新增的 presence 布尔字段按 true 试；
// 取值有约束的新字段把合法样例补进这里。
func TestUpdateSettingsEveryFieldIsClassified(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	before := saveSettings(t, h, validSettings())
	fields := before.ProtoReflect().Descriptor().Fields()
	for _, name := range slices.Concat(appearanceFields, readOnlySettingsFields) {
		if fields.ByName(name) == nil {
			t.Errorf("classified field %s is not in Settings", name)
		}
	}
	channel := saveChannel(t, h, webhook("https://hooks.example/classified")).Id
	appearance := map[protoreflect.Name]string{"title": "新标题", "theme": "light", "accent_color": "#abcdef", "logo": "data:image/png;base64,iVBORw0KGgo=", "custom_css": "a{}"}
	type sample struct{ in, echo func(*heronv1.Settings) }
	presence := map[protoreflect.Name]sample{
		"public_enabled": {in: func(s *heronv1.Settings) { s.PublicEnabled = proto.Bool(false) }},
		"geo_enabled":    {in: func(s *heronv1.Settings) { s.GeoEnabled = proto.Bool(true) }},
		"geo_url":        {in: func(s *heronv1.Settings) { s.GeoUrl = proto.String("https://geo.example/{ip}") }},
		"backup": {
			in:   func(s *heronv1.Settings) { s.Backup = &heronv1.BackupSettings{ConfigKeep: proto.Uint32(24)} },
			echo: func(s *heronv1.Settings) { s.Backup.ConfigKeep = proto.Uint32(24) },
		},
		"login_notify": {in: func(s *heronv1.Settings) { s.LoginNotify = &heronv1.LoginNotify{ChannelIds: []int64{channel}} }},
		// heartbeat 是只写地址：回显里 url 恒为空，has_url 与 url_host 由库里的 url 推出，不是输入里给的。
		"heartbeat": {
			in: func(s *heronv1.Settings) {
				s.Heartbeat = &heronv1.Heartbeat{Url: "https://hc.example/ping/abc", IntervalS: 120, Method: heronv1.HeartbeatMethod_HEARTBEAT_METHOD_POST}
			},
			echo: func(s *heronv1.Settings) {
				s.Heartbeat = &heronv1.Heartbeat{IntervalS: 120, Method: heronv1.HeartbeatMethod_HEARTBEAT_METHOD_POST, HasUrl: true, UrlHost: "hc.example"}
			},
		},
		"traffic_report": {in: func(s *heronv1.Settings) {
			s.TrafficReport = &heronv1.TrafficReport{Enabled: true, Weekly: true, Hour: 9, ChannelIds: []int64{channel}}
		}},
	}
	for i := range fields.Len() {
		fd := fields.Get(i)
		t.Run(string(fd.Name()), func(t *testing.T) {
			in := &heronv1.Settings{}
			// 起点是写回 before 之后库里的实际设置：heartbeat 一旦配置过就总在回显里（has_url 由库里的 url 推出），写回
			// before 不会把它变回缺席，前面子用例留下的它属于"其余各组原样保留"。
			saveSettings(t, h, before)
			want := currentSettings(t, h)
			switch {
			case slices.Contains(appearanceFields, fd.Name()):
				v, ok := appearance[fd.Name()]
				if !ok {
					v = "x"
				}
				if fd.Name() != "theme" {
					alone := &heronv1.Settings{}
					alone.ProtoReflect().Set(fd, protoreflect.ValueOfString(v))
					rejected(t, h, alone, `settings.theme must be one of auto, light, dark; got ""`, before)
					in.Theme, want.Theme = "auto", "auto"
				}
				for _, name := range appearanceFields {
					want.ProtoReflect().Clear(fields.ByName(name))
				}
				in.ProtoReflect().Set(fd, protoreflect.ValueOfString(v))
				want.ProtoReflect().Set(fd, protoreflect.ValueOfString(v))
				if fd.Name() != "theme" {
					want.Theme = "auto"
				}
			case slices.Contains(readOnlySettingsFields, fd.Name()):
				switch fd.Kind() {
				case protoreflect.StringKind:
					in.ProtoReflect().Set(fd, protoreflect.ValueOfString("x"))
				case protoreflect.EnumKind:
					in.ProtoReflect().Set(fd, protoreflect.ValueOfEnum(1))
				default:
					t.Fatalf("no sample for read-only field %s of kind %s", fd.Name(), fd.Kind())
				}
				rejected(t, h, in, noGroup, before)
				return
			case fd.HasPresence():
				c, ok := presence[fd.Name()]
				switch {
				case ok:
				case fd.Kind() == protoreflect.BoolKind:
					c.in = func(s *heronv1.Settings) { s.ProtoReflect().Set(fd, protoreflect.ValueOfBool(true)) }
				default:
					t.Fatalf("no sample for presence field %s of kind %s", fd.Name(), fd.Kind())
				}
				if c.echo == nil {
					c.echo = c.in
				}
				c.in(in)
				c.echo(want)
			default:
				t.Fatalf("field %s is neither in appearanceFields nor in readOnlySettingsFields and has no presence: classify it", fd.Name())
			}
			saveSettings(t, h, before)
			if got := saveSettings(t, h, in); !proto.Equal(got, want) {
				t.Fatalf("%s alone: echo = %v, want %v", fd.Name(), got, want)
			}
		})
	}
}

func TestUpdateSettingsLogoAcceptsOnlyOneShape(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	before := saveSettings(t, h, validSettings())
	shape := "settings.logo must be empty or data:<type>;base64,<data> with <type> one of image/png, image/jpeg, image/webp, image/svg+xml"
	data := "settings.logo: the data after ;base64, must be non-empty standard base64"
	for _, c := range []struct{ name, logo, want string }{
		{"scheme case", "DATA:image/png;base64,aGk=", shape},
		{"type case", "data:IMAGE/PNG;base64,aGk=", shape},
		{"parameter", "data:image/png;charset=utf-8;base64,aGk=", shape},
		{"not base64", "data:image/svg+xml,<svg/>", shape},
		{"not an image", "data:text/html;base64,PGgxPg==", shape},
		{"type prefix", "data:image/pngx;base64,aGk=", shape},
		{"javascript", "javascript:alert(1)", shape},
		{"leading space", " data:image/png;base64,aGk=", shape},
		{"empty data", "data:image/png;base64,", data},
		{"trailing newline", "data:image/png;base64,aGk=\n", data},
		{"inner newline", "data:image/png;base64,aG\nk=", data},
		{"url-safe alphabet", "data:image/png;base64,-_8=", data},
		{"unpadded", "data:image/png;base64,aGk", data},
		{"fragment", "data:image/png;base64,aGk=#x", data},
		{"too large", "data:image/png;base64," + strings.Repeat("A", 131052), "settings.logo must be at most 131072 bytes as a data: URL; got 131074"},
		// 大小检查排在形态检查之前，报哪一条就说明边界落在哪：恰好 131072 字节的值过了大小检查、错在 base64 长度，
		// 多一个字节就是大小错误。
		{"at the limit", "data:image/png;base64," + strings.Repeat("A", 131050), data},
		{"one byte over", "data:image/png;base64," + strings.Repeat("A", 131051), "settings.logo must be at most 131072 bytes as a data: URL; got 131073"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rejected(t, h, withSettings(func(s *heronv1.Settings) { s.Logo = c.logo }), c.want, before)
		})
	}
	for _, logo := range []string{
		"data:image/png;base64," + strings.Repeat("A", 131048), // 131070 字节：base64 长度须为 4 的倍数，这是不超过上限的最大值
		"data:image/jpeg;base64,/9j/4A==",
		"data:image/webp;base64,UklGRg==",
		"data:image/svg+xml;base64,PHN2Zy8+",
		longestLogo(),
		"",
	} {
		if got := saveSettings(t, h, withSettings(func(s *heronv1.Settings) { s.Logo = logo })); got.GetLogo() != logo {
			t.Fatalf("logo not stored verbatim: %.40q", got.GetLogo())
		}
	}
}

func TestUpdateSettingsCustomCSSRejectsOnlyLiteralEndTagOpen(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	before := saveSettings(t, h, validSettings())
	for _, c := range []struct {
		css string
		at  int
	}{{"</style>", 0}, {"a{}</STYLE>", 3}, {"</ style>", 0}, {"a</b", 1}, {"</", 0}} {
		t.Run(c.css, func(t *testing.T) {
			want := fmt.Sprintf(`settings.custom_css must not contain "</" (it could end the page's <style> element); found at byte %d`, c.at)
			rejected(t, h, withSettings(func(s *heronv1.Settings) { s.CustomCss = c.css }), want, before)
		})
	}
	rejected(t, h, withSettings(func(s *heronv1.Settings) { s.CustomCss = strings.Repeat("a", 65537) }), "settings.custom_css must be at most 65536 bytes; got 65537", before)
	// 到不了 HTML 标记化器的写法：CSS 转义与 HTML 实体在 <style> 的 RAWTEXT 里都不被解码。
	for _, css := range []string{`a::before { content: "\3c/style>" }`, "/* &lt;/style> */", `a::after { content: "<\/style>" }`, "/* ＜/style> */", "a < /style {}", strings.Repeat("a", 65536)} {
		if got := saveSettings(t, h, withSettings(func(s *heronv1.Settings) { s.CustomCss = css })); got.GetCustomCss() != css {
			t.Fatalf("css not stored verbatim: %.40q", got.GetCustomCss())
		}
	}
}

// longestLogo 是不超过 maxLogoBytes 的最长合法 logo：对每个类型，base64 数据取不超过余量的最大 4 的倍数，
// 再在类型之间取总长最大的。前缀最长的类型不一定最长，取整会吃掉差额。
func longestLogo() string {
	var out string
	for _, typ := range logoTypes {
		prefix := "data:" + typ + ";base64,"
		logo := prefix + strings.Repeat("A", (maxLogoBytes-len(prefix))/4*4)
		if len(logo) > len(out) {
			out = logo
		}
	}
	return out
}

// worstCaseSettings 里用例要核对保存结果的几项。服务地址与 endpoint 带固定开头才是合法值：服务地址的 {ip} 在路径里、
// < 在查询串里；endpoint 的 & 在路径里。
const (
	worstGeoPrefix      = "https://geo.example/{ip}?"
	worstEndpointPrefix = "https://s3.example/"
)

var (
	worstGeoURL       = worstGeoPrefix + strings.Repeat("<", maxGeoURLBytes-len(worstGeoPrefix))
	worstEndpoint     = worstEndpointPrefix + strings.Repeat("&", maxEndpointBytes-len(worstEndpointPrefix))
	worstBackupPrefix = strings.Repeat("<", maxPrefixBytes)
	worstBackupSecret = strings.Repeat("\x01", maxSecretBytes)
	// 心跳地址在预算里按最坏转义登记，但那样的值（控制字符）url.Parse 拒绝；换成等长、合法且不含凭据片段的地址。
	worstHeartbeatURL = "https://hc.example/ping/" + strings.Repeat("a", maxHeartbeatURLBytes-len("https://hc.example/ping/"))
)

// worstCaseSettings 复用字段生成器，把编码边界中不满足业务约束的值替换为可保存的满额设置。
// 渠道 ID 由调用方给出、填进每个渠道列表（备份通知与登录通知），其存在性决定保存结果；不在这里复制预算或语法算式。
func worstCaseSettings(t *testing.T, channelIDs []string) []byte {
	t.Helper()
	generators := settingsValueGenerators(channelIDs)
	for path, value := range map[string]any{
		"logo": longestLogo(), "geo_url": worstGeoURL,
		"backup.endpoint": worstEndpoint, "backup.bucket": strings.Repeat("b", maxBucketBytes),
		"backup.region": strings.Repeat("<", maxRegionBytes), "backup.access_key": strings.Repeat("<", maxAccessKeyBytes),
		"backup.prefix":            worstBackupPrefix,
		"backup.config_interval_s": 86400, "backup.metrics_interval_s": 604800,
		"backup.config_keep": 1000, "backup.metrics_keep": 1000,
		"heartbeat.url": worstHeartbeatURL, "heartbeat.interval_s": 3600, "heartbeat.method": "HEARTBEAT_METHOD_POST",
		"traffic_report.enabled": true, "traffic_report.daily": true, "traffic_report.hour": store.TrafficReportMaxHour,
	} {
		generators[path] = func() any { return value }
	}
	return settingsBody(t, generators)
}

// postUpdateSettings 以 JSON 调 UpdateSettings：解码预算按线上的字节计，只有 JSON 请求才测得到它。
func postUpdateSettings(t *testing.T, h *harness, body []byte) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+"/heron.v1.AdminService/UpdateSettings", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// 解码预算不够时，connect 在方法体之前就以 ResourceExhausted 拒绝，校验根本到不了。解码预算装得下满额设置在最坏转义下
// 的 JSON（worstCaseSettings）。渠道取满额个数的已存在渠道，这次保存才能写入；它们的 ID 很短，满额长度的 ID 由
// TestUpdateSettingsBudgetFitsFullBackupWithWorstCaseEscaping 覆盖。connect 丢弃不认识的字段，保存后总闸、国家查询两项、
// 备份各项与登录通知渠道确实变成请求里的值，才说明它们按字段被解码、这是一份全字段的设置；查询开关从未保存过时就是
// false，所以先把它打开。
func TestUpdateSettingsBudgetFitsFullSettingsWithWorstCaseEscaping(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	saveSettings(t, h, &heronv1.Settings{Theme: "auto", GeoEnabled: proto.Bool(true)})
	var ids []int64
	var idTexts []string
	for i := range maxNotifyChannels {
		id := saveChannel(t, h, webhook(fmt.Sprintf("https://hooks.example/%d", i))).Id
		ids = append(ids, id)
		idTexts = append(idTexts, fmt.Sprint(id))
	}
	body := worstCaseSettings(t, idTexts)
	// 请求长度独立于 Handler 的上限变量；上限少算一字节时，同一请求必须被拒。
	// 空白不改变字段值，恰好达到预算仍须成功保存，证明读取上限包含等号。
	if len(body) > budgetTotal() {
		t.Fatalf("full settings body=%d exceeds budget=%d", len(body), budgetTotal())
	}
	body = append(body, bytes.Repeat([]byte(" "), budgetTotal()-len(body))...)
	if status, b := postUpdateSettings(t, h, body); status != http.StatusOK {
		t.Fatalf("full settings escaped worst case: %d %s", status, b)
	}
	if h.store.PublicEnabled() {
		t.Fatal("public_enabled in the worst-case request was not applied")
	}
	st, err := h.store.Settings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if st.Geo.Enabled || st.Geo.URL != worstGeoURL {
		t.Fatalf("geo_enabled or geo_url in the worst-case request was not applied: enabled=%v url=%.40q", st.Geo.Enabled, st.Geo.URL)
	}
	if b := st.Backup; b.Target.Endpoint != worstEndpoint || b.Target.Secret != worstBackupSecret || b.Prefix != worstBackupPrefix ||
		b.MetricsIntervalS != 604800 || b.MetricsKeep != 1000 || !slices.Equal(b.Channels, ids) {
		t.Fatalf("backup in the worst-case request was not applied: endpoint=%.40q prefix=%.40q secret %d bytes, metrics %ds/%d, channels %v",
			b.Target.Endpoint, b.Prefix, len(b.Target.Secret), b.MetricsIntervalS, b.MetricsKeep, b.Channels)
	}
	if !slices.Equal(st.LoginChannelIDs, ids) {
		t.Fatalf("login_notify in the worst-case request was not applied: %v, want %v", st.LoginChannelIDs, ids)
	}
	if r := st.TrafficReport; !r.Enabled || r.Hour != store.TrafficReportMaxHour || !slices.Equal(r.Channels, ids) {
		t.Fatalf("traffic_report in the worst-case request was not applied: %+v, want channels %v", r, ids)
	}
}

func TestGetStorageStatsMatchesTheStore(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	h.createNode(t, "n")
	resp, err := h.admin.GetStorageStats(t.Context(), connect.NewRequest(&heronv1.GetStorageStatsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	counts := rowCounts(t, h.dbPath)
	if resp.Msg.SqlObservedAt == nil || resp.Msg.GetSqlObservedAt() != h.clk.Now().Unix() {
		t.Fatalf("sql_observed_at = %v, want %d", resp.Msg.SqlObservedAt, h.clk.Now().Unix())
	}
	var names []string
	for _, tr := range resp.Msg.GetTables() {
		names = append(names, tr.GetName())
		if int64(tr.GetRows()) != counts[tr.GetName()] {
			t.Errorf("%s: %d rows, store says %d", tr.GetName(), tr.GetRows(), counts[tr.GetName()])
		}
	}
	if resp.Msg.GetDbBytes() == 0 || len(names) != len(counts) || !slices.IsSorted(names) || counts["node"] != 1 {
		t.Fatalf("stats = %v, store = %v", resp.Msg, counts)
	}
}

// store.NotifyLists 是全部通知渠道选择列表；api 为每个列表登记请求里的字段路径（notifyListFields），两边不能与登记表
// 脱节：漏登记路径的列表报错时点不出字段。
func TestNotifyListsHaveRequestFields(t *testing.T) {
	t.Parallel()
	for _, l := range store.NotifyLists {
		if notifyListFields[l.List] == "" {
			t.Errorf("notify list %s has no request field path in notifyListFields", l.List)
		}
	}
	if len(notifyListFields) != len(store.NotifyLists) {
		t.Errorf("notifyListFields registers %d lists; store.NotifyLists has %d", len(notifyListFields), len(store.NotifyLists))
	}
}
