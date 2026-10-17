package api

import (
	"context"
	"encoding/base64"
	"errors"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/geo"
	"github.com/xjetry/heron/internal/hub/sanitize"
	"github.com/xjetry/heron/internal/hub/store"
)

// 外观的上限。面板的 web/src/lib/appearance.ts 用同值做提交前提示，由 appearanceLimits.test.ts 对照本文件。
const (
	maxTitleRunes = 64
	// maxTitleBytes 限制清洗前的标题：清洗会去掉控制字符与首尾空白，只限清洗后的字符数，原始标题就没有上限，
	// 装不进解码预算的合法请求也就存在（settings_budget.go 的 settingsBudget 按每个字段的上限登记预算）。
	maxTitleBytes = 1 << 10
	maxLogoBytes  = 128 << 10
	maxCSSBytes   = 64 << 10
	// maxGeoURLBytes 限制国家查询的服务地址，同样是 settingsBudget 给这一项登记份额的前提。
	maxGeoURLBytes = 2 << 10
	// maxHeartbeatURLBytes 是 Settings.heartbeat.url 的字节上限，也是 settingsBudget 给这一项登记份额的前提。
	maxHeartbeatURLBytes = 2 << 10
	// maxMMDBPathBytes 是 Settings.geo_mmdb_path 在 settingsBudget 里登记的字节上限。hub 只回显自己启动参数里的路径，
	// 请求里的值被忽略，但客户端可能把 GetSettings 的回显整份送回，合法回送不能被拒；任何能打开的路径不超过
	// Linux 的 PATH_MAX 4096（macOS 为 1024），所以回显的路径落在这个份额内。比它长的路径只会来自不回送回显的
	// 客户端自造的值，超出预算时得到 resource_exhausted。
	maxMMDBPathBytes = 4 << 10
)

var (
	themes    = []string{"auto", "light", "dark"}
	logoTypes = []string{"image/png", "image/jpeg", "image/webp", "image/svg+xml"}
	accentRE  = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
)

// cleanSettings 按组判定并校验一次更新，各组彼此独立：
//   - 外观五项是一组。proto3 的 string 没有 presence，分不开"没给"与"给了空串"，所以五项任一非空即视为给出
//     （appearanceGiven），给出就整体替换并按整体校验（cleanAppearance：theme 必填，其余为空即清空）。按任一项非空
//     判定，只带 title 不带 theme 的请求得到点名 theme 的错误，而不是被当作"没给外观"静默丢弃。
//   - 总闸、国家查询两项、backup、login_notify、heartbeat 与 traffic_report 是 presence 字段，给出即改、缺席即不变
//     （见 store.SettingsUpdate、cleanGeo；backup 各项的 presence 见 cleanBackup；login_notify 给出空列表是显式关闭；
//     heartbeat 与 traffic_report 给出即整组替换）：只改其中一组的客户端不必重发外观，也就不会把它手里可能已过时的
//     外观写回去。
//
// 一组都没给出的请求什么都不会改，返回 InvalidArgument 点名各组，而不是回一个看似成功的空操作。任一项不合约束即返回
// 错误，调用方什么都不写。
func cleanSettings(in *heronv1.Settings) (store.SettingsUpdate, error) {
	if in == nil {
		in = &heronv1.Settings{}
	}
	out := store.SettingsUpdate{PublicEnabled: in.PublicEnabled}
	if appearanceGiven(in) {
		appearance, err := cleanAppearance(in)
		if err != nil {
			return store.SettingsUpdate{}, err
		}
		out.Appearance = &appearance
	}
	geoUpdate, err := cleanGeo(in)
	if err != nil {
		return store.SettingsUpdate{}, err
	}
	out.Geo = geoUpdate
	if out.Backup, err = cleanBackup(in.GetBackup()); err != nil {
		return store.SettingsUpdate{}, err
	}
	if ln := in.GetLoginNotify(); ln != nil {
		if out.LoginChannels, err = cleanChannelIDs(store.LoginNotifyList, ln.GetChannelIds()); err != nil {
			return store.SettingsUpdate{}, err
		}
	}
	if out.Heartbeat, err = cleanHeartbeat(in.GetHeartbeat()); err != nil {
		return store.SettingsUpdate{}, err
	}
	if out.TrafficReport, err = cleanTrafficReport(in.GetTrafficReport()); err != nil {
		return store.SettingsUpdate{}, err
	}
	if out.Appearance == nil && out.PublicEnabled == nil && out.Geo.Enabled == nil && out.Geo.URL == nil && out.Backup == nil && out.LoginChannels == nil && out.Heartbeat == nil && out.TrafficReport == nil {
		return store.SettingsUpdate{}, invalid("settings must give at least one group: the appearance (title, theme, accent_color, logo, custom_css; given when any of them is non-empty), public_enabled, the country lookup (geo_enabled, geo_url), backup, login_notify, heartbeat, or traffic_report")
	}
	return out, nil
}

// Settings 的每个字段属于下面三类之一，UpdateSettings 按类判定，TestUpdateSettingsEveryFieldIsClassified 按
// proto 描述枚举全部字段逐类核对：
//   - appearanceFields：外观这一组。proto3 的 string 没有 presence，任一项非空即算给出（appearanceGiven）。
//   - readOnlySettingsFields：只读回显，取自 hub 启动时的选择（settingsProto）。UpdateSettings 中缺席或给出均忽略，
//     不算给出任何一组。
//   - 其余字段都必须有 presence，各自按 presence 判定给出（cleanSettings 的"至少一组"逐个列出它们）。
//
// 既不在两份清单里、又没有 presence 的字段无从判定"给出"，枚举用例对它直接失败：新增字段的人必须表态它属哪一类。
var (
	appearanceFields       = []protoreflect.Name{"title", "theme", "accent_color", "logo", "custom_css"}
	readOnlySettingsFields = []protoreflect.Name{"geo_backend", "geo_mmdb_path"}
)

// maxNotifyChannels 是一个通知渠道选择列表（backup.notify、login_notify 与 traffic_report）的条数上限，按请求里的原始
// 条数计、重复也算：去重在解码之后，约束不了请求的字节数。没有它，合法请求的字节数就没有上界，算不出解码预算
// （settings_budget.go 的 settingsBudget 按它登记每个渠道列表）。
const maxNotifyChannels = 16

// notifyListFields 是每个通知渠道选择列表在请求里的字段路径，条数超限与渠道不存在的错误都按它点名。store.NotifyLists
// 里的每个列表都要在这里登记（TestNotifyListsHaveRequestFields 核对），漏登记的列表报错时点不出字段名。
var notifyListFields = map[store.NotifyList]string{
	store.BackupNotifyList:        "backup.notify.channel_ids",
	store.LoginNotifyList:         "settings.login_notify.channel_ids",
	store.TrafficReportNotifyList: "settings.traffic_report.channel_ids",
}

// cleanChannelIDs 是各通知渠道选择列表共用的协议层校验：只核对条数；渠道是否存在由 store.SaveSettings 在写事务里
// 裁决（saveChannelIDs），不存在时返回点名列表的 store.ChannelListError。
func cleanChannelIDs(list store.NotifyList, ids []int64) (*[]int64, error) {
	if len(ids) > maxNotifyChannels {
		return nil, invalid("%s must list at most %d channel IDs, duplicates included; got %d", notifyListFields[list], maxNotifyChannels, len(ids))
	}
	return &ids, nil
}

// appearanceGiven 是外观这一组"给出"的判定：appearanceFields 任一非空。它只读这份清单，外观的取值由 cleanAppearance
// 逐项转抄：清单新增一项而 cleanAppearance 没有转抄时，枚举用例单给那一项的样例值、核对回显，在那里失败。
func appearanceGiven(in *heronv1.Settings) bool {
	m := in.ProtoReflect()
	fields := m.Descriptor().Fields()
	for _, name := range appearanceFields {
		if m.Get(fields.ByName(name)).String() != "" {
			return true
		}
	}
	return false
}

// cleanAppearance 校验并清洗给出的外观，返回可以原样存储与下发的值。
// 标题会显示在页面与标签页上，与节点名（store.CleanName）同用 sanitize.Text 清洗；logo 与 CSS 是数据与代码，改写任何字节都可能改变含义，只校验不清洗。
func cleanAppearance(in *heronv1.Settings) (store.SiteAppearance, error) {
	if n := len(in.GetTitle()); n > maxTitleBytes {
		return store.SiteAppearance{}, invalid("settings.title must be at most %d bytes before cleaning; got %d", maxTitleBytes, n)
	}
	title := sanitize.Text(in.GetTitle(), len(in.GetTitle()))
	if n := utf8.RuneCountInString(title); n > maxTitleRunes {
		return store.SiteAppearance{}, invalid("settings.title must be at most %d characters after removing control characters and surrounding whitespace; got %d", maxTitleRunes, n)
	}
	if !slices.Contains(themes, in.GetTheme()) {
		return store.SiteAppearance{}, invalid("settings.theme must be one of %s; got %q", strings.Join(themes, ", "), in.GetTheme())
	}
	if c := in.GetAccentColor(); c != "" && !accentRE.MatchString(c) {
		return store.SiteAppearance{}, invalid("settings.accent_color must be empty (the default color) or #rrggbb with six hex digits; got %q", c)
	}
	if err := checkLogo(in.GetLogo()); err != nil {
		return store.SiteAppearance{}, err
	}
	if err := checkCSS(in.GetCustomCss()); err != nil {
		return store.SiteAppearance{}, err
	}
	return store.SiteAppearance{
		Title: title, Theme: in.GetTheme(), AccentColor: strings.ToLower(in.GetAccentColor()),
		Logo: in.GetLogo(), CustomCSS: in.GetCustomCss(),
	}, nil
}

// checkLogo 只接受 data:<type>;base64,<data> 这一种写法：type 在白名单内、全小写、不带参数，data 是带填充的
// 标准 base64。写法收窄到一种，"是不是白名单里的图片"就只有一个答案——宽松解析与浏览器的解析一旦不一致
// （参数、大小写、非 base64 形态），白名单就能被绕过。公开页只把它放进 <img src>，SVG 在 <img> 里不执行脚本。
// settingsBudget 的 logo 项按原始字节数加引号计：这里限定的前缀与 base64 字母表无需 JSON 转义。
// 放宽为接受原始 SVG 等写法会破坏这一编码前提，必须同时调整预算规则与契约用例。
// logoPrefix 是 checkLogo 接受的 data URL 开头；接受集就是"logoTypes 的每个前缀 × isBase64Char 的字母表"，
// 再经 Strict 解码。settings_budget.go 的 logoBoundary 从这两个谓词推 logo 的解码预算：前缀与字母表放宽时
// 上界随之变化，登记项不用改；绕开它们另开一条接受路径，预算就会漏算，所以新的写法必须经过这两个谓词。
func logoPrefix(mediaType string) string {
	return "data:" + mediaType + ";base64,"
}

func checkLogo(logo string) error {
	if logo == "" {
		return nil
	}
	if len(logo) > maxLogoBytes {
		return invalid("settings.logo must be at most %d bytes as a data: URL; got %d", maxLogoBytes, len(logo))
	}
	data, found := "", false
	for _, mediaType := range logoTypes {
		if data, found = strings.CutPrefix(logo, logoPrefix(mediaType)); found {
			break
		}
	}
	if !found {
		return invalid("settings.logo must be empty or data:<type>;base64,<data> with <type> one of %s; got a value starting with %q", strings.Join(logoTypes, ", "), head(logo))
	}
	const want = "settings.logo: the data after ;base64, must be non-empty standard base64 (A–Z, a–z, 0–9, + and /, padded with =)"
	// 标准库的解码器会跳过 \r 与 \n，先逐字节核对字母表，换行与空白都不算合法数据。
	if data == "" || strings.IndexFunc(data, func(r rune) bool { return !isBase64Char(r) }) >= 0 {
		return invalid("%s", want)
	}
	if _, err := base64.StdEncoding.Strict().DecodeString(data); err != nil {
		return invalid("%s: %v", want, err)
	}
	return nil
}

func isBase64Char(r rune) bool {
	return r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '+' || r == '/' || r == '='
}

// head 是错误信息里回显的开头：到第一个逗号为止、最多 64 字节，不把整张图片写进错误。
func head(s string) string {
	if i := strings.IndexByte(s, ','); i >= 0 {
		s = s[:i+1]
	}
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

// checkCSS 拒绝字面的 "</"。公开页用 textContent 写进 <style>，不经 HTML 解析；但第三方主题与任何把它内联进
// HTML 的消费者都会让 "</style" 结束元素，所以约束放在唯一的写入口。只查字面序列就覆盖了全部大小写变体
// （"</" 本身不含字母）；CSS 转义（\3c/）与 HTML 实体（&lt;/）在 <style> 的 RAWTEXT 里都不被解码，结束不了元素。
func checkCSS(css string) error {
	if len(css) > maxCSSBytes {
		return invalid("settings.custom_css must be at most %d bytes; got %d", maxCSSBytes, len(css))
	}
	if i := strings.Index(css, "</"); i >= 0 {
		return invalid(`settings.custom_css must not contain "</" (it could end the page's <style> element); found at byte %d`, i)
	}
	return nil
}

// geoSamples 是校验服务地址时填入 {ip} 的样例，两个地址族各一个，都取文档地址（RFC 5737、RFC 3849）。
var geoSamples = [2]netip.Addr{netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("2001:db8::1")}

// cleanGeo 校验国家查询的两项；缺失的项不改（见 store.GeoUpdate）。服务地址是 hub 的出站目标之一，是它
// 唯一的写入口：只接受 http(s)、必须含 {ip}，{ip} 不得在主机或端口位置、必须在路径或查询串里，且不含用户信息——请求只发地址、不带凭据，
// 而 URL 里的用户信息会被 net/http 的客户端转成 Basic 认证的 Authorization 头；这项还会原样回显，本身也不该是凭据。
//
// "{ip} 不在主机或端口位置"按它要保证的性质判定，不去切分 URL 文本：两族样例各填一次，都必须解析成带 host 的绝对
// http(s) URL，而且 host（含端口）相同。host 随地址变化，hub 会连到哪里就读不出来，写成 [{ip}] 时 hub 会直连
// IPv6 节点自己的端口；只对一族能解析的模板会让另一族的节点每次查询都失败。按 go1.27 的 url.Parse 实测：裸写在主机
// 位置或与主机名拼在一起（{ip}.example）时 IPv6 样例的冒号让解析失败；[{ip}] 时 IPv4 样例不是合法的 IP 字面量；放在
// IPv6 字面量的区域标识里（[fe80::1%25{ip}]）时两族都能解析、host 不同。这几种都报"不得在主机或端口"。裸写在端口
// 位置时两族都解析失败，与别的非法 URL 分不开，报通用的那条，它同样写明这条约束。
func cleanGeo(in *heronv1.Settings) (store.GeoUpdate, error) {
	out := store.GeoUpdate{Enabled: in.GeoEnabled}
	if in.GeoUrl == nil {
		return out, nil
	}
	raw := in.GetGeoUrl()
	if n := len(raw); n > maxGeoURLBytes {
		return store.GeoUpdate{}, invalid("settings.geo_url must be at most %d bytes; got %d", maxGeoURLBytes, n)
	}
	if !strings.Contains(raw, geo.Placeholder) {
		return store.GeoUpdate{}, invalid("settings.geo_url must contain the %s placeholder for the node address; got %q", geo.Placeholder, raw)
	}
	var parsed [2]*url.URL
	for i, sample := range geoSamples {
		if u, err := url.Parse(geo.Target(raw, sample)); err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.Opaque == "" {
			parsed[i] = u
		}
	}
	v4, v6 := parsed[0], parsed[1]
	switch {
	case v4 == nil && v6 == nil:
		return store.GeoUpdate{}, invalid("settings.geo_url must be an absolute http:// or https:// URL once {ip} is filled in, with {ip} outside the host and port; got %q", raw)
	case v4 == nil || v6 == nil || v4.Host != v6.Host:
		return store.GeoUpdate{}, invalid("settings.geo_url must not put {ip} in the host or port, including the [{ip}] form: the server the lookup goes to must not depend on the node address; got %q", raw)
	}
	if v6.User != nil {
		return store.GeoUpdate{}, invalid("settings.geo_url must not contain user information (user:password@); the lookup request carries only the address")
	}
	// {ip} 只有在请求真正发出的部分（路径或查询串）才起作用：片段不随请求发出。两族样例填入后请求行仍相同，说明地址
	// 没有进请求，每个节点发出的请求都一样；服务若按请求方地址作答，所有节点都会被记成 hub 所在的国家，且地址不变就不再重查。
	if v4.RequestURI() == v6.RequestURI() {
		return store.GeoUpdate{}, invalid("settings.geo_url must put {ip} in the path or query so the request carries the node address; a fragment (#...) is never sent; got %q", raw)
	}
	out.URL = &raw
	return out, nil
}

// settingsProto 是 GetSettings 与 UpdateSettings 共用的回显。login_notify 总带，渠道列表为空即关闭：省掉它，"已关闭"
// 与"hub 不认识这个字段"在响应里就分不出来；把读到的整份设置原样写回时，回显的空 message 是显式关闭，与当前状态
// 一致，读改写不改变它。traffic_report 同理总带：它的每一项都有库里的取值或默认值，原样写回就是原样保存。
func (s *Service) settingsProto(st store.Settings) *heronv1.Settings {
	backend, path := heronv1.GeoBackend_GEO_BACKEND_HTTP, s.cfg.Geo.MMDBPath()
	if path != "" {
		backend = heronv1.GeoBackend_GEO_BACKEND_MMDB
	}
	out := &heronv1.Settings{Title: st.Site.Title, Theme: st.Site.Theme, AccentColor: st.Site.AccentColor, Logo: st.Site.Logo, CustomCss: st.Site.CustomCSS,
		PublicEnabled: proto.Bool(st.Site.PublicEnabled), GeoEnabled: proto.Bool(st.Geo.Enabled), GeoUrl: proto.String(st.Geo.URL),
		GeoBackend: backend, GeoMmdbPath: path, Backup: backupProto(st.Backup), LoginNotify: &heronv1.LoginNotify{ChannelIds: st.LoginChannelIDs},
		TrafficReport: trafficReportProto(st.TrafficReport)}
	// heartbeat 只在库里已有这一组的键时回显：从未配置过就没有可回显的取值，也没有 has_url 可言；配置过（哪怕随后清空了
	// url）就带上它，has_url=false 表示已停用，与"不认识这个字段"区分开——与 login_notify 总带它同一理由。
	if st.Heartbeat.Set {
		out.Heartbeat = heartbeatProto(st.Heartbeat)
	}
	return out
}

// heartbeatProto 是心跳这一组的读侧回显。url 恒不回显：ping 地址本身就是密钥。has_url 与 url_host 都按库里的 url 计算。
func heartbeatProto(h store.HeartbeatSettings) *heronv1.Heartbeat {
	out := &heronv1.Heartbeat{IntervalS: h.IntervalS, Method: heartbeatMethodProto(h.Method), HasUrl: h.URL != ""}
	if h.URL != "" {
		if u, err := url.Parse(h.URL); err == nil {
			out.UrlHost = u.Host
		}
	}
	return out
}

func heartbeatMethodProto(m store.HeartbeatMethod) heronv1.HeartbeatMethod {
	switch m {
	case store.HeartbeatGet:
		return heronv1.HeartbeatMethod_HEARTBEAT_METHOD_GET
	case store.HeartbeatHead:
		return heronv1.HeartbeatMethod_HEARTBEAT_METHOD_HEAD
	default:
		return heronv1.HeartbeatMethod_HEARTBEAT_METHOD_POST
	}
}

// cleanHeartbeat 校验给出的 heartbeat 并构造存储更新；nil 表示请求里没有这一组，存储不动任何 heartbeat.* 键。
// 整组替换，所以三项都要有合法取值：url 为空是清空并停用；method 必须显式选择，UNSPECIFIED 不当作"取默认"。
// interval_s 的范围由 store.SaveSettings 在写事务里裁决（范围表只在 store 一处）。
func cleanHeartbeat(in *heronv1.Heartbeat) (*store.HeartbeatUpdate, error) {
	if in == nil {
		return nil, nil
	}
	raw := in.GetUrl()
	if len(raw) > maxHeartbeatURLBytes {
		return nil, invalid("settings.heartbeat.url must be at most %d bytes; got %d", maxHeartbeatURLBytes, len(raw))
	}
	if raw != "" {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Opaque != "" {
			return nil, invalid("settings.heartbeat.url must be empty (disables the heartbeat) or an absolute http:// or https:// URL with a host; got %q", raw)
		}
	}
	var method store.HeartbeatMethod
	switch in.GetMethod() {
	case heronv1.HeartbeatMethod_HEARTBEAT_METHOD_GET:
		method = store.HeartbeatGet
	case heronv1.HeartbeatMethod_HEARTBEAT_METHOD_POST:
		method = store.HeartbeatPost
	case heronv1.HeartbeatMethod_HEARTBEAT_METHOD_HEAD:
		method = store.HeartbeatHead
	default:
		return nil, invalid("settings.heartbeat.method must be GET, POST or HEAD; HEARTBEAT_METHOD_UNSPECIFIED is not allowed and is not taken as a default")
	}
	return &store.HeartbeatUpdate{URL: raw, IntervalS: in.GetIntervalS(), Method: method}, nil
}

func trafficReportProto(r store.TrafficReportSettings) *heronv1.TrafficReport {
	return &heronv1.TrafficReport{Enabled: r.Enabled, Daily: r.Daily, Weekly: r.Weekly, Monthly: r.Monthly, Hour: r.Hour, ChannelIds: r.Channels}
}

// cleanTrafficReport 构造这一组的存储更新；nil 表示请求里没有这一组，存储不动任何 traffic_report.* 键与它的渠道列表。
// 这里只核对渠道列表的条数（与其它选择列表同一上限）；"启用时至少一种周期"与时刻范围由 store.SaveSettings 在写事务
// 里裁决（取值表只在 store 一处，读侧按同一张表核对库里的值），返回点名字段的 store.TrafficReportError。
func cleanTrafficReport(in *heronv1.TrafficReport) (*store.TrafficReportUpdate, error) {
	if in == nil {
		return nil, nil
	}
	ids, err := cleanChannelIDs(store.TrafficReportNotifyList, in.GetChannelIds())
	if err != nil {
		return nil, err
	}
	return &store.TrafficReportUpdate{Enabled: in.GetEnabled(), Daily: in.GetDaily(), Weekly: in.GetWeekly(), Monthly: in.GetMonthly(), Hour: in.GetHour(), Channels: *ids}, nil
}

// GetHeartbeatStatus 读心跳循环的进程内状态：是否已配置取自库里的 url，其余取自内存。两个时刻与 next_at 是墙钟
// Unix 秒，从不曾跑过为 0。
func (s *Service) GetHeartbeatStatus(ctx context.Context, _ *connect.Request[heronv1.GetHeartbeatStatusRequest]) (*connect.Response[heronv1.GetHeartbeatStatusResponse], error) {
	st, err := s.store.HeartbeatSettings(ctx)
	if err != nil {
		s.log.Error("reading heartbeat settings failed", "err", err)
		return nil, internalError("reading heartbeat settings failed")
	}
	h := s.heartbeat.Status()
	return connect.NewResponse(&heronv1.GetHeartbeatStatusResponse{
		Enabled:           st.URL != "",
		LastSuccessAt:     unixOrZero(h.LastSuccessAt),
		LastFailureAt:     unixOrZero(h.LastFailureAt),
		FailureCategory:   h.FailureCategory,
		FailureHttpStatus: int32(h.FailureHTTPStatus),
		NextAt:            unixOrZero(h.NextAt),
	}), nil
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func (s *Service) GetSettings(ctx context.Context, _ *connect.Request[heronv1.GetSettingsRequest]) (*connect.Response[heronv1.GetSettingsResponse], error) {
	st, err := s.store.Settings(ctx)
	if err != nil {
		s.log.Error("reading settings failed", "err", err)
		return nil, internalError("reading settings failed")
	}
	return connect.NewResponse(&heronv1.GetSettingsResponse{Settings: s.settingsProto(st)}), nil
}

func (s *Service) UpdateSettings(ctx context.Context, req *connect.Request[heronv1.UpdateSettingsRequest]) (*connect.Response[heronv1.UpdateSettingsResponse], error) {
	in, err := cleanSettings(req.Msg.GetSettings())
	if err != nil {
		return nil, err
	}
	saved, err := s.store.SaveSettings(ctx, in)
	if err != nil {
		var list store.ChannelListError
		var missing store.NotFoundError
		var outOfRange store.BackupRangeError
		var hbOutOfRange store.HeartbeatRangeError
		var report store.TrafficReportError
		switch {
		case errors.As(err, &list) && errors.As(list.Err, &missing) && missing.Kind == store.ObjectNotifyChannel:
			return nil, invalid("%s: channel %d does not exist", notifyListFields[list.List], missing.ID)
		case errors.As(err, &outOfRange):
			return nil, invalid("%s", outOfRange)
		case errors.As(err, &hbOutOfRange):
			return nil, invalid("%s", hbOutOfRange)
		case errors.As(err, &report):
			return nil, invalid("%s", report)
		}
		s.log.Error("saving settings failed", "err", err)
		return nil, internalError("saving settings failed")
	}
	return connect.NewResponse(&heronv1.UpdateSettingsResponse{Settings: s.settingsProto(saved)}), nil
}

func (s *Service) GetStorageStats(ctx context.Context, _ *connect.Request[heronv1.GetStorageStatsRequest]) (*connect.Response[heronv1.GetStorageStatsResponse], error) {
	stats, err := s.store.StorageStats(ctx)
	if err != nil {
		s.log.Error("reading storage stats failed", "err", err)
		return nil, internalError("reading storage stats failed")
	}
	out := &heronv1.GetStorageStatsResponse{DbBytes: uint64(stats.DBBytes), LastPruneAt: stats.LastPrune, LastRollupAt: stats.LastRollup, SqlObservedAt: &stats.SQLObservedAt,
		CleanupPending: proto.Uint64(uint64(stats.CleanupPending))}
	out.Wal = walObservationProto(stats.WAL)
	out.WalSizeLimitBytes = uint64(stats.WALSizeLimit)
	out.SchemaBackupsError = stats.SchemaBackupsError
	for _, b := range stats.SchemaBackups {
		out.SchemaBackups = append(out.SchemaBackups, &heronv1.SchemaBackup{Path: b.Path, SchemaVersion: uint32(b.SchemaVersion), Bytes: uint64(b.Bytes), ModifiedAt: b.ModifiedAt})
	}
	for _, t := range stats.Tables {
		out.Tables = append(out.Tables, &heronv1.TableRows{Name: t.Name, Rows: uint64(t.Rows)})
	}
	// 标红只在 store.SeriesHealth.Staleness 一处判定，这里原样带上它的结论与判定用的原始数值。
	now := s.clk.Now()
	for _, h := range stats.Series {
		stale := h.Staleness(now, s.cfg.Retention)
		out.Series = append(out.Series, &heronv1.SeriesTableHealth{
			Table: h.Table, BucketS: uint32(h.Level.Bucket), RetentionS: uint64(s.cfg.Retention.ForLevel(h.Level.Name) / time.Second),
			OldestTs: h.Oldest, WatermarkTs: h.Watermark, OldestStale: stale.Oldest, WatermarkStale: stale.Watermark,
		})
	}
	return connect.NewResponse(out), nil
}

func walObservationProto(w store.WALObservation) *heronv1.WalFileObservation {
	out := &heronv1.WalFileObservation{ObservedAt: w.ObservedAt}
	switch {
	case w.Bytes != nil:
		out.Result = &heronv1.WalFileObservation_Bytes{Bytes: uint64(*w.Bytes)}
	case w.Absent:
		out.Result = &heronv1.WalFileObservation_Absent{Absent: true}
	default:
		out.Result = &heronv1.WalFileObservation_Error{Error: w.Error}
	}
	return out
}
