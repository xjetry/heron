package store

import (
	"context"

	"github.com/xjetry/heron/internal/hub/metric"
)

// EvaluationReader 是 hub 自己的告警评估读历史的唯一入口：读走评估池 ev，不与请求驱动的读共用 r / hr（原则见
// readPoolFor）。额度、分级与水位拼接与 Store 的同名方法相同，只有选池不同：它不看预计扫描量、不封顶重跑，评估窗口再长也留在 ev。
type EvaluationReader struct{ s *Store }

// Evaluation 返回评估读者。调用方只应是告警引擎：评估池的上限按引擎的评估互斥与单协程的基线重算论证（见 evaluationPoolSize），
// 别的调用方经它读历史会占用那份余量。
func (s *Store) Evaluation() *EvaluationReader { return &EvaluationReader{s: s} }

// QueryMetrics 的语义同 Store.QueryMetrics。
func (e *EvaluationReader) QueryMetrics(ctx context.Context, nodeID int64, from, to int64, lv Level, step int64) ([]metric.Row, error) {
	return e.s.queryMetrics(ctx, evaluationRead, nodeID, from, to, lv, step)
}

// QueryProbes 的语义同 Store.QueryProbes。不收序列数：评估池不按预计扫描量选池，估计无处可用。
func (e *EvaluationReader) QueryProbes(ctx context.Context, nodeID int64, from, to int64, lv Level, step int64) ([]metric.ProbeRow, error) {
	return e.s.queryProbes(ctx, evaluationRead, nodeID, from, to, lv, step, 0)
}
