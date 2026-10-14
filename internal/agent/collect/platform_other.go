//go:build !linux && !darwin

package collect

import (
	"errors"
	"runtime"

	"github.com/xjetry/heron/internal/clock"
)

func NewPlatform(_ string, _ clock.Clock, _, _ []string, hostRoot string) (*Collector, error) {
	if hostRoot != "" {
		return nil, errHostRootUnsupported(hostRoot)
	}
	return nil, errors.New("metrics collection is not implemented for " + runtime.GOOS)
}

// DefaultNetExclude 在不支持采集的平台上没有意义。
func DefaultNetExclude() []string { return nil }
