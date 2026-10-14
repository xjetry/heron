// Package shutdowntest 只给测试用：让子进程以指定的 SIGINT 处置运行被测入口，钉住 shutdown.Notify 的契约。
package shutdowntest

import (
	"os"
	"os/signal"
	"syscall"
)

// InterruptEnv 设为 InterruptIgnored 时，ExecWithInterrupt 让子进程以启动方已忽略 SIGINT 的处置运行；其余取值
// （含未设）是交互启动时的默认处置。父进程把它放进子进程的环境。
const (
	InterruptEnv     = "HERON_TEST_CHILD_SIGINT"
	InterruptIgnored = "ignore"
)

// ExecWithInterrupt 在子进程入口最先调用：SIGINT 处置与 InterruptEnv 要求的不符时，就地改好再 exec 自身，
// 处置相符时直接返回 nil；exec 成功不返回，失败返回其错误。
//
// 子进程的处置不能随测试二进制的启动方式变：无作业控制的 shell 以忽略 SIGINT 启动后台作业（POSIX 异步列表），
// 直接后台运行测试二进制时这份忽略经 exec 传到子进程；经 go test 启动时 cmd/go 已订阅 SIGINT，测试二进制以默认
// 处置开始。Go 运行时把进程启动时的处置记作原处置，signal.Stop 之后恢复的是它，所以要在 exec 之前设好、由新
// 映像的运行时在启动时记下，才与真实启动方式下的进程一致：exec 保留被忽略的信号、把被捕获的复位为默认，要忽略就
// 先 Ignore，要默认就先订阅（装上运行时的处理器）。exec 之后处置已与要求一致，不会再进这里。
func ExecWithInterrupt() error {
	wantIgnored := os.Getenv(InterruptEnv) == InterruptIgnored
	if signal.Ignored(syscall.SIGINT) == wantIgnored {
		return nil
	}
	if wantIgnored {
		signal.Ignore(syscall.SIGINT)
	} else {
		signal.Notify(make(chan os.Signal, 1), syscall.SIGINT)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return syscall.Exec(exe, os.Args, os.Environ())
}
