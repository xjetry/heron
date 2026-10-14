package deployenv

import (
	"testing"
	"testing/fstest"
)

// 每个标识单独存在时两个判定的取值：镜像部署只认 Docker / Podman 的标识，容器证据认全部四个。
// LXC guest 里的 systemd 安装（run/systemd/container 与 container=lxc 同在）是容器证据，不是镜像部署。
func TestMarkersAndPredicates(t *testing.T) {
	for name, tc := range map[string]struct {
		fsys             fstest.MapFS
		want             Markers
		image, container bool
	}{
		"none":                  {fstest.MapFS{}, Markers{}, false, false},
		"docker":                {fstest.MapFS{".dockerenv": {}}, Markers{DockerEnv: true}, true, true},
		"podman":                {fstest.MapFS{"run/.containerenv": {}}, Markers{ContainerEnv: true}, true, true},
		"systemd in container":  {fstest.MapFS{"run/systemd/container": {Data: []byte("lxc\n")}}, Markers{SystemdContainer: true}, false, true},
		"pid1 container=":       {fstest.MapFS{"proc/1/environ": {Data: []byte("PATH=/bin\x00container=lxc\x00")}}, Markers{PID1Container: "lxc"}, false, true},
		"pid1 empty container=": {fstest.MapFS{"proc/1/environ": {Data: []byte("container=\x00HOME=/\x00")}}, Markers{}, false, false},
		"lxc systemd install": {fstest.MapFS{
			"run/systemd/container": {Data: []byte("lxc\n")},
			"proc/1/environ":        {Data: []byte("container=lxc\x00")},
		}, Markers{SystemdContainer: true, PID1Container: "lxc"}, false, true},
	} {
		m := Read(tc.fsys)
		if m != tc.want {
			t.Errorf("%s: Read = %+v, want %+v", name, m, tc.want)
		}
		if m.ImageDeployment() != tc.image || m.ContainerEvidence() != tc.container {
			t.Errorf("%s: ImageDeployment=%v ContainerEvidence=%v, want %v %v", name, m.ImageDeployment(), m.ContainerEvidence(), tc.image, tc.container)
		}
	}
}
