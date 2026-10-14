package agentwire

import (
	"fmt"
	"math"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
)

// ExecutionScope 的形态约束。agent 上报前自检，hub 落库前同形校验，
// 两边同一份，边界才不会各说各话。nil 表示这次没有带执行环境，接受；
// 不能写成空对象，空对象的 kind 是未指定，校验会拒绝。
const (
	MaxScopeNotes = 8
	MaxScopeCores = 65536
)

// ValidateCPUCores 校验 Facts.cpu_cores 的上界。0 表示有效核数未知，仍然合法。
// 上界与 ExecutionScope 的容量共用 MaxScopeCores，避免两处各写一个数以后漂开。
func ValidateCPUCores(n uint32) error {
	if n > MaxScopeCores {
		return fmt.Errorf("facts.cpu_cores: must be at most %d", MaxScopeCores)
	}
	return nil
}

// ValidateExecutionScope 校验 Facts.execution：带了就必须给出非未指定的 kind
// （未指定不是识别结果）；kind 决定各资源范围能取哪些值；容量字段（有效核数、内存/swap 上限、按核负载
// 分母）只在对应资源范围可见时出现、且必须是正的有限值；说明去重、至多 8 个。
func ValidateExecutionScope(e *heronv1.ExecutionScope) error {
	if e == nil {
		return nil
	}
	if len(e.ProtoReflect().GetUnknown()) != 0 {
		return fmt.Errorf("execution: unknown fields are not accepted")
	}
	switch e.GetKind() {
	case heronv1.ScopeKind_SCOPE_KIND_HOST,
		heronv1.ScopeKind_SCOPE_KIND_CGROUP_NAMESPACE,
		heronv1.ScopeKind_SCOPE_KIND_CGROUP_V1_LEGACY,
		heronv1.ScopeKind_SCOPE_KIND_IDENTIFY_FAILED:
	default:
		// UNSPECIFIED 与一切越界值：枚举演化后的未知 kind 不得放行。
		return fmt.Errorf("execution.kind: %d out of range", e.GetKind())
	}
	scopes := []struct {
		name  string
		value heronv1.ResourceScope
	}{
		{"cpu", e.GetCpu()},
		{"memory", e.GetMemory()},
		{"swap", e.GetSwap()},
		{"load", e.GetLoad()},
	}
	for _, s := range scopes {
		if s.value == heronv1.ResourceScope_RESOURCE_SCOPE_UNSPECIFIED {
			return fmt.Errorf("execution.%s: must not be unspecified", s.name)
		}
	}
	host := heronv1.ResourceScope_RESOURCE_SCOPE_HOST
	env := heronv1.ResourceScope_RESOURCE_SCOPE_ENVIRONMENT
	unknown := heronv1.ResourceScope_RESOURCE_SCOPE_UNKNOWN
	legacy := heronv1.ResourceScope_RESOURCE_SCOPE_LEGACY
	switch e.GetKind() {
	case heronv1.ScopeKind_SCOPE_KIND_HOST:
		for _, s := range scopes {
			if s.value != host && s.value != unknown {
				return fmt.Errorf("execution.%s: host scope must be host or unknown, got %s", s.name, s.value)
			}
		}
	case heronv1.ScopeKind_SCOPE_KIND_CGROUP_NAMESPACE:
		// 环境里负载仍可能是主机范围（loadavg 是 procfs），但 CPU/内存/swap 是环境
		// 自己的；legacy 范围与"命名空间"的说法矛盾。
		for _, s := range scopes {
			if s.value == legacy {
				return fmt.Errorf("execution.%s: environment scope must not be legacy", s.name)
			}
		}
		if e.GetCpu() != env && e.GetCpu() != unknown {
			return fmt.Errorf("execution.cpu: environment must be environment or unknown, got %s", e.GetCpu())
		}
		if e.GetMemory() != env && e.GetMemory() != unknown {
			return fmt.Errorf("execution.memory: environment must be environment or unknown, got %s", e.GetMemory())
		}
		if e.GetSwap() != env && e.GetSwap() != unknown {
			return fmt.Errorf("execution.swap: environment must be environment or unknown, got %s", e.GetSwap())
		}
		if e.GetLoad() != host && e.GetLoad() != unknown {
			return fmt.Errorf("execution.load: environment must be host or unknown, got %s", e.GetLoad())
		}
	case heronv1.ScopeKind_SCOPE_KIND_CGROUP_V1_LEGACY:
		for _, s := range scopes {
			if s.value != legacy && s.value != unknown {
				return fmt.Errorf("execution.%s: legacy scope must be legacy or unknown, got %s", s.name, s.value)
			}
		}
	case heronv1.ScopeKind_SCOPE_KIND_IDENTIFY_FAILED:
		for _, s := range scopes {
			if s.value != unknown {
				return fmt.Errorf("execution.%s: identify failed must leave everything unknown, got %s", s.name, s.value)
			}
		}
	}
	// 容量与范围一致：有分母必有范围，范围未知不得带容量。
	if e.CpuEffectiveCores != nil {
		if e.GetCpu() == unknown {
			return fmt.Errorf("execution.cpu_effective_cores: set while cpu scope is unknown")
		}
		if v := e.GetCpuEffectiveCores(); !(v > 0 && v <= MaxScopeCores && !math.IsInf(v, 1)) {
			return fmt.Errorf("execution.cpu_effective_cores: must be positive and at most %d", MaxScopeCores)
		}
	}
	if e.MemoryLimitBytes != nil && e.GetMemory() == unknown {
		return fmt.Errorf("execution.memory_limit_bytes: set while memory scope is unknown")
	}
	if e.SwapLimitBytes != nil && e.GetSwap() == unknown {
		return fmt.Errorf("execution.swap_limit_bytes: set while swap scope is unknown")
	}
	if e.MemoryLimitBytes != nil && e.GetMemoryLimitBytes() == 0 {
		return fmt.Errorf("execution.memory_limit_bytes: must be positive")
	}
	// swap 上限的已知 0 合法（proto：宿主没有 swap、或容器被写 0 禁 swap，读数同为 0/0），
	// 不做正性检查；内存上限出现时必为正。
	if e.LoadCores != nil {
		if e.GetLoad() == unknown {
			return fmt.Errorf("execution.load_cores: set while load scope is unknown")
		}
		if v := e.GetLoadCores(); v == 0 || v > MaxScopeCores {
			return fmt.Errorf("execution.load_cores: must be positive and at most %d", MaxScopeCores)
		}
	}
	if len(e.Notes) > MaxScopeNotes {
		return fmt.Errorf("execution.notes: at most %d notes", MaxScopeNotes)
	}
	for i, n := range e.Notes {
		if n == heronv1.ScopeNote_SCOPE_NOTE_UNSPECIFIED {
			return fmt.Errorf("execution.notes: must not contain unspecified")
		}
		if i > 0 && e.Notes[i-1] >= n {
			return fmt.Errorf("execution.notes: must be distinct and sorted")
		}
	}
	return nil
}
