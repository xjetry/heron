package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// NotifyList 标识一个通知渠道选择列表：备份失败通知、登录通知与流量报告各一个，编码与保存
// 语义相同（见
// saveChannelIDs、parseStoredChannels）。值是它在 setting 表里的键，是库里的持久标识，改名要迁移。
type NotifyList string

const (
	BackupNotifyList NotifyList = "notify.backup_channels"
	LoginNotifyList  NotifyList = "notify.login_channels"
	// TrafficReportNotifyList 是流量报告的渠道。它属于 traffic_report 这一组（整组替换时与四个开关、时刻一起写），
	// 键仍按 notify.* 命名、登记在 NotifyLists：删渠道的摘除与面板的删除确认对每个列表一视同仁。
	TrafficReportNotifyList NotifyList = "notify.traffic_report_channels"
)

// NotifyListSpec 登记一个通知渠道选择列表：List 是它的键，settings 给出 readSettings 把它解码到 Settings 的哪个字段。
type NotifyListSpec struct {
	List     NotifyList
	settings func(*Settings) *[]int64
}

// NotifyLists 是全部通知渠道选择列表。凡"对每个列表都要做"的事都遍历它：readSettings 按它选键、解码，
// DeleteNotifyChannel 按它把被删的渠道从每个列表摘除，api 按它核对每个列表都登记了请求里的字段路径。
// 新增列表只在这里加一行；漏登记的列表读不回来、删渠道时不摘除，会留下指向已删渠道的引用。面板另有一张按键对应的
// 表（web/src/lib/alerts.ts 的 NOTIFY_LISTS，删渠道确认按它逐个列表写影响），notifyLists.test.ts 读这里与它双向核对。
var NotifyLists = []NotifyListSpec{
	{List: BackupNotifyList, settings: func(s *Settings) *[]int64 { return &s.Backup.Channels }},
	{List: LoginNotifyList, settings: func(s *Settings) *[]int64 { return &s.LoginChannelIDs }},
	{List: TrafficReportNotifyList, settings: func(s *Settings) *[]int64 { return &s.TrafficReport.Channels }},
}

// ChannelListError 是保存一个通知渠道选择列表失败：SaveSettings 在一个事务里可能同时保存几个列表，List 指明是哪一个，
// 调用方据此点名字段。Err 是原因，列表里的 ID 指向不存在的渠道时是 NotFoundError（errors.As 可取出）。
type ChannelListError struct {
	List NotifyList
	Err  error
}

func (e ChannelListError) Error() string { return string(e.List) + ": " + e.Err.Error() }
func (e ChannelListError) Unwrap() error { return e.Err }

func parseStoredChannels(list NotifyList, v string) ([]int64, error) {
	var ids []int64
	if err := json.Unmarshal([]byte(v), &ids); err != nil {
		return nil, fmt.Errorf("invalid stored %s", list)
	}
	return ids, nil
}

// saveChannelIDs 保存一个通知渠道选择列表：重复 ID 合并（与告警规则同用 sortedAlertIDs）；每个 ID 在这个写事务里
// 核对存在，与删渠道串行，不会留下指向已删渠道的引用；不存在时返回点名列表的 ChannelListError。空选择写 "[]"，
// 不写 JSON null。
func saveChannelIDs(tx *sql.Tx, list NotifyList, ids []int64) error {
	ids = sortedAlertIDs(ids)
	for _, id := range ids {
		if err := requireAlertReference(tx, "notify_channel", ObjectNotifyChannel, id); err != nil {
			return ChannelListError{List: list, Err: err}
		}
	}
	return putChannelIDs(tx, list, ids)
}

func putChannelIDs(tx *sql.Tx, list NotifyList, ids []int64) error {
	if len(ids) == 0 {
		ids = []int64{}
	}
	data, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	return putSetting(tx, string(list), string(data))
}

// storedChannelIDs 在写事务里读一个选择列表，解码与 readSettings 同经 parseStoredChannels；键不存在即没有选择。
// 读列表再据此写事件或改列表的调用方（发通知、删渠道）都在同一事务里读，与改设置、删渠道由写协程串行。
func storedChannelIDs(tx *sql.Tx, list NotifyList) ([]int64, error) {
	var value string
	err := tx.QueryRow("SELECT value FROM setting WHERE key = ?", list).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return parseStoredChannels(list, value)
}

// removeChannelID 在删渠道的同一事务里把它从一个选择列表中摘除；键不存在或列表里没有它即不写。其余 ID 不重新核对：
// 它们由 saveChannelIDs 写入时核对过，此后每次删渠道都在同一事务里摘除，列表里只会有存在的渠道。
func removeChannelID(tx *sql.Tx, list NotifyList, id int64) error {
	ids, err := storedChannelIDs(tx, list)
	if err != nil || !slices.Contains(ids, id) {
		return err
	}
	return putChannelIDs(tx, list, slices.DeleteFunc(ids, func(v int64) bool { return v == id }))
}
