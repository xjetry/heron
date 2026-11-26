package store

import (
	"fmt"
	"regexp"
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

// BillingTextError 是价格或币种不合格式。Field 是协议字段路径，文案原样作为 InvalidArgument 返回。
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
