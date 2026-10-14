package api

import (
	"fmt"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
)

// projectionFixtures 造几组消息：Src 是 int32 a = 1，Pair 是 a = 1、b = 2，Painted 是 a = 1 与枚举 c = 2，Outer 的 a = 1
// 是 Pair 消息；其余各在一处与它们不对齐（OtherEnumField 是与 EnumField 不对齐），或（PairPublic、PaintedPublic、MsgField、
// EnumField、OuterPublic 之外）按 newProjection 的某一条约束写错。
func projectionFixtures(t *testing.T) protoreflect.FileDescriptor {
	t.Helper()
	i32 := descriptorpb.FieldDescriptorProto_TYPE_INT32.Enum()
	i64 := descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum()
	single := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	many := descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
	field := func(name string, number int32, typ *descriptorpb.FieldDescriptorProto_Type, label *descriptorpb.FieldDescriptorProto_Label) *descriptorpb.FieldDescriptorProto {
		return &descriptorpb.FieldDescriptorProto{Name: proto.String(name), Number: proto.Int32(number), Type: typ, Label: label, JsonName: proto.String(name)}
	}
	typed := func(name string, typ *descriptorpb.FieldDescriptorProto_Type, typeName string) *descriptorpb.FieldDescriptorProto {
		f := field(name, 1, typ, single)
		f.TypeName = proto.String(typeName)
		return f
	}
	msg := func(name string, fs ...*descriptorpb.FieldDescriptorProto) *descriptorpb.DescriptorProto {
		return &descriptorpb.DescriptorProto{Name: proto.String(name), Field: fs}
	}
	// reserve 给 m 加 reserved：numbers 是 {start, end} 闭区间，names 是字段名。
	reserve := func(m *descriptorpb.DescriptorProto, numbers [][2]int32, names ...string) *descriptorpb.DescriptorProto {
		for _, r := range numbers {
			m.ReservedRange = append(m.ReservedRange, &descriptorpb.DescriptorProto_ReservedRange{Start: proto.Int32(r[0]), End: proto.Int32(r[1] + 1)})
		}
		m.ReservedName = names
		return m
	}
	a := func() *descriptorpb.FieldDescriptorProto { return field("a", 1, i32, single) }
	colorC := field("c", 2, descriptorpb.FieldDescriptorProto_TYPE_ENUM.Enum(), single)
	colorC.TypeName = proto.String(".projfix.Color")
	repeatedMsg := field("a", 1, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(), many)
	repeatedMsg.TypeName = proto.String(".projfix.Pair")
	// proto3 optional 由一个合成 oneof 承载 presence。
	optional := msg("WithPresence", a())
	optional.Field[0].Proto3Optional, optional.Field[0].OneofIndex = proto.Bool(true), proto.Int32(0)
	optional.OneofDecl = []*descriptorpb.OneofDescriptorProto{{Name: proto.String("_a")}}
	fd, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name: proto.String("projection_fixtures.proto"), Package: proto.String("projfix"), Syntax: proto.String("proto3"),
		EnumType: []*descriptorpb.EnumDescriptorProto{
			{Name: proto.String("Color"), Value: []*descriptorpb.EnumValueDescriptorProto{{Name: proto.String("COLOR_UNSPECIFIED"), Number: proto.Int32(0)}}},
			{Name: proto.String("Shade"), Value: []*descriptorpb.EnumValueDescriptorProto{{Name: proto.String("SHADE_UNSPECIFIED"), Number: proto.Int32(0)}}},
		},
		MessageType: []*descriptorpb.DescriptorProto{
			msg("Src", a()),
			msg("Renamed", field("z", 1, i32, single)),
			msg("Renumbered", field("a", 2, i32, single)),
			msg("Wider", field("a", 1, i64, single)),
			msg("Repeated", field("a", 1, i32, many)),
			optional,
			msg("MsgField", typed("a", descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(), ".projfix.Src")),
			msg("EnumField", typed("a", descriptorpb.FieldDescriptorProto_TYPE_ENUM.Enum(), ".projfix.Color")),
			msg("OtherEnumField", typed("a", descriptorpb.FieldDescriptorProto_TYPE_ENUM.Enum(), ".projfix.Shade")),
			msg("Painted", a(), colorC),
			reserve(msg("PaintedPublic", a()), [][2]int32{{2, 2}}, "c"),
			msg("PaintedUnreserved", a()),
			msg("Pair", a(), field("b", 2, i32, single)),
			reserve(msg("PairPublic", a()), [][2]int32{{2, 2}}, "b"),
			msg("PairUnreserved", a()),
			reserve(msg("PairNumberOnly", a()), [][2]int32{{2, 2}}),
			reserve(msg("PairNameOnly", a()), nil, "b"),
			reserve(msg("PairStaleName", a()), [][2]int32{{2, 2}}, "b", "c"),
			reserve(msg("PairStaleNumber", a()), [][2]int32{{2, 2}, {5, 5}}, "b"),
			reserve(msg("PairWideRange", a()), [][2]int32{{2, 536870911}}, "b"),
			msg("Outer", typed("a", descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(), ".projfix.Pair")),
			msg("OuterPublic", typed("a", descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(), ".projfix.PairPublic")),
			msg("OuterUnreserved", typed("a", descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(), ".projfix.PairUnreserved")),
			msg("RepeatedMsg", repeatedMsg),
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return fd
}

func TestNewProjectionRejectsMisalignedFields(t *testing.T) {
	t.Parallel()
	fd := projectionFixtures(t)
	desc := func(name string) protoreflect.MessageDescriptor { return fd.Messages().ByName(protoreflect.Name(name)) }
	const hideB = `projfix.Pair.b is not public, so projfix.%s must reserve both its number 2 and its name "b"`
	for _, c := range []struct{ dst, src, want string }{
		{"Renamed", "Src", "has no counterpart named z"},
		{"Renumbered", "Src", "projfix.Renumbered.a is field 2 but projfix.Src.a is field 1"},
		{"Wider", "Src", "does not match"},
		{"WithPresence", "Src", "does not match"},
		{"Repeated", "Src", "only singular fields"},
		{"Src", "Repeated", "only singular fields"},
		{"RepeatedMsg", "RepeatedMsg", "only singular fields"},
		{"Src", "Outer", "does not match"},
		// 子消息不整条复制：公开子消息没 reserve 源子消息的私有字段，同样起不来。
		{"OuterUnreserved", "Outer", fmt.Sprintf(hideB, "PairUnreserved")},
		{"OtherEnumField", "EnumField", "projfix.OtherEnumField.a uses enum projfix.Shade but projfix.EnumField.a uses enum projfix.Color"},
		{"PaintedUnreserved", "Painted", `projfix.Painted.c is not public, so projfix.PaintedUnreserved must reserve both its number 2 and its name "c"`},
		{"PairUnreserved", "Pair", fmt.Sprintf(hideB, "PairUnreserved")},
		{"PairNumberOnly", "Pair", fmt.Sprintf(hideB, "PairNumberOnly")},
		{"PairNameOnly", "Pair", fmt.Sprintf(hideB, "PairNameOnly")},
		{"PairStaleName", "Pair", `projfix.PairStaleName reserves name "c", which is not a non-public field of projfix.Pair`},
		{"PairStaleNumber", "Pair", "projfix.PairStaleNumber reserves number 5, which is not a non-public field of projfix.Pair"},
		{"PairWideRange", "Pair", "projfix.PairWideRange reserves numbers 2 to 536870911, more than the 1 non-public fields of projfix.Pair"},
	} {
		t.Run(c.dst+"<-"+c.src, func(t *testing.T) {
			expectPanic(t, c.want, func() { newProjection(dynamicpb.NewMessageType(desc(c.dst)), desc(c.src)) })
		})
	}
	newProjection(dynamicpb.NewMessageType(desc("Src")), desc("Src"))
	newProjection(dynamicpb.NewMessageType(desc("PairPublic")), desc("Pair"))
	newProjection(dynamicpb.NewMessageType(desc("EnumField")), desc("EnumField"))
	newProjection(dynamicpb.NewMessageType(desc("PaintedPublic")), desc("Painted"))
	newProjection(dynamicpb.NewMessageType(desc("MsgField")), desc("MsgField"))
	newProjection(dynamicpb.NewMessageType(desc("OuterPublic")), desc("Outer"))
}

// PublicFacts.network 由 Facts.network 逐层投影：每个地址族只剩状态，地址与探测时间不出现；缺失的 network 与
// 缺失的地址族（首轮探测之前）各自保留缺失。
func TestPublicFactsProjectOnlyTheAddressFamilyStates(t *testing.T) {
	t.Parallel()
	p := newProjection((&heronv1.PublicFacts{}).ProtoReflect().Type(), (&heronv1.Facts{}).ProtoReflect().Descriptor())
	got := p.apply(&heronv1.Facts{Hostname: "secret-host", Os: "Debian 12", Network: &heronv1.NetworkInfo{
		Ipv4: &heronv1.AddressDetection{State: heronv1.AddressDetectionState_ADDRESS_DETECTION_STATE_AVAILABLE, Address: "8.8.4.4", CheckedAt: 123},
		Ipv6: &heronv1.AddressDetection{State: heronv1.AddressDetectionState_ADDRESS_DETECTION_STATE_FAILED, CheckedAt: 123},
	}})
	want := &heronv1.PublicFacts{Os: "Debian 12", Network: &heronv1.PublicNetworkInfo{
		Ipv4: &heronv1.PublicAddressDetection{State: heronv1.AddressDetectionState_ADDRESS_DETECTION_STATE_AVAILABLE},
		Ipv6: &heronv1.PublicAddressDetection{State: heronv1.AddressDetectionState_ADDRESS_DETECTION_STATE_FAILED},
	}}
	if !proto.Equal(got, want) {
		t.Fatalf("projected = %v, want %v", got, want)
	}
	if raw, err := protojson.Marshal(got); err != nil || strings.Contains(string(raw), "8.8.4.4") || strings.Contains(string(raw), "checkedAt") {
		t.Fatalf("public facts JSON = %s, %v", raw, err)
	}
	firstRound := p.apply(&heronv1.Facts{Network: &heronv1.NetworkInfo{}}).(*heronv1.PublicFacts)
	if firstRound.Network == nil || firstRound.Network.Ipv4 != nil || firstRound.Network.Ipv6 != nil {
		t.Fatalf("empty network before the first detection = %v", firstRound)
	}
	if old := p.apply(&heronv1.Facts{Os: "Debian 12"}).(*heronv1.PublicFacts); old.Network != nil {
		t.Fatalf("an agent that does not report network got %v", old.Network)
	}
}

// 只复制源里存在的字段：optional 缺失仍是缺失，显式的 0 仍是 0；目标没有的字段（boot_id）不出现。
func TestProjectionKeepsPresenceAndDropsUndeclaredFields(t *testing.T) {
	t.Parallel()
	p := newProjection((&heronv1.PublicMetrics{}).ProtoReflect().Type(), (&heronv1.Metrics{}).ProtoReflect().Descriptor())
	got := p.apply(&heronv1.Metrics{BootId: testBootID, MemUsed: proto.Uint64(0), Load1: proto.Float64(0.5),
		DiskReadBps: proto.Uint64(0), CpuStealPct: proto.Float64(2.5), CpuIowaitPct: proto.Float64(0)}).(*heronv1.PublicMetrics)
	want := &heronv1.PublicMetrics{MemUsed: proto.Uint64(0), Load1: proto.Float64(0.5),
		DiskReadBps: proto.Uint64(0), CpuStealPct: proto.Float64(2.5), CpuIowaitPct: proto.Float64(0)}
	if !proto.Equal(got, want) || got.CpuPct != nil || got.DiskWriteBps != nil {
		t.Fatalf("projected = %v, want %v", got, want)
	}
}

// PublicBilling 由 Billing 投影：周期按编号原样复制，自动续期不出现，days_left 的缺失与 0 各自保留。期望值从 JSON 读入、
// days_left 经反射取：两个消息对不齐时本测试照常编译，红在构造投影的 panic 上。
func TestPublicBillingProjectsFromBilling(t *testing.T) {
	t.Parallel()
	p := newProjection((&heronv1.PublicBilling{}).ProtoReflect().Type(), (&heronv1.Billing{}).ProtoReflect().Descriptor())
	got := p.apply(&heronv1.Billing{Price: "12.50", Currency: "USD", BillingCycle: heronv1.BillingCycle_BILLING_CYCLE_YEARLY,
		ExpiresOn: "2026-10-01", AutoRenew: true, DaysLeft: proto.Int32(0)})
	want := &heronv1.PublicBilling{}
	if err := protojson.Unmarshal([]byte(`{"price": "12.50", "currency": "USD", "billingCycle": "BILLING_CYCLE_YEARLY", "expiresOn": "2026-10-01", "daysLeft": 0}`), want); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(got, want) {
		t.Fatalf("projected = %v, want %v", got, want)
	}
	noDate := p.apply(&heronv1.Billing{Price: "5", Currency: "EUR"}).ProtoReflect()
	if noDate.Has(noDate.Descriptor().Fields().ByName("days_left")) {
		t.Fatalf("no expiry date projected days_left: %v", noDate.Interface())
	}
}

// buf.yaml 对 public.proto 豁免 RESERVED_MESSAGE_NO_DELETE 的前提：这个文件里的 reserved 只标记"源字段尚未公开"。
// 带 reserved 的消息都必须是公开投影（含子投影）的目标——newProjection 已核对它的 reserved 与源的未公开字段一一对应，
// 删掉 reserved 后重新启用的号只能是同名同型的源字段。要退役字段号的非投影消息不能放进这个文件。
func TestPublicReservedOnlyMarksUnpublishedSourceFields(t *testing.T) {
	t.Parallel()
	targets := map[protoreflect.FullName]bool{}
	var walkProjection func(p projection)
	walkProjection = func(p projection) {
		targets[p.dst.Descriptor().FullName()] = true
		for _, f := range p.fields {
			if f.sub != nil {
				walkProjection(*f.sub)
			}
		}
	}
	for _, p := range publicProjections().all() {
		walkProjection(p)
	}
	checked := 0
	var walkMessages func(ms protoreflect.MessageDescriptors)
	walkMessages = func(ms protoreflect.MessageDescriptors) {
		for i := 0; i < ms.Len(); i++ {
			m := ms.Get(i)
			walkMessages(m.Messages())
			if m.ReservedNames().Len() == 0 && m.ReservedRanges().Len() == 0 {
				continue
			}
			checked++
			if !targets[m.FullName()] {
				t.Errorf("%s has reserved fields but is not the target of a public projection; buf.yaml exempts public.proto from RESERVED_MESSAGE_NO_DELETE only for projection targets", m.FullName())
			}
		}
	}
	walkMessages(heronv1.File_heron_v1_public_proto.Messages())
	if checked == 0 {
		t.Fatal("found no message with reserved fields in public.proto; the walk did not reach them")
	}
}
