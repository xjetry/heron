package api

import (
	"fmt"
	"testing"

	"connectrpc.com/connect"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/gen/heron/v1/heronv1connect"
)

// 单节点探测查询的读池只看节点的全部任务数，不看调用方能见几个：节点 x 共 16 个任务，其中只有 1 个只分配给 x
// （受限于 x 的 token 只见这一个，其余 15 个还分配给 y），6 小时 1m 级 360 桶。按全部任务估 360×16=5760 行，
// 超过轻池分界 4096，查询进大扫描池；按可见任务估只有 360 行，会进轻池。管理员与受限 token 在各自的新库上
// 做同一判定，两者都必须落在大扫描池。
func TestProbeQueryPoolFollowsAllNodeTasks(t *testing.T) {
	t.Parallel()
	const tasks = 16
	for _, who := range []string{"admin", "restricted"} {
		t.Run(who, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, "")
			h.login(t)
			x, _ := h.createNode(t, "x")
			y, _ := h.createNode(t, "y")
			for i := range tasks {
				nodes := []int64{x, y}
				if i == 0 {
					nodes = []int64{x}
				}
				if _, err := h.admin.SaveProbeTask(t.Context(), connect.NewRequest(&heronv1.SaveProbeTaskRequest{Task: probeTask(fmt.Sprintf("192.0.2.%d", i+1)), NodeIds: nodes})); err != nil {
					t.Fatal(err)
				}
			}
			client := h.admin
			if who == "restricted" {
				var scoped heronv1connect.AdminServiceClient
				scoped, _, _ = grantedClient(t, h, &heronv1.TokenGrant{NodeIds: []int64{x}, Permissions: []heronv1.TokenPermission{heronv1.TokenPermission_TOKEN_PERMISSION_CONFIGURE}})
				list, err := scoped.ListProbeTasks(t.Context(), connect.NewRequest(&heronv1.ListProbeTasksRequest{}))
				if err != nil || len(list.Msg.GetTasks()) != 1 {
					t.Fatalf("restricted token lists %d tasks (err %v), want exactly the one assigned to x alone", len(list.Msg.GetTasks()), err)
				}
				client = scoped
			}
			// 每个 harness 有自己的库，大扫描池在此之前没有被任何请求用过：先断言它还没开过连接，判据才成立。
			if n := h.store.ReadPoolStats().History.OpenConnections; n != 0 {
				t.Fatalf("history pool has %d open connections before the query, want 0 on a fresh harness", n)
			}
			now := h.clk.Now().Unix()
			resp, err := client.QueryProbes(t.Context(), connect.NewRequest(&heronv1.QueryProbesRequest{NodeId: x, From: now - 6*3600, To: now, MaxPoints: 720}))
			if err != nil {
				t.Fatal(err)
			}
			if resp.Msg.GetLevel() != "1m" || resp.Msg.GetStepS() != 60 {
				t.Fatalf("query ran at %s / %ds, want 1m / 60s", resp.Msg.GetLevel(), resp.Msg.GetStepS())
			}
			if n := h.store.ReadPoolStats().History.OpenConnections; n == 0 {
				t.Errorf("%s token's 6h query on a %d-task node never used the history pool; the read-pool estimate must count all of the node's tasks", who, tasks)
			}
		})
	}
}
