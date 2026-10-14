package client

import (
	"hash/fnv"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"google.golang.org/protobuf/proto"
)

// FactsHash 是 agent 侧的静态信息摘要：hub 只比较、不重算，所以只需在
// 同一 agent 内稳定。确定性序列化保证同样的 Facts 得到同样的字节。
func FactsHash(f *heronv1.Facts) uint64 { return messageHash(f) }

// UpdateHash 是 UpdateStatus 的摘要（ReportRequest.update_hash），与 FactsHash 同一种算法、同一个前提：hub 只拿它与
// 同一 agent 进程此前给出的摘要比较相等。进程重启后的首次上报总带全文，所以不需要跨进程或跨版本稳定。
func UpdateHash(s *heronv1.UpdateStatus) uint64 { return messageHash(s) }

func messageHash(m proto.Message) uint64 {
	b, _ := proto.MarshalOptions{Deterministic: true}.Marshal(m)
	h := fnv.New64a()
	h.Write(b)
	return h.Sum64()
}
