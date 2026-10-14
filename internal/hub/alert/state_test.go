package alert

import (
	"reflect"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/hub/store"
)

func ptr[T any](v T) *T { return &v }

func TestNextOffline(t *testing.T) {
	ttl, grace := 30*time.Second, 90*time.Second
	cases := []struct {
		name   string
		cur    store.AlertState
		unseen time.Duration
		want   store.AlertState
		tr     *store.Transition
	}{
		{"在线保持 ok", store.StateOK, 5 * time.Second, store.StateOK, nil},
		{"超过 TTL 进入 pending", store.StateOK, 31 * time.Second, store.StatePending, nil},
		{"超过宽限期触发", store.StatePending, 90 * time.Second, store.StateFiring, ptr(store.TransitionFiring)},
		{"ok 直接超过宽限期也触发", store.StateOK, 100 * time.Second, store.StateFiring, ptr(store.TransitionFiring)},
		{"firing 保持", store.StateFiring, 200 * time.Second, store.StateFiring, nil},
		{"firing 收到上报恢复", store.StateFiring, 3 * time.Second, store.StateOK, ptr(store.TransitionRecovered)},
		{"pending 收到上报回 ok 不发事件", store.StatePending, 3 * time.Second, store.StateOK, nil},
		{"firing 但仍超过 TTL 未到宽限期仍 firing", store.StateFiring, 40 * time.Second, store.StateFiring, nil},
		{"恰好 TTL 已离线", store.StateOK, ttl, store.StatePending, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, tr := NextOffline(c.cur, Observation{Reported: true, Unseen: c.unseen, Grace: grace, TTL: ttl})
			if got != c.want || !reflect.DeepEqual(tr, c.tr) {
				t.Fatalf("got %s %v, want %s %v", got, tr, c.want, c.tr)
			}
		})
	}
}

func TestNextProbe(t *testing.T) {
	high, low, missing := MinuteSample{Present: true, Exceeds: true}, MinuteSample{Present: true}, MinuteSample{}
	cases := []struct {
		name    string
		cur     store.AlertState
		samples []MinuteSample
		want    store.AlertState
		tr      *store.Transition
	}{
		{"三分钟全超阈", store.StateOK, []MinuteSample{high, high, high}, store.StateFiring, ptr(store.TransitionFiring)},
		{"两分钟超阈", store.StateOK, []MinuteSample{low, high, high}, store.StatePending, nil},
		{"窗口不足", store.StateOK, []MinuteSample{high, high}, store.StatePending, nil},
		{"firing 恢复", store.StateFiring, []MinuteSample{high, high, low}, store.StateOK, ptr(store.TransitionRecovered)},
		{"pending 恢复无事件", store.StatePending, []MinuteSample{high, high, low}, store.StateOK, nil},
		{"最近分钟缺失保持", store.StateFiring, []MinuteSample{high, high, missing}, store.StateFiring, nil},
		{"pending 缺失保持", store.StatePending, []MinuteSample{missing}, store.StatePending, nil},
		{"firing 早期缺失保持", store.StateFiring, []MinuteSample{missing, high, high}, store.StateFiring, nil},
		{"中间缺失不触发", store.StateOK, []MinuteSample{high, missing, high}, store.StatePending, nil},
		{"无窗口保持", store.StatePending, nil, store.StatePending, nil},
		{"重复触发无事件", store.StateFiring, []MinuteSample{high, high, high}, store.StateFiring, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, tr := NextProbe(c.cur, c.samples, 3)
			if got != c.want || !reflect.DeepEqual(tr, c.tr) {
				t.Fatalf("got %s %v, want %s %v", got, tr, c.want, c.tr)
			}
		})
	}
}
