package store

import (
	"database/sql"
	"fmt"
	"regexp"
	"time"
)

// 价格与币种的格式是这两列唯一的定义：价格是至多 9 位整数、至多 2 位小数的十进制文本，币种是三个大写字母（ISO 4217
// 的形状，不核对是否真有这个币种），价格非空时必须有币种。RE2 的 \d 只匹配 ASCII 数字，全角数字不算。
// CheckPriceCurrency 由协议层（api 的 billingOf，CreateNode 与 UpdateNode 共用）在入口调用给出点名字段的错误，
// 由两个写入口（insertNode、UpdateNodeTasks）在事务里复查，由快照恢复预检（validateSnapshotNodes）复核；早于这条
// 校验写进库的值由迁移 54 规范化。所以库里每一行的价格与币种都满足它，面板的费用汇总（web/src/lib/billing.ts）
// 据此直接按原币种计算，不再自己过滤。
var (
	pricePattern    = regexp.MustCompile(`^\d{1,9}(\.\d{1,2})?$`)
	currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)
)

// BillingTextError 是价格、币种或到期日不合格式。Field 是协议字段路径，文案原样作为 InvalidArgument 返回。
type BillingTextError struct {
	Field  string
	detail string
}

func (e BillingTextError) Error() string { return e.Field + ": " + e.detail }

// CheckPriceCurrency 裁决一对价格与币种（规则见上）。空价格与空币种都是"没填"，合法。
func CheckPriceCurrency(price, currency string) error {
	if price != "" && !pricePattern.MatchString(price) {
		return BillingTextError{Field: "billing.price", detail: fmt.Sprintf("must match %s, e.g. 12.50; got %q", pricePattern, price)}
	}
	if currency != "" && !currencyPattern.MatchString(currency) {
		return BillingTextError{Field: "billing.currency", detail: fmt.Sprintf("must be three uppercase letters (ISO 4217), e.g. USD; got %q", currency)}
	}
	if price != "" && currency == "" {
		return BillingTextError{Field: "billing.currency", detail: "required when billing.price is set"}
	}
	return nil
}

// node.billing_anchor_day 的合法日号范围（1–31）。0 在 Go 结构里对应库里的 NULL，只在 expires_on 为空时合法，
// 表示节点没有到期日、因而没有续期锚定日号。
const (
	MinBillingAnchorDay = 1
	MaxBillingAnchorDay = 31
)

// BillingAnchorDayError 是计费锚定日号违反不变式（越界、无到期日却非空，或有合法到期日却为空）。
type BillingAnchorDayError struct {
	ExpiresOn string
	Got       int
	Valid     bool
}

func (e BillingAnchorDayError) Error() string {
	if e.ExpiresOn == "" {
		return fmt.Sprintf("billing_anchor_day must be empty when expires_on is empty; got %d", e.Got)
	}
	if !e.Valid {
		return fmt.Sprintf("billing_anchor_day must be between %d and %d when expires_on (%q) is set; got empty", MinBillingAnchorDay, MaxBillingAnchorDay, e.ExpiresOn)
	}
	return fmt.Sprintf("billing_anchor_day must be between %d and %d; got %d", MinBillingAnchorDay, MaxBillingAnchorDay, e.Got)
}

// ParseBillingDate 解析 YYYY-MM-DD，写入口、迁移 59 与快照恢复预检共用这一个口径（与 alert.ParseDate 相同）。
func ParseBillingDate(s string) (time.Time, error) {
	return time.Parse(time.DateOnly, s)
}

// nullAnchorDay 把 Go 里的 0 编成 SQL NULL，1–31 编成整型值。
func nullAnchorDay(day int) any {
	if day == 0 {
		return nil
	}
	return day
}

// resolveAnchorDay 在写入口（insertNode、UpdateNodeTasks）裁决并计算落库的计费锚定日号：
//   - 调用方若显式传入越界值（< 0 或 > 31）或无到期日却传非零锚定日号，直接拒绝；
//   - 清空到期日时返回 0（写 NULL）；
//   - 写入非空到期日时先校验其为合法 YYYY-MM-DD：到期日变化（或新建）时锚定日号设为新到期日的日号；
//     到期日与库内原值相同时保留原锚定日号（例如 1-31 续到 2-28 后用户只改节点备注或标签，不能把锚定日号
//     洗成 28），仅在库内原值缺失或越界时补为该日期的日号。
func resolveAnchorDay(expiresOn string, given int, oldExpiresOn string, oldAnchor int) (int, error) {
	if given < 0 || given > MaxBillingAnchorDay {
		return 0, BillingAnchorDayError{ExpiresOn: expiresOn, Got: given, Valid: true}
	}
	if expiresOn == "" {
		if given != 0 {
			return 0, BillingAnchorDayError{ExpiresOn: "", Got: given, Valid: true}
		}
		return 0, nil
	}
	d, err := ParseBillingDate(expiresOn)
	if err != nil {
		return 0, BillingTextError{Field: "billing.expires_on", detail: fmt.Sprintf("must be an existing date in YYYY-MM-DD form; got %q", expiresOn)}
	}
	if expiresOn == oldExpiresOn && oldAnchor >= MinBillingAnchorDay && oldAnchor <= MaxBillingAnchorDay {
		return oldAnchor, nil
	}
	return d.Day(), nil
}

// checkBillingAnchorDay 裁决快照恢复预检里一行节点的 (expires_on, billing_anchor_day)：
//   - expires_on 为空时，billing_anchor_day 必须为 NULL；
//   - billing_anchor_day 非 NULL 时，必须在 1–31（即便 expires_on 不可解析，也不接受越界日号）；
//   - expires_on 是合法 YYYY-MM-DD 时，billing_anchor_day 必须非 NULL 且在 1–31。
//
// 不可解析的 expires_on（只可能来自绕过写入口手改的库）配 NULL 放行，由到期扫描与接口跳过。
func checkBillingAnchorDay(expiresOn string, day sql.NullInt64) error {
	if expiresOn == "" {
		if day.Valid {
			return BillingAnchorDayError{ExpiresOn: "", Got: int(day.Int64), Valid: true}
		}
		return nil
	}
	if day.Valid {
		if day.Int64 < MinBillingAnchorDay || day.Int64 > MaxBillingAnchorDay {
			return BillingAnchorDayError{ExpiresOn: expiresOn, Got: int(day.Int64), Valid: true}
		}
		return nil
	}
	if _, err := ParseBillingDate(expiresOn); err == nil {
		return BillingAnchorDayError{ExpiresOn: expiresOn, Valid: false}
	}
	return nil
}
