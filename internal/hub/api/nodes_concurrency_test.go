package api

import (
	"google.golang.org/protobuf/proto"
	"sync"
	"testing"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/store"
)

func TestConcurrentUpdatesKeepResetDayConsistent(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	id, _, err := h.auth.CreateNode(t.Context(), store.NewNode{Name: "n"})
	if err != nil {
		t.Fatal(err)
	}
	for round := range 2000 {
		start := make(chan struct{})
		errs := make(chan error, 2)
		for _, day := range []uint32{15, 20} {
			go func() {
				<-start
				_, err := h.svc.UpdateNode(t.Context(), connect.NewRequest(&heronv1.UpdateNodeRequest{Id: id, Name: "n", TrafficResetDay: day, OfflineGraceS: proto.Uint32(0)}))
				errs <- err
			}()
		}
		close(start)
		for range 2 {
			if err := <-errs; err != nil {
				t.Fatal(err)
			}
		}
		n, err := h.store.GetNode(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if day := h.book.View(id).ResetDay; day != n.TrafficResetDay {
			t.Fatalf("round %d: memory reset day %d differs from stored %d", round, day, n.TrafficResetDay)
		}
	}
}

// 此用例声明 DeleteNode 侧必须与更新共用临界区：Forget 后不能再由 SetResetDay 重建状态。
// 暴露缺陷需要更新恰在库提交后、SetResetDay 前被抢占，并让删除完成事务与 Forget，窗口很窄。
// 它是不变式的声明而非可靠探测器；TestConcurrentUpdatesKeepResetDayConsistent 更稳定地探测无锁分叉。
func TestConcurrentUpdateCannotReviveDeletedResetDay(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	for round := range 2000 {
		id, _, err := h.auth.CreateNode(t.Context(), store.NewNode{Name: "n"})
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Go(func() {
			<-start
			// 删除可以先提交，更新此时无须成功；最终内存状态不能因更新复活。
			_, _ = h.svc.UpdateNode(t.Context(), connect.NewRequest(&heronv1.UpdateNodeRequest{Id: id, Name: "n", TrafficResetDay: 20, OfflineGraceS: proto.Uint32(0)}))
		})
		wg.Go(func() {
			<-start
			if _, err := h.svc.DeleteNode(t.Context(), connect.NewRequest(&heronv1.DeleteNodeRequest{Id: id})); err != nil {
				t.Error(err)
			}
		})
		close(start)
		wg.Wait()
		if day := h.book.View(id).ResetDay; day != 1 {
			t.Fatalf("round %d: deleted node retained reset day %d", round, day)
		}
	}
}
