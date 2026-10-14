package deploy

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
)

// stubRoot 存放各替身表的共享目录，TestMain 在全部用例结束后删除。
var stubRoot string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "heron-deploy-stubs-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	stubRoot = dir
	code := m.Run()
	// stubBin 把各替身目录设为只读，删除其中的文件要先恢复目录的写权限。
	if err := errors.Join(restoreWritable(dir), os.RemoveAll(dir)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

var stubBins struct {
	mu   sync.Mutex
	dirs map[string]string
}

// stubBin 返回写有 stubs 的目录：同一张替身表在整个测试进程里只写一次，各用例把它放在 PATH 最前面共用。
//
// 共用成立的前提：替身写成之后只读，用例之间的差别全部经环境变量（STUB_STATE 与各 STUB_* 开关）传入，没有用例
// 改写或增删自己的替身；要按用例换替身，就得另建一张表，它按内容得到自己的目录。目录写完即设为只读，往里写的用例
// 当场失败，不会悄悄改掉别的用例正在用的替身。
//
// 共用的理由：macOS 上每个新可执行文件第一次执行都要多等一段，同一个文件再次执行不再等，内容相同的新文件照样要等；
// 等的时候不占本进程的 CPU。每个用例各写一份替身，就把这段等待乘上用例数与每例用到的替身数。安装出的假二进制
// 按用例新建，是被测行为本身，不在此列。
func stubBin(t *testing.T, stubs map[string]string) string {
	t.Helper()
	h := sha256.New()
	for _, name := range slices.Sorted(maps.Keys(stubs)) {
		fmt.Fprintf(h, "%d:%s%d:%s", len(name), name, len(stubs[name]), stubs[name])
	}
	key := hex.EncodeToString(h.Sum(nil))
	stubBins.mu.Lock()
	defer stubBins.mu.Unlock()
	if dir, ok := stubBins.dirs[key]; ok {
		return dir
	}
	dir := filepath.Join(stubRoot, key[:16])
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	if stubBins.dirs == nil {
		stubBins.dirs = map[string]string{}
	}
	stubBins.dirs[key] = dir
	return dir
}

func restoreWritable(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		errs = append(errs, os.Chmod(filepath.Join(root, e.Name()), 0o755))
	}
	return errors.Join(errs...)
}
