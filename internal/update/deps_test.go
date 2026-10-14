package update

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

const sigtestPkg = "github.com/xjetry/heron/internal/releasesig/sigtest"

func goList(t *testing.T, args ...string) []string {
	t.Helper()
	cmd := exec.Command("go", append([]string{"list"}, args...)...)
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list %v: %v\n%s", args, err, out)
	}
	return strings.Fields(string(out))
}

// 测试公钥只经构造参数进入测试程序；正式二进制链接 sigtest 就等于多了一把受信钥匙的来源。
// 测试依赖里必须看得见它，否则这条检查对"列表为空"与"确实没有"分不出来。
func TestProductionBinariesDoNotLinkSigtest(t *testing.T) {
	if !slices.Contains(goList(t, "-deps", "-test", "github.com/xjetry/heron/internal/update"), sigtestPkg) {
		t.Fatal("update tests do not depend on sigtest; the production check below would pass vacuously")
	}
	for _, pkg := range []string{"github.com/xjetry/heron/cmd/updater", "github.com/xjetry/heron/cmd/agent", "github.com/xjetry/heron/cmd/hub"} {
		if slices.Contains(goList(t, "-deps", pkg), sigtestPkg) {
			t.Fatalf("%s links %s", pkg, sigtestPkg)
		}
	}
}
