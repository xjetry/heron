package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"

	"github.com/xjetry/heron/internal/hub/store"
)

var (
	ErrNoAdmin      = errors.New("no admin password has been set")
	ErrBadPassword  = errors.New("wrong password")
	ErrLocked       = errors.New("too many failed logins from this source (one IPv4 address, or one IPv6 /64)")
	ErrLoginBusy    = errors.New("password verification is busy; please try again later")
	ErrWeakPassword = fmt.Errorf("password must be at least %d characters", MinPasswordLen)
)

const (
	MinPasswordLen = 12
	// 面板是长期打开的运维页面：24 小时无操作视为离开；30 天是无论如何都要
	// 重新证明身份的上限。
	SessionAbsolute = 30 * 24 * time.Hour
	SessionIdle     = 24 * time.Hour
	// 只有距已落库的最近使用时刻满一分钟才异步刷新，避免为每次轮询写库。
	// 并发请求可能在刷新落库前重复投递，不能把此阈值理解为严格的写频率上限。
	touchEvery = time.Minute

	argonTime    uint32 = 3
	argonMemory  uint32 = 64 * 1024
	argonThreads uint8  = 4
	argonKeyLen  uint32 = 32

	// 未设置管理员也支付相同的默认哈希成本；固定摘要不授予身份，存在性另行守卫。
	absentAdminPHC = "$argon2id$v=19$m=65536,t=3,p=4$MDEyMzQ1Njc4OWFiY2RlZg$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
)

// HashPassword 输出 PHC 字符串。成本参数随哈希保存，校验预算内的旧哈希按
// 自带参数重算，下次改密码时自然换成新参数。
func HashPassword(plain string) (string, error) {
	var salt [16]byte
	if _, err := rand.Read(salt[:]); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(plain), salt[:], argonTime, argonMemory, argonThreads, argonKeyLen)
	enc := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads,
		enc.EncodeToString(salt[:]), enc.EncodeToString(key)), nil
}

// VerifyPassword 按字符串自带的参数重算并常量时间比较。格式错误是存储损坏，
// 作为错误返回而不是当作"密码不对"。
func VerifyPassword(phc, plain string) (bool, error) {
	parts := strings.Split(phc, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return false, errors.New("stored password hash is not an argon2id PHC string")
	}
	if parts[2] != fmt.Sprintf("v=%d", argon2.Version) {
		return false, fmt.Errorf("stored password hash has unsupported argon2 version %q", parts[2])
	}
	var m, t uint32
	var p uint8
	if n, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil || n != 3 || parts[3] != fmt.Sprintf("m=%d,t=%d,p=%d", m, t, p) {
		return false, fmt.Errorf("stored password hash has malformed parameters %q", parts[3])
	}
	// 自带参数允许旧哈希继续校验，但损坏的存储不能触发 panic 或无界内存、CPU 消耗。
	// 校验预算独立于新哈希的默认成本：最多 256 MiB、10 轮、16 路并行。
	if t < 1 || t > 10 || p < 1 || p > 16 || m < 8*uint32(p) || m > 256*1024 {
		return false, fmt.Errorf("stored password hash parameters out of range: require 1<=t<=10, 1<=p<=16, 8*p<=m<=262144 KiB; got %q", parts[3])
	}
	enc := base64.RawStdEncoding.Strict()
	salt, err := enc.DecodeString(parts[4])
	if err != nil {
		return false, fmt.Errorf("stored password hash has malformed salt: %w", err)
	}
	want, err := enc.DecodeString(parts[5])
	if err != nil {
		return false, fmt.Errorf("stored password hash has malformed digest: %w", err)
	}
	if len(salt) != 16 || len(want) != int(argonKeyLen) {
		return false, errors.New("stored password hash requires a 16-byte salt and a 32-byte digest")
	}
	got := argon2.IDKey([]byte(plain), salt, t, m, p, uint32(len(want)))
	// 比较耗时不应泄露摘要匹配的位置，即使 Argon2 的耗时远大于比较本身。
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// SetPassword 只由 hub 主机上的 heron-hub passwd 调用：没有经网络的首次设置页，
// 也就没有"谁先访问谁占有"的窗口。写库的同一事务清空全部会话（store 保证）。
func (a *Auth) SetPassword(ctx context.Context, plain string) error {
	if utf8.RuneCountInString(plain) < MinPasswordLen {
		return ErrWeakPassword
	}
	phc, err := HashPassword(plain)
	if err != nil {
		return err
	}
	return a.store.SetAdminPassword(ctx, phc)
}

// Login 用密码换会话 token，明文只返回这一次。
//
// admin 表为空时一律失败：空表的语义是"无人可登录"而不是"无需认证"，由这里的
// 显式检查承载，并在日志里指明该跑 heron-hub passwd。失败按来源键计数（SourceKey：IPv4 按地址、IPv6 按 /64），
// 锁定期间的拒绝不依赖输入的密码，正确密码也不能提前解除锁定。
func (a *Auth) Login(ctx context.Context, password string, from netip.Addr) (string, error) {
	return a.LoginFactors(ctx, password, "", "", from)
}

// notifyLogin 记一条登录事件并交给投递队列，at 是事件时刻。
//
// 只在密码、Passkey 登录和主动重新认证入口调用，不在会话/API token 鉴权中调用：自动化轮询不是一次人工登录，不应刷屏。
// 会话或锁定已经生效后，请求断开不能取消记账，所以用 WithoutCancel；写入不设期限：store.write 入队后无条件等
// 单写协程执行完这条写，期限到点缩短不了入队之后的等待，只会在轮到它时把这条通知丢掉——登录通知是安全事件，宁可晚到
// 不可丢。等待时间由写协程的积压决定，与别的登录签发会话的写同一条队列。失败记录日志且不伪报身份判定失败。
//
// 摘要自带事件时刻：投递会重试、重启后续投，接收方看到的送达时刻不是登录时刻；Telegram 只发摘要，
// 只引用摘要的 webhook 模板也一样，时刻不写进摘要就到不了这些接收方。时刻按 a.loc 写成带偏移的
// RFC 3339，只到秒，与库里事件的 at（RecordLoginEvent 截到秒）是同一秒。
func (a *Auth) notifyLogin(ctx context.Context, transition store.Transition, what string, from netip.Addr, at time.Time) {
	summary := fmt.Sprintf("%s：来源 %s，时间 %s", what, from, at.In(a.loc).Format(time.RFC3339))
	ev, err := a.store.RecordLoginEvent(context.WithoutCancel(ctx), store.AlertEvent{Transition: transition, At: at, Summary: summary})
	if err != nil {
		a.log.Error("recording login notification failed", "err", err)
		return
	}
	if ev.ID != 0 && a.loginSender != nil {
		a.loginSender.Enqueue(ev)
	}
}

// verifyLoginPassword 判锁定、取门，在门内校验密码并记账。返回存储的 PHC、这次失败是否设下了锁定
// （failureTracker.record 的报告，原样传出）与校验结果。
func (a *Auth) verifyLoginPassword(ctx context.Context, password string, from netip.Addr) (string, bool, error) {
	a.mu.Lock()
	if a.login.locked(from, a.clk.Mono()) {
		a.mu.Unlock()
		return "", false, ErrLocked
	}
	// 排队会保留整波匿名请求的慢哈希成本，并把合法登录推到队尾，故直接拒绝。
	//
	// 容量固定为一，不按核数推导。一次校验按存储的 PHC 自带参数分配 m KiB：新哈希取
	// argonMemory 即 64 MiB，VerifyPassword 最多接受 256 MiB，已存的旧哈希按它自带的 m 算，
	// 调小 argonMemory 降不下它。一次校验还为 p 条 lane 各起一个协程并行计算，新哈希取
	// argonThreads 即 4。所以核数既不约束 m，一次校验也已能并行占用 p 个核。
	//
	// loginGate 不分来源：ErrLoginBusy 只说明另一个请求正在校验，与本来源给的密码无关；
	// 失败按请求自己的 SourceKey 记。把忙碌记作失败，任何来源占住门都能让另一来源的
	// 管理员重试几次后被锁满 failWindow。
	if !a.loginGate.TryLock() {
		a.mu.Unlock()
		return "", false, ErrLoginBusy
	}
	a.mu.Unlock()
	defer a.loginGate.Unlock()
	phc, ok, err := a.store.AdminPasswordHash(ctx)
	if err != nil {
		return "", false, err
	}
	if !ok {
		phc = absentAdminPHC
	}
	match, err := VerifyPassword(phc, password)
	if err != nil {
		return "", false, err
	}
	if !ok || !match {
		a.mu.Lock()
		count, newlyLocked := a.login.record(from, a.clk.Mono())
		a.mu.Unlock()
		a.log.Warn("login failed", "from", from, "failures", count)
		if !ok {
			a.log.Warn("login refused: no admin password is set; run `heron-hub passwd`", "from", from)
			return "", newlyLocked, ErrNoAdmin
		}
		return "", newlyLocked, ErrBadPassword
	}
	// 密码只是一个因子；第二因素消费和会话签发成功后，登录入口才清除来源失败记录。
	return phc, false, nil
}

// AuthenticateSession 在会话 cookie 的全部候选值里找出第一个对应活着的会话的，返回它；一个都没有时第二个返回值为 false。
//
// 候选是请求里全部非空的 heron_session 值，按出现顺序排列（api 包的 sessionCandidates），其中可以混着别的主机写进
// 浏览器的值。不变式：多出来的候选不改变有效候选的结论——逐个校验、任一有效即通过，不设个数上限；设上限等于让写
// cookie 的一方用更多的值把有效值挤出去。校验不通过只返回 false：这里不调用 Login，也不碰按来源的登录失败计数
// （计数只在主动认证入口记），所以无效候选再多也不会让任何来源被锁定。
//
// 多个候选都有效时取顺序上第一个。hub 只有一个管理员，任一有效 token 证明的是同一身份，选哪个不影响准入；
// 调用方把选中的那个放进 ctx，它决定 Logout 吊销哪个会话、撤销当前会话时是否清 cookie、会话列表把哪个标为当前，
// 这里也只刷新它的最近使用时刻。按候选顺序选，会话状态不变时同一个 Cookie 头每次选中同一个会话。
//
// 这条路径匿名可达、不经登录门，候选数又不设上限，所以成本按最坏的头部算。匹配在内存里做：读一次会话表，把候选
// 逐个哈希后按顺序查表。
//   - 读表的成本随会话行数变化，与请求内容无关。行只由密码校验通过的 Login 写入，匿名请求加不了行；Login 顺带删掉
//     已绝对过期的行，清理成功时行数不超过最近一次成功登录之前 SessionAbsolute 内的成功登录次数。
//   - 候选侧每个成形候选一次 SHA-256。n 个成形候选在 Cookie 头里至少占 79n-1 字节（"heron_session=" 14 字节、
//     token 64 字节、除最后一个外各一个分隔符 ";"），所以次数与头部字节成正比，头部字节由 http.Server 的请求头上限约束。
//   - 形状不是 NewToken 明文的值不可能是会话，哈希前丢弃。这只省掉不可能命中的 SHA-256，不承担正确性：
//     不丢弃时它们的哈希在表里也查不到。
//   - 不按候选查库：带上万个候选时，那条上万个变量的 IN 查询占了鉴权耗时的大头，端到端是同尺寸普通 Cookie 头的
//     二三十倍（modernc.org/sqlite v1.59.0 实测）。
//
// 会话时刻必须跨重启持久化，所以过期与最近使用用墙钟；回拨会推迟过期，前拨可能提前过期。候选里查到的过期会话
// 尝试删除，成功后回拨也不会复活它；删除失败记日志。
func (a *Auth) AuthenticateSession(ctx context.Context, candidates []string) (string, bool, error) {
	var tokens []string
	var hashes [][32]byte
	seen := map[string]bool{}
	for _, c := range candidates {
		if isTokenShaped(c) && !seen[c] {
			seen[c] = true
			tokens = append(tokens, c)
			hashes = append(hashes, HashToken(c))
		}
	}
	if len(tokens) == 0 {
		return "", false, nil
	}
	rows, err := a.store.Sessions(ctx)
	if err != nil {
		return "", false, err
	}
	stored := make(map[[32]byte]store.Session, len(rows))
	for _, sess := range rows {
		stored[sess.TokenHash] = sess
	}
	now := a.clk.Now()
	chosen := -1
	for i, h := range hashes {
		sess, ok := stored[h]
		switch {
		case !ok:
		case !sessionAlive(sess, now):
			if err := a.store.DeleteSession(ctx, h); err != nil {
				a.log.Warn("deleting expired session failed", "err", err)
			}
		case chosen < 0:
			chosen = i
		}
	}
	if chosen < 0 {
		return "", false, nil
	}
	h := hashes[chosen]
	if now.Sub(stored[h].LastUsedAt) >= touchEvery {
		a.store.TouchSessionAsync(h, now, func(err error) {
			if err != nil {
				a.log.Warn("recording session use failed", "err", err)
			}
		})
	}
	return tokens[chosen], true, nil
}

func (a *Auth) Logout(ctx context.Context, token string) error {
	return a.store.DeleteSession(ctx, HashToken(token))
}
