package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/hub/store"
)

// 每种面板安全操作在成功路径上恰好写一条记录：主体是发起它的会话，动作是 securityActions 给的那个；提交里吊销了全部会话的
// 种类，记录照样落在吊销它的那个事务里。挑战、TOTP 秘密、恢复码、证明令牌、凭据 id 与会话令牌一律不进记录。
func TestEverySecurityActionRecordsExactlyOnce(t *testing.T) {
	a, st, clk := setup(t)
	ctx := WithWebAuthnOrigin(context.Background(), "https://admin.example")
	from := netip.MustParseAddr("192.0.2.1")
	if err := st.SetAdminPassword(ctx, cheapPHC(goodPassword)); err != nil {
		t.Fatal(err)
	}
	var totpSecret string
	secrets := []string{goodPassword}
	login := func() string {
		t.Helper()
		var session string
		var err error
		if totpSecret == "" {
			session, err = a.Login(ctx, goodPassword, from)
		} else {
			clk.Advance(30 * time.Second)
			session, err = a.LoginFactors(ctx, goodPassword, totpCode(totpSecret, clk.Now().Unix()/30), "", from)
		}
		if err != nil {
			t.Fatal(err)
		}
		secrets = append(secrets, session)
		return session
	}
	session := login()
	all := func() []store.Operation {
		t.Helper()
		list, err := st.ListOperations(ctx, store.OperationFilter{Limit: 500})
		if err != nil {
			t.Fatal(err)
		}
		return list
	}
	covered := map[SecurityActionKind]bool{}
	do := func(in SecurityInput) SecurityResult {
		t.Helper()
		h := HashToken(session)
		caller := store.Caller{Kind: store.PrincipalSession, Session: hex.EncodeToString(h[:]), Source: SourceText(from), Method: "SecurityAction"}
		seen := map[string]bool{}
		for _, o := range all() {
			seen[o.ID] = true
		}
		out, err := a.SecurityAction(store.WithAudit(store.WithCaller(ctx, caller), &store.Audit{}), in, session, from)
		if err != nil {
			t.Fatalf("%s: %v", in.Action, err)
		}
		// 夹具时钟不动时提交时刻相同，列表顺序落到随机的 request_id 上：按 id 取新增的那条。
		var added []store.Operation
		for _, o := range all() {
			if !seen[o.ID] {
				added = append(added, o)
			}
		}
		if len(added) != 1 {
			t.Fatalf("%s wrote %d records, want 1", in.Action, len(added))
		}
		o := added[0]
		if o.Action != securityActions[in.Action].String() || o.Principal != store.PrincipalSession || o.SessionID != caller.Session ||
			o.Source != "192.0.2.1" || o.Method != "SecurityAction" || o.ResourceID != 0 || o.ResourceKey != "" {
			t.Fatalf("%s record: %+v", in.Action, o)
		}
		covered[in.Action] = true
		secrets = append(secrets, out.ChallengeID, out.TOTPSecret, out.ProofToken)
		secrets = append(secrets, out.RecoveryCodes...)
		if out.RevokeSession {
			session = login()
		}
		return out
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := []byte("credential-identifier-for-records")
	credentialID := base64.RawURLEncoding.EncodeToString(id)
	secrets = append(secrets, credentialID)

	begin := do(SecurityInput{Action: SecurityPasskeyBegin, Password: goodPassword})
	do(SecurityInput{Action: SecurityPasskeyRegister, ChallengeID: begin.ChallengeID, Name: "记录用密钥",
		CredentialJSON: softwareCredential(t, key, id, nil, challengeValue(t, begin.OptionsJSON), "https://admin.example", true, 0)})
	do(SecurityInput{Action: SecurityPasskeyRebindBegin, Password: goodPassword})
	_, state, err := a.readSecurity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	reauth := do(SecurityInput{Action: SecurityReauthBegin})
	proof := do(SecurityInput{Action: SecurityReauthFinish, ChallengeID: reauth.ChallengeID,
		CredentialJSON: softwareCredential(t, key, id, state.UserID, challengeValue(t, reauth.OptionsJSON), "https://admin.example", false, 1)})
	do(SecurityInput{Action: SecurityPasskeyDelete, CredentialID: credentialID, ProofToken: proof.ProofToken})
	totp := do(SecurityInput{Action: SecurityTOTPBegin, Password: goodPassword})
	// TOTP 启用之后的登录与证明都要下一个时间步的码：同一步的码已被消费。启用会吊销会话，do 里的重新登录就要用它。
	totpSecret = totp.TOTPSecret
	do(SecurityInput{Action: SecurityTOTPEnable, ChallengeID: totp.ChallengeID, OTP: totpCode(totp.TOTPSecret, clk.Now().Unix()/30)})
	clk.Advance(30 * time.Second)
	do(SecurityInput{Action: SecurityRecoveryRegenerate, Password: goodPassword, OTP: totpCode(totpSecret, clk.Now().Unix()/30)})
	clk.Advance(30 * time.Second)
	code := totpCode(totpSecret, clk.Now().Unix()/30)
	totpSecret = ""
	do(SecurityInput{Action: SecurityTOTPDisable, Password: goodPassword, OTP: code})

	for kind := range securityActions {
		if !covered[kind] {
			t.Errorf("security action %s has no case here", kind)
		}
	}
	for _, o := range all() {
		text := strings.Join([]string{o.RequestID, o.BeforeJSON, o.AfterJSON, o.ResourceKey}, "\x00")
		for _, s := range secrets {
			if s != "" && strings.Contains(text, s) {
				t.Errorf("record %s carries a secret %q", o.Action, s)
			}
		}
	}
}
