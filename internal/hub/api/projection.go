package api

import (
	"fmt"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// projection 把一种消息按字段名投影成它的公开形态。字段集合由目标（公开）消息决定：源消息以后新增的字段
// 不会出现在公开消息里，除非公开消息也声明它——"默认私有"由此落在类型上，而不是一处可能漏改的逐字段拷贝。
//
// newProjection 是唯一把公开消息与源消息配对的地方，下面这些对公开消息与源消息的约束都在这里核对，任一不符即 panic：
//   - 每个公开字段在源里有同名字段，号、类型、基数与 presence 都相同，枚举字段两侧引用同一个枚举类型；
//   - 消息字段两侧都是单值消息，公开侧的子消息本身也是源侧子消息的投影，按这几条递归核对；
//   - 源里没公开的字段，号与名都在公开消息的 reserved 里；每个 reserved 的号与名都对应一个没公开的源字段。
//
// 后一条让"公开一个源字段"必须是显式动作（删掉 reserved 再声明），源消息新增字段时公开消息不跟着 reserve
// 就起不来，reserved 的号也始终能读成"对应的源字段不公开"。描述符来自生成代码，只有改了 proto 却没对齐时
// 才会不符，那时 hub 在构造 Public 时就起不来。
type projection struct {
	dst    protoreflect.MessageType
	fields []projectedField
}

type projectedField struct {
	dst, src protoreflect.FieldDescriptor
	// sub 只在消息字段上非空：子消息同样按字段名投影，源子消息里没公开的字段（如地址）不会被整条消息带出去。
	sub *projection
}

func newProjection(dst protoreflect.MessageType, src protoreflect.MessageDescriptor) projection {
	p := projection{dst: dst}
	dd := dst.Descriptor()
	fields := dd.Fields()
	for i := 0; i < fields.Len(); i++ {
		d := fields.Get(i)
		s := src.Fields().ByName(d.Name())
		switch {
		case s == nil:
			panic(fmt.Sprintf("%s has no counterpart named %s in %s", d.FullName(), d.Name(), src.FullName()))
		case d.Number() != s.Number():
			panic(fmt.Sprintf("%s is field %d but %s is field %d; public fields keep the source numbers", d.FullName(), d.Number(), s.FullName(), s.Number()))
		case !projectable(d) || !projectable(s):
			panic(fmt.Sprintf("%s: only singular fields can be projected", d.FullName()))
		case d.Kind() != s.Kind() || d.HasPresence() != s.HasPresence():
			panic(fmt.Sprintf("%s (%v, presence %v) does not match %s (%v, presence %v)",
				d.FullName(), d.Kind(), d.HasPresence(), s.FullName(), s.Kind(), s.HasPresence()))
		case d.Kind() == protoreflect.EnumKind && d.Enum().FullName() != s.Enum().FullName():
			panic(fmt.Sprintf("%s uses enum %s but %s uses enum %s; enums are copied by number, so both sides must use the same enum",
				d.FullName(), d.Enum().FullName(), s.FullName(), s.Enum().FullName()))
		}
		f := projectedField{dst: d, src: s}
		if d.Kind() == protoreflect.MessageKind {
			sub := newProjection(dst.New().NewField(d).Message().Type(), s.Message())
			f.sub = &sub
		}
		p.fields = append(p.fields, f)
	}
	hiddenNumbers := map[protoreflect.FieldNumber]bool{}
	hiddenNames := map[protoreflect.Name]bool{}
	for i := 0; i < src.Fields().Len(); i++ {
		s := src.Fields().Get(i)
		if fields.ByName(s.Name()) != nil {
			continue
		}
		hiddenNumbers[s.Number()], hiddenNames[s.Name()] = true, true
		if !dd.ReservedRanges().Has(s.Number()) || !dd.ReservedNames().Has(s.Name()) {
			panic(fmt.Sprintf("%s is not public, so %s must reserve both its number %d and its name %q", s.FullName(), dd.FullName(), s.Number(), s.Name()))
		}
	}
	for i := 0; i < dd.ReservedNames().Len(); i++ {
		if name := dd.ReservedNames().Get(i); !hiddenNames[name] {
			panic(fmt.Sprintf("%s reserves name %q, which is not a non-public field of %s", dd.FullName(), name, src.FullName()))
		}
	}
	for i := 0; i < dd.ReservedRanges().Len(); i++ {
		r := dd.ReservedRanges().Get(i) // [start, end)
		// 先比区间长度：reserved 10 to max 这类区间逐个号走一遍要上亿次，而它必然多于没公开的源字段。
		if int(r[1]-r[0]) > len(hiddenNumbers) {
			panic(fmt.Sprintf("%s reserves numbers %d to %d, more than the %d non-public fields of %s", dd.FullName(), r[0], r[1]-1, len(hiddenNumbers), src.FullName()))
		}
		for n := r[0]; n < r[1]; n++ {
			if !hiddenNumbers[n] {
				panic(fmt.Sprintf("%s reserves number %d, which is not a non-public field of %s", dd.FullName(), n, src.FullName()))
			}
		}
	}
	return p
}

// projectable 限于单值字段：标量、枚举与消息。列表与 map 的逐项语义各不相同，公开消息目前不需要。枚举按编号复制，
// 编号的含义由枚举类型决定，所以 newProjection 另要求两侧引用同一个枚举类型。消息不整条复制，而是递归投影。
func projectable(f protoreflect.FieldDescriptor) bool {
	return f.Cardinality() != protoreflect.Repeated && f.Kind() != protoreflect.GroupKind
}

// apply 只复制源里存在的字段：optional 缺失仍是缺失，显式的 0 仍是 0，缺失的子消息仍缺失。
func (p projection) apply(src proto.Message) proto.Message {
	sm := src.ProtoReflect()
	out := p.dst.New()
	for _, f := range p.fields {
		if !sm.Has(f.src) {
			continue
		}
		v := sm.Get(f.src)
		if f.sub != nil {
			v = protoreflect.ValueOfMessage(f.sub.apply(v.Message().Interface()).ProtoReflect())
		}
		out.Set(f.dst, v)
	}
	return out.Interface()
}
