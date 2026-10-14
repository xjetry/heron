import { useMutation, useTransport } from "@connectrpc/connect-query";
import { Code } from "@connectrpc/connect";
import { useQueryClient } from "@tanstack/react-query";
import { type FormEvent, useState } from "react";
import { useNavigate } from "react-router";
import { AdminService } from "../gen/heron/v1/admin_pb";
import { errorText } from "../api/auth";
import { createClient } from "@connectrpc/connect";
import { passkeyCredential } from "../lib/passkey";
import { HeronMark } from "../components/HeronMark";
import { Icon } from "../components/Icon";

export function Login() {
  const transport = useTransport();
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const [password, setPassword] = useState("");
  const [otp, setOTP] = useState("");
  const [recoveryCode, setRecoveryCode] = useState("");
  const [passkeyBusy, setPasskeyBusy] = useState(false);
  const [passkeyError, setPasskeyError] = useState("");
  const passkeyLogin = async () => {
    setPasskeyBusy(true); setPasskeyError("");
    try {
      const client = createClient(AdminService, transport);
      const start = await client.beginPasskeyLogin({});
      const credentialJson = await passkeyCredential(start.optionsJson);
      await client.finishPasskeyLogin({ challengeId: start.challengeId, credentialJson });
      queryClient.clear(); void navigate("/", { replace: true });
    } catch (error) { setPasskeyError(errorText(error)); }
    finally { setPasskeyBusy(false); }
  };
  const login = useMutation(AdminService.method.login, {
    onSuccess: () => { queryClient.clear(); void navigate("/", { replace: true }); },
  });
  const onSubmit = (e: FormEvent) => {
    e.preventDefault();
    login.mutate({ password, otp, recoveryCode });
  };
  return (
    <main className="login">
      <form onSubmit={onSubmit} className="card login-card">
        <h1 className="brand"><HeronMark />Heron</h1>
        <p className="login-description">轻量自托管主机监控</p>
        <label>
          管理员密码
          <input
            type="password"
            autoComplete="current-password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            autoFocus
          />
        </label>
        <label>动态验证码（已启用 TOTP 时必填）<input autoComplete="one-time-code" inputMode="numeric" value={otp} onChange={(e) => { setOTP(e.target.value); setRecoveryCode(""); }} /></label>
        <details><summary>使用一次性恢复码</summary><label>恢复码<input autoComplete="off" value={recoveryCode} onChange={(e) => { setRecoveryCode(e.target.value); setOTP(""); }} /></label></details>
        <button className="primary-button" type="submit" disabled={login.isPending || passkeyBusy || password === ""}>
          登录
        </button>
        <button className="passkey-button" type="button" disabled={login.isPending || passkeyBusy} onClick={() => void passkeyLogin()}><Icon name="key" />使用 Passkey 登录</button>
        {passkeyError && <p role="alert" className="error">{passkeyError}</p>}
        {login.error && (
          <p role="alert" className="error">
            {login.error.code === Code.ResourceExhausted ? "登录繁忙，请稍后再试。" : errorText(login.error)}
          </p>
        )}
      </form>
    </main>
  );
}
