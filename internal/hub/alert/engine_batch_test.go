package alert

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// 评估调用出错返回时，已经提交的转换照样交给队列：它们已经落库，不能等到下一次补货或重启才发。
func TestEvaluationErrorStillEnqueuesCommittedTransitions(t *testing.T) {
	f := newFixture(t)
	c := telegramChannel(t, f, 20)
	offlineRuleTo(t, f, "离线", c)
	deliverySQL(t, deliveryDB(t, f), "CREATE TRIGGER fail_node2 BEFORE INSERT ON alert_event WHEN NEW.node_id = 2 BEGIN SELECT RAISE(ABORT, 'node2 blocked'); END")
	sender := &recorder{}
	f.e.SetSender(sender)
	f.sweep(t)
	f.clk.Advance(31 * time.Second)
	err := f.e.SweepOffline(t.Context())
	if err == nil || !strings.Contains(err.Error(), "node2 blocked") {
		t.Fatalf("sweep err=%v, want the blocked transition", err)
	}
	if len(sender.events) != 1 || sender.events[0].NodeID != 1 {
		t.Fatalf("enqueued=%+v, want node1's committed transition", sender.events)
	}
}

// 引擎记下每一行实际所在的批次，而不是它请求加入的：请求的批次已经开始尝试时，存储层让这一行新开一批，同一次调用
// 里后面同键的行加入这个新批，而不是各开一批。
func TestEngineFollowsTheBatchARowActuallyJoined(t *testing.T) {
	f := newFixture(t)
	f.nodes(t, 3)
	c := telegramChannel(t, f, 20)
	offlineRuleTo(t, f, "离线", c)
	// 第一行一插进去就像被 worker 开始尝试过：之后请求加入它的行只能新开一批。
	deliverySQL(t, deliveryDB(t, f), "CREATE TRIGGER started AFTER INSERT ON alert_delivery WHEN NEW.id = 1 BEGIN UPDATE alert_delivery SET attempts = 1 WHERE id = 1; END")
	fireAll(t, f)
	var batches []int64
	for _, d := range deliveriesOf(t, f, c.ID) {
		batches = append(batches, d.BatchID)
	}
	if !slices.Equal(batches, []int64{1, 2, 2}) {
		t.Fatalf("batches=%v, want [1 2 2]: the third row joins the batch the second row opened", batches)
	}
}
