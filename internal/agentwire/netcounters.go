package agentwire

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
)

// MaxNetInterfaces 是 ReportRequest.net_interfaces 的条数上限。计入网卡多于它时 agent 不带列表、只带合计与集合摘要，
// hub 退回对合计做差分；上行预算按 MaxNetInterfaces 项、每项 MaxInterfaceNameBytes 字节的名称与两个最大计数的满值编码计算。
const MaxNetInterfaces = 64

// ValidateNetInterfaces 约束逐网卡计数列表的形状：至多 MaxNetInterfaces 项，名称经 ValidateInterfaceName 准入，
// 按字节序严格递增（因而唯一）。NetInterfaceSummary 的结果只对满足这条形状的列表有确定含义：顺序进集合摘要，
// 重名会让同一张网卡在合计里算两次。
func ValidateNetInterfaces(list []*heronv1.NetInterfaceCounter) error {
	if len(list) > MaxNetInterfaces {
		return fmt.Errorf("net_interfaces: must contain at most %d interfaces; got %d", MaxNetInterfaces, len(list))
	}
	for i, c := range list {
		if err := ValidateInterfaceName(c.GetName()); err != nil {
			return fmt.Errorf("net_interfaces: %w", err)
		}
		if i > 0 && list[i-1].GetName() >= c.GetName() {
			return fmt.Errorf("net_interfaces: names must be unique and sorted by bytes")
		}
	}
	return nil
}

// NetSummary 是一组网卡计数推导出的合计视图：Metrics.net_rx_total、net_tx_total 与 net_counter_epoch 的取值。
type NetSummary struct {
	Rx, Tx uint64
	Epoch  string
}

// SummarizeNetInterfaces 是合计与集合摘要的唯一推导：agent 用它填 Metrics，hub 用它从上报的列表与逐网卡基线推出
// 合计视图，两侧算出的值必须逐字节相同，跨形态衔接与一致性校验才有意义。items 须已按名称字节序排列且名称唯一
// （ValidateNetInterfaces 的形状）；本函数不排序，顺序就是摘要的输入。
//   - 合计：逐项 uint64 相加，总和到 2^64 字节回绕，与引入逐网卡列表之前 agent 填 net_rx_total 的算术相同。
//   - 摘要：名称列表的 JSON 编码（encoding/json，[]string）的 SHA-256 小写十六进制，与之前的 net_counter_epoch 逐字节相同。
//
// 空集合不是合法输入：agent 没有计入网卡时报网络采集失败，合计与摘要一起缺失。
func SummarizeNetInterfaces[T any](items []T, counter func(T) (name string, rx, tx uint64)) NetSummary {
	var sum NetSummary
	names := make([]string, 0, len(items))
	for _, it := range items {
		name, rx, tx := counter(it)
		sum.Rx += rx
		sum.Tx += tx
		names = append(names, name)
	}
	encoded, _ := json.Marshal(names)
	digest := sha256.Sum256(encoded)
	sum.Epoch = hex.EncodeToString(digest[:])
	return sum
}

// NetInterfaceSummary 是 SummarizeNetInterfaces 对协议列表的取法。
func NetInterfaceSummary(list []*heronv1.NetInterfaceCounter) NetSummary {
	return SummarizeNetInterfaces(list, func(c *heronv1.NetInterfaceCounter) (string, uint64, uint64) {
		return c.GetName(), c.GetRxBytes(), c.GetTxBytes()
	})
}
