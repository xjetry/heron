package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/testwait"
)

// 流量报告从 serve 的装配一路送到接收方：启用日报而库里没有已发标记，调度启动时的那次检查即到期，经投递队列发出。
// 调度没起、或没接上投递队列时，这条用例在等送达时超时。
func TestServeDeliversTrafficReport(t *testing.T) {
	t.Parallel()
	receiver, bodies := alertReceiver(t)
	clk := clock.NewFake(time.Date(2026, 9, 28, 1, 0, 0, 0, time.UTC))
	client, _, stop := startAlertHub(t, clk, func(st *store.Store) {
		config, err := json.Marshal(map[string]string{"url": receiver, "method": "POST"})
		if err != nil {
			t.Fatal(err)
		}
		channel, err := st.SaveNotifyChannel(t.Context(), store.NotifyChannel{Name: "report", Kind: store.ChannelWebhook, Config: string(config)})
		if err != nil {
			t.Fatal(err)
		}
		report := store.TrafficReportUpdate{Enabled: true, Daily: true, Channels: []int64{channel.ID}}
		if _, err := st.SaveSettings(t.Context(), store.SettingsUpdate{TrafficReport: &report}); err != nil {
			t.Fatal(err)
		}
	})
	defer stop()
	awaitDelivered(t, client, bodies, string(store.TransitionTrafficReport), testwait.Bound)
}
