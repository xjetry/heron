package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/heartbeat"
	"github.com/xjetry/heron/internal/hub/live"
	"github.com/xjetry/heron/internal/hub/store"
)

// newCountsFixture 给出心跳计数的两个读侧来源：真实库与一份 live 在线判定，与 serve 的装配同一形状。
func newCountsFixture(t *testing.T) (*store.Store, *live.Live) {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"), clk, slog.Default(), store.MigrateSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st, live.New(clk, 30*time.Second)
}

// createNode 直接经 store 登记节点：计数的输入是库里的节点行，不经过 api 层。token_hash 有唯一约束，用节点名充当。
func createNode(t *testing.T, st *store.Store, spec store.NewNode) int64 {
	t.Helper()
	id, _, err := st.CreateNode(t.Context(), spec, []byte(spec.Name))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// markOnline 让节点在 live 里有一次上报：在线判定规则见 live.Live，库里的节点行本身不产生在线。
func markOnline(t *testing.T, lv *live.Live, ids ...int64) {
	t.Helper()
	for _, id := range ids {
		lv.Observe(id, "", &heronv1.Metrics{})
	}
}

// 计数口径：online 按 live；rented 数全部已出租节点；offline 不含已出租——外部监控按 offline 告警时不该被出租机器触发。
func TestHeartbeatCountsExcludeRentedFromOffline(t *testing.T) {
	st, lv := newCountsFixture(t)
	rentedOffline := createNode(t, st, store.NewNode{Name: "leased", Rented: true})
	rentedOnline := createNode(t, st, store.NewNode{Name: "leased-still-reporting", Rented: true})
	plainOffline := createNode(t, st, store.NewNode{Name: "down"})
	plainOnline := createNode(t, st, store.NewNode{Name: "up"})
	markOnline(t, lv, rentedOnline, plainOnline)
	_ = rentedOffline
	_ = plainOffline
	got, err := heartbeatSource{st: st, live: lv}.HeartbeatCounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := heartbeat.Counts{NodesTotal: 4, Online: 2, Offline: 1, Rented: 2}
	if got != want {
		t.Fatalf("counts = %+v, want %+v", got, want)
	}
}
