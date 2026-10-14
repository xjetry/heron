package geo

import (
	"bytes"
	"crypto/sha256"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/oschwald/maxminddb-golang/v2"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/testwait"
)

// fixtureCopy 把提交的夹具复制到临时目录：用例改写的是副本，不动夹具本身。
func fixtureCopy(t *testing.T) (path string, data []byte) {
	t.Helper()
	data, err := os.ReadFile("testdata/country.mmdb")
	if err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(t.TempDir(), "country.mmdb")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, data
}

func mustOpenMMDB(t *testing.T, path string) *MMDB {
	t.Helper()
	db, err := OpenMMDB(path)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func mmdbCountry(t *testing.T, db *MMDB, addr string) string {
	t.Helper()
	country, err := db.Lookup(t.Context(), store.GeoSettings{}, netip.MustParseAddr(addr))
	if err != nil {
		t.Fatal(err)
	}
	return country
}

func fileSum(t *testing.T, path string) [sha256.Size]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(data)
}

// 打开之后把文件原地改写成另一份同尺寸的库（cp 覆盖即如此：截断后写回同一个 inode），运行中的答案不变。改写把
// 8.8.8.8 在搜索树里的叶子改指 AU 那条记录，改写后的文件另行打开确实答 AU，所以答案不变只能是因为打开的库不再读
// 文件。改的是树叶而不是国家码字符串：读取器按偏移缓存解码过的字符串，只改字符串时缓存命中会遮住"还在读文件"；
// 树叶每次查询都要读。
func TestMMDBIgnoresAnInPlaceRewrite(t *testing.T) {
	path, data := fixtureCopy(t)
	plain, err := maxminddb.OpenBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	if plain.Metadata.RecordSize != 24 {
		t.Fatalf("fixture: record size %d, want 24", plain.Metadata.RecordSize)
	}
	leaf := func(addr string) []byte {
		t.Helper()
		res := plain.Lookup(netip.MustParseAddr(addr))
		if !res.Found() {
			t.Fatalf("fixture: no record for %s", addr)
		}
		v := uint(res.Offset()) + plain.Metadata.NodeCount + 16
		return []byte{byte(v >> 16), byte(v >> 8), byte(v)}
	}
	us, au := leaf("8.8.8.8"), leaf("2606:4700::1111")
	var at []int
	for i := 0; i+3 <= int(plain.Metadata.NodeCount)*6; i += 3 {
		if bytes.Equal(data[i:i+3], us) {
			at = append(at, i)
		}
	}
	if len(at) != 1 {
		t.Fatalf("fixture: the US record is referenced by %d tree records, want one", len(at))
	}
	rewritten := bytes.Clone(data)
	copy(rewritten[at[0]:], au)

	db := mustOpenMMDB(t, path)
	if got := mmdbCountry(t, db, "8.8.8.8"); got != "US" {
		t.Fatalf("fixture: 8.8.8.8 = %q, want US", got)
	}
	before := fileSum(t, path)
	if err := os.WriteFile(path, rewritten, 0o600); err != nil {
		t.Fatal(err)
	}
	if fileSum(t, path) == before {
		t.Fatal("fixture: the rewrite left the file unchanged")
	}
	reread, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := maxminddb.OpenBytes(reread)
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		Country struct {
			ISOCode string `maxminddb:"iso_code"`
		} `maxminddb:"country"`
	}
	if err := fresh.Lookup(netip.MustParseAddr("8.8.8.8")).Decode(&record); err != nil || record.Country.ISOCode != "AU" {
		t.Fatalf("fixture: the rewritten file answers %q %v, want AU", record.Country.ISOCode, err)
	}
	if got := mmdbCountry(t, db, "8.8.8.8"); got != "US" {
		t.Errorf("after an in-place rewrite the open database answers %q, want the US read at startup", got)
	}
}

// 打开之后把文件截断到 0：查询不崩溃，答案不变。
func TestMMDBIgnoresTruncation(t *testing.T) {
	path, _ := fixtureCopy(t)
	db := mustOpenMMDB(t, path)
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Size() != 0 {
		t.Fatalf("fixture: truncated file = %v %v, want size 0", info, err)
	}
	if got := mmdbCountry(t, db, "8.8.8.8"); got != "US" {
		t.Errorf("after truncation the open database answers %q, want the US read at startup", got)
	}
}

// 不是普通文件的路径在打开之前按配置错误拒绝，错误写明路径与原因，而且立即返回。命名管道没有写者：打开它会一直
// 阻塞，等不到返回就说明 OpenMMDB 去打开了它。等待的上界只为阻塞时能以失败结束；超时后以写端打开一次，放走被阻塞
// 的那次打开。
func TestOpenMMDBRejectsNonRegularFiles(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "country.mmdb")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(fifo); err != nil || info.Mode().Type() != os.ModeNamedPipe {
		t.Fatalf("fixture: %s = %v %v, want a named pipe", fifo, info, err)
	}
	for _, c := range []struct{ name, path string }{
		{"directory", t.TempDir()},
		{"named pipe", fifo},
		{"device", "/dev/zero"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := os.Stat(c.path); err != nil {
				t.Skipf("no %s here: %v", c.path, err)
			}
			done := make(chan error, 1)
			go func() {
				_, err := OpenMMDB(c.path)
				done <- err
			}()
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), c.path) || !strings.Contains(err.Error(), "not a regular file") {
					t.Errorf("%s error = %v, want path %q and \"not a regular file\"", c.name, err, c.path)
				}
			case <-time.After(testwait.Bound):
				t.Errorf("OpenMMDB(%s) still blocked after %v, want it rejected before opening", c.path, testwait.Bound)
				if c.path == fifo {
					if w, err := os.OpenFile(fifo, os.O_WRONLY, 0); err == nil {
						w.Close()
					}
				}
			}
		})
	}
}

// 超过上限的文件按配置错误拒绝，错误写明路径与原因。上限写字面值 256 MiB，不引用常量：常量改了而 spec 没改，这里要红。
// 超限文件是稀疏文件，"在读之前拒绝"由分配量证明：读进内存就至少分配 256 MiB。
func TestOpenMMDBRejectsOversizedFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "huge.mmdb")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, 256<<20+1); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Size() != 256<<20+1 {
		t.Fatalf("fixture: oversized file = %v %v, want 256 MiB + 1 byte", info, err)
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err = OpenMMDB(path)
	runtime.ReadMemStats(&after)
	if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "256 MiB") {
		t.Errorf("oversized error = %v, want path %q and the 256 MiB limit", err, path)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 16<<20 {
		t.Errorf("rejecting the oversized file allocated %d bytes, want it rejected before reading", alloc)
	}
}

// 空路径按配置错误拒绝，且拒绝来自显式检查而不是打开失败：api 以 MMDBPath 是否为空区分两个后端，空路径的本地库
// 会被面板回显成 HTTP 服务。
func TestOpenMMDBRejectsAnEmptyPath(t *testing.T) {
	if _, err := OpenMMDB(""); err == nil || !strings.Contains(err.Error(), `--geo-mmdb "": empty path`) {
		t.Errorf("empty path error = %v, want --geo-mmdb \"\": empty path", err)
	}
}

// zeros 是一段 n 字节的全零输入。
type zeros struct{ n int64 }

func (z *zeros) Read(p []byte) (int, error) {
	if z.n <= 0 {
		return 0, io.EOF
	}
	k := min(int64(len(p)), z.n)
	clear(p[:k])
	z.n -= k
	return int(k), nil
}

// 读取本身按上限截断：Stat 之后变大的文件过了读之前的大小检查，读到的比 Stat 报的多。这里用一段 64 MiB 的输入与
// 1 MiB 的上限代替它，断言报出上限，且读进内存的不超过上限太多；读取不截断时会把 64 MiB 全读进来。
func TestReadAtMostCapsASourceLongerThanTheLimit(t *testing.T) {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := readAtMost(&zeros{n: 64 << 20}, 1<<20)
	runtime.ReadMemStats(&after)
	if err == nil || !strings.Contains(err.Error(), "1 MiB limit") {
		t.Errorf("reading 64 MiB with a 1 MiB limit = %v, want the 1 MiB limit", err)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 16<<20 {
		t.Errorf("reading with a 1 MiB limit allocated %d bytes, want at most about the limit", alloc)
	}
}

// 结构损坏的库：翻转数据段中点的 4 个字节，那里的数据无法按类型解码。不做校验的读取器照样能打开它（所以拒绝来自
// Verify，不是打开时的格式检查），OpenMMDB 在启动时就拒绝，不让它带着坏记录通过启动。
func TestOpenMMDBRejectsACorruptDataSection(t *testing.T) {
	path, data := fixtureCopy(t)
	plain, err := maxminddb.OpenBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	start := int(plain.Metadata.NodeCount*plain.Metadata.RecordSize/4) + 16
	end := bytes.LastIndex(data, []byte("\xab\xcd\xefMaxMind.com"))
	if start <= 0 || end <= start+8 {
		t.Fatalf("fixture: data section [%d, %d) too small to corrupt", start, end)
	}
	corrupt := bytes.Clone(data)
	for i := (start + end) / 2; i < (start+end)/2+4; i++ {
		corrupt[i] ^= 0xff
	}
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(corrupt) == sha256.Sum256(data) || fileSum(t, path) != sha256.Sum256(corrupt) {
		t.Fatal("fixture: the corrupted copy was not written")
	}
	if _, err := maxminddb.OpenBytes(corrupt); err != nil {
		t.Fatalf("fixture: the corrupted copy no longer opens without verification: %v", err)
	}
	if _, err := OpenMMDB(path); err == nil || !strings.Contains(err.Error(), path) {
		t.Errorf("corrupt data section error = %v, want a startup error naming %q", err, path)
	}
}
