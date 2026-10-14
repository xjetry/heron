package main

import (
	"fmt"
	"io"
	"math"
	"strings"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/agentwire"
	"github.com/xjetry/heron/internal/update"
)

// runBounds 给出 ReportRequest 中随主机变化的字段在编码里能占的最大字节（含字段标签与长度前缀），与 analyze 报的
// 实测值对照：实测是"这台主机上是多少"，这里是"换一台主机最多能到多少"。上界取自协议的约束本身：
//   - Metrics：每个数值字段取编码最长的值（varint 取类型最大值，double 恒 8 字节），boot_id 取 UUID 文本的 36 字节、
//     net_counter_epoch 取 SHA-256 小写十六进制的 64 字节（hub 只收这两种写法，agentwire.ValidateCounterEpoch 等）；
//     带列表时省略这三项，单独列出其大小用于核对编码预算；
//   - UpdateStatus：原因与版本交给 update.StatusProto 本身截断，所以上界跟着它的截断长度走，不在这里另抄一份；
//     全文只在首次与被索要时出现，update_hash 每次携带；
//   - tasks_digest 恰 agentwire.TasksDigestLen 字节；
//   - Facts 的诊断里网卡名至多 agentwire.MaxDiagnosticInterfaces 个、每个至多 agentwire.MaxInterfaceNameBytes 字节；
//   - net_interfaces 至多 agentwire.MaxNetInterfaces 项，名称各至多 agentwire.MaxInterfaceNameBytes 字节、两个计数各取
//     10 字节的 uint64 最大值（hub 的 TestMaxNetInterfacesWire 钉住同一个数）。
//
// 探测结果单条的最坏编码与 Metrics+Facts 合计的满值由 hub 的测试钉住（internal/hub/ingest 的
// TestMaxProbeResultWire、TestHostPayloadFitsMetricsBudget），这里不重复。
func runBounds(w io.Writer) error {
	m := &heronv1.Metrics{BootId: strings.Repeat("0", 36), NetCounterEpoch: strings.Repeat("0", 64)}
	maxScalars(m.ProtoReflect())
	fmt.Fprintf(w, "metrics (field 1), every numeric field at its longest encoding: %d bytes\n", fieldSize(1, proto.Size(m)))
	totals := &heronv1.Metrics{NetRxTotal: m.NetRxTotal, NetTxTotal: m.NetTxTotal, NetCounterEpoch: m.NetCounterEpoch}
	fmt.Fprintf(w, "metrics net_rx_total, net_tx_total, net_counter_epoch at their longest: %d bytes, absent when the request carries net_interfaces\n",
		proto.Size(totals))

	healthy := update.StatusProto(update.Status{Supported: true, Source: "github"}, "v0.11.1")
	worst := update.StatusProto(update.Status{Reason: strings.Repeat("x", 1<<16), Source: "github"}, strings.Repeat("x", 1<<10))
	fmt.Fprintf(w, "update (field 6): supported, version v0.11.1, source github: %d bytes; longest reason and version update.StatusProto keeps: %d bytes\n",
		fieldSize(6, proto.Size(healthy)), fieldSize(6, proto.Size(worst)))

	fmt.Fprintf(w, "tasks_digest (field 8): %d bytes; facts_hash (field 4): %d bytes\n",
		fieldSize(8, agentwire.TasksDigestLen), protowire.SizeTag(4)+protowire.SizeFixed64())
	fmt.Fprintf(w, "update_hash (field 9, optional fixed64): %d bytes whenever given, 0 included\n", protowire.SizeTag(9)+protowire.SizeFixed64())

	fmt.Fprintf(w, "reply want_update (field 7): %d bytes when true\n", protowire.SizeTag(7)+1)

	d := &heronv1.AgentDiagnostics{NetInterfacesTotal: agentwire.MaxDiagnosticInterfaces}
	for range agentwire.MaxDiagnosticInterfaces {
		d.NetInterfaces = append(d.NetInterfaces, strings.Repeat("x", agentwire.MaxInterfaceNameBytes))
	}
	one := &heronv1.AgentDiagnostics{NetInterfaces: []string{"eth0"}, NetInterfacesTotal: 1}
	fmt.Fprintf(w, "facts.diagnostics (field 11), interface names only: one name eth0 %d bytes; %d names of %d bytes %d bytes (each name costs its length + 2)\n",
		fieldSize(11, proto.Size(one)), agentwire.MaxDiagnosticInterfaces, agentwire.MaxInterfaceNameBytes, fieldSize(11, proto.Size(d)))
	fmt.Fprintf(w, "net_interfaces (field 10): %d entries, %d-byte names, maximal counters: %d bytes; one entry eth0 with counters below 2^35 (5-byte varints): %d bytes\n",
		agentwire.MaxNetInterfaces, agentwire.MaxInterfaceNameBytes, proto.Size(&heronv1.ReportRequest{NetInterfaces: maxNetInterfaces()}),
		proto.Size(&heronv1.ReportRequest{NetInterfaces: []*heronv1.NetInterfaceCounter{{Name: "eth0", RxBytes: 1<<35 - 1, TxBytes: 1<<35 - 1}}}))
	return nil
}

// maxNetInterfaces 是编码最长的合法逐网卡列表：名称取满且互不相同，计数取 uint64 最大值。
func maxNetInterfaces() []*heronv1.NetInterfaceCounter {
	out := make([]*heronv1.NetInterfaceCounter, agentwire.MaxNetInterfaces)
	for i := range out {
		out[i] = &heronv1.NetInterfaceCounter{Name: fmt.Sprintf("%03d%s", i, strings.Repeat("x", agentwire.MaxInterfaceNameBytes-3)), RxBytes: math.MaxUint64, TxBytes: math.MaxUint64}
	}
	return out
}

// maxScalars 把消息里每个数值标量字段置为编码最长的值。
func maxScalars(m protoreflect.Message) {
	fs := m.Descriptor().Fields()
	for i := range fs.Len() {
		f := fs.Get(i)
		switch f.Kind() {
		case protoreflect.Uint64Kind:
			m.Set(f, protoreflect.ValueOfUint64(math.MaxUint64))
		case protoreflect.Uint32Kind:
			m.Set(f, protoreflect.ValueOfUint32(math.MaxUint32))
		case protoreflect.DoubleKind:
			m.Set(f, protoreflect.ValueOfFloat64(math.MaxFloat64))
		}
	}
}
