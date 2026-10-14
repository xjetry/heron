// Package deployenv 读部署标识：容器运行时与 init 留在进程自己根上的文件和 PID 1 的环境变量。
//
// 标识描述的是读者进程自己所在的环境，不是被观测的主机，所以调用方必须传进程自己的根 "/"——
// agent 以 --host-root 采集宿主时也一样（宿主根上的标识属于宿主，与 agent 怎么部署无关）。
//
// 两个消费者对"在容器里"的口径不同，所以这里只给证据，判定分成两个，不合成一个布尔：
//   - ImageDeployment：镜像部署，二进制随镜像替换，不能在线更新（update.Client.Status，agent 与 hub 共用）。
//     只认 Docker 与 Podman 的标识。LXC / OrbStack 机器里的 systemd 安装也有 run/systemd/container
//     与 container=，但二进制不随镜像替换、就地更新；把它们算进镜像部署会取消更新资格。
//   - ContainerEvidence：读者有任一容器证据（collect 的执行环境说明 container_signal_on_host_root），
//     说明 agent 与被观测对象不是同一个环境。
package deployenv

import (
	"io/fs"
	"strings"
)

// Markers 是一次读取的结果：每个标识存在与否，以及 PID 1 的 container= 值。
type Markers struct {
	DockerEnv        bool   // .dockerenv：Docker（也是 Podman 的兼容标识）
	ContainerEnv     bool   // run/.containerenv：Podman
	SystemdContainer bool   // run/systemd/container：systemd 在容器里启动时写下
	PID1Container    string // proc/1/environ 的 container= 值；读不到或没有为空
}

// Read 从 root 读全部标识，路径相对 root。读不到的标识按不存在：存在性只看 Stat 成功，
// PID 1 的环境读不出（非 root 读别人的 environ 是 EACCES）时 PID1Container 为空。
func Read(root fs.FS) Markers {
	var m Markers
	m.DockerEnv = exists(root, ".dockerenv")
	m.ContainerEnv = exists(root, "run/.containerenv")
	m.SystemdContainer = exists(root, "run/systemd/container")
	if b, err := fs.ReadFile(root, "proc/1/environ"); err == nil {
		for _, kv := range strings.Split(string(b), "\x00") {
			if v, ok := strings.CutPrefix(kv, "container="); ok && v != "" {
				m.PID1Container = v
				break
			}
		}
	}
	return m
}

func exists(root fs.FS, name string) bool {
	_, err := fs.Stat(root, name)
	return err == nil
}

// ImageDeployment 报告读者是 Docker / Podman 镜像里的进程：只认两者各自的标识文件。
func (m Markers) ImageDeployment() bool { return m.DockerEnv || m.ContainerEnv }

// ContainerEvidence 报告读者有任一容器证据。
func (m Markers) ContainerEvidence() bool {
	return m.DockerEnv || m.ContainerEnv || m.SystemdContainer || m.PID1Container != ""
}
