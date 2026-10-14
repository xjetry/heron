package geo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"strings"

	"github.com/oschwald/maxminddb-golang/v2"
	"github.com/xjetry/heron/internal/hub/store"
)

// Backend 只负责一次查询；准入、校验、退避与条件写入由 Resolver 承载。
type Backend interface {
	// Lookup 用 Resolver 判定准入时读到的那份设置查询：HTTP 用 s.URL 组目标，mmdb 忽略 s。
	Lookup(ctx context.Context, s store.GeoSettings, addr netip.Addr) (string, error)
	// Service 给出退避键里"服务"一项：HTTP 为 s.URL（换服务地址即换键），mmdb 为库路径（改 geo.url 不影响 mmdb 的退避）。
	Service(s store.GeoSettings) string
	// MMDBPath 是面板回显的本地库路径，HTTP 后端为空串。api 以它是否为空区分两个后端，MMDB 的路径非空由 OpenMMDB
	// 拒绝空路径保证。
	MMDBPath() string
}

// maxResponseBytes 是读取应答体的上限。合法应答是两个字母加少量空白，超出即判失败，不再往下读。
const maxResponseBytes = 64

// HTTP 把地址发给设置里的服务地址。它不持有 store：发往哪里只由调用方传入的那份设置决定，而 Resolver 传入的正是它
// 判定开关时读到的那一份，所以同一次外呼的开关与服务地址出自同一次读取（见 Resolver.Sweep 的不变式）。
type HTTP struct {
	client *http.Client
}

// NewHTTP 的 client 应当是通知渠道用的那一个（serve 里的 outbound.NewClient(alert.NotifyTimeout)：不跟随重定向、带总时限），
// 查询与通知的出站行为是同一套。
func NewHTTP(client *http.Client) *HTTP {
	return &HTTP{client: client}
}

// Lookup 发出一次查询。请求只带地址：GET、无请求体，不设任何凭据头；服务地址不含用户信息由 api 的 UpdateSettings
// 保证。只认 200：客户端不跟随重定向，3xx 在这里按失败处理。
func (h *HTTP) Lookup(ctx context.Context, s store.GeoSettings, addr netip.Addr) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, Target(s.URL, addr), nil)
	if err != nil {
		return "", err
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxResponseBytes {
		return "", fmt.Errorf("response longer than %d bytes", maxResponseBytes)
	}
	return strings.TrimSpace(string(data)), nil
}

// Service 是服务地址：运维换了服务，旧服务留下的退避不挡住对新服务的查询。
func (h *HTTP) Service(s store.GeoSettings) string { return s.URL }

func (h *HTTP) MMDBPath() string { return "" }

// MaxMMDBBytes 是本地国家库文件的大小上限。整个文件在启动时读进内存，上限让误指向的超大文件按配置错误启动失败，
// 而不是先占用等量的内存。
const MaxMMDBBytes = 256 << 20

// MMDB 在本机查国家，不出网。OpenMMDB 把库文件整读进内存，读取器只持有这份私有副本，运行期不再访问文件：原地覆盖
// 或截断文件都不改变运行中的答案，替换文件要重启才生效。它不持有文件句柄或映射，没有要归还给系统的资源，所以没有
// Close：关停时不需要与在途查询排先后，内存随对象回收。
type MMDB struct {
	path string
	db   *maxminddb.Reader
}

// OpenMMDB 把 path 处的 MaxMind 库整读进内存并做结构校验。下列情形在启动时报错：
//   - 路径为空，或不是普通文件（目录、命名管道、设备文件）：打开之前拒绝，见 readLimited；
//   - 大小超过 MaxMMDBBytes：读之前拒绝；
//   - 不是 MaxMind DB 格式：OpenBytes 拒绝，例如找不到元数据标记；
//   - 结构损坏：Verify 遍历元数据、搜索树与数据段，例如元数据缺 description、搜索树指针越界、数据段分隔符不为零、
//     数据无法按类型解码（含非法 UTF-8）。
//
// 库文件不带校验和，内容层面的改写查不出：把一个合法值换成另一个合法值的库（例如把某条记录的 US 改成 UT）照常打开，
// 查询答出改写后的值。这样的答案只要是两个大写字母，也能通过 Resolver 的 store.IsCountryCode；它是不是正确的国家，
// 只能由运维核对库的来源与版本（启动行记有库路径、数据库类型与构建时间）。
func OpenMMDB(path string) (*MMDB, error) {
	fail := func(err error) (*MMDB, error) { return nil, fmt.Errorf("--geo-mmdb %q: %w", path, err) }
	// 空路径在 MMDBPath 里与 HTTP 后端无法区分，面板会把本地库回显成 HTTP 服务；它也不是一个可打开的文件，按配置错误拒绝。
	if path == "" {
		return fail(errors.New("empty path"))
	}
	data, err := readLimited(path, MaxMMDBBytes)
	if err != nil {
		return fail(err)
	}
	db, err := maxminddb.OpenBytes(data)
	if err != nil {
		return fail(err)
	}
	if err := db.Verify(); err != nil {
		return fail(err)
	}
	return &MMDB{path: path, db: db}, nil
}

// readLimited 读入 path 处的普通文件，至多 limit 字节。打开之前按 os.Stat 拒绝两类：不是普通文件的（目录、命名管道、
// 设备文件）——打开一个没有写者的命名管道会一直阻塞，hub 会在打开数据库与监听之前无日志地挂住；Stat 报出的大小超过
// limit 的。读取另由 readAtMost 按 limit 截断，挡住 Stat 之后变大的文件。
func readLimited(path string, limit int64) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file (mode %v)", info.Mode())
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("%d bytes exceeds the %d MiB limit", info.Size(), limit>>20)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readAtMost(f, limit)
}

// readAtMost 读出 r 的全部内容，多于 limit 字节即报错；读进内存的至多 limit+1 字节（多读的一个字节用来判定超限）。
func readAtMost(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("more than the %d MiB limit", limit>>20)
	}
	return data, nil
}

// errNoCountryRecord 是本地库对地址没有国家的答案：库里没有这个地址的记录，或记录里没有 country.iso_code。
var errNoCountryRecord = errors.New("no country record for address")

// Lookup 只读 GeoLite2-Country 的 country.iso_code，不以 registered_country 等字段替代，不沿用其它含义的国家。
// 查不到或缺字段返回 errNoCountryRecord，由 Resolver 按失败退避；查得的码是否合法由 Resolver 的 store.IsCountryCode
// 判定，与 HTTP 后端同一处。
func (m *MMDB) Lookup(ctx context.Context, _ store.GeoSettings, addr netip.Addr) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var record struct {
		Country struct {
			ISOCode string `maxminddb:"iso_code"`
		} `maxminddb:"country"`
	}
	if err := m.db.Lookup(addr).Decode(&record); err != nil {
		return "", err
	}
	if record.Country.ISOCode == "" {
		return "", errNoCountryRecord
	}
	return record.Country.ISOCode, nil
}

// Service 是库路径，不取 geo.url：mmdb 下 geo.url 不生效，改它不应清掉查不到的地址的退避。
func (m *MMDB) Service(store.GeoSettings) string { return m.path }

func (m *MMDB) MMDBPath() string { return m.path }

// Metadata 是库文件自带的元数据。serve 把其中的数据库类型与构建时间写进启动行，运维据此确认加载的是哪一版库。
func (m *MMDB) Metadata() maxminddb.Metadata { return m.db.Metadata }
