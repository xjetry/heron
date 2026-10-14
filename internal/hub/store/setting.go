package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// SiteAppearance 是公开页外观，给出时 UpdateSettings 整体替换它：标题、主色、logo、自定义 CSS 为空串表示使用内置值，
// 明暗由 api 限定为 auto/light/dark；约束由 api 的 UpdateSettings 裁决。SiteSettings 嵌入它，SettingsUpdate 整个
// 持有它，保存时整体取用而不逐字段转抄：转抄漏掉的外观项编译照过，每次保存都被写成空串。
type SiteAppearance struct {
	Title       string
	Theme       string
	AccentColor string
	Logo        string
	CustomCSS   string
}

// SiteSettings 是已解析默认值的公开页设置：外观与总闸。国家查询与备份的设置见 GeoSettings、BackupSettings。
type SiteSettings struct {
	SiteAppearance
	PublicEnabled bool
}

// Settings 是 readSettings 读出的全部设置，各组来自同一个快照。LoginChannelIDs 是登录通知选定的渠道，
// 空即不通知。
type Settings struct {
	Site            SiteSettings
	Geo             GeoSettings
	Backup          BackupSettings
	LoginChannelIDs []int64
	Heartbeat       HeartbeatSettings
	TrafficReport   TrafficReportSettings
}

// SettingsUpdate 是 SaveSettings 的输入，按组给出、各组彼此独立：Appearance 非 nil 时整体替换五项外观；PublicEnabled
// 与 Geo 里的各项非 nil 时写入；Backup 非 nil 时各项按 BackupSettingsUpdate 的语义写；LoginChannels 非 nil 时替换
// 登录通知的渠道列表，指向空列表是显式关闭；Heartbeat、TrafficReport 非 nil 时整组替换。nil 表示这一组（项）不改，
// 不写对应的键。"哪一组算给出"由 api 的
// UpdateSettings 按请求判定。更新与读取分用不同类型，避免把缺席误当作关闭，也避免向读者泄漏未解析的值。
type SettingsUpdate struct {
	Appearance    *SiteAppearance
	PublicEnabled *bool
	Geo           GeoUpdate
	Backup        *BackupSettingsUpdate
	LoginChannels *[]int64
	Heartbeat     *HeartbeatUpdate
	TrafficReport *TrafficReportUpdate
}

// DefaultTheme 是从未保存过外观时的明暗：跟随访客系统。
const DefaultTheme = "auto"

// publicEnabledKey 是总闸在 setting 表里的键，值编码见 flagField。键名是库里的持久标识，改名要迁移。
const publicEnabledKey = "site.public_enabled"

type settingField struct {
	key   string
	value *string
}

// fields 列出每项外观的键，readSettings 与 SaveSettings 都按它读写：SiteAppearance 新增的字段不在这里登记，
// 就存不进库、读出来恒为空（TestSiteAppearanceRoundTripsEveryField 逐字段核对）。键名是库里的持久标识，改名要迁移。
// site.* 下除这五个外观键外还有总闸（publicEnabledKey）；国家查询占 geo.* 两个键（见 GeoSettings）；备份占 backup.*
// （见 BackupSettings）；心跳占 heartbeat.*，流量报告占 traffic_report.*；各通知渠道选择列表各占一个 notify.* 键
// （见 NotifyLists）。
func (a *SiteAppearance) fields() []settingField {
	return []settingField{
		{"site.title", &a.Title},
		{"site.theme", &a.Theme},
		{"site.accent_color", &a.AccentColor},
		{"site.logo", &a.Logo},
		{"site.custom_css", &a.CustomCSS},
	}
}

// GeoSettings 是国家查询的开关与服务地址。与外观同存 setting 表、同一个写入口，但不是整体替换：
// SaveSettings 只写调用方给出的项（GeoUpdate 里非 nil 的），见 GeoUpdate。
type GeoSettings struct {
	Enabled bool
	// URL 含 {ip} 占位，查询时替换为节点的来源地址；取值约束由 api 的 UpdateSettings 裁决。
	URL string
}

// DefaultGeoURL 是从未保存过服务地址时的值。国家查询从未保存过开关时为开（readSettings），HTTP 后端在运维没有
// 换过地址、也没有关掉查询时，就把节点的公网来源地址逐个发到这里。
const DefaultGeoURL = "https://ipinfo.io/{ip}/country"

// GeoUpdate 里 nil 表示不改。国家查询的开关决定 hub 是否把节点地址发给第三方，不认识这两项的客户端（改个标题的
// 脚本、旧面板）按外观的整体替换提交时不得顺手改掉它们：缺席若等于"关"与"默认地址"，一个只带 enabled 的请求会把
// 运维选定的服务换回默认服务并开始向它发送地址。
type GeoUpdate struct {
	Enabled *bool
	URL     *string
}

// 国家查询开关与总闸同一编码（见 flagField）。它的值不按任一方向猜的理由：猜成关会静默停掉运维开启的查询，猜成开
// 会在运维不知情时向第三方发地址。
const (
	geoEnabledKey = "geo.enabled"
	geoURLKey     = "geo.url"
)

// flagField 与 parseFlag 是开关（总闸、国家查询开关）在 setting 表里的唯一编码："0" / "1"。写只经 SaveSettings
// 调 flagField，所以库里出现别的值只可能来自 hub 之外改库的途径；parseFlag 对它返回错误，不按任一方向猜。Open 读一次
// 设置（见 openStore），所以这样的库在打开时就被拒绝：总闸的内存副本不会从非法值加载，查询器也不会等到第一轮才在
// 日志里报错。
func flagField(key string, on bool) settingField {
	v := "0"
	if on {
		v = "1"
	}
	return settingField{key, &v}
}

func parseFlag(key, v string) (bool, error) {
	if v != "0" && v != "1" {
		return false, fmt.Errorf("setting %s must be 0 or 1; got %q", key, v)
	}
	return v == "1", nil
}

// putSetting 更新设置值：外观、开关、国家查询、备份配置与渠道列表都经它，写事务由调用方给。
func putSetting(tx *sql.Tx, key, value string) error {
	_, err := tx.Exec("INSERT INTO setting (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value", key, value)
	return err
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// readSettings 用一条 SELECT 读出全部 site.*（外观与总闸）、geo.*、backup.*、heartbeat.*、traffic_report.* 与
// NotifyLists 登记的每个通知渠道列表键：单条语句在 WAL 下读同一个快照，SaveSettings 又在一个写事务里写全部给出的键，两者合起来保证读者拿不到新旧
// 混合的设置。改成逐键或分组查询会失去前一半。库里有必须合法才能解释的编码：开关只认 0 / 1（parseFlag），备份的
// 四个数值须在 backupNumber 的范围内（parseStored），心跳与流量报告的数值按各自的范围表（parseStoredHeartbeatInterval、
// parseStoredReportHour），渠道列表须是 JSON 整数数组（parseStoredChannels）；不合即返回错误，不按默认值猜。
func readSettings(ctx context.Context, q querier) (Settings, error) {
	// 两个开关都是"键缺失即开"：公开页总闸与国家查询从未保存过时为开，保存过的开或关照旧生效。非法的已保存值
	// 不能被解释成开（parseFlag 报错）：库里的坏值既不会让公开页对外开放，也不会让 hub 开始向 geo_url 发送地址。
	out := Settings{
		Site:      SiteSettings{SiteAppearance: SiteAppearance{Theme: DefaultTheme}, PublicEnabled: true},
		Geo:       GeoSettings{URL: DefaultGeoURL, Enabled: true},
		Backup:    backupDefaults(),
		Heartbeat: heartbeatDefaults(),
	}
	strs := map[string]*string{geoURLKey: &out.Geo.URL}
	for _, f := range out.Site.fields() {
		strs[f.key] = f.value
	}
	for _, f := range out.Backup.texts() {
		strs[f.key] = f.value
	}
	flags := map[string]*bool{publicEnabledKey: &out.Site.PublicEnabled, geoEnabledKey: &out.Geo.Enabled}
	for _, f := range out.TrafficReport.flags() {
		flags[f.key] = f.on
	}
	numbers := map[string]numberField{}
	for _, f := range out.Backup.numbers() {
		numbers[f.n.key] = f
	}
	lists := map[NotifyList]*[]int64{}
	listKeys := make([]any, 0, len(NotifyLists))
	for _, l := range NotifyLists {
		lists[l.List] = l.settings(&out)
		listKeys = append(listKeys, l.List)
	}
	query := "SELECT key, value FROM setting WHERE key GLOB 'site.*' OR key GLOB 'geo.*' OR key GLOB 'backup.*' OR key GLOB 'heartbeat.*' OR key GLOB 'traffic_report.*' OR key IN (" +
		strings.TrimSuffix(strings.Repeat("?, ", len(listKeys)), ", ") + ")"
	rows, err := q.QueryContext(ctx, query, listKeys...)
	if err != nil {
		return Settings{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return Settings{}, err
		}
		if p := flags[k]; p != nil {
			if *p, err = parseFlag(k, v); err != nil {
				return Settings{}, err
			}
		} else if p := strs[k]; p != nil {
			*p = v
		} else if f, ok := numbers[k]; ok {
			if err := f.parseStored(v); err != nil {
				return Settings{}, err
			}
		} else if k == heartbeatURLKey {
			out.Heartbeat.URL, out.Heartbeat.Set = v, true
		} else if k == heartbeatIntervalKey {
			n, err := parseStoredHeartbeatInterval(v)
			if err != nil {
				return Settings{}, err
			}
			out.Heartbeat.IntervalS, out.Heartbeat.Set = n, true
		} else if k == heartbeatMethodKey {
			m, err := parseStoredHeartbeatMethod(v)
			if err != nil {
				return Settings{}, err
			}
			out.Heartbeat.Method, out.Heartbeat.Set = m, true
		} else if k == trafficReportHourKey {
			if out.TrafficReport.Hour, err = parseStoredReportHour(v); err != nil {
				return Settings{}, err
			}
		} else if p := lists[NotifyList(k)]; p != nil {
			if *p, err = parseStoredChannels(NotifyList(k), v); err != nil {
				return Settings{}, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return Settings{}, err
	}
	return out, nil
}

// Settings 读出全部设置，各组来自同一个快照。
func (s *Store) Settings(ctx context.Context) (Settings, error) {
	return readSettings(ctx, s.r)
}

func (s *Store) SiteSettings(ctx context.Context) (SiteSettings, error) {
	st, err := readSettings(ctx, s.r)
	return st.Site, err
}

func (s *Store) GeoSettings(ctx context.Context) (GeoSettings, error) {
	st, err := readSettings(ctx, s.r)
	return st.Geo, err
}

func (s *Store) BackupSettings(ctx context.Context) (BackupSettings, error) {
	st, err := readSettings(ctx, s.r)
	return st.Backup, err
}

// PublicEnabled 读启动时加载、SaveSettings 提交成功后发布的内存值。
// 匿名请求与静态资源都要检查总闸，读原子副本避免每次准入都占用数据库连接。
func (s *Store) PublicEnabled() bool { return s.publicEnabled.Load() }

// SaveSettings 在 s.write 的一个事务里写给出的外观（五个键整体）、给出的总闸、Geo 里给出的项、给出的备份项与给出的
// 登录通知渠道列表，任一条失败整体回滚：库里不会留下半套设置，与 readSettings 的单条 SELECT 一起保证读侧看不到新旧
// 混合。缺席的组（项）表示不变，不写对应的键。备份的数值范围与两个渠道列表里的渠道是否存在也在这个事务里裁决
// （saveBackup、saveChannelIDs）：出范围返回 BackupRangeError；渠道不存在返回点名列表的 ChannelListError，其 Err
// 是 NotFoundError（errors.As 可取出）；同样整体回滚。渠道的存在性在这个写事务里核对，删渠道（DeleteNotifyChannel）
// 在它自己的写事务里把渠道从 NotifyLists 的每个列表摘除，两者由写协程串行；改成在事务之外先查再写，两步之间删掉的
// 渠道就会留在列表里。返回值是同一个事务里写入之后读回的全部设置（未给出的项是库里原值），就是这次提交的状态。失败时
// 不发布内存值。
//
// 不变式：publicEnabled 等于库里最近一次提交的总闸键（publicEnabledKey）。前提有二：Open 从库加载它；hub 运行期间
// 只有这里写这个键（现有离线子命令都不写它；此外改库的途径，如整表覆盖的 restore，必须在 hub 停止时
// 进行，由下次 Open 重新加载）。runWriter 串行提交，但各调用方醒来后的发布顺序不受它约束：不持 siteWriteMu
// 时，两次并发保存可以按 A、B 提交却按 B、A 发布，内存与库从此分叉，直到下一次显式保存总闸或重启。所以写者持锁
// 直到提交与发布都完成。读总闸（PublicEnabled）不取这把锁，读到的是最近一次发布的值。国家查询与备份没有内存副本：
// 读者（GeoSettings、BackupSettings）每次读库，回显在写事务里读回，它们的一致性由单个写事务与单条 SELECT 承载，
// 不依赖这把锁。
func (s *Store) SaveSettings(ctx context.Context, in SettingsUpdate) (Settings, error) {
	s.siteWriteMu.Lock()
	defer s.siteWriteMu.Unlock()
	var puts []settingField
	if in.Appearance != nil {
		puts = in.Appearance.fields()
	}
	if in.PublicEnabled != nil {
		puts = append(puts, flagField(publicEnabledKey, *in.PublicEnabled))
	}
	if in.Geo.Enabled != nil {
		puts = append(puts, flagField(geoEnabledKey, *in.Geo.Enabled))
	}
	if in.Geo.URL != nil {
		puts = append(puts, settingField{geoURLKey, in.Geo.URL})
	}
	var out Settings
	err := s.write(ctx, func(tx *sql.Tx) error {
		for _, f := range puts {
			if err := putSetting(tx, f.key, *f.value); err != nil {
				return err
			}
		}
		if in.Backup != nil {
			if err := saveBackup(tx, in.Backup); err != nil {
				return err
			}
		}
		if in.LoginChannels != nil {
			if err := saveChannelIDs(tx, LoginNotifyList, *in.LoginChannels); err != nil {
				return err
			}
		}
		if in.Heartbeat != nil {
			if err := saveHeartbeat(tx, in.Heartbeat); err != nil {
				return err
			}
		}
		if in.TrafficReport != nil {
			if err := saveTrafficReport(tx, in.TrafficReport); err != nil {
				return err
			}
		}
		var err error
		out, err = readSettings(ctx, tx)
		return err
	})
	if err != nil {
		return Settings{}, err
	}
	if in.PublicEnabled != nil {
		s.publicEnabled.Store(out.Site.PublicEnabled)
	}
	return out, nil
}
