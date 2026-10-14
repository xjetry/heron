package agentwire

import (
	"math"
	"testing"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
)

func f64(v float64) *float64 { return &v }
func u64(v uint64) *uint64   { return &v }
func u32(v uint32) *uint32   { return &v }

func validScope() *heronv1.ExecutionScope {
	return &heronv1.ExecutionScope{
		Kind:              heronv1.ScopeKind_SCOPE_KIND_CGROUP_NAMESPACE,
		Cpu:               heronv1.ResourceScope_RESOURCE_SCOPE_ENVIRONMENT,
		Memory:            heronv1.ResourceScope_RESOURCE_SCOPE_ENVIRONMENT,
		Swap:              heronv1.ResourceScope_RESOURCE_SCOPE_UNKNOWN,
		Load:              heronv1.ResourceScope_RESOURCE_SCOPE_HOST,
		CpuEffectiveCores: f64(1.5),
		LoadCores:         u32(4),
		MemoryLimitBytes:  u64(512 << 20),
		Notes: []heronv1.ScopeNote{
			heronv1.ScopeNote_SCOPE_NOTE_CPUINFO_NOT_PROCFS,
			heronv1.ScopeNote_SCOPE_NOTE_SWAP_ACCOUNTING_MISSING,
		},
	}
}

func TestValidateExecutionScope(t *testing.T) {
	if err := ValidateExecutionScope(nil); err != nil {
		t.Fatalf("nil（未上报执行环境）必须接受: %v", err)
	}
	if err := ValidateExecutionScope(validScope()); err != nil {
		t.Fatalf("合法块被拒: %v", err)
	}
	swapZero := validScope()
	swapZero.Swap = heronv1.ResourceScope_RESOURCE_SCOPE_ENVIRONMENT
	swapZero.SwapLimitBytes = u64(0)
	if err := ValidateExecutionScope(swapZero); err != nil {
		t.Fatalf("swap 上限的已知 0 必须接受: %v", err)
	}

	tests := []struct {
		name string
		mut  func(*heronv1.ExecutionScope)
	}{
		{"kind 未指定", func(e *heronv1.ExecutionScope) { e.Kind = heronv1.ScopeKind_SCOPE_KIND_UNSPECIFIED }},
		{"资源范围未指定", func(e *heronv1.ExecutionScope) { e.Swap = heronv1.ResourceScope_RESOURCE_SCOPE_UNSPECIFIED }},
		{"kind=主机却给环境范围", func(e *heronv1.ExecutionScope) {
			e.Kind = heronv1.ScopeKind_SCOPE_KIND_HOST
			e.Load = heronv1.ResourceScope_RESOURCE_SCOPE_HOST
		}},
		{"kind=环境却给 legacy 范围", func(e *heronv1.ExecutionScope) { e.Load = heronv1.ResourceScope_RESOURCE_SCOPE_LEGACY }},
		{"kind=v1 却给环境范围", func(e *heronv1.ExecutionScope) {
			e.Kind = heronv1.ScopeKind_SCOPE_KIND_CGROUP_V1_LEGACY
			e.Cpu = heronv1.ResourceScope_RESOURCE_SCOPE_ENVIRONMENT
		}},
		{"识别失败却给可见范围", func(e *heronv1.ExecutionScope) {
			e.Kind = heronv1.ScopeKind_SCOPE_KIND_IDENTIFY_FAILED
			e.Cpu = heronv1.ResourceScope_RESOURCE_SCOPE_HOST
		}},
		{"范围未知却带容量", func(e *heronv1.ExecutionScope) { e.SwapLimitBytes = u64(1 << 20) }},
		{"有效核数非正", func(e *heronv1.ExecutionScope) { e.CpuEffectiveCores = f64(0) }},
		{"有效核数超上限", func(e *heronv1.ExecutionScope) { e.CpuEffectiveCores = f64(65537) }},
		{"有效核数不是数", func(e *heronv1.ExecutionScope) { e.CpuEffectiveCores = f64(math.Inf(1)) }},
		{"内存上限为 0", func(e *heronv1.ExecutionScope) {
			e.Memory = heronv1.ResourceScope_RESOURCE_SCOPE_ENVIRONMENT
			e.MemoryLimitBytes = u64(0)
		}},
		{"kind 越界", func(e *heronv1.ExecutionScope) {
			e.Kind = heronv1.ScopeKind(99)
		}},
		{"按核负载分母为 0", func(e *heronv1.ExecutionScope) { e.LoadCores = u32(0) }},
		{"说明含未指定值", func(e *heronv1.ExecutionScope) {
			e.Notes = append(e.Notes, heronv1.ScopeNote_SCOPE_NOTE_UNSPECIFIED)
		}},
		{"说明未去重排序", func(e *heronv1.ExecutionScope) {
			e.Notes = []heronv1.ScopeNote{heronv1.ScopeNote_SCOPE_NOTE_SWAP_ACCOUNTING_MISSING, heronv1.ScopeNote_SCOPE_NOTE_CPUINFO_NOT_PROCFS}
		}},
		{"说明超过 8 个", func(e *heronv1.ExecutionScope) {
			e.Notes = []heronv1.ScopeNote{
				heronv1.ScopeNote_SCOPE_NOTE_CONTAINER_SIGNAL_ON_HOST_ROOT,
				heronv1.ScopeNote_SCOPE_NOTE_MOUNTINFO_UNREADABLE,
				heronv1.ScopeNote_SCOPE_NOTE_LOADAVG_NOT_PROCFS,
				heronv1.ScopeNote_SCOPE_NOTE_CPUINFO_NOT_PROCFS,
				heronv1.ScopeNote_SCOPE_NOTE_MEMORY_CONTROLLER_MISSING,
				heronv1.ScopeNote_SCOPE_NOTE_SWAP_ACCOUNTING_MISSING,
				heronv1.ScopeNote(7), heronv1.ScopeNote(8), heronv1.ScopeNote(9),
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := validScope()
			tt.mut(e)
			if err := ValidateExecutionScope(e); err == nil {
				t.Fatal("非法块必须被拒")
			}
		})
	}
}
