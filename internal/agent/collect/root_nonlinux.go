//go:build !linux

package collect

import (
	"errors"
	"runtime"
)

// errHostRootUnsupported 是非 Linux 平台收到 --host-root：宿主根契约（openat2、宿主 procfs 与 sysfs）只在 Linux 上成立。
func errHostRootUnsupported(dir string) error {
	return &HostRootError{Dir: dir, Err: errors.New("not supported on " + runtime.GOOS), Hint: "--host-root is only accepted on linux"}
}
