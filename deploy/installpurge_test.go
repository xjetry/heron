package deploy

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSystemdUninstallDropInScope(t *testing.T) {
	for _, service := range []string{"heron-agent", "heron-hub"} {
		for _, purge := range []bool{false, true} {
			for _, unitMissing := range []bool{false, true} {
				for _, linked := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/purge=%t/missing=%t/linked=%t", service, purge, unitMissing, linked), func(t *testing.T) {
						t.Parallel()
						e := newLinuxHost(t)
						args := []string{"--uninstall"}
						if service == "heron-hub" {
							e.script = "install-hub.sh"
							args = append(args, "--yes")
						}
						if purge {
							args = append(args, "--purge")
						}
						if !unitMissing {
							e.put("etc/systemd/system/"+service+".service", "[Service]\n")
						}
						preserved := []string{
							"var/log/journal/machine/system.journal",
							"run/log/journal/machine/system.journal",
							"etc/systemd/system/service.d/shared.conf",
							"etc/systemd/system/heron-.service.d/shared.conf",
							"etc/systemd/system/other.service.d/override.conf",
							"usr/lib/systemd/system/" + service + ".service.d/vendor.conf",
							"srv/custom/override.conf",
						}
						for _, path := range preserved {
							e.put(path, path)
						}
						var dirs []string
						for _, base := range []string{"etc", "run"} {
							dir := base + "/systemd/system/" + service + ".service.d"
							dirs = append(dirs, dir)
							if linked {
								if err := os.Symlink(filepath.Join(e.root, "srv/custom"), filepath.Join(e.root, dir)); err != nil {
									t.Fatal(err)
								}
							} else {
								e.put(dir+"/override.conf", "[Service]\nEnvironment=EXAMPLE=1\n")
							}
						}
						out, code := e.run(args...)
						if code != 0 || !strings.Contains(out, service+" uninstalled") {
							t.Fatalf("exit %d:\n%s", code, out)
						}
						for _, dir := range dirs {
							_, err := os.Lstat(filepath.Join(e.root, dir))
							if purge && !os.IsNotExist(err) {
								t.Errorf("purge retained drop-in %s: %v", dir, err)
							} else if !purge && err != nil {
								t.Errorf("uninstall removed drop-in %s: %v", dir, err)
							}
						}
						for _, path := range preserved {
							got, err := os.ReadFile(filepath.Join(e.root, path))
							if err != nil || string(got) != path {
								t.Errorf("uninstall changed unrelated content %s: %q, %v", path, got, err)
							}
						}
					})
				}
			}
		}
	}
}
