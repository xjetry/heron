package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 更新器只替身维护握手；实际 socket 身份检查与 systemd 启停由隔离机验收覆盖。
const fakeUpdater = `#!/bin/sh
cat > /dev/null
echo "heron-updater $*" >> "$STUB_STATE/calls"
if [ -n "${STUB_UPDATER_BUSY-}" ]; then echo 'update transaction in progress' >&2; exit 1; fi
`

func (e *env) updaterPackage(dir, arch, version string) {
	e.t.Helper()
	files := []packFile{{"heron-updater", fakeUpdater + "# " + version + " " + arch + "\n", 0o755}}
	for _, role := range []string{"agent", "hub"} {
		name := "heron-updater-" + role + ".service"
		unit, err := os.ReadFile(filepath.Join("systemd", name))
		if err != nil {
			e.t.Fatal(err)
		}
		files = append(files, packFile{name, string(unit), 0o644})
	}
	e.pack(dir, "heron-updater_linux_"+arch+".tar.gz", files)
}

func TestUpdaterInstallationAndMaintenance(t *testing.T) {
	for _, role := range []string{"agent", "hub"} {
		t.Run(role, func(t *testing.T) {
			var e *env
			var install func() (string, int)
			if role == "hub" {
				e = newHubInstalled(t)
				install = func() (string, int) { return e.hubInstall() }
			} else {
				e = newLinuxInstalled(t)
				e.linuxRelease("amd64", "v2")
				install = e.linuxInstall
			}
			binary := "usr/local/bin/heron-updater-" + role
			if !strings.Contains(e.file(binary), "# v1 amd64") {
				t.Fatal("first install did not install the updater")
			}
			e.resetCalls()
			out, code := install()
			if code != 0 {
				t.Fatalf("upgrade exit %d: %s", code, out)
			}
			calls := e.calls()
			maint := index(calls, "heron-updater --role "+role+" --maintenance")
			stopUpdater := index(calls, "systemctl stop heron-updater-"+role)
			stopMain := index(calls, "systemctl stop heron-"+role)
			if maint < 0 || stopUpdater <= maint || stopMain <= stopUpdater {
				t.Fatalf("maintenance must precede updater and main stop: %q", calls)
			}
			if !strings.Contains(e.file(binary), "# v2 amd64") {
				t.Fatal("upgrade did not replace updater")
			}
		})
	}
}

func TestUpdaterBusyRefusesInstallAndUninstall(t *testing.T) {
	for _, role := range []string{"agent", "hub"} {
		for _, uninstall := range []bool{false, true} {
			t.Run(role+"/uninstall="+map[bool]string{true: "yes", false: "no"}[uninstall], func(t *testing.T) {
				var e *env
				var install func() (string, int)
				if role == "hub" {
					e = newHubInstalled(t)
					install = func() (string, int) { return e.hubInstall() }
				} else {
					e = newLinuxInstalled(t)
					install = e.linuxInstall
				}
				e.vars = append(e.vars, "STUB_UPDATER_BUSY=1")
				e.resetCalls()
				var out string
				var code int
				if uninstall {
					args := []string{"--uninstall", "--purge"}
					if role == "hub" {
						args = append(args, "--yes")
					}
					out, code = e.run(args...)
				} else {
					out, code = install()
				}
				if code == 0 || !strings.Contains(out, "update transaction in progress") || index(e.calls(), "systemctl stop") >= 0 {
					t.Fatalf("busy transaction must refuse without stopping: exit %d, calls %q: %s", code, e.calls(), out)
				}
				if !e.exists("usr/local/bin/heron-updater-"+role) || !e.exists("proc/4242/status") {
					t.Fatal("busy refusal removed updater or stopped main service")
				}
			})
		}
	}
}

func TestUpdaterTamperedDownloadKeepsRunningService(t *testing.T) {
	for _, role := range []string{"agent", "hub"} {
		t.Run(role, func(t *testing.T) {
			var e *env
			var install func() (string, int)
			if role == "hub" {
				e = newHubInstalled(t)
				install = func() (string, int) { return e.hubInstall() }
			} else {
				e = newLinuxInstalled(t)
				install = e.linuxInstall
			}
			e.updaterPackage(e.dist, "amd64", "tampered")
			e.resetCalls()
			out, code := install()
			if code == 0 || !strings.Contains(out, "checksum mismatch for heron-updater_linux_amd64.tar.gz") || index(e.calls(), "systemctl stop") >= 0 {
				t.Fatalf("damaged updater must fail before stop: exit %d, calls %q: %s", code, e.calls(), out)
			}
		})
	}
}

func TestUpdaterInactiveRecoveryStateRefusesMutation(t *testing.T) {
	for _, role := range []string{"agent", "hub"} {
		for _, file := range []string{"state.json", "pending"} {
			t.Run(role+"/"+file, func(t *testing.T) {
				var e *env
				var install func() (string, int)
				if role == "hub" {
					e = newHubInstalled(t)
					install = func() (string, int) { return e.hubInstall() }
				} else {
					e = newLinuxInstalled(t)
					install = e.linuxInstall
				}
				e.put("var/lib/heron-update-"+role+"/"+file, "recovery")
				e.vars = []string{"STUB_UPDATER_STATE=inactive"}
				e.resetCalls()
				out, code := install()
				if code == 0 || !strings.Contains(out, "updater recovery state exists") || index(e.calls(), "systemctl stop") >= 0 {
					t.Fatalf("inactive recovery must refuse: exit %d, calls %q: %s", code, e.calls(), out)
				}
			})
		}
	}
}

func TestUpdaterUninstallRemovesItsStateButKeepsBusinessData(t *testing.T) {
	for _, role := range []string{"agent", "hub"} {
		t.Run(role, func(t *testing.T) {
			var e *env
			args := []string{"--uninstall"}
			business := "etc/heron-agent/config.json"
			if role == "hub" {
				e = newHubInstalled(t)
				args = append(args, "--yes")
				business = "var/lib/heron/heron.db"
			} else {
				e = newLinuxInstalled(t)
			}
			e.put("var/lib/heron-update-"+role+"/state.json", "completed")
			e.put("run/heron-update-"+role+"/updater.sock", "")
			out, code := e.run(args...)
			if code != 0 {
				t.Fatalf("uninstall exit %d: %s", code, out)
			}
			for _, file := range []string{"usr/local/bin/heron-updater-" + role, "etc/systemd/system/heron-updater-" + role + ".service", "var/lib/heron-update-" + role, "run/heron-update-" + role} {
				if e.exists(file) {
					t.Errorf("updater artifact left: %s", file)
				}
			}
			if !e.exists(business) {
				t.Fatal("non-purge uninstall removed business data")
			}
		})
	}
}

func TestUpdaterRestartsAfterMainStopFailure(t *testing.T) {
	e := newHubInstalled(t)
	e.vars = []string{"STUB_STOP_FAILS=1"}
	e.resetCalls()
	out, code := e.hubInstall()
	calls := e.calls()
	if code == 0 || !strings.Contains(out, "failed to stop heron-hub") || index(calls, "systemctl restart heron-updater-hub") <= index(calls, "systemctl stop heron-updater-hub") {
		t.Fatalf("failed install must restore updater: exit %d, calls %q: %s", code, calls, out)
	}
}

func TestUpdaterConcurrentInstallerRefusesBeforeMutation(t *testing.T) {
	for _, role := range []string{"agent", "hub"} {
		t.Run(role, func(t *testing.T) {
			var e *env
			var install func() (string, int)
			if role == "hub" {
				e = newHubInstalled(t)
				install = func() (string, int) { return e.hubInstall() }
			} else {
				e = newLinuxInstalled(t)
				install = e.linuxInstall
			}
			e.vars = []string{"STUB_INSTALLER_BUSY=1"}
			e.resetCalls()
			out, code := install()
			if code == 0 || !strings.Contains(out, "another heron-"+role+" installer is running") || strings.Join(e.calls(), "") != "" {
				t.Fatalf("concurrent installer must refuse before downloading or mutating: exit %d, calls %q: %s", code, e.calls(), out)
			}
		})
	}
}
