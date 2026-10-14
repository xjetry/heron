package agentwire

import (
	"encoding/hex"
	"fmt"
	"path"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"google.golang.org/protobuf/reflect/protoreflect"
)

const (
	MaxNetPatterns          = 64
	MaxNetPatternBytes      = 128
	MaxDiagnosticInterfaces = 128
	MaxInterfaceNameBytes   = 64
)

// ValidateCounterEpoch 接受空标识：合计形态可以不带集合摘要。空与非空切换仍由流量账本重建基线。
func ValidateCounterEpoch(epoch string) error {
	if epoch == "" {
		return nil
	}
	if len(epoch) != 64 || strings.ToLower(epoch) != epoch {
		return fmt.Errorf("net_counter_epoch: must be empty or a lowercase SHA-256 digest")
	}
	if _, err := hex.DecodeString(epoch); err != nil {
		return fmt.Errorf("net_counter_epoch: must be empty or a lowercase SHA-256 digest")
	}
	return nil
}

func safeDiagnosticText(s string, limit int) bool {
	return s != "" && len(s) <= limit && utf8.ValidString(s) && !strings.ContainsFunc(s, unicode.IsControl)
}

// ValidateInterfaceName 是网卡名的唯一准入谓词：诊断里的网卡名、上报的逐网卡计数与账本落盘的逐网卡基线共用。
// 错误不带字段名，由调用方按所在字段加前缀。
func ValidateInterfaceName(name string) error {
	if !safeDiagnosticText(name, MaxInterfaceNameBytes) {
		return fmt.Errorf("names must be non-empty UTF-8 without control characters, at most %d bytes", MaxInterfaceNameBytes)
	}
	return nil
}

// ValidateNetFilters 同时约束本机启动参数和上报预算，避免本机接受的配置只能生成被 hub 永久拒绝的请求。
func ValidateNetFilters(include, exclude []string) error {
	for i, patterns := range [][]string{include, exclude} {
		name := []string{"net_include", "net_exclude"}[i]
		if len(patterns) > MaxNetPatterns {
			return fmt.Errorf("%s: must contain at most %d patterns", name, MaxNetPatterns)
		}
		for _, p := range patterns {
			if !safeDiagnosticText(p, MaxNetPatternBytes) {
				return fmt.Errorf("%s: patterns must be non-empty UTF-8 without control characters, at most %d bytes", name, MaxNetPatternBytes)
			}
			if _, err := path.Match(p, ""); err != nil {
				return fmt.Errorf("%s: invalid glob pattern", name)
			}
		}
	}
	return nil
}

// ValidateDiagnostics 是上报、落库、读库与恢复共用的白名单边界；不接收任意错误文本或配置 map。
func ValidateDiagnostics(d *heronv1.AgentDiagnostics) error {
	if d == nil {
		return nil
	}
	if len(d.ProtoReflect().GetUnknown()) != 0 {
		return fmt.Errorf("diagnostics: unknown fields are not accepted")
	}
	if err := ValidateNetFilters(d.NetInclude, d.NetExclude); err != nil {
		return err
	}
	if len(d.NetInclude) > 0 && len(d.NetExclude) > 0 {
		return fmt.Errorf("diagnostics: only the effective include or exclude policy may be provided")
	}
	if len(d.NetInterfaces) > MaxDiagnosticInterfaces || uint32(len(d.NetInterfaces)) != min(d.NetInterfacesTotal, MaxDiagnosticInterfaces) {
		return fmt.Errorf("diagnostics.net_interfaces: must contain the first %d names or the complete smaller set", MaxDiagnosticInterfaces)
	}
	for i, name := range d.NetInterfaces {
		if err := ValidateInterfaceName(name); err != nil {
			return fmt.Errorf("diagnostics.net_interfaces: %w", err)
		}
		if i > 0 && d.NetInterfaces[i-1] >= name {
			return fmt.Errorf("diagnostics.net_interfaces: names must be unique, sorted, and at most %d bytes without control characters", MaxInterfaceNameBytes)
		}
	}
	seen := map[heronv1.CollectionComponent]bool{}
	for _, part := range d.FailedCollectors {
		if !knownEnum(part) || seen[part] {
			return fmt.Errorf("diagnostics.failed_collectors: must contain distinct known components")
		}
		seen[part] = true
	}
	netFailed := slices.Contains(d.FailedCollectors, heronv1.CollectionComponent_COLLECTION_COMPONENT_NET)
	if netFailed && d.NetInterfacesTotal != 0 {
		return fmt.Errorf("diagnostics.net_interfaces: failed network collection must not claim a sampled set")
	}
	if err := validateNetStructure(d, netFailed); err != nil {
		return err
	}
	if d.ReportIntervalMs != 0 {
		if _, ok := ClampReportInterval(d.ReportIntervalMs); !ok {
			return fmt.Errorf("diagnostics.report_interval_ms: outside agent reporting bounds")
		}
	}
	return nil
}

// knownEnum 以生成的枚举描述符为唯一取值表，拒绝未定义值与 UNSPECIFIED，不另存一份会随协议新增而过期的上界。
func knownEnum[E interface {
	~int32
	Descriptor() protoreflect.EnumDescriptor
}](v E) bool {
	return v != 0 && v.Descriptor().Values().ByNumber(protoreflect.EnumNumber(v)) != nil
}

// validateNetStructure 约束结构规则诊断：未启用时没有排除或退回，退回时没有实际排除，网络失败时没有判定结果。
// 结构规则只属于平台默认；显式排除与平台默认在诊断里都是展开后的 glob，无法区分，因此只能拒绝与显式包含同现。
func validateNetStructure(d *heronv1.AgentDiagnostics, netFailed bool) error {
	if d.NetStructureRules && len(d.NetInclude) > 0 {
		return fmt.Errorf("diagnostics.net_structure_rules: an explicit include policy disables the structure rules")
	}
	if !d.NetStructureRules && (d.NetStructureExcludedTotal != 0 || d.NetStructureFallback) {
		return fmt.Errorf("diagnostics.net_structure_rules: structure exclusions and fallback require the structure rules")
	}
	if d.NetStructureFallback && d.NetStructureExcludedTotal != 0 {
		return fmt.Errorf("diagnostics.net_structure_fallback: a fallback sample excludes nothing by structure")
	}
	if netFailed && (d.NetStructureExcludedTotal != 0 || d.NetStructureFallback) {
		return fmt.Errorf("diagnostics.net_structure_excluded: failed network collection must not claim a sampled set")
	}
	if len(d.NetStructureExcluded) > MaxDiagnosticInterfaces || uint32(len(d.NetStructureExcluded)) != min(d.NetStructureExcludedTotal, MaxDiagnosticInterfaces) {
		return fmt.Errorf("diagnostics.net_structure_excluded: must contain the first %d entries or the complete smaller set", MaxDiagnosticInterfaces)
	}
	for i, e := range d.NetStructureExcluded {
		if len(e.ProtoReflect().GetUnknown()) != 0 {
			return fmt.Errorf("diagnostics.net_structure_excluded: unknown fields are not accepted")
		}
		if err := ValidateInterfaceName(e.GetName()); err != nil {
			return fmt.Errorf("diagnostics.net_structure_excluded: %w", err)
		}
		if i > 0 && d.NetStructureExcluded[i-1].GetName() >= e.GetName() {
			return fmt.Errorf("diagnostics.net_structure_excluded: names must be unique and sorted")
		}
		if !knownEnum(e.GetReason()) {
			return fmt.Errorf("diagnostics.net_structure_excluded: reason must be a known NetStructureReason")
		}
	}
	return nil
}
