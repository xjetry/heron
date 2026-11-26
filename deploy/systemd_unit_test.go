package deploy

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// parseServiceDirectives 读取 systemd 单元 [Service] 段的键值对，允许同一键多次出现（如多行 SystemCallFilter=）。
func parseServiceDirectives(t *testing.T, unitPath string) map[string][]string {
	t.Helper()
	b, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("read %s: %v", unitPath, err)
	}
	inService := false
	got := map[string][]string{}
	for raw := range strings.SplitSeq(string(b), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			inService = line == "[Service]"
			continue
		}
		if !inService {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		got[k] = append(got[k], v)
	}
	return got
}

func requireSingleValue(t *testing.T, unit string, d map[string][]string, key, want string) {
	t.Helper()
	vals := d[key]
	if len(vals) != 1 || vals[0] != want {
		t.Errorf("%s: %s = %q, want [%q]", unit, key, vals, want)
	}
}

func requireAbsent(t *testing.T, unit string, d map[string][]string, key string) {
	t.Helper()
	if vals, ok := d[key]; ok {
		t.Errorf("%s: must not set %s (got %q)", unit, key, vals)
	}
}

func requireAddressFamilies(t *testing.T, unit string, d map[string][]string, want []string) {
	t.Helper()
	vals := d["RestrictAddressFamilies"]
	if len(vals) != 1 {
		t.Errorf("%s: RestrictAddressFamilies = %q, want one line", unit, vals)
		return
	}
	got := strings.Fields(vals[0])
	slices.Sort(got)
	wantSorted := slices.Clone(want)
	slices.Sort(wantSorted)
	if !slices.Equal(got, wantSorted) {
		t.Errorf("%s: RestrictAddressFamilies = %v, want %v", unit, got, wantSorted)
	}
}

// 四个 systemd 单元共享的最小沙箱基线，与角色专属的隔离边界一起静态核对：
//   - agent 必须保留原 /proc 挂载实例并可见 PID 1 与 /proc 全局统计（不得设 ProtectProc、ProcSubset，也不得设在 systemd 252
//     上会重挂新 /proc 实例并覆盖子挂载的 ProtectKernelTunables、ProtectControlGroups），且需 AF_NETLINK 枚举本机网卡与地址；
//   - hub 经 unix socket 连接本机更新器（必须含 AF_UNIX），且隐藏其余进程与 /proc 非进程项；
//   - 更新器需遍历 /proc/<pid> 确认退出与校验二进制（不得设 ProtectProc/ProcSubset），且需 setuid/chown（不得排除 ~@privileged）。
func TestSystemdUnitHardeningInvariants(t *testing.T) {
	t.Parallel()
	common := map[string]string{
		"NoNewPrivileges":         "yes",
		"ProtectSystem":           "strict",
		"ProtectHome":             "yes",
		"PrivateTmp":              "yes",
		"PrivateDevices":          "yes",
		"ProtectKernelModules":    "yes",
		"ProtectKernelLogs":       "yes",
		"ProtectClock":            "yes",
		"ProtectHostname":         "yes",
		"RestrictNamespaces":      "yes",
		"RestrictRealtime":        "yes",
		"RestrictSUIDSGID":        "yes",
		"LockPersonality":         "yes",
		"MemoryDenyWriteExecute":  "yes",
		"SystemCallArchitectures": "native",
		"KeyringMode":             "private",
	}

	units := []string{
		"heron-hub.service",
		"heron-agent.service",
		"heron-updater-hub.service",
		"heron-updater-agent.service",
	}
	for _, name := range units {
		d := parseServiceDirectives(t, filepath.Join("systemd", name))
		for k, want := range common {
			requireSingleValue(t, name, d, k, want)
		}
		if !slices.Contains(d["SystemCallFilter"], "@system-service") {
			t.Errorf("%s: SystemCallFilter = %q, must include @system-service", name, d["SystemCallFilter"])
		}
	}

	hub := parseServiceDirectives(t, filepath.Join("systemd", "heron-hub.service"))
	requireSingleValue(t, "heron-hub.service", hub, "ProtectKernelTunables", "yes")
	requireSingleValue(t, "heron-hub.service", hub, "ProtectControlGroups", "yes")
	requireSingleValue(t, "heron-hub.service", hub, "ProtectProc", "invisible")
	requireSingleValue(t, "heron-hub.service", hub, "ProcSubset", "pid")
	requireAddressFamilies(t, "heron-hub.service", hub, []string{"AF_UNIX", "AF_INET", "AF_INET6"})
	if !slices.Contains(hub["SystemCallFilter"], "~@privileged") {
		t.Errorf("heron-hub.service: SystemCallFilter = %q, must include ~@privileged", hub["SystemCallFilter"])
	}

	agent := parseServiceDirectives(t, filepath.Join("systemd", "heron-agent.service"))
	requireAbsent(t, "heron-agent.service", agent, "ProtectKernelTunables")
	requireAbsent(t, "heron-agent.service", agent, "ProtectControlGroups")
	requireAbsent(t, "heron-agent.service", agent, "ProtectProc")
	requireAbsent(t, "heron-agent.service", agent, "ProcSubset")
	requireAddressFamilies(t, "heron-agent.service", agent, []string{"AF_UNIX", "AF_INET", "AF_INET6", "AF_NETLINK"})
	if !slices.Contains(agent["SystemCallFilter"], "~@privileged") {
		t.Errorf("heron-agent.service: SystemCallFilter = %q, must include ~@privileged", agent["SystemCallFilter"])
	}

	for _, name := range []string{"heron-updater-hub.service", "heron-updater-agent.service"} {
		upd := parseServiceDirectives(t, filepath.Join("systemd", name))
		requireSingleValue(t, name, upd, "ProtectKernelTunables", "yes")
		requireSingleValue(t, name, upd, "ProtectControlGroups", "yes")
		requireAbsent(t, name, upd, "ProtectProc")
		requireAbsent(t, name, upd, "ProcSubset")
		requireAddressFamilies(t, name, upd, []string{"AF_UNIX", "AF_INET", "AF_INET6"})
		if slices.Contains(upd["SystemCallFilter"], "~@privileged") {
			t.Errorf("%s: SystemCallFilter must not exclude ~@privileged (needs setuid/setgid/chown)", name)
		}
	}
}

// scripts/update-credentials-accept.sh 用 zzzz- drop-in 重申会被容器全局覆盖（zzz-lxc-service.conf）重置的指令；
// 新增了同组指令却漏同步到脚本时，隔离机降权验收会在未真正启用新加固的环境下静默通过。
func TestUpdateCredentialsAcceptReappliesHardeningDirectives(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile(filepath.Join("..", "scripts", "update-credentials-accept.sh"))
	if err != nil {
		t.Fatalf("read update-credentials-accept.sh: %v", err)
	}
	script := string(b)
	for _, key := range []string{
		"NoNewPrivileges",
		"ProtectSystem",
		"ProtectHome",
		"PrivateTmp",
		"PrivateDevices",
		"ProtectKernelTunables",
		"ProtectKernelModules",
		"ProtectKernelLogs",
		"ProtectControlGroups",
		"ReadWritePaths",
	} {
		if !strings.Contains(script, "/^"+key+"=/p") {
			t.Errorf("scripts/update-credentials-accept.sh missing /%s=/p in zzzz-credentials.conf extraction", key)
		}
	}
}
