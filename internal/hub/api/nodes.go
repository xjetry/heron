package api

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/alert"
	"github.com/xjetry/heron/internal/hub/sanitize"
	"github.com/xjetry/heron/internal/hub/store"
)

const (
	maxNameRunes = 64
	maxNoteRunes = 1024
	// 公开备注是站长写给访客的一行说明，与私有备注 note 同在 UpdateNode 整体替换；见 cleanPublicRemark。
	maxPublicRemarkRunes = 100
	minWindowTTL         = 60 * time.Second
	maxWindowTTL         = 7 * 24 * time.Hour
	maxWindowNodes       = 1000
	minResetDay          = 1
	// 28 是每个月都有的最大日；更大的日子在短月里没有零点可对齐。
	maxResetDay = 28
)

// billingCycles 是协议枚举与库里周期文本的一一对应，未指定对应"没有周期"；TestBillingCyclesMapEveryValue 按两侧全集核对。
var billingCycles = map[heronv1.BillingCycle]store.BillingCycle{
	heronv1.BillingCycle_BILLING_CYCLE_UNSPECIFIED:  store.CycleNone,
	heronv1.BillingCycle_BILLING_CYCLE_MONTHLY:      store.CycleMonthly,
	heronv1.BillingCycle_BILLING_CYCLE_QUARTERLY:    store.CycleQuarterly,
	heronv1.BillingCycle_BILLING_CYCLE_SEMIANNUAL:   store.CycleSemiannual,
	heronv1.BillingCycle_BILLING_CYCLE_YEARLY:       store.CycleYearly,
	heronv1.BillingCycle_BILLING_CYCLE_BIENNIAL:     store.CycleBiennial,
	heronv1.BillingCycle_BILLING_CYCLE_TRIENNIAL:    store.CycleTriennial,
	heronv1.BillingCycle_BILLING_CYCLE_QUINQUENNIAL: store.CycleQuinquennial,
}

var trafficQuotaModes = map[heronv1.TrafficQuotaMode]string{
	heronv1.TrafficQuotaMode_TRAFFIC_QUOTA_MODE_SUM: "sum",
	heronv1.TrafficQuotaMode_TRAFFIC_QUOTA_MODE_RX:  "rx",
	heronv1.TrafficQuotaMode_TRAFFIC_QUOTA_MODE_TX:  "tx",
	heronv1.TrafficQuotaMode_TRAFFIC_QUOTA_MODE_MAX: "max",
}

// 价格与币种只供展示，不参与金额计算：pricePattern 只接受十进制文本，currencyPattern 只接受三个大写字母；价格非空而币种为空由 billingOf 拒绝。RE2 的 \d 只匹配 ASCII 数字，全角数字不算。
var (
	pricePattern    = regexp.MustCompile(`^\d{1,9}(\.\d{1,2})?$`)
	currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)
)

// billingOf 是计费字段唯一的校验，CreateNode 与 UpdateNode 两个入口共用；UpdateNode 里它与
// traffic_reset_day、offline_grace_s 同在入口裁决。m 为 nil
// （请求没带 billing）时各项取零值，即五项全清。days_left 由 hub 计算，请求里的值不读。到期日与到期扫描、days_left
// 共用 alert.ParseDate，写进去的日期读侧一定读得懂。自动续期要求周期与到期日都非空：推后需要这两项。扫描对缺周期
// 另有守卫（alert.cycleMonths），不依赖这里。
func billingOf(m *heronv1.Billing) (store.Billing, error) {
	b := store.Billing{Price: m.GetPrice(), Currency: m.GetCurrency(), ExpiresOn: m.GetExpiresOn(), AutoRenew: m.GetAutoRenew()}
	if b.Price != "" && !pricePattern.MatchString(b.Price) {
		return store.Billing{}, invalid("billing.price: must match %s, e.g. 12.50; got %q", pricePattern, b.Price)
	}
	if b.Currency != "" && !currencyPattern.MatchString(b.Currency) {
		return store.Billing{}, invalid("billing.currency: must be three uppercase letters (ISO 4217), e.g. USD; got %q", b.Currency)
	}
	if b.Price != "" && b.Currency == "" {
		return store.Billing{}, invalid("billing.currency: required when billing.price is set")
	}
	cycle, ok := billingCycles[m.GetBillingCycle()]
	if !ok {
		values := heronv1.BillingCycle(0).Descriptor().Values()
		names := make([]string, values.Len())
		for i := range names {
			names[i] = string(values.Get(i).Name())
		}
		return store.Billing{}, invalid("billing.billing_cycle: must be one of %s; got %s", strings.Join(names, ", "), m.GetBillingCycle())
	}
	b.Cycle = cycle
	if b.ExpiresOn != "" {
		if _, err := alert.ParseDate(b.ExpiresOn); err != nil {
			return store.Billing{}, invalid("billing.expires_on: must be an existing date in YYYY-MM-DD form; got %q", b.ExpiresOn)
		}
	}
	if b.AutoRenew && (b.Cycle == store.CycleNone || b.ExpiresOn == "") {
		return store.Billing{}, invalid("billing.auto_renew: requires billing.billing_cycle and billing.expires_on to be set")
	}
	return b, nil
}

// billingProto 是 Node.billing，也是公开端 PublicBilling 的投影来源。五项都没填时为 nil，字段缺失。today 是 hub 时区的
// 今天（alert.Today）；没有到期日或库里的值读不懂时 days_left 缺失。
func billingProto(b store.Billing, today time.Time) *heronv1.Billing {
	if b == (store.Billing{}) {
		return nil
	}
	out := &heronv1.Billing{Price: b.Price, Currency: b.Currency, BillingCycle: enumFor(billingCycles, b.Cycle), ExpiresOn: b.ExpiresOn, AutoRenew: b.AutoRenew}
	if d, ok := alert.DaysLeft(b.ExpiresOn, today); ok {
		out.DaysLeft = proto.Int32(int32(d))
	}
	return out
}

// countrySources 是库层显示值来源与协议枚举的一一对应；TestCountrySourcesMapEveryValue 按两侧全集核对。
var countrySources = map[store.CountrySource]heronv1.CountrySource{
	store.CountryNone:   heronv1.CountrySource_COUNTRY_SOURCE_UNSPECIFIED,
	store.CountryManual: heronv1.CountrySource_COUNTRY_SOURCE_MANUAL,
	store.CountryLookup: heronv1.CountrySource_COUNTRY_SOURCE_LOOKUP,
}

// addressSources 是库层出口地址显示值来源与协议枚举的一一对应；TestAddressSourcesMapEveryValue 按两侧全集核对。
var addressSources = map[store.AddressSource]heronv1.AddressSource{
	store.AddressNone:     heronv1.AddressSource_ADDRESS_SOURCE_UNSPECIFIED,
	store.AddressDetected: heronv1.AddressSource_ADDRESS_SOURCE_DETECTED,
	store.AddressManual:   heronv1.AddressSource_ADDRESS_SOURCE_MANUAL,
	store.AddressHubSeen:  heronv1.AddressSource_ADDRESS_SOURCE_HUB_SEEN,
}

// nodeNetworkProto 是 Node.network：两族都给出，判定只在 store.Node.DisplayNetwork。
func nodeNetworkProto(display [2]store.DisplayAddress) *heronv1.NodeNetwork {
	var families [2]*heronv1.NodeAddress
	for i, d := range display {
		families[i] = &heronv1.NodeAddress{Address: d.Address, Source: addressSources[d.Source], State: d.State}
	}
	return &heronv1.NodeNetwork{Ipv4: families[0], Ipv6: families[1]}
}

// addressPinInvalid 给出手填出口地址被拒时的字段路径与期望取值；判定本身只在 store（UpdateNodeTasks）。示例取真实的
// 公网地址：文档段（203.0.113.0/24、2001:db8::/32）不是公网地址，写进示例会被同一个判定拒绝。
func addressPinInvalid(e store.AddressPinError) error {
	if e.IPv4 {
		return invalid("ipv4_pin: must be empty or a public IPv4 address, e.g. 8.8.8.8; got %q", e.Got)
	}
	return invalid("ipv6_pin: must be empty or a public IPv6 address without a zone, e.g. 2606:4700::1111; got %q", e.Got)
}

// nodeProto 的 today 是 hub 时区的今天（alert.Today）。
func nodeProto(n store.Node, today time.Time) *heronv1.Node {
	out := &heronv1.Node{Id: n.ID, Name: n.Name, Public: n.Public, Note: n.Note, PublicRemark: n.PublicRemark, SortOrder: n.SortOrder, Position: n.Position, CreatedAt: n.CreatedAt.Unix(), Facts: n.Facts, TrafficResetDay: uint32(n.TrafficResetDay),
		Billing: billingProto(n.Billing, today), LastSource: n.LastSource, CountryIp: n.CountryIP, CountryPin: n.CountryPin, CountryLookup: n.Country, Tags: n.Tags, Maintenance: n.Maintenance, Rented: n.Rented,
		TrafficQuotaBytes: n.TrafficQuotaBytes, TrafficQuotaMode: enumFor(trafficQuotaModes, n.TrafficQuotaMode),
		Ipv4Pin: n.IPv4Pin, Ipv6Pin: n.IPv6Pin, Network: nodeNetworkProto(n.DisplayNetwork())}
	country, source := n.DisplayCountry()
	out.Country, out.CountrySource = country, countrySources[source]
	if !n.LastSeenAt.IsZero() {
		out.LastSeenAt = proto.Int64(n.LastSeenAt.Unix())
	}
	if n.Facts != nil {
		out.FactsUpdatedAt = proto.Int64(n.FactsUpdatedAt.Unix())
	}
	if n.OfflineGraceS != 0 {
		out.OfflineGraceS = proto.Uint32(uint32(n.OfflineGraceS))
	}
	return out
}

// cleanName 按 sanitize.Text 清洗，页面标题（cleanAppearance）用同一个口径。
// 按完整清洗结果计字符数；先截字节会把超长输入变成合法名称。
func cleanName(raw string) (string, error) {
	name := sanitize.Text(raw, len(raw))
	if n := utf8.RuneCountInString(name); n == 0 || n > maxNameRunes {
		return "", invalid("name must be 1–%d characters after trimming whitespace and control characters; got %d", maxNameRunes, n)
	}
	return name, nil
}

func cleanNote(raw string) (string, error) {
	note := sanitize.String(raw, len(raw))
	if n := utf8.RuneCountInString(note); n > maxNoteRunes {
		return "", invalid("note must be at most %d characters; got %d", maxNoteRunes, n)
	}
	return note, nil
}

// cleanPublicRemark 校验公开备注：单行、至多 maxPublicRemarkRunes 个码点、不含控制字符。与 note、标签名同在
// 这一处准入（不在各入口各写一份）；控制字符按 sanitize.IsControl 判定，与标签名同一个口径（note 是剔除、这里
// 是拒绝：访客看到的是原文，剔除会把站长写下的字悄悄换掉）。超长返回 InvalidArgument、不截断：截断会把超长输入
// 伪装成合法值。空串合法，表示没有公开备注。
func cleanPublicRemark(raw string) (string, error) {
	if strings.ContainsFunc(raw, sanitize.IsControl) {
		return "", invalid("public_remark must not contain control characters; got %q", raw)
	}
	if n := utf8.RuneCountInString(raw); n > maxPublicRemarkRunes {
		return "", invalid("public_remark must be at most %d characters; got %d", maxPublicRemarkRunes, n)
	}
	return raw, nil
}

func (s *Service) ListNodes(ctx context.Context, req *connect.Request[heronv1.ListNodesRequest]) (*connect.Response[heronv1.ListNodesResponse], error) {
	tags, err := cleanTags("tags", req.Msg.GetTags())
	if err != nil {
		return nil, err
	}
	// 非空 tags 要求节点带有所选全部标签，untagged 要求节点没有任何标签，两者同时给出的交集必然为空；空结果会把
	// 写错的条件伪装成"没有这样的节点"，所以在查询之前按参数错误拒绝。tags 为空时不构成组合，untagged 单独决定。
	if req.Msg.GetUntagged() && len(tags) > 0 {
		return nil, invalid("tags and untagged: must not be combined; untagged selects nodes with no tags, so tags must be empty")
	}
	// today 只取一次：同一个日期既拼进版本、又交给 nodeProto 算 days_left，版本与内容说的是同一天。
	today := s.today()
	revision := func(counter uint64) string {
		return fmt.Sprintf("%s.%d.%s", s.nodeListEpoch, counter, today.Format(time.DateOnly))
	}
	known := req.Msg.GetKnownRevision()
	list, err := s.store.ReadNodeList(ctx, store.NodeFilter{Tags: tags, Untagged: req.Msg.GetUntagged()},
		func(counter uint64) bool { return known != "" && known == revision(counter) })
	if err != nil {
		s.log.Error("listing nodes failed", "err", err)
		return nil, internalError("listing nodes failed")
	}
	out := &heronv1.ListNodesResponse{Revision: revision(list.Revision), Unchanged: list.Unchanged}
	for _, n := range list.Nodes {
		out.Nodes = append(out.Nodes, nodeProto(n, today))
	}
	return connect.NewResponse(out), nil
}

func (s *Service) CreateNode(ctx context.Context, req *connect.Request[heronv1.CreateNodeRequest]) (*connect.Response[heronv1.CreateNodeResponse], error) {
	name, err := cleanName(req.Msg.GetName())
	if err != nil {
		return nil, err
	}
	billing, err := billingOf(req.Msg.GetBilling())
	if err != nil {
		return nil, err
	}
	id, tok, err := s.nodes.Create(ctx, store.NewNode{Name: name, Billing: billing, Rented: req.Msg.GetRented()})
	if errors.Is(err, store.ErrNodeLimit) {
		return nil, connect.NewError(connect.CodeResourceExhausted, err)
	}
	if err != nil {
		s.log.Error("creating node failed", "err", err)
		return nil, internalError("creating node failed")
	}
	// Create 返回时带计费的新节点已扫描过一次，回读到的到期日与 days_left 是扫描之后的值。
	n, err := s.store.GetNode(ctx, id)
	if err != nil {
		s.log.Error("reading created node failed", "err", err)
		return nil, internalError("reading created node failed")
	}
	s.log.Info("node created", "node", id, "name", name)
	return connect.NewResponse(&heronv1.CreateNodeResponse{Node: nodeProto(n, s.today()), Token: tok}), nil
}

func (s *Service) UpdateNode(ctx context.Context, req *connect.Request[heronv1.UpdateNodeRequest]) (*connect.Response[heronv1.UpdateNodeResponse], error) {
	name, err := cleanName(req.Msg.GetName())
	if err != nil {
		return nil, err
	}
	note, err := cleanNote(req.Msg.GetNote())
	if err != nil {
		return nil, err
	}
	remark, err := cleanPublicRemark(req.Msg.GetPublicRemark())
	if err != nil {
		return nil, err
	}
	day := int(req.Msg.GetTrafficResetDay())
	if day < minResetDay || day > maxResetDay {
		return nil, invalid("traffic_reset_day must be between %d and %d; got %d", minResetDay, maxResetDay, day)
	}
	quota := req.Msg.GetTrafficQuotaBytes()
	if quota >= 1<<62 {
		return nil, invalid("traffic_quota_bytes: must be between 0 (inclusive) and 2^62 (exclusive); got %d", quota)
	}
	mode := "sum"
	if quota > 0 {
		var ok bool
		mode, ok = trafficQuotaModes[req.Msg.GetTrafficQuotaMode()]
		if !ok {
			return nil, invalid("traffic_quota_mode: must be SUM, RX, TX or MAX when traffic_quota_bytes is nonzero; got %s", req.Msg.GetTrafficQuotaMode())
		}
	}
	if req.Msg.OfflineGraceS == nil {
		return nil, invalid("offline_grace_s: required; 0 clears it")
	}
	grace := req.Msg.GetOfflineGraceS()
	if grace != 0 && time.Duration(grace)*time.Second < s.cfg.TTL {
		return nil, invalid("offline_grace_s: must be 0 or at least %d seconds (--offline-after, HERON_OFFLINE_AFTER); got %d", (s.cfg.TTL+time.Second-1)/time.Second, grace)
	}
	billing, err := billingOf(req.Msg.GetBilling())
	if err != nil {
		return nil, err
	}
	pin := req.Msg.GetCountryPin()
	if pin != "" && !store.IsCountryCode(pin) {
		return nil, invalid("country_pin: must be empty or two uppercase letters (ISO 3166-1 alpha-2), e.g. US; got %q", pin)
	}
	tags, err := cleanTags("tags", req.Msg.GetTags())
	if err != nil {
		return nil, err
	}
	edit := store.NodeEdit{Name: name, Public: req.Msg.GetPublic(), Note: note, PublicRemark: remark, TrafficResetDay: day, TrafficQuotaBytes: quota, TrafficQuotaMode: mode, OfflineGraceS: int(grace), Billing: billing, CountryPin: pin, Maintenance: req.Msg.GetMaintenance(), Rented: req.Msg.GetRented(), Tags: tags,
		IPv4Pin: req.Msg.GetIpv4Pin(), IPv6Pin: req.Msg.GetIpv6Pin()}
	err = s.nodes.Update(ctx, req.Msg.GetId(), edit)
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFound(req.Msg.GetId())
	}
	var pinErr store.AddressPinError
	if errors.As(err, &pinErr) {
		return nil, addressPinInvalid(pinErr)
	}
	if err != nil {
		return nil, s.operationError(err, "tags", "updating node failed")
	}
	// Update 返回时计费与流量引起的扫描都已做完，回读到的到期日与 days_left 已是推后之后的值。Update 放锁之后节点
	// 可能已被并发的删除删掉，这时按不存在应答。
	n, err := s.store.GetNode(ctx, req.Msg.GetId())
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFound(req.Msg.GetId())
	}
	if err != nil {
		s.log.Error("reading updated node failed", "err", err)
		return nil, internalError("reading updated node failed")
	}
	return connect.NewResponse(&heronv1.UpdateNodeResponse{Node: nodeProto(n, s.today())}), nil
}

// RenewNodeBilling 是"已续费"。推后的日期只由 alert.RenewedExpiry 算出，与自动续期共用步进与月末钳制；
// 今天是 hub 时区的今天（s.today），与同一响应里的 days_left 同一口径。写回经 store.RenewNodeBilling 的条件写：
// 这里读到节点到写入之间计费若被改过（并发的 UpdateNode、日界扫描的自动续期），条件不成立、什么都不写，回答
// FailedPrecondition，调用方刷新后按新值再决定是否续费。写入后与 UpdateNode 改计费一样立刻扫描一次，到期
// 告警在续费后即恢复；修改已提交，扫描失败只记日志，日界扫描会补上。不经 nodeops：只写到期日一列，不碰流量账本与
// 标签作用域，nodeops.mu 守护的不变式与它无关。
func (s *Service) RenewNodeBilling(ctx context.Context, req *connect.Request[heronv1.RenewNodeBillingRequest]) (*connect.Response[heronv1.RenewNodeBillingResponse], error) {
	id := req.Msg.GetNodeId()
	n, err := s.store.GetNode(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFound(id)
	}
	if err != nil {
		s.log.Error("reading node for renewal failed", "err", err)
		return nil, internalError("reading node for renewal failed")
	}
	to, err := alert.RenewedExpiry(n.Billing, s.today())
	switch {
	case errors.Is(err, alert.ErrRenewNoCycle):
		return nil, invalid("billing.billing_cycle: node %d has no billing cycle to renew by; set one with UpdateNode first", id)
	case errors.Is(err, alert.ErrRenewNoExpiry):
		return nil, invalid("billing.expires_on: node %d has no expiry date to renew from; set one with UpdateNode first", id)
	case err != nil:
		// 写入口都校验日期，读不懂的值只可能来自手改的库：请求本身没错，是节点的当前状态不能续费。
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("billing.expires_on: node %d stores %q, not a YYYY-MM-DD date; correct it with UpdateNode first", id, n.Billing.ExpiresOn))
	}
	err = s.store.RenewNodeBilling(ctx, id, n.Billing.Cycle, n.Billing.ExpiresOn, to)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, notFound(id)
	case errors.Is(err, store.ErrPrecondition):
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	case err != nil:
		// 类型化变更的预览、重放与拒绝由 runChange 按 Change.Err 回答，这里的返回值不影响它们。
		s.log.Error("renewing node billing failed", "err", err)
		return nil, internalError("renewing node billing failed")
	}
	s.log.Info("node billing renewed", "node", id, "cycle", string(n.Billing.Cycle), "from", n.Billing.ExpiresOn, "to", to)
	if err := s.alerts.SweepExpiry(context.WithoutCancel(ctx)); err != nil {
		s.log.Error("expiry sweep after node renewal failed", "node", id, "err", err)
	}
	n, err = s.store.GetNode(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFound(id)
	}
	if err != nil {
		s.log.Error("reading renewed node failed", "err", err)
		return nil, internalError("reading renewed node failed")
	}
	return connect.NewResponse(&heronv1.RenewNodeBillingResponse{Node: nodeProto(n, s.today())}), nil
}

// DeleteNode 返回成功同时意味着持久化删除完成与进程内状态清除（见 nodeops.Service.Delete）。
func (s *Service) DeleteNode(ctx context.Context, req *connect.Request[heronv1.DeleteNodeRequest]) (*connect.Response[heronv1.DeleteNodeResponse], error) {
	err := s.nodes.Delete(ctx, req.Msg.GetId())
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFound(req.Msg.GetId())
	}
	if err != nil {
		s.log.Error("deleting node failed", "err", err)
		return nil, internalError("deleting node failed")
	}
	s.log.Info("node deleted", "node", req.Msg.GetId())
	return connect.NewResponse(&heronv1.DeleteNodeResponse{}), nil
}

func (s *Service) RotateNodeToken(ctx context.Context, req *connect.Request[heronv1.RotateNodeTokenRequest]) (*connect.Response[heronv1.RotateNodeTokenResponse], error) {
	tok, err := s.nodes.RotateToken(ctx, req.Msg.GetId())
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFound(req.Msg.GetId())
	}
	// 凭据已被别的进程（离线 rotate-token）换过、hub 尚未重载：库与映射都没动，重载之后重试即可。
	if errors.Is(err, store.ErrCredentialChanged) {
		return nil, changeError(err)
	}
	if err != nil {
		s.log.Error("rotating token failed", "err", err)
		return nil, internalError("rotating token failed")
	}
	s.log.Info("node token rotated", "node", req.Msg.GetId())
	return connect.NewResponse(&heronv1.RotateNodeTokenResponse{Token: tok}), nil
}

func (s *Service) ReorderNodes(ctx context.Context, req *connect.Request[heronv1.ReorderNodesRequest]) (*connect.Response[heronv1.ReorderNodesResponse], error) {
	err := s.store.ReorderNodes(ctx, req.Msg.GetIds())
	if errors.Is(err, store.ErrBadOrder) {
		return nil, invalid("ids must list every existing node exactly once; got %d ids", len(req.Msg.GetIds()))
	}
	if err != nil {
		s.log.Error("reordering nodes failed", "err", err)
		return nil, internalError("reordering nodes failed")
	}
	return connect.NewResponse(&heronv1.ReorderNodesResponse{}), nil
}

// MoveNodes 的错误消息自带合法区间的推导（N、k 由 store 在写事务里读到），不截断到区间端点。
func (s *Service) MoveNodes(ctx context.Context, req *connect.Request[heronv1.MoveNodesRequest]) (*connect.Response[heronv1.MoveNodesResponse], error) {
	err := s.store.MoveNodes(ctx, req.Msg.GetIds(), req.Msg.GetPosition())
	switch {
	case errors.Is(err, store.ErrEmptyMove):
		return nil, invalid("ids: must list at least one node")
	case errors.Is(err, store.ErrNotFound):
		var missing store.NotFoundError
		errors.As(err, &missing)
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("ids: node %d does not exist; nothing was moved", missing.ID))
	}
	var outOfRange store.MoveRangeError
	if errors.As(err, &outOfRange) {
		return nil, invalid("position: must be between 1 and %d (N = %d nodes, k = %d selected); got %d",
			outOfRange.Total-outOfRange.Moving+1, outOfRange.Total, outOfRange.Moving, outOfRange.Position)
	}
	if err != nil {
		s.log.Error("moving nodes failed", "err", err)
		return nil, internalError("moving nodes failed")
	}
	return connect.NewResponse(&heronv1.MoveNodesResponse{}), nil
}

func (s *Service) OpenRegisterWindow(ctx context.Context, req *connect.Request[heronv1.OpenRegisterWindowRequest]) (*connect.Response[heronv1.OpenRegisterWindowResponse], error) {
	ttl := time.Duration(req.Msg.GetTtlS()) * time.Second
	if ttl < minWindowTTL || ttl > maxWindowTTL {
		return nil, invalid("ttl_s must be between %d and %d seconds; got %d", int(minWindowTTL/time.Second), int(maxWindowTTL/time.Second), req.Msg.GetTtlS())
	}
	maxNodes := req.Msg.GetMaxNodes()
	if maxNodes < 1 || maxNodes > maxWindowNodes {
		return nil, invalid("max_nodes must be between 1 and %d; got %d", maxWindowNodes, maxNodes)
	}
	key, until, err := s.auth.OpenWindow(ctx, ttl, int(maxNodes))
	if err != nil {
		s.log.Error("opening register window failed", "err", err)
		return nil, internalError("opening register window failed")
	}
	s.log.Info("register window opened", "expires_at", until, "max_nodes", maxNodes)
	return connect.NewResponse(&heronv1.OpenRegisterWindowResponse{Key: key, ExpiresAt: until.Unix(), MaxNodes: maxNodes}), nil
}

func (s *Service) CloseRegisterWindow(ctx context.Context, _ *connect.Request[heronv1.CloseRegisterWindowRequest]) (*connect.Response[heronv1.CloseRegisterWindowResponse], error) {
	if err := s.auth.CloseWindow(ctx); err != nil {
		s.log.Error("closing register window failed", "err", err)
		return nil, internalError("closing register window failed")
	}
	return connect.NewResponse(&heronv1.CloseRegisterWindowResponse{}), nil
}

// GetRegisterWindow 的 open 与 RegisterNode 事务里的判定同口径：存在、未到期、有名额。
func (s *Service) GetRegisterWindow(ctx context.Context, _ *connect.Request[heronv1.GetRegisterWindowRequest]) (*connect.Response[heronv1.GetRegisterWindowResponse], error) {
	w, ok, err := s.auth.Window(ctx)
	if err != nil {
		s.log.Error("reading register window failed", "err", err)
		return nil, internalError("reading register window failed")
	}
	if !ok || !s.clk.Now().Before(w.ExpiresAt) || w.Remaining <= 0 {
		return connect.NewResponse(&heronv1.GetRegisterWindowResponse{Open: false}), nil
	}
	return connect.NewResponse(&heronv1.GetRegisterWindowResponse{Open: true, ExpiresAt: w.ExpiresAt.Unix(), Remaining: uint32(w.Remaining)}), nil
}
