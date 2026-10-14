package main

import (
	"context"

	"github.com/xjetry/heron/internal/hub/alert"
	"github.com/xjetry/heron/internal/hub/traffic"
)

// 启动扫描依赖账本和状态均已装入；任何加载失败都不以空缓存评估已持久化的 firing。
func loadTrafficAlerts(ctx context.Context, book *traffic.Book, alerts *alert.Engine) error {
	if err := book.Load(ctx); err != nil {
		return err
	}
	if err := alerts.Load(ctx); err != nil {
		return err
	}
	return alerts.SweepTraffic(ctx)
}
