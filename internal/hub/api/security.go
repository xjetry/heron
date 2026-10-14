package api

import (
	"context"
	"encoding/base64"
	"errors"
	"time"

	"connectrpc.com/connect"
	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/auth"
	"github.com/xjetry/heron/internal/hub/store"
)

func securityError(err error) error {
	if errors.Is(err, auth.ErrLoginBusy) {
		return connect.NewError(connect.CodeResourceExhausted, err)
	}
	if errors.Is(err, auth.ErrSecurity) || errors.Is(err, auth.ErrBadPassword) || errors.Is(err, auth.ErrNoAdmin) || errors.Is(err, auth.ErrLocked) || errors.Is(err, store.ErrAdminChanged) {
		return unauthenticated("认证失败、已过期或认证配置已改变，请重新证明身份")
	}
	return internalError("认证操作失败")
}
func securityResponse(r auth.SecurityResult) *heronv1.SecurityActionResponse {
	return &heronv1.SecurityActionResponse{ChallengeId: r.ChallengeID, OptionsJson: r.OptionsJSON, TotpSecret: r.TOTPSecret, TotpUri: r.TOTPURI, RecoveryCodes: r.RecoveryCodes, ProofToken: r.ProofToken}
}
func (s *Service) GetSecurity(ctx context.Context, _ *connect.Request[heronv1.GetSecurityRequest]) (*connect.Response[heronv1.GetSecurityResponse], error) {
	info, err := s.auth.SecurityInfo(ctx)
	if err != nil {
		return nil, securityError(err)
	}
	out := &heronv1.GetSecurityResponse{TotpEnabled: info.TOTPEnabled, PasskeyAvailable: info.PasskeyAvailable, RecoveryCodesRemaining: uint32(info.RecoveryRemaining), Origin: info.Origin, CurrentOrigin: info.CurrentOrigin, UnavailableReason: info.UnavailableReason}
	for _, p := range info.Passkeys {
		out.Passkeys = append(out.Passkeys, &heronv1.SecurityCredential{Id: base64.RawURLEncoding.EncodeToString(p.Credential.ID), Name: p.Name})
	}
	return connect.NewResponse(out), nil
}
func (s *Service) BeginPasskeyLogin(ctx context.Context, _ *connect.Request[heronv1.BeginPasskeyLoginRequest]) (*connect.Response[heronv1.BeginPasskeyLoginResponse], error) {
	r, err := s.auth.BeginPasskeyLogin(ctx, ctx.Value(peerKey{}).(peerInfo).from)
	if err != nil {
		return nil, securityError(err)
	}
	return connect.NewResponse(&heronv1.BeginPasskeyLoginResponse{ChallengeId: r.ChallengeID, OptionsJson: r.OptionsJSON}), nil
}
func (s *Service) FinishPasskeyLogin(ctx context.Context, req *connect.Request[heronv1.FinishPasskeyLoginRequest]) (*connect.Response[heronv1.FinishPasskeyLoginResponse], error) {
	if len(req.Msg.CredentialJson) > 65536 || len(req.Msg.ChallengeId) > 128 {
		return nil, invalid("Passkey 响应过大")
	}
	peer := ctx.Value(peerKey{}).(peerInfo)
	token, err := s.auth.FinishPasskeyLogin(ctx, req.Msg.ChallengeId, req.Msg.CredentialJson, peer.from)
	if err != nil {
		return nil, securityError(err)
	}
	resp := connect.NewResponse(&heronv1.FinishPasskeyLoginResponse{})
	resp.Header().Add("Set-Cookie", sessionCookie(token, peer.scheme == "https", int(auth.SessionAbsolute/time.Second)).String())
	return resp, nil
}
func (s *Service) SecurityAction(ctx context.Context, req *connect.Request[heronv1.SecurityActionRequest]) (*connect.Response[heronv1.SecurityActionResponse], error) {
	in := req.Msg
	if len(in.Password) > 4096 || len(in.Otp) > 16 || len(in.RecoveryCode) > 128 || len(in.CredentialJson) > 65536 || len(in.Name) > 128 || len(in.CredentialId) > 2048 || len(in.ChallengeId) > 128 || len(in.ProofToken) > 128 {
		return nil, invalid("认证字段过大")
	}
	actions := map[heronv1.SecurityActionKind]auth.SecurityActionKind{
		heronv1.SecurityActionKind_SECURITY_ACTION_KIND_TOTP_BEGIN:           auth.SecurityTOTPBegin,
		heronv1.SecurityActionKind_SECURITY_ACTION_KIND_TOTP_ENABLE:          auth.SecurityTOTPEnable,
		heronv1.SecurityActionKind_SECURITY_ACTION_KIND_TOTP_DISABLE:         auth.SecurityTOTPDisable,
		heronv1.SecurityActionKind_SECURITY_ACTION_KIND_PASSKEY_BEGIN:        auth.SecurityPasskeyBegin,
		heronv1.SecurityActionKind_SECURITY_ACTION_KIND_PASSKEY_REBIND_BEGIN: auth.SecurityPasskeyRebindBegin,
		heronv1.SecurityActionKind_SECURITY_ACTION_KIND_PASSKEY_REGISTER:     auth.SecurityPasskeyRegister,
		heronv1.SecurityActionKind_SECURITY_ACTION_KIND_PASSKEY_DELETE:       auth.SecurityPasskeyDelete,
		heronv1.SecurityActionKind_SECURITY_ACTION_KIND_RECOVERY_REGENERATE:  auth.SecurityRecoveryRegenerate,
		heronv1.SecurityActionKind_SECURITY_ACTION_KIND_REAUTH_BEGIN:         auth.SecurityReauthBegin,
		heronv1.SecurityActionKind_SECURITY_ACTION_KIND_REAUTH_FINISH:        auth.SecurityReauthFinish,
	}
	action, ok := actions[in.Action]
	if !ok {
		return nil, invalid("未知认证操作")
	}
	r, err := s.auth.SecurityAction(ctx, auth.SecurityInput{Action: action, Password: in.Password, OTP: in.Otp, RecoveryCode: in.RecoveryCode, ChallengeID: in.ChallengeId, CredentialJSON: in.CredentialJson, Name: in.Name, CredentialID: in.CredentialId, ProofToken: in.ProofToken}, ctx.Value(sessionKey{}).(string), ctx.Value(peerKey{}).(peerInfo).from)
	if err != nil {
		return nil, securityError(err)
	}
	resp := connect.NewResponse(securityResponse(r))
	if r.RevokeSession {
		clearSessionCookie(ctx, resp.Header())
	}
	return resp, nil
}
