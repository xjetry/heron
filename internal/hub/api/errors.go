package api

import (
	"errors"
	"fmt"
	"math"
	"sort"

	"connectrpc.com/connect"
	"github.com/xjetry/heron/internal/hub/alert"
	"github.com/xjetry/heron/internal/hub/probe"
	"github.com/xjetry/heron/internal/hub/store"
)

// SQLite 的任务编号为有符号整数；拒绝不能表示的协议值，不能让它回绕成另一个编号。
func checkTaskID(id uint64, field string) error {
	if id > math.MaxInt64 {
		return invalid("%s: out of range", field)
	}
	return nil
}

// 校验方提供路径和约束，枚举词汇由双向转换共用的表裁定；包装错误不改变字段定位。
func fieldMessage(root string, field alert.FieldError) string {
	field = renderField(root, field)
	return root + "." + field.Path + " " + field.Detail()
}

func renderField(root string, field alert.FieldError) alert.FieldError {
	render := func(v string) string {
		switch {
		case root == "rule" && field.Path == "kind":
			return enumFor(alertKinds, store.AlertKind(v)).String()
		case root == "rule" && field.Path == "metric":
			return enumFor(probeMetrics, store.ProbeMetric(v)).String()
		case root == "rule" && field.Path == "resource_metric":
			return enumFor(resourceMetrics, store.ResourceMetric(v)).String()
		case root == "rule" && field.Path == "rtt_mode":
			return enumFor(rttModes, store.RttMode(v)).String()
		case root == "rule" && field.Path == "baseline_mode":
			return enumFor(baselineModes, store.BaselineMode(v)).String()
		case root == "channel" && field.Path == "kind":
			return enumFor(channelKinds, store.ChannelKind(v)).String()
		default:
			return v
		}
	}
	allowed := make([]string, len(field.Allowed))
	for i, v := range field.Allowed {
		allowed[i] = render(v)
	}
	field.Allowed = allowed
	field.Got = render(field.Got)
	return field
}

// 协议枚举必须在转换前拒绝表外值；否则 map 零值会抹掉实际输入，错误只能回显 UNSPECIFIED。
func parseEnum[K interface {
	comparable
	fmt.Stringer
}, V ~string](values map[K]V, value K, root, path string) (V, error) {
	if v, ok := values[value]; ok {
		return v, nil
	}
	allowed := make([]string, 0, len(values))
	for _, v := range values {
		allowed = append(allowed, string(v))
	}
	sort.Strings(allowed)
	field := renderField(root, alert.FieldError{Path: path, Allowed: allowed})
	field.Got = value.String()
	var zero V
	return zero, invalid("%s.%s %s", root, path, field.Detail())
}

// 存储层携带对象种类；保存与删除各自指定请求根路径，不从错误文本猜测对象。
// root 为请求顶层的 id 字段本身（id、delivery_id）时，缺失的就是它。
func missingField(root string, kind store.ObjectKind) string {
	if root == "id" || root == "delivery_id" {
		return root
	}
	switch kind {
	case store.ObjectNode:
		if root == "task" {
			return "node_ids"
		}
		return root + ".node_ids"
	case store.ObjectNotifyChannel:
		if root == "rule" {
			return "rule.channel_ids"
		}
	case store.ObjectProbeTask:
		if root == "rule" {
			return "rule.task_id"
		}
	}
	return root + ".id"
}

func (s *Service) operationError(err error, root, operation string) error {
	var kindField store.KindFieldError
	if errors.As(err, &kindField) {
		return invalid("%s.%s %s", root, kindField.Field, kindField.Constraint)
	}
	// rented 与 maintenance 的互斥只在 store 裁决（UpdateNodeTasks 与快照恢复预检共用一处判定，api、ExecuteChange
	// 与恢复三条写路径都会撞上）；协议层把拒绝翻译成点名两个字段的 InvalidArgument，所有写 handler 共用这一处映射。
	var rentedConflict store.RentedConflictError
	if errors.As(err, &rentedConflict) {
		return invalid("maintenance and rented cannot both be true: a node is either in maintenance or rented out; clear one of them")
	}
	switch {
	case errors.Is(err, alert.ErrInvalid):
		var field alert.FieldError
		if errors.As(err, &field) {
			return connect.NewError(connect.CodeInvalidArgument, errors.New(fieldMessage(root, field)))
		}
	case errors.Is(err, probe.ErrInvalid):
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, store.ErrPrecondition):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, store.ErrNotFound):
		var missing store.NotFoundError
		errors.As(err, &missing)
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("%s: %w", missingField(root, missing.Kind), err))
	case errors.Is(err, store.ErrInUse):
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s: %w", root, err))
	case errors.Is(err, store.ErrNodeLimit):
		return connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("%s: %w", missingField(root, store.ObjectNode), err))
	case errors.Is(err, store.ErrPreview), errors.Is(err, store.ErrReplay):
		// 预览与重放不是失败：主写按变更的要求回滚或跳过，handler 经这里退出，runChange 按 Change.Err 回答预览结果或原回执。
		// 原样返回、不记日志；记成 Error 会让每次预览都在日志里留下一条"操作失败"。
		return err
	}
	s.log.Error(operation, "err", err)
	return internalError(operation)
}
