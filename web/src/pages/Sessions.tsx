import { createConnectQueryKey, useMutation, useQuery } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { errorText } from "../api/auth";
import { errorBanner, queryGate } from "../api/queryGate";
import { useLeaveSession } from "../api/useLeaveSession";
import { PageHeader } from "../components/PageHeader";
import { RowMenu } from "../components/RowMenu";
import { AdminService } from "../gen/heron/v1/admin_pb";
import { Link } from "react-router";
import { dateTime } from "../lib/format";
import { EmptyState } from "../components/EmptyState";

export function Sessions() {
  const queryClient = useQueryClient();
  const leaveSession = useLeaveSession();
  const list = useQuery(AdminService.method.listSessions, {});
  const security = useQuery(AdminService.method.getSecurity, {});
  const revoke = useMutation(AdminService.method.revokeSession);
  const gate = queryGate(list);
  if (!gate.ready) return gate.loading ?? errorBanner(...gate.errors);
  const remove = (id: string, current: boolean) => revoke.mutate({ id }, {
    onSuccess: () => {
      if (current) {
        leaveSession();
      } else {
        void queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: AdminService.method.listSessions, cardinality: "finite" }) });
      }
    },
  });
  return (
    <section>
      {gate.banner}
      <PageHeader title="安全" />
      <div className="cards-3">
        <article className="card" aria-label="密码">
          <h2>密码</h2>
          <p className="muted">密码由 hub 的启动配置提供；认证设备全部丢失时由运维者在 hub 主机运行安全重置命令。</p>
        </article>
        <article className="card" aria-label="TOTP">
          <h2>TOTP</h2>
          {security.error ? <p className="error">读取失败：{errorText(security.error)}</p> : security.data
            ? <p>{security.data.totpEnabled ? `已启用，剩余 ${security.data.recoveryCodesRemaining} 个恢复码` : "未启用"}</p>
            : <p className="muted">加载中…</p>}
          <Link to="/security/credentials">管理 TOTP、恢复码与 Passkey</Link>
        </article>
        <article className="card" aria-label="Passkey">
          <h2>Passkey</h2>
          {security.error ? <p className="error">读取失败：{errorText(security.error)}</p> : security.data
            ? <><p>已注册 {security.data.passkeys.length} 个</p><p className="muted">已绑定来源 {security.data.origin || "尚未绑定"}</p></>
            : <p className="muted">加载中…</p>}
          <Link to="/security/credentials">管理 TOTP、恢复码与 Passkey</Link>
        </article>
      </div>
      <p className="muted">有效登录会话。撤销后，该会话立即失效；撤销当前会话会返回登录页。</p>
      <p className="muted">
        本次请求的来源：{gate.data.requestSource}。hub 按 --trusted-proxies 与转发头解析得到；如果它是反代或 CDN
        的地址，请核对可信代理范围与反代的转发头配置——登录锁定与登录通知看到的也是这个值。
      </p>
      {revoke.error && <p role="alert" className="error">{errorText(revoke.error)}</p>}
      {gate.data.sessions.length === 0 ? <EmptyState title="没有有效会话。" /> : <div className="table-scroll" role="region" aria-label="登录会话" tabIndex={0}>
        <table className="nodes">
          <thead><tr><th>会话</th><th>创建于</th><th>最近使用</th><th>来源</th><th><span className="sr-only">操作</span></th></tr></thead>
          <tbody>
            {gate.data.sessions.map((session) => (
              <tr key={session.id}>
                <td data-label="会话"><code title={session.id}>{session.id.slice(0, 12)}</code>{session.current && <> <span className="muted">当前</span></>}</td>
                <td data-label="创建于">{dateTime(session.createdAt)}</td>
                <td data-label="最近使用">{dateTime(session.lastUsedAt)}</td>
                <td data-label="来源">
                  {session.source || (
                    <span className="muted" title="迁移前已存在的会话，或没有请求的签发路径">
                      未记录
                    </span>
                  )}
                </td>
                <td data-label="操作">
                  <RowMenu label={session.id.slice(0, 12)} items={[{
                    label: "撤销会话", danger: true, confirm: `确认撤销会话 ${session.id.slice(0, 12)}`,
                    note: session.current ? "将退出当前登录" : "该会话将立即失效",
                    disabled: revoke.isPending, onSelect: () => remove(session.id, session.current),
                  }]} />
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>}
    </section>
  );
}
