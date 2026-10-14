package agentwire

import (
	"cmp"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"slices"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"google.golang.org/protobuf/proto"
)

// TasksDigestLen 是 ReportRequest.tasks_digest 给出时的字节数：TasksDigest 产出的 SHA-256。
const TasksDigestLen = sha256.Size

// TasksDigest 是任务清单对账摘要的唯一算法，agent 上报（ReportRequest.tasks_digest）与
// hub 决定要不要重发清单调用同一函数，两侧不能各自持有一份。
// 编码对象是清单本身而不含版本计数：版本在备份恢复后可能与 agent 持有的内容错位，
// 按计数对账会让旧配置一直留在 agent 上，按内容摘要不会。
// 任务按 task_id 升序（收到顺序不影响结果），各自确定性 protobuf 编码，前缀 8 字节
// 大端长度后依次拼接，对整串取 SHA-256。长度前缀使拼接边界无歧义：少了它，
// 两组不同的任务序列可能拼出同一串字节。
// 空清单返回空串的 SHA-256，与"尚未收到任何清单"（调用方不带该字段）严格区分。
//
// 错误只在任务消息无法确定性编码时返回（proto3 string 字段含非法 UTF-8）。agent 侧不会
// 发生：清单经 protobuf 解码而来，解码已拒绝非法 UTF-8。hub 侧的清单由存储行组装，CheckTask
// 与存储都不检查 UTF-8，所以两侧调用方都必须处理错误：按"本次摘要缺席"对待，不阻塞上报
// 或对账主流程。
func TasksDigest(tasks []*heronv1.ProbeTask) ([]byte, error) {
	sorted := slices.SortedFunc(slices.Values(tasks), func(a, b *heronv1.ProbeTask) int { return cmp.Compare(a.GetId(), b.GetId()) })
	h := sha256.New()
	var size [8]byte
	for _, t := range sorted {
		b, err := proto.MarshalOptions{Deterministic: true}.Marshal(t)
		if err != nil {
			return nil, fmt.Errorf("encode task %d for tasks digest: %w", t.GetId(), err)
		}
		binary.BigEndian.PutUint64(size[:], uint64(len(b)))
		h.Write(size[:])
		h.Write(b)
	}
	return h.Sum(nil), nil
}
