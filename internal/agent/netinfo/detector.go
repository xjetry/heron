// Package netinfo 异步探测节点的双栈公网出口，不参与指标采集和上报调度。
package netinfo

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"syscall"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/netaddr"
	"google.golang.org/protobuf/proto"
)

// 下标是地址族，与 clients 和 detect 的 family 一致：0 为 IPv4，1 为 IPv6。
// 每族的回显主机只发布该族的 DNS 记录，但测到哪一族由 clients 强制的 tcp4/tcp6 拨号保证；
// 两项互换不会测到另一族：拨号解析不到本族地址，结果是探测失败。
var endpoints = [2]string{"https://api-ipv4.ip.sb/ip", "https://api-ipv6.ip.sb/ip"}

const (
	interval       = 5 * time.Minute
	requestTimeout = 10 * time.Second
	maxResponse    = 64
)

type Detector struct {
	mu sync.RWMutex
	// current 里每族的 checked_at 是该族当前结果（state + address）首次得到的时刻，不是最近一次探测的时刻：refresh
	// 写回时与该族上一份结果比较，state 与 address 都相同就沿用上一份的 checked_at（stamp）；Skip 转停用时上一份不可能
	// 是 DISABLED，转恢复时把该族退回缺失，两者之后的结果都取新时刻。client.FactsHash 覆盖整份 Facts，由此摘要只随
	// 结果变化而变化，结果不变的周期探测不让 hub 重新索取、重写 facts。
	current *heronv1.NetworkInfo
	// skip 是 hub 最近一次应答要求停用探测的族，下标同 endpoints；只由 Skip 改写。refresh 在发探测之前读它决定
	// 拨哪些族，写回结果时再读一次：探测在途时 hub 可能刚要求停用，Skip 已写下的 DISABLED 不能被迟到的结果盖掉。
	skip [2]bool
	// resumed 是由停用转为恢复、还没探测过的族；wake 让 Run 不等 ticker 立即探测它们。refresh 开始探测某族时清掉
	// 它的标记，所以 ticker 先到时不会再多探一次。
	resumed    [2]bool
	wake       chan struct{}
	interval   time.Duration
	interfaces func() ([]netip.Addr, error)
	clients    [2]*http.Client
	now        func() time.Time
}

func New() *Detector {
	dialer := &net.Dialer{Timeout: requestTimeout}
	return newDetector(interfaceAddresses, dialer.DialContext)
}

func newDetector(addresses func() ([]netip.Addr, error), dial func(context.Context, string, string) (net.Conn, error)) *Detector {
	d := &Detector{interfaces: addresses, now: time.Now, current: &heronv1.NetworkInfo{}, wake: make(chan struct{}, 1), interval: interval}
	for i, family := range []string{"tcp4", "tcp6"} {
		transport := &http.Transport{
			// 不继承环境代理；代理回显的是代理的出口，不能代表节点的地址族能力。
			DialContext:         func(ctx context.Context, _, address string) (net.Conn, error) { return dial(ctx, family, address) },
			TLSHandshakeTimeout: requestTimeout,
			DisableKeepAlives:   true,
		}
		d.clients[i] = &http.Client{Transport: transport, Timeout: requestTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	return d
}

// Snapshot 返回独立副本；Run 与 Skip 在锁内整体替换，调用方修改 Facts 不会改变探测器的状态。
func (d *Detector) Snapshot() *heronv1.NetworkInfo {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return proto.Clone(d.current).(*heronv1.NetworkInfo)
}

// Skip 采纳 hub 在一次应答里给出的逐族开关，由 client.Runner 在每次成功应答后调用；nil 与各项为 false 相同，都是
// 两族照常探测。某族由探测转为停用时，立即把它换成 DISABLED（没有地址，checked_at 是此刻，
// 即停用生效、这份结果首次得到的时刻；重复同一开关在入口直接返回，不改它），
// 此后 refresh 不再拨它；由停用转为探测时，立即把它退回缺失并唤醒 Run 探测一次。退回缺失而不是留着 DISABLED 到
// 探测完成：已恢复探测，但尚无本轮结果。DISABLED 只在最近一次应答仍要求停用时出现在上报里。
//
// 停用状态不落盘，只来自应答：进程启动后到第一次成功应答之前，探测器不知道哪些族被停用，照常探测（第一轮立即
// 开始，与第一次上报并行；hub 不可达时按周期继续）。
func (d *Detector) Skip(detection *heronv1.NetworkDetection) {
	want := [2]bool{detection.GetSkipIpv4(), detection.GetSkipIpv6()}
	d.mu.Lock()
	defer d.mu.Unlock()
	if want == d.skip {
		return
	}
	next := proto.Clone(d.current).(*heronv1.NetworkInfo)
	wake := false
	for i := range want {
		switch {
		case want[i] && !d.skip[i]:
			setFamily(next, i, &heronv1.AddressDetection{State: heronv1.AddressDetectionState_ADDRESS_DETECTION_STATE_DISABLED, CheckedAt: d.now().Unix()})
			d.resumed[i] = false
		case !want[i] && d.skip[i]:
			setFamily(next, i, nil)
			d.resumed[i] = true
			wake = true
		}
	}
	d.skip, d.current = want, next
	if wake {
		select {
		case d.wake <- struct{}{}:
		default: // 已有一个未取的唤醒，Run 取到它时按 resumed 一并探测。
		}
	}
}

func setFamily(n *heronv1.NetworkInfo, family int, result *heronv1.AddressDetection) {
	if family == 0 {
		n.Ipv4 = result
	} else {
		n.Ipv6 = result
	}
}

// Run 由入口单独启动，首次立即检测；网络等待不占用 Runner 的采集循环。此后每个周期探测未停用的族，Skip 恢复某族时
// 立即探测那一族。
func (d *Detector) Run(ctx context.Context) {
	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()
	families := [2]bool{true, true}
	for {
		if ctx.Err() != nil {
			return
		}
		d.refresh(ctx, families)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			families = [2]bool{true, true}
		case <-d.wake:
			d.mu.RLock()
			families = d.resumed
			d.mu.RUnlock()
		}
	}
}

// refresh 探测 want 里未被停用的族，把结果写进快照，其余族保持原样。
func (d *Detector) refresh(ctx context.Context, want [2]bool) {
	var probe [2]bool
	d.mu.Lock()
	for i := range probe {
		probe[i] = want[i] && !d.skip[i]
		if probe[i] {
			d.resumed[i] = false
		}
	}
	d.mu.Unlock()
	if !probe[0] && !probe[1] {
		return
	}
	addresses, err := d.interfaces()
	var results [2]*heronv1.AddressDetection
	var wg sync.WaitGroup
	for i := range results {
		if !probe[i] {
			continue
		}
		wg.Add(1)
		go func() { defer wg.Done(); results[i] = d.detect(ctx, i, addresses, err) }()
	}
	wg.Wait()
	for i, result := range results {
		// 一轮的结果整体写入或整体丢弃：调用方取消上下文说明本轮被中止，detect 返回 nil。此时保留上一轮快照，
		// 否则 shutdown/reload 触发的取消会把已知的能力结论覆盖成失败。
		if probe[i] && result == nil {
			return
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	next := proto.Clone(d.current).(*heronv1.NetworkInfo)
	now := d.now().Unix()
	for i, result := range results {
		// 探测在途时被停用的族：Skip 已写下 DISABLED，迟到的结果不覆盖它。
		if probe[i] && !d.skip[i] {
			stamp(familyOf(next, i), result, now)
			setFamily(next, i, result)
		}
	}
	d.current = next
}

// stamp 给本轮结果定 checked_at：与该族上一份结果的 state、address 都相同就沿用上一份的时刻，否则是 now。比较必须
// 覆盖结果的全部内容（地址族由字段位置决定，不在消息里）：漏掉 address 时换出口地址不推进时刻，面板会把新地址说成
// 早于它的时刻就已探测到。
func stamp(prev, result *heronv1.AddressDetection, now int64) {
	if prev != nil && prev.GetState() == result.GetState() && prev.GetAddress() == result.GetAddress() {
		result.CheckedAt = prev.GetCheckedAt()
		return
	}
	result.CheckedAt = now
}

func familyOf(n *heronv1.NetworkInfo, family int) *heronv1.AddressDetection {
	if family == 0 {
		return n.GetIpv4()
	}
	return n.GetIpv6()
}

// detect 只给出 state 与 address；checked_at 由 refresh 写回时经 stamp 决定，因为它取决于该族的上一份结果。
func (d *Detector) detect(ctx context.Context, family int, addresses []netip.Addr, interfaceErr error) *heronv1.AddressDetection {
	out := &heronv1.AddressDetection{State: heronv1.AddressDetectionState_ADDRESS_DETECTION_STATE_FAILED}
	if interfaceErr != nil {
		return out
	}
	usable := false
	for _, address := range addresses {
		address = address.Unmap()
		// 私网接口可经 NAT 出网；回环与链路本地接口不能证明该族具有公网出口。
		if address.IsGlobalUnicast() && address.Is4() == (family == 0) {
			usable = true
			break
		}
	}
	if !usable {
		out.State = heronv1.AddressDetectionState_ADDRESS_DETECTION_STATE_UNSUPPORTED
		return out
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoints[family], nil)
	if err != nil {
		return out
	}
	resp, err := d.clients[family].Do(req)
	if err != nil {
		// 取消是调用方中止本轮（shutdown/reload），不是被探测地址族的能力结论；
		// 返回 nil，由 refresh 丢弃整轮而不覆盖上一轮快照。
		if errors.Is(err, context.Canceled) {
			return nil
		}
		var dnsError *net.DNSError
		// DNS 服务器不可达不等于目标地址族不可用；只有实际连接返回的系统错误能证明不支持。
		if !errors.As(err, &dnsError) && (errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EAFNOSUPPORT)) {
			out.State = heronv1.AddressDetectionState_ADDRESS_DETECTION_STATE_UNSUPPORTED
		}
		return out
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return out
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil || len(body) > maxResponse {
		return out
	}
	address, valid := netaddr.ParsePublicFamily(strings.TrimSpace(string(body)), family == 0)
	if !valid {
		return out
	}
	out.State, out.Address = heronv1.AddressDetectionState_ADDRESS_DETECTION_STATE_AVAILABLE, address.String()
	return out
}

func interfaceAddresses() ([]netip.Addr, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var out []netip.Addr
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addresses, err := iface.Addrs()
		if err != nil {
			return nil, err
		}
		for _, address := range addresses {
			prefix, err := netip.ParsePrefix(address.String())
			if err != nil {
				return nil, err
			}
			out = append(out, prefix.Addr())
		}
	}
	return out, nil
}
