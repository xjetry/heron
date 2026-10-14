// Package collect 把主机的原始读数变成一次上报。
//
// 平台差异只在 Host 的实现里：Linux 读 /proc 与 /sys（procfs.go），darwin 读 sysctl
// 与 Mach 接口（darwinraw.go 解析，platform_darwin.go 取数）。解析与组合都不带 build tag，
// 任何平台上都能用真机快照或按头文件布局构造的字节测试；带 build tag 的只有系统调用层。
package collect

import "errors"

// Host 是一个平台取原始读数的全部入口。每个方法独立失败，失败只让对应读数缺失。
//
// 合计型读数（conns 合计多张表，ifaces 列出多块网卡）对成员的约定：成员不存在即不计；
// 成员存在却读不出时整个读数返回 error。列出之后、读完之前被删除的网卡也是不存在的成员，
// 怎么认出"已删除"随来源而异，见各实现的 ifaces。缺一块的合计会被当成真值——连接数静默跌落，
// 流量合计变小让 hub 只换基线、恢复时把那一块的历史计数当增量入账（规则见 traffic.Book）；
// 缺失的读数则让面板显示未知、hub 保持基线。两个平台的实现都按这条约定。
//
// 方法名不导出，实现只能在本包内：Linux 的 ProcFS（procfs.go）与 darwin 的
// darwinHost（darwinraw.go）。两次采样的差分、网卡过滤与"读不到即缺失"由此只在 Collector 里实现一次，
// 平台只负责把各自的来源翻译成同一组原始读数。used ≤ total 由 Collector 的 checkUsage 拒收，
// 但它只看得见最终值：中间差的回绕见 memory、swap 的约定。
type Host interface {
	bootID() (string, error)
	// cpuTimes 返回累计 tick，Collector 只用两次读数之差，起点无意义。两次之间 total 不增或
	// idle 变小（如 Linux 的 iowait 会回退）时，由 cpuPercent 放弃这一次读数，不由实现保证单调。
	cpuTimes() (cpuTimes, error)
	// identify 做一次执行环境识别：挂载根是真根还是本环境的 cgroup、每个
	// 被消费的 /proc 文件来自哪里、各资源的范围与读数口径。runner 每个上报周期只调一次，
	// 快照交给 Metrics 与 Facts，识别文件不被它们二次读取。旧版 cgroupCPU 的每周期
	// 限额探测由此并入识别。
	identify() *execSnapshot
	// memory、swap 的 used 若由多项先减后加得出，实现必须自己拒收任何一步减法的回绕：
	// 回绕后再加回的值可能落在 total 以内，checkUsage 看不出。
	memory() (usage, error)
	swap() (usage, error)
	disk() (usage, error)
	load() (loadAvg, error)
	procs() (uint32, error)
	uptime() (uint64, error)
	conns() (tcp, udp uint32, err error)
	// tcpStats 返回 /proc/net/snmp 的 TCP 累计计数，Collector 只用两次读数之差。平台不提供时
	// （darwin）返回 errNoTCPStats，Collector 据此让四项缺失、不记失败；其余错误记 TCP_STATS 失败。
	tcpStats() (tcpCounters, error)
	// ifaces 返回全部网卡，不做过滤：哪些网卡计入流量只由 Collector.netCounters 决定，
	// 实现里漏掉或写错过滤不会把被排除的网卡带进合计。
	ifaces() ([]ifaceCounters, error)
	// diskCounters 返回整盘设备的累计读/写字节，分区与 loop/ram/zram、dm/md 设备不计。
	// 两次采样的差分、设备集合变化与"读不到即缺失"由 Collector 统一处理；平台不提供该读数
	// （darwin）时返回 errNoDiskCounters，Collector 据此只让两项缺失，不记失败。
	diskCounters() ([]diskCounters, error)
	// defaultNetExclude 是未给 --net-exclude 时不计入流量的网卡；网卡命名随平台而异。
	defaultNetExclude() []string
	// hasNetStructure 报告本平台是否提供网卡的结构属性：Linux 为真，darwin 为假。
	hasNetStructure() bool
	// netStructure 读名为 name 的网卡的结构属性，Collector 只对名字规则放行的网卡调用。网卡已不存在（列出计数之后
	// 被删除）时返回 errIfaceGone，Collector 按不存在的成员略过；网卡还在而属性读不出时返回其他错误，整个网络读数缺失。
	netStructure(name string) (netStructure, error)
	facts() hostFacts
	// close 释放实现持有的资源（Linux 的两个根 fd）；Collector.Close 调它，之后不再读取。
	close() error
}

type usage struct{ total, used uint64 }

type ifaceCounters struct {
	name   string
	rx, tx uint64
}

// diskCounters 是一块整盘设备的累计读/写字节；Collector 只用两次读数之差。
type diskCounters struct {
	name        string
	read, write uint64
}

type hostFacts struct {
	hostname, os, kernel, virtualization, cpuModel string
}

// tcpCounters 是内核 TCP 计数的一次读数，四项来自同一次读取。
type tcpCounters struct {
	activeOpens, passiveOpens, outSegs, retransSegs uint64
}

// errNoTCPStats 表示平台不提供 TCP 计数，与读取失败区分：前者不是采集故障。
var errNoTCPStats = errors.New("tcp stats are not provided on this platform")

// netStructure 是一张网卡在 sysfs 里的结构事实，判定只在 structureReason 里做。
type netStructure struct {
	stacked    bool   // 目录下有 lower_* 链接
	bridgePort bool   // 目录下有 brport
	hasDevice  bool   // 目录下有 device 链接
	devType    string // uevent 的 DEVTYPE，没有该行时为空
	arpType    uint16 // type 文件的 ARPHRD 值
}

// errIfaceGone 表示网卡在列出之后已被删除，与"还在却读不出"区分。
var errIfaceGone = errors.New("interface no longer exists")
