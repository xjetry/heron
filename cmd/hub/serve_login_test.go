package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/testwait"
)

// 登录通知从 serve 的装配一路送到接收方：startAlertHub 的那次登录写下 login_success，投递队列须在运行期间发出它。
// 发送者没接上时通知只写库、不入队（见 auth.New），要等下次重启才发出，这条用例在等送达时超时。
func TestServeDeliversLoginNotification(t *testing.T) {
	t.Parallel()
	receiver, bodies := alertReceiver(t)
	clk := clock.NewFake(time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC))
	client, _, stop := startAlertHub(t, clk, func(st *store.Store) {
		config, err := json.Marshal(map[string]string{"url": receiver, "method": "POST"})
		if err != nil {
			t.Fatal(err)
		}
		channel, err := st.SaveNotifyChannel(t.Context(), store.NotifyChannel{Name: "login", Kind: store.ChannelWebhook, Config: string(config)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.SaveSettings(t.Context(), store.SettingsUpdate{LoginChannels: &[]int64{channel.ID}}); err != nil {
			t.Fatal(err)
		}
	})
	defer stop()
	awaitDelivered(t, client, bodies, "login_success", testwait.Bound)
}
