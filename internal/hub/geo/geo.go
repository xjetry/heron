// Package geo 按公网来源地址查询国家 / 地区，把答案与所查地址成对写回节点。
package geo

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"strings"
	"time"

	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/store"
	"github.com/xjetry/heron/internal/netaddr"
)

const (
	// SweepEvery 是查询器巡检节点的间隔。来源地址随分钟行刷出落盘（WriteMinuteBatch），地址变化到重查至多再等一个
	// 间隔加上正在进行的那一轮：一轮里的查询串行发出，最坏是待查节点数乘以客户端的总超时。
	SweepEvery = 30 * time.Second
	// RetryAfter 是一次失败之后同一节点、同一地址、同一服务（见 Backend.Service）的退避：没有退避，一个坏链路（服务
	// 宕机、限流、返回错误页）或一个本地库里没有答案的地址会让每一轮巡检都对同一地址查一次。
	RetryAfter = time.Hour
	// answersPerNode 是查询器为每个节点记住答案的地址数，保留最近用到的那几个。v4 与 v6 交替上报的节点在两个地址
	// 之间来回，记住它们，来回切换就命中而不外呼；上界让出口不断变化的节点不会让表无限增长，代价是超过这个数的地址
	// 轮换时被挤出的地址会再查。对外文字写的是这个数：admin.proto 里 Settings.geo_enabled 的注释由
	// TestExternalTextsStateTheAnswerBound 对照，面板的 ANSWERS_PER_NODE（web/src/lib/country.ts）由 countryLimits.test.ts 对照。
	answersPerNode = 4
)

// Placeholder 是 geo.url 里被替换为地址的占位。
const Placeholder = "{ip}"

// Target 把地址填进服务地址的占位。地址取 netip 的文本（数字、十六进制字母、点与冒号），在路径与查询串里都无需转义。
func Target(tmpl string, addr netip.Addr) string {
	return strings.ReplaceAll(tmpl, Placeholder, addr.WithZone("").String())
}

// Resolver 是 hub 内唯一的国家查询者，由 Run 在一个协程里串行调用 Sweep。
type Resolver struct {
	store   *store.Store
	backend Backend
	clk     clock.Clock
	log     *slog.Logger
	// 下面两张表只在内存、只由 Sweep 读写（Sweep 不并发，不加锁）。hub 重启后清空，尚无答案的地址各重查一次。
	//
	// answers 是每个节点记住的答案，最近用到的地址在前，至多 answersPerNode 个。不重复外呼由两处合起来承载，各有边界：
	// 节点停在同一地址时，库里那一对（country、country_ip）让它不再待查，重启后也还在；库里只存当前地址的那一对，地址
	// 一变就清空，节点换回之前查过的地址时，只要那个地址还在这张表里就由它写回、不再外呼。超过 answersPerNode 个地址
	// 轮换时，被挤出的地址再来会再查；重启清空这张表，节点之后换到的地址各再查一次。
	answers map[int64][]answer
	// retryAt 是每个（节点, 地址, 服务）下次允许查询的单调钟时刻，只记失败。服务一项由后端给出（Backend.Service）：
	// HTTP 是服务地址，运维换了服务，旧服务留下的退避不挡住对新服务的查询；mmdb 是库路径，改不生效的 geo.url 不清掉
	// 退避。用单调钟：墙钟被拨动时退避不会提前结束或拖长（见 clock）。
	retryAt map[target]time.Duration
}

type answer struct{ addr, country string }

type target struct {
	node    int64
	addr    string
	service string
}

func New(st *store.Store, backend Backend, clk clock.Clock, log *slog.Logger) *Resolver {
	return &Resolver{store: st, backend: backend, clk: clk, log: log, answers: map[int64][]answer{}, retryAt: map[target]time.Duration{}}
}

func (r *Resolver) Run(ctx context.Context) {
	t := time.NewTicker(SweepEvery)
	defer t.Stop()
	for {
		if err := r.Sweep(ctx); err != nil && ctx.Err() == nil {
			r.log.Error("country lookup sweep failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Sweep 查一轮：对来源地址是公网、库里尚无该地址答案（last_source != country_ip）的节点，记住过这个地址的答案就
// 直接写回，没有且不在退避期就向后端查询一次。
//
// 不变式：每次外呼都由发出时刻的开关与服务地址授权。一轮开头读一次设置，此后每个待查节点处理之前再读一次
// （GeoSettings 是读池上的一条 SELECT），读到关闭即结束本轮；读到的这一份原样交给后端（Backend.Lookup 的参数），
// 退避键的服务一项也由后端按这一份给出（Backend.Service）。两个后端都不持有 store（NewHTTP 只接出站客户端，
// OpenMMDB 只接路径），不会另读设置，所以一次外呼的开关与服务地址出自同一次读取，退避键里的服务就是实际查询的那个。
// 读与发出之间只有内存里的判断（答案表、退避表）。查询在本协程里串行发出，所以关闭之后至多还有一个请求——在途的，或刚读完设置、正要
// 发出的那一个——上界是客户端的总超时。一轮最坏是待查节点数乘以总超时，只在开头读一次、整轮沿用，关闭开关或
// 换服务就要等这么久才生效。
//
// geo.enabled 表达是否给节点标国家，后端仅改变地址是否离开本机，所以两个后端共用开关、准入与退避。
func (r *Resolver) Sweep(ctx context.Context) error {
	settings, err := r.store.GeoSettings(ctx)
	if err != nil || !settings.Enabled {
		return err
	}
	nodes, err := r.store.ListMonitoringNodes(ctx)
	if err != nil {
		return err
	}
	listed := make(map[int64]bool, len(nodes))
	pending := map[target]bool{}
	for _, n := range nodes {
		listed[n.ID] = true
		if n.LastSource == "" || n.LastSource == n.CountryIP {
			continue
		}
		addr, err := netip.ParseAddr(n.LastSource)
		if err != nil || !netaddr.IsPublic(addr) {
			continue
		}
		if settings, err = r.store.GeoSettings(ctx); err != nil || !settings.Enabled {
			return err
		}
		if country, ok := r.recall(n.ID, n.LastSource); ok {
			if err := r.write(ctx, n, country); err != nil {
				return err
			}
			continue
		}
		k := target{n.ID, n.LastSource, r.backend.Service(settings)}
		pending[k] = true
		if at, ok := r.retryAt[k]; ok && r.clk.Mono() < at {
			continue
		}
		country, err := r.backend.Lookup(ctx, settings, addr)
		if err == nil && !store.IsCountryCode(country) {
			err = errNotCountry
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			r.retryAt[k] = r.clk.Mono() + RetryAfter
			r.log.Warn("country lookup failed", "node", n.ID, "addr", n.LastSource, "retry_after", RetryAfter, "err", err)
			continue
		}
		delete(r.retryAt, k)
		r.remember(n.ID, n.LastSource, country)
		if err := r.write(ctx, n, country); err != nil {
			return err
		}
	}
	// 两张表只为仍在的节点保留：节点删除后它的答案与退避在这里丢掉。退避另外只保留这一轮仍待查的（节点, 地址,
	// 服务）：节点换了地址、已有答案、HTTP 下运维换了服务地址之后的旧条目都丢掉，表不随地址或服务的变化增长。读到关闭时
	// Sweep 在这之前就返回了，关闭期间的旧条目到重新开启后的第一轮才丢；关闭期间的巡检不添条目，表也不增长。
	for id := range r.answers {
		if !listed[id] {
			delete(r.answers, id)
		}
	}
	for k := range r.retryAt {
		if !pending[k] {
			delete(r.retryAt, k)
		}
	}
	return nil
}

// errNotCountry 是后端给出的答案不是两个大写字母：HTTP 的应答去掉首尾空白之后，或本地库记录里的 country.iso_code。
var errNotCountry = errors.New("answer is not two uppercase letters")

// recall 返回节点在 addr 上记住的答案，命中的地址移到最前：表里保留的是节点最近用到的地址。
func (r *Resolver) recall(node int64, addr string) (string, bool) {
	as := r.answers[node]
	for i, a := range as {
		if a.addr == addr {
			copy(as[1:i+1], as[:i])
			as[0] = a
			return a.country, true
		}
	}
	return "", false
}

// remember 把节点在 addr 上查得的答案放到最前，超出 answersPerNode 的最久未用的地址丢掉。只在 recall 对同一个
// 地址未命中之后调用，表里没有 addr，不会出现重复的地址。
func (r *Resolver) remember(node int64, addr, country string) {
	as := append([]answer{{addr, country}}, r.answers[node]...)
	r.answers[node] = as[:min(len(as), answersPerNode)]
}

// write 把答案写回库。答案对节点被列出时的地址成立；写入之前节点换了地址或被删除时不写，只记日志。
func (r *Resolver) write(ctx context.Context, n store.Node, country string) error {
	set, err := r.store.SetLookupCountry(ctx, n.ID, n.LastSource, country)
	switch {
	case errors.Is(err, store.ErrNotFound):
		r.log.Info("country answer dropped: node deleted before it was written", "node", n.ID, "addr", n.LastSource)
	case err != nil:
		return err
	case !set:
		r.log.Info("country answer dropped: node address changed before it was written", "node", n.ID, "addr", n.LastSource)
	}
	return nil
}
