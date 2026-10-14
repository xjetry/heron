package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/xjetry/heron/internal/hub/store"
)

func TestTOTPEnrollmentLoginRecoveryAndRevocation(t *testing.T) {
	a, st, clk := setup(t)
	ctx := context.Background()
	from := netip.MustParseAddr("192.0.2.1")
	if err := st.SetAdminPassword(ctx, cheapPHC(goodPassword)); err != nil {
		t.Fatal(err)
	}
	session, err := a.Login(ctx, goodPassword, from)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.SecurityAction(ctx, SecurityInput{Action: "totp_begin"}, session, from); err == nil {
		t.Fatal("old session alone enrolled factor")
	}
	begin, err := a.SecurityAction(ctx, SecurityInput{Action: "totp_begin", Password: goodPassword}, session, from)
	if err != nil {
		t.Fatal(err)
	}
	code := totpCode(begin.TOTPSecret, clk.Now().Unix()/30)
	finish, err := a.SecurityAction(ctx, SecurityInput{Action: "totp_enable", ChallengeID: begin.ChallengeID, OTP: code}, session, from)
	if err != nil {
		t.Fatal(err)
	}
	if len(finish.RecoveryCodes) != 10 {
		t.Fatal("missing one-time recovery codes")
	}
	if _, ok, _ := a.AuthenticateSession(ctx, []string{session}); ok {
		t.Fatal("factor change retained old session")
	}
	if _, err = a.Login(ctx, goodPassword, from); !errors.Is(err, ErrSecurity) {
		t.Fatalf("password bypassed factor: %v", err)
	}
	if _, err = a.LoginFactors(ctx, goodPassword, code, "", from); !errors.Is(err, ErrSecurity) {
		t.Fatalf("enrollment code replay: %v", err)
	}
	clk.Advance(30 * time.Second)
	code = totpCode(begin.TOTPSecret, clk.Now().Unix()/30)
	if _, err = a.LoginFactors(ctx, goodPassword, code, "", from); err != nil {
		t.Fatal(err)
	}
	if _, err = a.LoginFactors(ctx, goodPassword, code, "", from); !errors.Is(err, ErrSecurity) {
		t.Fatal("TOTP replay accepted")
	}
	if _, err = a.LoginFactors(ctx, goodPassword, "", finish.RecoveryCodes[0], from); err != nil {
		t.Fatal(err)
	}
	if _, err = a.LoginFactors(ctx, goodPassword, "", finish.RecoveryCodes[0], from); !errors.Is(err, ErrSecurity) {
		t.Fatal("recovery replay accepted")
	}
	b, err := st.AdminSecurity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range finish.RecoveryCodes {
		if strings.Contains(b.Data, code) {
			t.Fatal("recovery plaintext persisted")
		}
	}
	// TOTP 启用后不走第二因素的登录在真实路径上被拒，且不签发会话。
	countSessions := func() int {
		rows, err := st.Sessions(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return len(rows)
	}
	sessionsBefore := countSessions()
	if _, err = a.LoginFactors(ctx, goodPassword, "", "", from); !errors.Is(err, ErrSecurity) {
		t.Fatal("password-only login bypassed TOTP")
	}
	if got := countSessions(); got != sessionsBefore {
		t.Fatalf("rejected password-only login changed session count: %d -> %d", sessionsBefore, got)
	}
	if err = a.ResetSecurity(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = a.Login(ctx, goodPassword, from); err != nil {
		t.Fatal(err)
	}
}

func TestSecurityCASAndChallengeBindings(t *testing.T) {
	a, st, clk := setup(t)
	ctx := context.Background()
	from := netip.MustParseAddr("192.0.2.1")
	if err := st.SetAdminPassword(ctx, cheapPHC(goodPassword)); err != nil {
		t.Fatal(err)
	}
	b, s, err := a.readSecurity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.commitSecurity(ctx, b, s, false, nil, ""); err != nil {
		t.Fatal(err)
	}
	_, h := NewToken()
	if err = a.commitSecurity(ctx, b, s, false, &h, ""); !errors.Is(err, store.ErrAdminChanged) {
		t.Fatal("stale security state issued session")
	}
	for _, kind := range []string{"session", "purpose", "generation", "expiration", "replay"} {
		t.Run(kind, func(t *testing.T) {
			id, err := a.putChallenge(securityChallenge{Kind: "proof", Session: "session", Generation: 7})
			if err != nil {
				t.Fatal(err)
			}
			purpose, session, generation := "proof", "session", int64(7)
			switch kind {
			case "session":
				session = "other"
			case "purpose":
				purpose = "login"
			case "generation":
				generation++
			case "expiration":
				clk.Advance(5 * time.Minute)
			case "replay":
				if _, err = a.takeChallenge(id, purpose, session, generation); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = a.takeChallenge(id, purpose, session, generation); !errors.Is(err, ErrSecurity) {
				t.Fatal("challenge binding failed")
			}
		})
	}
	session, err := a.Login(ctx, goodPassword, from)
	if err != nil {
		t.Fatal(err)
	}
	begin, err := a.SecurityAction(ctx, SecurityInput{Action: "totp_begin", Password: goodPassword}, session, from)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Logout(ctx, session); err != nil {
		t.Fatal(err)
	}
	if _, err = a.SecurityAction(ctx, SecurityInput{Action: "totp_enable", ChallengeID: begin.ChallengeID, OTP: totpCode(begin.TOTPSecret, clk.Now().Unix()/30)}, session, from); !errors.Is(err, store.ErrAdminChanged) {
		t.Fatal("revoked session completed enrollment")
	}
}

func TestWebAuthnOrigins(t *testing.T) {
	a, _, _ := setup(t)
	for _, origin := range []string{"http://example.com", "https://admin.example/path", "https://user:pass@admin.example", "https://admin.example?x=1"} {
		if err := a.ConfigureWebAuthn(origin); err == nil {
			t.Fatalf("bad origin accepted: %s", origin)
		}
	}
	if err := a.ConfigureWebAuthn("https://ADMIN.example."); err != nil {
		t.Fatal(err)
	}
	if err := a.ConfigureWebAuthn("http://localhost:8080"); err != nil {
		t.Fatal(err)
	}
}

func TestWebAuthnMissingRequestOriginRejected(t *testing.T) {
	a, st, _ := setup(t)
	ctx := context.Background()
	if err := st.SetAdminPassword(ctx, cheapPHC(goodPassword)); err != nil {
		t.Fatal(err)
	}
	b, s, err := a.readSecurity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.Passkeys = []Passkey{{Credential: webauthn.Credential{ID: []byte("bound-key")}}}
	if err = a.commitSecurity(ctx, b, s, false, nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := a.ConfigureWebAuthn("https://admin.example"); err != nil {
		t.Fatal(err)
	}
	from := netip.MustParseAddr("192.0.2.1")
	if _, err := a.BeginPasskeyLogin(ctx, from); !errors.Is(err, ErrSecurity) {
		t.Fatalf("missing request origin accepted: %v", err)
	}
	if _, err := a.BeginPasskeyLogin(WithWebAuthnOrigin(ctx, "https://admin.example"), from); err != nil {
		t.Fatal("valid request origin could not begin login", err)
	}
}

func TestPasskeyChallengeAndVerificationAdmission(t *testing.T) {
	a, st, _ := setup(t)
	ctx := WithWebAuthnOrigin(context.Background(), "https://admin.example")
	first := netip.MustParseAddr("2001:db8:1::1")
	second := netip.MustParseAddr("2001:db8:2::1")
	if err := st.SetAdminPassword(ctx, cheapPHC(goodPassword)); err != nil {
		t.Fatal(err)
	}
	b, s, err := a.readSecurity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.Origin = "https://admin.example"
	s.RPID = "admin.example"
	s.Passkeys = []Passkey{{Credential: webauthn.Credential{ID: []byte("admission-test")}}}
	if err := a.commitSecurity(ctx, b, s, false, nil, ""); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := a.BeginPasskeyLogin(ctx, first); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.BeginPasskeyLogin(ctx, netip.MustParseAddr("2001:db8:1::2")); !errors.Is(err, ErrLoginBusy) {
		t.Fatal("same source filled challenge pool")
	}
	if _, err := a.BeginPasskeyLogin(ctx, second); err != nil {
		t.Fatal("one source denied independent source")
	}
	a.loginGate.Lock()
	_, err = a.FinishPasskeyLogin(ctx, "unknown", "{}", second)
	a.loginGate.Unlock()
	if !errors.Is(err, ErrLoginBusy) {
		t.Fatal("Passkey verification bypassed shared gate")
	}
	for range failLimit {
		if _, err := a.FinishPasskeyLogin(ctx, "unknown", "{}", second); !errors.Is(err, ErrSecurity) {
			t.Fatal(err)
		}
	}
	if _, err := a.BeginPasskeyLogin(ctx, second); !errors.Is(err, ErrLocked) {
		t.Fatal("Passkey failures did not lock beginning")
	}
	if _, err := a.Login(ctx, goodPassword, second); !errors.Is(err, ErrLocked) {
		t.Fatal("Passkey failures did not lock password login")
	}
}

func challengeValue(t *testing.T, options string) string {
	t.Helper()
	var v struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal([]byte(options), &v); err != nil {
		t.Fatal(err)
	}
	return v.PublicKey.Challenge
}

func softwareCredential(t *testing.T, key *ecdsa.PrivateKey, id, userID []byte, challenge, origin string, register bool, count uint32, overrideFlags ...byte) string {
	t.Helper()
	enc := base64.RawURLEncoding.EncodeToString
	typ := "webauthn.get"
	if register {
		typ = "webauthn.create"
	}
	client, _ := json.Marshal(map[string]any{"type": typ, "challenge": challenge, "origin": origin, "crossOrigin": false})
	u, err := url.Parse(origin)
	if err != nil {
		t.Fatal(err)
	}
	rp := sha256.Sum256([]byte(u.Hostname()))
	data := append([]byte{}, rp[:]...)
	flags := byte(5)
	if register {
		flags |= 64
	}
	if len(overrideFlags) > 0 {
		flags = overrideFlags[0]
	}
	data = append(data, flags)
	data = binary.BigEndian.AppendUint32(data, count)
	response := map[string]any{"clientDataJSON": enc(client)}
	if register {
		cose, err := cbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: key.X.FillBytes(make([]byte, 32)), -3: key.Y.FillBytes(make([]byte, 32))})
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, make([]byte, 16)...)
		data = binary.BigEndian.AppendUint16(data, uint16(len(id)))
		data = append(data, id...)
		data = append(data, cose...)
		att, err := cbor.Marshal(map[string]any{"fmt": "none", "attStmt": map[string]any{}, "authData": data})
		if err != nil {
			t.Fatal(err)
		}
		response["attestationObject"] = enc(att)
	} else {
		clientHash := sha256.Sum256(client)
		signed := append(append([]byte{}, data...), clientHash[:]...)
		hash := sha256.Sum256(signed)
		sig, err := ecdsa.SignASN1(rand.Reader, key, hash[:])
		if err != nil {
			t.Fatal(err)
		}
		response["authenticatorData"] = enc(data)
		response["signature"] = enc(sig)
		response["userHandle"] = enc(userID)
	}
	raw, _ := json.Marshal(map[string]any{"id": enc(id), "rawId": enc(id), "type": "public-key", "response": response, "clientExtensionResults": map[string]any{}})
	return string(raw)
}

func TestPasskeyRegistrationLoginOriginProofAndDeletion(t *testing.T) {
	a, st, _ := setup(t)
	ctx := WithWebAuthnOrigin(context.Background(), "https://admin.example")
	from := netip.MustParseAddr("192.0.2.1")
	if err := st.SetAdminPassword(ctx, cheapPHC(goodPassword)); err != nil {
		t.Fatal(err)
	}
	session, err := a.Login(ctx, goodPassword, from)
	if err != nil {
		t.Fatal(err)
	}
	begin, err := a.SecurityAction(ctx, SecurityInput{Action: "passkey_begin", Password: goodPassword}, session, from)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := []byte("credential-identifier-for-test")
	raw := softwareCredential(t, key, id, nil, challengeValue(t, begin.OptionsJSON), "https://admin.example", true, 0)
	if _, err = a.SecurityAction(ctx, SecurityInput{Action: "passkey_register", ChallengeID: begin.ChallengeID, CredentialJSON: raw, Name: "测试密钥"}, session, from); err != nil {
		t.Fatal(err)
	}
	_, state, err := a.readSecurity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	login, err := a.BeginPasskeyLogin(ctx, from)
	if err != nil {
		t.Fatal(err)
	}
	raw = softwareCredential(t, key, id, state.UserID, challengeValue(t, login.OptionsJSON), "https://theme.example", false, 1)
	if _, err = a.FinishPasskeyLogin(ctx, login.ChallengeID, raw, from); !errors.Is(err, ErrSecurity) {
		t.Fatal("foreign origin assertion accepted")
	}
	login, err = a.BeginPasskeyLogin(ctx, from)
	if err != nil {
		t.Fatal(err)
	}
	raw = softwareCredential(t, key, id, state.UserID, challengeValue(t, login.OptionsJSON), "https://admin.example", false, 1, 1)
	if _, err = a.FinishPasskeyLogin(ctx, login.ChallengeID, raw, from); !errors.Is(err, ErrSecurity) {
		t.Fatal("assertion without user verification accepted")
	}
	// 成功的 passkey 登录换一个来源地址：与前面的密码会话区分开，按新会话的 hash 定位行。
	passkeyFrom := netip.MustParseAddr("192.0.2.99")
	login, err = a.BeginPasskeyLogin(ctx, passkeyFrom)
	if err != nil {
		t.Fatal(err)
	}
	raw = softwareCredential(t, key, id, state.UserID, challengeValue(t, login.OptionsJSON), "https://admin.example", false, 1)
	session, err = a.FinishPasskeyLogin(ctx, login.ChallengeID, raw, passkeyFrom)
	if err != nil {
		t.Fatal(err)
	}
	// 签发来源随会话行落库：passkey 会话的 source 就是 Finish 时请求的地址。
	rows, err := st.Sessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].TokenHash != HashToken(session) || rows[0].Source != SourceText(passkeyFrom) {
		t.Fatalf("passkey session rows = %+v, want the new session with source %s", rows, SourceText(passkeyFrom))
	}
	if _, err = a.FinishPasskeyLogin(ctx, login.ChallengeID, raw, passkeyFrom); !errors.Is(err, ErrSecurity) {
		t.Fatal("passkey assertion replay accepted")
	}
	proof, err := a.SecurityAction(ctx, SecurityInput{Action: "reauth_begin"}, session, from)
	if err != nil {
		t.Fatal(err)
	}
	raw = softwareCredential(t, key, id, state.UserID, challengeValue(t, proof.OptionsJSON), "https://admin.example", false, 2)
	proof, err = a.SecurityAction(ctx, SecurityInput{Action: "reauth_finish", ChallengeID: proof.ChallengeID, CredentialJSON: raw}, session, from)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.SecurityAction(ctx, SecurityInput{Action: "passkey_delete", CredentialID: base64.RawURLEncoding.EncodeToString(id), ProofToken: proof.ProofToken}, session, from); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := a.AuthenticateSession(ctx, []string{session}); ok {
		t.Fatal("credential removal retained session")
	}
	_, state, err = a.readSecurity(ctx)
	if err != nil || len(state.Passkeys) != 0 {
		t.Fatal("credential removal did not persist")
	}
}

// 密码登录签发的会话把签发来源写进行里：SourceText 规范文本，与登录通知同一口径。
func TestPasswordLoginRecordsIssuingSource(t *testing.T) {
	a, st, _ := setup(t)
	ctx := context.Background()
	if err := st.SetAdminPassword(ctx, cheapPHC(goodPassword)); err != nil {
		t.Fatal(err)
	}
	from := netip.MustParseAddr("2001:db8::10")
	if _, err := a.Login(ctx, goodPassword, from); err != nil {
		t.Fatal(err)
	}
	rows, err := st.Sessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Source != SourceText(from) {
		t.Fatalf("session rows = %+v, want one session with source %s", rows, SourceText(from))
	}
}

func TestPasskeyFirstSuccessfulRegistrationBindsOrigin(t *testing.T) {
	a, st, _ := setup(t)
	ctx := WithWebAuthnOrigin(context.Background(), "https://ADMIN.example:443")
	from := netip.MustParseAddr("192.0.2.1")
	if err := st.SetAdminPassword(ctx, cheapPHC(goodPassword)); err != nil {
		t.Fatal(err)
	}
	if err := a.ConfigureWebAuthn("https://unused.example"); err != nil {
		t.Fatal(err)
	}
	info, err := a.SecurityInfo(ctx)
	if err != nil || !info.PasskeyAvailable || info.Origin != "" || info.CurrentOrigin != "https://admin.example" {
		t.Fatalf("unbound security info: %+v, %v", info, err)
	}
	if _, err := a.BeginPasskeyLogin(ctx, from); !errors.Is(err, ErrSecurity) {
		t.Fatal("anonymous login initialized an unbound origin")
	}
	session, err := a.Login(ctx, goodPassword, from)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, attempt := range []string{"cancelled", "invalid credential", "different request origin", "successful"} {
		t.Run(attempt, func(t *testing.T) {
			begin, err := a.SecurityAction(ctx, SecurityInput{Action: SecurityPasskeyBegin, Password: goodPassword}, session, from)
			if err != nil {
				t.Fatal(err)
			}
			_, s, err := a.readSecurity(ctx)
			if err != nil || s.Origin != "" || len(s.Passkeys) != 0 {
				t.Fatal("beginning registration changed the binding")
			}
			if attempt == "cancelled" {
				return
			}
			raw := softwareCredential(t, key, []byte("new-credential"), nil, challengeValue(t, begin.OptionsJSON), "https://admin.example", true, 0)
			finishCtx := ctx
			if attempt == "invalid credential" {
				raw = "{}"
			} else if attempt == "different request origin" {
				finishCtx = WithWebAuthnOrigin(ctx, "https://other.example")
			}
			result, err := a.SecurityAction(finishCtx, SecurityInput{Action: SecurityPasskeyRegister, ChallengeID: begin.ChallengeID, CredentialJSON: raw, Name: "new"}, session, from)
			_, s, readErr := a.readSecurity(ctx)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if attempt != "successful" {
				if !errors.Is(err, ErrSecurity) || s.Origin != "" || len(s.Passkeys) != 0 {
					t.Fatalf("failed registration altered binding: state = %+v, err = %v", s, err)
				}
				return
			}
			if err != nil || !result.RevokeSession || s.Origin != "https://admin.example" || len(s.Passkeys) != 1 {
				t.Fatalf("successful registration did not atomically bind: state = %+v, err = %v", s, err)
			}
		})
	}
	if _, ok, err := a.AuthenticateSession(ctx, []string{session}); err != nil || ok {
		t.Fatal("registration retained previous session")
	}
	other := WithWebAuthnOrigin(ctx, "https://other.example")
	info, err = a.SecurityInfo(other)
	if err != nil || info.PasskeyAvailable || info.UnavailableReason != "origin_mismatch" || info.Origin != "https://admin.example" || info.CurrentOrigin != "https://other.example" {
		t.Fatalf("wrong domain not diagnosed: %+v, %v", info, err)
	}
	if _, err = a.BeginPasskeyLogin(other, from); !errors.Is(err, ErrSecurity) {
		t.Fatal("foreign request origin began bound login")
	}
	// 有持久绑定时 auth 层以绑定为准，绑定短路先于格式校验，连非法的旧参数也不在这一层报错。启动层不靠这一点：serve 的
	// parseServeOptions 不看绑定就拒绝非法的 --admin-origin，非法值到不了这里。
	if err = a.ConfigureWebAuthn("not a valid old flag"); err != nil {
		t.Fatal("old flag was validated despite the persistent binding", err)
	}
	info, err = a.SecurityInfo(ctx)
	if err != nil || !info.PasskeyAvailable || info.Origin != "https://admin.example" {
		t.Fatal("old flag changed persistent binding")
	}
	stored, err := st.AdminSecurity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var persisted map[string]any
	if err := json.Unmarshal([]byte(stored.Data), &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted["rp_id"] != "admin.example" {
		t.Fatalf("RP ID was not persisted with its origin: %v", persisted["rp_id"])
	}
}

func TestPasskeyLegacyOriginMigration(t *testing.T) {
	for _, oldOrigin := range []string{"", "https://ADMIN.example.:443"} {
		t.Run(oldOrigin, func(t *testing.T) {
			a, st, _ := setup(t)
			ctx := WithWebAuthnOrigin(context.Background(), "https://admin.example")
			if err := st.SetAdminPassword(ctx, cheapPHC(goodPassword)); err != nil {
				t.Fatal(err)
			}
			b, s, err := a.readSecurity(ctx)
			if err != nil {
				t.Fatal(err)
			}
			s.Passkeys = []Passkey{{Name: "legacy", Credential: webauthn.Credential{ID: []byte("legacy-key")}}}
			if err = a.commitSecurity(ctx, b, s, false, nil, ""); err != nil {
				t.Fatal(err)
			}
			if err = a.ConfigureWebAuthn(oldOrigin); err != nil {
				t.Fatal(err)
			}
			info, err := a.SecurityInfo(ctx)
			if err != nil || len(info.Passkeys) != 1 {
				t.Fatal("legacy credentials were lost")
			}
			if oldOrigin == "" {
				if info.Origin != "" || info.PasskeyAvailable || info.UnavailableReason != "binding_missing" {
					t.Fatalf("legacy missing origin guessed from request: %+v", info)
				}
				if _, err := a.BeginPasskeyLogin(ctx, netip.MustParseAddr("192.0.2.1")); !errors.Is(err, ErrSecurity) {
					t.Fatal("unbound legacy credential used for login")
				}
			} else if info.Origin != "https://admin.example" || !info.PasskeyAvailable {
				t.Fatalf("trusted origin not imported: %+v", info)
			}
			_, state, err := a.readSecurity(ctx)
			if err != nil || (oldOrigin != "" && state.RPID != "admin.example") || (oldOrigin == "" && state.RPID != "") {
				t.Fatalf("legacy RP ID did not follow trusted migration input: %q, %v", state.RPID, err)
			}
		})
	}
}

// 旧来源只导入一次：无绑定、有旧凭据时合法来源落成绑定；此后再给另一个合法来源，绑定不变也不报错——换域名只能走
// 重新认证后的显式改绑，启动参数改不动它。
func TestConfigureWebAuthnImportsOnceThenKeepsTheBinding(t *testing.T) {
	a, st, _ := setup(t)
	ctx := context.Background()
	if err := st.SetAdminPassword(ctx, cheapPHC(goodPassword)); err != nil {
		t.Fatal(err)
	}
	b, s, err := a.readSecurity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.Passkeys = []Passkey{{Name: "legacy", Credential: webauthn.Credential{ID: []byte("legacy-key")}}}
	if err = a.commitSecurity(ctx, b, s, false, nil, ""); err != nil {
		t.Fatal(err)
	}
	if err = a.ConfigureWebAuthn("https://admin.example"); err != nil {
		t.Fatal(err)
	}
	_, s, err = a.readSecurity(ctx)
	if err != nil || s.Origin != "https://admin.example" || s.RPID != "admin.example" {
		t.Fatalf("legacy origin not imported: origin %q, rp %q, %v", s.Origin, s.RPID, err)
	}
	if err = a.ConfigureWebAuthn("https://other.example"); err != nil {
		t.Fatal("a different legacy origin was rejected despite the persistent binding", err)
	}
	_, s, err = a.readSecurity(ctx)
	if err != nil || s.Origin != "https://admin.example" || s.RPID != "admin.example" || len(s.Passkeys) != 1 {
		t.Fatalf("a different legacy origin changed the persistent binding: origin %q, rp %q, %d passkeys, %v", s.Origin, s.RPID, len(s.Passkeys), err)
	}
}

func TestPasskeyRebindingRequiresPasswordAndCommitsAtomically(t *testing.T) {
	a, st, _ := setup(t)
	ctx := WithWebAuthnOrigin(context.Background(), "https://old.example")
	newCtx := WithWebAuthnOrigin(ctx, "https://new.example:8443")
	from := netip.MustParseAddr("192.0.2.1")
	if err := st.SetAdminPassword(ctx, cheapPHC(goodPassword)); err != nil {
		t.Fatal(err)
	}
	b, s, err := a.readSecurity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.Origin = "https://old.example"
	s.RPID = "old.example"
	s.UserID = []byte("existing-admin-identity")
	s.Passkeys = []Passkey{{Name: "old", Credential: webauthn.Credential{ID: []byte("old-key")}}}
	if err = a.commitSecurity(ctx, b, s, false, nil, ""); err != nil {
		t.Fatal(err)
	}
	session, err := a.Login(newCtx, goodPassword, from)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err = a.readSecurity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := a.putChallenge(securityChallenge{Kind: "proof", Session: session, Generation: b.Generation, Origin: "https://new.example:8443"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.SecurityAction(newCtx, SecurityInput{Action: SecurityPasskeyRebindBegin, ProofToken: proof}, session, from); !errors.Is(err, ErrSecurity) {
		t.Fatal("proof token replaced password during rebind")
	}
	if _, err = a.SecurityAction(newCtx, SecurityInput{Action: SecurityPasskeyBegin, Password: goodPassword}, session, from); !errors.Is(err, ErrSecurity) {
		t.Fatal("ordinary registration replaced another domain binding")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, valid := range []bool{false, true} {
		begin, err := a.SecurityAction(newCtx, SecurityInput{Action: SecurityPasskeyRebindBegin, Password: goodPassword}, session, from)
		if err != nil {
			t.Fatal(err)
		}
		_, before, err := a.readSecurity(ctx)
		if err != nil || before.Origin != "https://old.example" || len(before.Passkeys) != 1 || before.Passkeys[0].Name != "old" {
			t.Fatal("rebind beginning replaced old credentials")
		}
		raw := "{}"
		if valid {
			raw = softwareCredential(t, key, []byte("replacement-key"), nil, challengeValue(t, begin.OptionsJSON), "https://new.example:8443", true, 0)
		}
		out, err := a.SecurityAction(newCtx, SecurityInput{Action: SecurityPasskeyRegister, ChallengeID: begin.ChallengeID, CredentialJSON: raw, Name: "replacement"}, session, from)
		if !valid {
			if !errors.Is(err, ErrSecurity) {
				t.Fatal("invalid replacement credential accepted")
			}
			_, after, err := a.readSecurity(ctx)
			if err != nil || after.Origin != before.Origin || len(after.Passkeys) != 1 || after.Passkeys[0].Name != "old" {
				t.Fatal("failed rebind changed persisted credentials")
			}
			continue
		}
		if err != nil || !out.RevokeSession {
			t.Fatalf("successful rebind: %+v, %v", out, err)
		}
	}
	_, s, err = a.readSecurity(newCtx)
	if err != nil || s.Origin != "https://new.example:8443" || s.RPID != "new.example" || len(s.Passkeys) != 1 || s.Passkeys[0].Name != "replacement" {
		t.Fatalf("rebind did not replace binding and credentials together: %+v, %v", s, err)
	}
	if _, ok, err := a.AuthenticateSession(ctx, []string{session}); err != nil || ok {
		t.Fatal("rebind retained previous session")
	}
	if _, err := a.BeginPasskeyLogin(ctx, from); !errors.Is(err, ErrSecurity) {
		t.Fatal("old domain remained available after rebind")
	}
	login, err := a.BeginPasskeyLogin(newCtx, from)
	if err != nil {
		t.Fatal(err)
	}
	raw := softwareCredential(t, key, []byte("replacement-key"), s.UserID, challengeValue(t, login.OptionsJSON), "https://new.example:8443", false, 1)
	if _, err := a.FinishPasskeyLogin(newCtx, login.ChallengeID, raw, from); err != nil {
		t.Fatal("replacement credential could not authenticate", err)
	}
}

func TestPasskeyRejectsInconsistentPersistedBinding(t *testing.T) {
	for _, rpID := range []string{"", "example", "other.example"} {
		t.Run(rpID, func(t *testing.T) {
			a, st, _ := setup(t)
			ctx := WithWebAuthnOrigin(context.Background(), "https://admin.example")
			from := netip.MustParseAddr("192.0.2.1")
			if err := st.SetAdminPassword(ctx, cheapPHC(goodPassword)); err != nil {
				t.Fatal(err)
			}
			b, state, err := a.readSecurity(ctx)
			if err != nil {
				t.Fatal(err)
			}
			state.Origin, state.RPID = "https://admin.example", rpID
			state.Passkeys = []Passkey{{Credential: webauthn.Credential{ID: []byte("bound-key")}}}
			if err := a.commitSecurity(ctx, b, state, false, nil, ""); err != nil {
				t.Fatal(err)
			}
			info, err := a.SecurityInfo(ctx)
			if err != nil || info.PasskeyAvailable || info.UnavailableReason != "binding_invalid" {
				t.Fatalf("inconsistent binding was available: %+v, %v", info, err)
			}
			if _, err := a.BeginPasskeyLogin(ctx, from); !errors.Is(err, ErrSecurity) {
				t.Fatal("inconsistent persisted RP ID began login")
			}
			session, err := a.Login(ctx, goodPassword, from)
			if err != nil {
				t.Fatal("inconsistent passkey binding prevented password recovery", err)
			}
			for _, action := range []SecurityActionKind{SecurityPasskeyBegin, SecurityReauthBegin} {
				if _, err := a.SecurityAction(ctx, SecurityInput{Action: action, Password: goodPassword}, session, from); !errors.Is(err, ErrSecurity) {
					t.Fatalf("inconsistent RP ID accepted action %s: %v", action, err)
				}
			}
			begin, err := a.SecurityAction(ctx, SecurityInput{Action: SecurityPasskeyRebindBegin, Password: goodPassword}, session, from)
			if err != nil {
				t.Fatal("explicit rebind recovery was unavailable", err)
			}
			a.security.mu.Lock()
			challenge := a.security.pending[begin.ChallengeID]
			a.security.mu.Unlock()
			if challenge.Origin != "https://admin.example" || challenge.RPID != "admin.example" || !challenge.Rebind {
				t.Fatalf("rebind did not freeze the candidate configuration: %+v", challenge)
			}
		})
	}
}

func TestPasskeyRebindingConsumesEnabledSecondFactor(t *testing.T) {
	for _, factor := range []string{"missing", "totp", "recovery"} {
		t.Run(factor, func(t *testing.T) {
			a, st, clk := setup(t)
			ctx := WithWebAuthnOrigin(context.Background(), "https://new.example")
			from := netip.MustParseAddr("192.0.2.1")
			if err := st.SetAdminPassword(ctx, cheapPHC(goodPassword)); err != nil {
				t.Fatal(err)
			}
			b, s, err := a.readSecurity(ctx)
			if err != nil {
				t.Fatal(err)
			}
			s.Origin = "https://old.example"
			s.RPID = "old.example"
			s.Secret = "JBSWY3DPEHPK3PXP"
			s.Passkeys = []Passkey{{Name: "old", Credential: webauthn.Credential{ID: []byte("old-key")}}}
			recovery := newRecovery(&s)
			if err = a.commitSecurity(ctx, b, s, false, nil, ""); err != nil {
				t.Fatal(err)
			}
			session, err := a.LoginFactors(ctx, goodPassword, "", recovery[0], from)
			if err != nil {
				t.Fatal(err)
			}
			in := SecurityInput{Action: SecurityPasskeyRebindBegin, Password: goodPassword}
			if factor == "totp" {
				in.OTP = totpCode(s.Secret, clk.Now().Unix()/30)
			} else if factor == "recovery" {
				in.RecoveryCode = recovery[1]
			}
			result, err := a.SecurityAction(ctx, in, session, from)
			_, after, readErr := a.readSecurity(ctx)
			if readErr != nil || after.Origin != s.Origin || len(after.Passkeys) != 1 {
				t.Fatal("rebind preparation altered the existing binding")
			}
			if factor == "missing" {
				if !errors.Is(err, ErrSecurity) || result.ChallengeID != "" || len(after.Recovery) != 9 {
					t.Fatal("password alone bypassed enabled second factor")
				}
				return
			}
			if err != nil || result.ChallengeID == "" {
				t.Fatalf("valid second factor rejected: %v", err)
			}
			if factor == "totp" && after.LastStep != clk.Now().Unix()/30 {
				t.Fatal("TOTP consumption was not persisted")
			}
			if factor == "recovery" && len(after.Recovery) != 8 {
				t.Fatal("recovery consumption was not persisted")
			}
			if _, err = a.SecurityAction(ctx, in, session, from); !errors.Is(err, ErrSecurity) {
				t.Fatal("rebind reused a consumed factor")
			}
		})
	}
}

func TestPasskeyRebindingRejectsRevokedSession(t *testing.T) {
	a, st, _ := setup(t)
	ctx := WithWebAuthnOrigin(context.Background(), "https://new.example")
	from := netip.MustParseAddr("192.0.2.1")
	if err := st.SetAdminPassword(ctx, cheapPHC(goodPassword)); err != nil {
		t.Fatal(err)
	}
	b, s, err := a.readSecurity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.Origin = "https://old.example"
	s.RPID = "old.example"
	s.Passkeys = []Passkey{{Name: "old", Credential: webauthn.Credential{ID: []byte("old-key")}}}
	if err = a.commitSecurity(ctx, b, s, false, nil, ""); err != nil {
		t.Fatal(err)
	}
	session, err := a.Login(ctx, goodPassword, from)
	if err != nil {
		t.Fatal(err)
	}
	begin, err := a.SecurityAction(ctx, SecurityInput{Action: SecurityPasskeyRebindBegin, Password: goodPassword}, session, from)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Logout(ctx, session); err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw := softwareCredential(t, key, []byte("replacement-key"), nil, challengeValue(t, begin.OptionsJSON), "https://new.example", true, 0)
	if _, err := a.SecurityAction(ctx, SecurityInput{Action: SecurityPasskeyRegister, ChallengeID: begin.ChallengeID, CredentialJSON: raw, Name: "replacement"}, session, from); !errors.Is(err, store.ErrAdminChanged) {
		t.Fatalf("revoked session completed rebind: %v", err)
	}
	_, s, err = a.readSecurity(ctx)
	if err != nil || s.Origin != "https://old.example" || len(s.Passkeys) != 1 || s.Passkeys[0].Name != "old" {
		t.Fatal("failed rebind transaction changed the old binding")
	}
}
