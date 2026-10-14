package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
)

// HeartbeatMethod 是心跳外呼的 HTTP 方法，取值就是线路上发的方法名。
type HeartbeatMethod string

const (
	HeartbeatGet  HeartbeatMethod = "GET"
	HeartbeatPost HeartbeatMethod = "POST"
	HeartbeatHead HeartbeatMethod = "HEAD"
)

// HeartbeatSettings 是读侧的心跳设置。URL 是只写设置：心跳循环读它发请求，协议读侧只回显由它推出的 has_url 与
// url_host，不回显原文（ping 地址本身就是密钥，与通知渠道的 WebhookConfig.url 同一做法）。Set 表示库里至少有一个
// heartbeat.* 键：从未配置过为 false，协议据此决定响应里带不带这一组。取值恒合法：readSettings 对库里的每个键按下面
// 同一张范围表核对，不合即报错。
type HeartbeatSettings struct {
	URL       string
	IntervalS uint32
	Method    HeartbeatMethod
	Set       bool
}

// HeartbeatUpdate 是一次 UpdateSettings 里给出的 heartbeat。整组替换：给出即 URL、interval_s、method 三项一起写，
// url 为空串表示清空并停用。nil 表示请求里没有这一组，不写任何 heartbeat.* 键。
type HeartbeatUpdate struct {
	URL       string
	IntervalS uint32
	Method    HeartbeatMethod
}

// 区间与默认值。写侧（saveHeartbeat）与读侧（readSettings）都按这份表裁决，范围只此一份。
// 库里没有 interval_s 键时为 60，没有 method 键时为 POST。
const (
	HeartbeatMinIntervalS     = 60
	HeartbeatMaxIntervalS     = 3600
	DefaultHeartbeatIntervalS = 60
)

// 三个键名是库里的持久标识，改名要迁移。url 与 method 之外的 heartbeat.* 键不参与设置（新增键要同时在 readSettings 的
// GLOB 与这里登记）。
const (
	heartbeatURLKey      = "heartbeat.url"
	heartbeatIntervalKey = "heartbeat.interval_s"
	heartbeatMethodKey   = "heartbeat.method"
)

// HeartbeatRangeError 是写侧给出的间隔出范围，api 把它映射为 InvalidArgument。0 同样出范围：0 会空转，不是"取默认"。
type HeartbeatRangeError struct {
	Got uint32
}

func (e HeartbeatRangeError) Error() string {
	return fmt.Sprintf("heartbeat.interval_s must be in [%d, %d]; got %d", HeartbeatMinIntervalS, HeartbeatMaxIntervalS, e.Got)
}

// heartbeatDefaults 是库里没有任何 heartbeat.* 键时的读侧值。
func heartbeatDefaults() HeartbeatSettings {
	return HeartbeatSettings{IntervalS: DefaultHeartbeatIntervalS, Method: HeartbeatPost}
}

// parseStoredHeartbeatInterval 解析库里的 interval_s，范围按写侧同一张表核对。%v 而不是 %w：库里的坏值不能被 api
// 当作请求的 InvalidArgument 报回去。
func parseStoredHeartbeatInterval(v string) (uint32, error) {
	n, err := strconv.ParseUint(v, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid stored %s", heartbeatIntervalKey)
	}
	if uint32(n) < HeartbeatMinIntervalS || uint32(n) > HeartbeatMaxIntervalS {
		return 0, fmt.Errorf("invalid stored %s: must be in [%d, %d]", heartbeatIntervalKey, HeartbeatMinIntervalS, HeartbeatMaxIntervalS)
	}
	return uint32(n), nil
}

func parseStoredHeartbeatMethod(v string) (HeartbeatMethod, error) {
	switch m := HeartbeatMethod(v); m {
	case HeartbeatGet, HeartbeatPost, HeartbeatHead:
		return m, nil
	default:
		return "", fmt.Errorf("invalid stored %s", heartbeatMethodKey)
	}
}

// saveHeartbeat 在校验之后整体写入三项。method 的合法性由协议层（枚举）保证，这里只守读侧能解释的取值集合。
func saveHeartbeat(tx *sql.Tx, u *HeartbeatUpdate) error {
	if u.IntervalS < HeartbeatMinIntervalS || u.IntervalS > HeartbeatMaxIntervalS {
		return HeartbeatRangeError{Got: u.IntervalS}
	}
	switch u.Method {
	case HeartbeatGet, HeartbeatPost, HeartbeatHead:
	default:
		return fmt.Errorf("heartbeat.method must be GET, POST or HEAD; got %q", u.Method)
	}
	values := []struct{ key, value string }{
		{heartbeatURLKey, u.URL},
		{heartbeatIntervalKey, strconv.FormatUint(uint64(u.IntervalS), 10)},
		{heartbeatMethodKey, string(u.Method)},
	}
	for _, f := range values {
		if err := putSetting(tx, f.key, f.value); err != nil {
			return err
		}
	}
	return nil
}

// HeartbeatSettings 读出心跳设置；组内三个键来自同一个快照。
func (s *Store) HeartbeatSettings(ctx context.Context) (HeartbeatSettings, error) {
	st, err := readSettings(ctx, s.r)
	return st.Heartbeat, err
}
