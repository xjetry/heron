import { createConnectQueryKey, useMutation, useQuery } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { type FormEvent, useState } from "react";
import { Link } from "react-router";
import { errorText } from "../api/auth";
import { errorBanner, queryGate } from "../api/queryGate";
import { useLatestError } from "../api/useLatestError";
import { InstallCommands } from "../components/InstallCommands";
import { Drawer } from "../components/Modal";
import { PageHeader } from "../components/PageHeader";
import { Select } from "../components/Select";
import { Secret } from "../components/Secret";
import { AdminService } from "../gen/heron/v1/admin_pb";
import { dateTime } from "../lib/format";

const TTLS = [
  { label: "10 分钟", seconds: 600 },
  { label: "1 小时", seconds: 3600 },
  { label: "24 小时", seconds: 86400 },
  { label: "7 天", seconds: 7 * 86400 },
];

export function RegisterWindow() {
  const qc = useQueryClient();
  const refresh = () => qc.invalidateQueries({ queryKey: createConnectQueryKey({ schema: AdminService.method.getRegisterWindow, cardinality: "finite" }) });
  const status = useQuery(AdminService.method.getRegisterWindow, {}, { refetchInterval: 10_000 });
  // hub 版本在进程生命周期内不变，不轮询。
  const snapshot = useQuery(AdminService.method.getSnapshot, {});
  const [drawerOpener, setDrawerOpener] = useState<HTMLElement | null>(null);
  const [key, setKey] = useState<string | null>(null);
  // 轮询看到窗口关了（到期、名额用完或在别处关闭），这把 key 已不能注册，在渲染期清掉，不先画出一帧失效的 key。
  const windowOpen = status.data?.open;
  const [seenOpen, setSeenOpen] = useState(windowOpen);
  if (seenOpen !== windowOpen) {
    setSeenOpen(windowOpen);
    if (windowOpen === false) setKey(null);
  }
  // 开窗与关窗是操作，错误生命周期独立于窗口状态的轮询。
  const { error, mutationOptions } = useLatestError();
  const open = useMutation(AdminService.method.openRegisterWindow, {
    ...mutationOptions,
    onSuccess: async (r) => { setKey(r.key); await refresh(); setDrawerOpener(null); },
  });
  const close = useMutation(AdminService.method.closeRegisterWindow, {
    ...mutationOptions,
    onSuccess: () => { setKey(null); void refresh(); },
  });
  const gate = queryGate(status);
  if (!gate.ready) return gate.loading ?? errorBanner(...gate.errors);
  // 命令区域只依赖快照；外壳（key、开窗表单）不等它。未就绪时不拼命令：用空版本先渲染 latest 命令
  // 再在快照到达后变成钉版本的命令，会让先复制的人装上与 hub 不同的版本。
  const snap = queryGate(snapshot);
  return (
    <section>
      <PageHeader title="批量添加节点" description="一次接入多台服务器，不用逐台在面板里建节点。" actions={<button type="button" className="primary-button" disabled={gate.data.open || open.isPending || close.isPending}
        onClick={(e) => { open.reset(); setDrawerOpener(e.currentTarget); }}>开启接入窗口</button>} />
      <section className="card register-guide" aria-label="使用说明">
        <p>开启一个限时、限量的接入窗口后，页面给出一条安装命令。在每台要监控的服务器上以 root 执行一次，它就自动注册为一个新节点，名称默认取主机名（命令末尾加 <code>--name 名称</code> 可另行指定）。</p>
        <p className="muted">窗口到期、名额用完或手动关闭后，这条命令不能再注册新节点；已经接入的节点不受影响。只加一台、或想先填好名称与费用再安装，到 <Link to="/nodes">节点</Link> 页点「添加节点」。</p>
      </section>
      <section className="card" aria-label="窗口状态">
        {gate.data.open ? (
          <p>接入窗口开启中：还能接入 {gate.data.remaining} 台，截止 {dateTime(gate.data.expiresAt)}。{" "}
            <button type="button" className="danger" onClick={() => close.mutate({})} disabled={close.isPending}>关闭接入窗口</button>
          </p>
        ) : (
          <p className="muted">当前没有开启的接入窗口。</p>
        )}
      </section>
      {key && (
        <>
          <Secret label="注册 key" value={key} />
          <p>在每台要监控的服务器上以 root 执行（agent 若经其他地址访问 hub，在下面的 Hub 连接地址里填写）：</p>
          {snap.ready ? (
            <InstallCommands hubVersion={snap.data.hubVersion} boundAgentVersion={snap.data.boundAgentVersion} origin={window.location.origin} registerKey={key} banner={snap.banner} />
          ) : (
            snap.loading ?? errorBanner(...snap.errors)
          )}
        </>
      )}
      {drawerOpener && <RegisterWindowDrawer opener={drawerOpener} pending={open.isPending} error={open.error}
        onClose={() => setDrawerOpener(null)} onOpen={(ttlS, maxNodes) => open.mutate({ ttlS, maxNodes })} />}
      {gate.banner}
      {!drawerOpener && error != null && <p role="alert" className="error">{errorText(error)}</p>}
    </section>
  );
}

function RegisterWindowDrawer({ opener, pending, error, onClose, onOpen }: {
  opener: HTMLElement; pending: boolean; error: unknown; onClose: () => void;
  onOpen: (ttlS: number, maxNodes: number) => void;
}) {
  const [ttl, setTtl] = useState(TTLS[1].seconds);
  const [maxNodes, setMaxNodes] = useState(5);
  const submit = (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    if (!e.currentTarget.checkValidity() || pending || !(maxNodes >= 1)) return;
    onOpen(ttl, maxNodes);
  };
  return <Drawer title="开启接入窗口" opener={opener} busy={pending} onClose={onClose}>
    <form onSubmit={submit} aria-label="开启接入窗口">
      <div className="modal-body">
        <Select label="有效期" caption="有效期" value={String(ttl)} options={TTLS.map((t) => ({ value: String(t.seconds), label: t.label }))} onChange={(next) => setTtl(Number(next))} />
        <label>最多接入台数<input type="number" min={1} max={1000} value={Number.isNaN(maxNodes) ? "" : maxNodes} onChange={(e) => setMaxNodes(e.target.valueAsNumber)} /></label>
        {error != null && <p role="alert" className="error">{errorText(error)}</p>}
      </div>
      <footer className="modal-footer">
        <button type="button" disabled={pending} onClick={onClose}>取消</button>
        <button type="submit" className="primary-button" disabled={pending || !(maxNodes >= 1)}>开启</button>
      </footer>
    </form>
  </Drawer>;
}
