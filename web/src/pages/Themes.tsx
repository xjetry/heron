import { createConnectQueryKey, useMutation, useQuery } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { type FormEvent, useMemo, useRef, useState } from "react";
import { errorText } from "../api/auth";
import { errorBanner, queryGate } from "../api/queryGate";
import { useLatestError } from "../api/useLatestError";
import { ConfirmDelete } from "../components/ConfirmDelete";
import { PageHeader } from "../components/PageHeader";
import { AdminService, type Theme } from "../gen/heron/v1/admin_pb";
import { toBase64 } from "../lib/base64";
import { bytes, dateTime } from "../lib/format";
import { FileInput } from "../components/FileInput";
import { EmptyState } from "../components/EmptyState";
import { downloadText } from "../lib/download";
import { buildThemeSkill, THEME_SKILL_FILENAME } from "../lib/themeSkill";

const maxPackageBytes = 8 * 1024 * 1024;
const versionLabel = (theme: Theme) => `${theme.name}（${theme.id}）${theme.version} [${theme.digest.slice(0, 12)}]`;

function themeStatus(theme: Theme) {
  if (theme.sdk !== 1) return `不支持 SDK ${theme.sdk}，仅归档`;
  if (theme.enabled) return "启用中";
  if (theme.previous) return "可回滚";
  return theme.published ? "曾启用" : "未启用";
}

function Preview({ theme }: { theme: Theme }) {
  const preview = useQuery(AdminService.method.getThemePreview, { id: theme.id, digest: theme.digest }, { enabled: theme.hasPreview });
  const imageURL = useMemo(() => preview.data ? `data:${preview.data.contentType};base64,${toBase64(preview.data.content)}` : "", [preview.data]);
  if (!theme.hasPreview) return <span className="muted">无</span>;
  if (preview.error != null) return <span className="error">{errorText(preview.error)}</span>;
  if (!preview.data) return <span className="muted">加载中…</span>;
  return <img className="theme-preview" alt={`${theme.name} 预览图`} src={imageURL} />;
}

export function Themes() {
  const qc = useQueryClient();
  const list = useQuery(AdminService.method.listThemes, {});
  const backup = useQuery(AdminService.method.getBackupStatus, {}, { refetchInterval: 5000 });
  const fileInput = useRef<HTMLInputElement>(null);
  const [file, setFile] = useState<File | null>(null);
  const [expectId, setExpectId] = useState("");
  const [repository, setRepository] = useState("");
  const [tag, setTag] = useState("");
  const [assetId, setAssetId] = useState("");
  const [githubExpectId, setGitHubExpectId] = useState("");
  const [reading, setReading] = useState(false);
  const [notice, setNotice] = useState<string | null>(null);
  const { error, mutationOptions } = useLatestError();
  const preview = useMutation(AdminService.method.previewTheme, mutationOptions);
  const refresh = () => {
    preview.reset();
    return Promise.all([
      qc.invalidateQueries({ queryKey: createConnectQueryKey({ schema: AdminService.method.listThemes, cardinality: "finite" }) }),
      qc.invalidateQueries({ queryKey: createConnectQueryKey({ schema: AdminService.method.getThemePreview, cardinality: "finite" }) }),
      qc.invalidateQueries({ queryKey: createConnectQueryKey({ schema: AdminService.method.getBackupStatus, cardinality: "finite" }) }),
    ]);
  };
  const installed = (theme?: Theme) => {
    setNotice(`已安装 ${theme?.name ?? ""}（${theme?.id ?? ""}）版本 ${theme?.version ?? ""}，${theme?.enabled ? "当前已启用此产物" : "尚未启用"}`);
    return refresh();
  };
  const upload = useMutation(AdminService.method.uploadTheme, {
    ...mutationOptions,
    onSuccess: (r) => {
      setFile(null); setExpectId("");
      if (fileInput.current) fileInput.current.value = "";
      return installed(r.theme);
    },
  });
  const releases = useMutation(AdminService.method.listThemeReleases, mutationOptions);
  const install = useMutation(AdminService.method.installThemeRelease, { ...mutationOptions, onSuccess: (r) => installed(r.theme) });
  const enable = useMutation(AdminService.method.enableTheme, { ...mutationOptions, onSuccess: () => { setNotice(null); return refresh(); } });
  const remove = useMutation(AdminService.method.deleteThemeVersion, { ...mutationOptions, onSuccess: () => { setNotice(null); return refresh(); } });
  const uninstall = useMutation(AdminService.method.deleteTheme, { ...mutationOptions, onSuccess: () => { setNotice(null); return refresh(); } });
  const archive = useMutation(AdminService.method.getThemePackage, {
    ...mutationOptions,
    onSuccess: (result, request) => {
      const url = URL.createObjectURL(new Blob([new Uint8Array(result.package)], { type: "application/zip" }));
      const link = document.createElement("a");
      link.href = url; link.download = `${request.id}-${request.digest}.zip`;
      try { link.click(); } finally { URL.revokeObjectURL(url); }
    },
  });
  const gate = queryGate(list);
  if (!gate.ready) return gate.loading ?? errorBanner(...gate.errors);
  const { themes, publicDir } = gate.data;
  const enabled = themes.find((t) => t.enabled);
  const targets = [...new Map(themes.map((t) => [t.id, t])).values()];
  const busy = reading || upload.isPending || install.isPending || enable.isPending || remove.isPending || uninstall.isPending || archive.isPending;
  const release = releases.data?.releases.find((r) => r.tag === tag);
  const asset = release?.assets.find((a) => a.id.toString() === assetId);
  const submit = async (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    if (!file || busy) return;
    setNotice(null);
    if (file.size > maxPackageBytes) { setNotice("主题包不能超过 8 MiB。"); return; }
    setReading(true);
    try {
      await upload.mutateAsync({ package: new Uint8Array(await file.arrayBuffer()), expectId });
    } catch (err) {
      setNotice(`上传 ${file.name} 失败：${errorText(err)}`);
    } finally {
      setReading(false);
    }
  };
  const purposeOptions = <><option value="">安装新主题或保留新版本</option>{targets.map((t) => <option key={t.id} value={t.id}>更新 {t.name}（{t.id}）：校验主题 id</option>)}</>;
  return <section>
    {gate.banner}
    <PageHeader title="主题" />
    <section className="card integrations" aria-label="开发主题">
      <div className="integration">
        <p>
          <strong>主题开发指南（给 AI agent）</strong>
          <span className="muted">主题是构建好的静态前端 ZIP，经 hub 提供的 SDK 读取公开数据。这份 skill 格式的指南写明有哪些数据与字段、SDK 与接口地址、沙箱里能做什么、包格式与限制、怎样构建与预览，文中的 hub 地址已填成当前站点。保存为 agent 的 skills 目录下的 heron-theme/SKILL.md（Claude Code 为 ~/.claude/skills/heron-theme/SKILL.md），再让 agent 做一个 Heron 公开页主题；人读也适用。</span>
        </p>
        <button type="button" onClick={() => downloadText(THEME_SKILL_FILENAME, buildThemeSkill(window.location.origin))}>下载主题开发指南</button>
      </div>
    </section>
    {backup.error != null && errorBanner(backup.error)}
    {backup.data?.themesWithoutPackage.map((id) => <p key={id}>主题 {id} 未备份：请重新上传原包</p>)}
    <p>{enabled ? `当前启用 ${enabled.name}（${enabled.id}）版本 ${enabled.version}。` : "当前使用内置公开页。"} <a href="/" target="_blank" rel="noreferrer">打开公开首页</a></p>
    <p className="muted">第三方主题在同域名沙箱内运行，只能通过 SDK 读取公开数据。安装不会自动切换首页；每个主题最多保留 3 个版本，全站最多 20 个主题。包根需包含 index.html 与声明 SDK 1 的 theme.json。</p>
    {publicDir && <p role="note" aria-label="公开页由目录接管" className="card">公开首页由 --public-dir 的目录接管，不能启用托管主题。移除该配置并重启后才可切换；仍可安装和管理版本。</p>}
    {enabled && <button type="button" disabled={busy || publicDir} onClick={() => enable.mutate({ id: "", digest: "" })}>切回内置主题</button>}
    <form className="card" aria-label="GitHub 安装" onSubmit={(e) => {
      e.preventDefault();
      if (!repository.trim() || busy || releases.isPending) return;
      setTag(""); setAssetId(""); setNotice(null); releases.reset();
      releases.mutate({ repository: repository.trim() });
    }}>
      <h2>从 GitHub 安装</h2>
      <div className="row">
        <label>GitHub 仓库或 Release 链接<input value={repository} disabled={busy || releases.isPending} placeholder="owner/repo 或 https://github.com/owner/repo/releases/tag/v1" onChange={(e) => { setRepository(e.target.value); setTag(""); setAssetId(""); releases.reset(); }} /></label>
        <button type="submit" disabled={!repository.trim() || busy || releases.isPending}>{releases.isPending ? "查询中…" : "查询版本"}</button>
      </div>
      <p className="muted">仅公开仓库，最多列出最新 30 个 Release；Release 链接只查指定版本。必须选择作者上传的已构建 ZIP，不下载源码、不执行构建脚本。</p>
      {releases.data && (releases.data.releases.length === 0 ? <p>没有公开的 Release。可上传作者提供的已构建主题包。</p> : <>
        <div className="row">
          <label>Release 版本<select value={tag} disabled={busy || releases.isPending} onChange={(e) => { setTag(e.target.value); setAssetId(""); }}><option value="">请选择版本</option>{releases.data.releases.map((r) => <option key={r.tag} value={r.tag}>{r.name || r.tag} ({r.tag}){r.prerelease ? " · 预发布" : ""}</option>)}</select></label>
          <label>ZIP 资产<select value={assetId} disabled={!release || busy || releases.isPending} onChange={(e) => setAssetId(e.target.value)}><option value="">请选择资产</option>{release?.assets.map((a) => <option key={a.id.toString()} value={a.id.toString()} disabled={a.size > BigInt(maxPackageBytes)}>{a.name} · {bytes(a.size)}{a.size > BigInt(maxPackageBytes) ? " · 超过 8 MiB" : ""}</option>)}</select></label>
          <label>GitHub 安装用途<select value={githubExpectId} disabled={busy} onChange={(e) => setGitHubExpectId(e.target.value)}>{purposeOptions}</select></label>
          <button type="button" disabled={!release || !asset || asset.size > BigInt(maxPackageBytes) || busy || releases.isPending} onClick={() => {
            if (release && asset) { setNotice(null); install.mutate({ repository: repository.trim(), tag: release.tag, assetId: asset.id, expectId: githubExpectId }); }
          }}>{install.isPending ? "安装中…" : "安装所选资产"}</button>
        </div>
        {release?.assets.length === 0 && <p>这个版本没有 ZIP 资产；GitHub 自动生成的源码归档不能安装。</p>}
      </>)}
    </form>
    <form className="card" aria-label="上传主题" onSubmit={(e) => void submit(e)}>
      <h2>上传本地 ZIP</h2>
      <div className="row">
        <FileInput label="主题包（zip，至多 8 MiB）" caption="主题包（zip，至多 8 MiB）" inputRef={fileInput} accept=".zip,application/zip" disabled={busy} fileName={file?.name}
          onChange={(e) => { setFile(e.target.files?.[0] ?? null); setNotice(null); upload.reset(); }} />
        <label>用途<select value={expectId} disabled={busy} onChange={(e) => setExpectId(e.target.value)}>{purposeOptions}</select></label>
        <button type="submit" disabled={!file || busy}>{reading || upload.isPending ? "上传中…" : "上传"}</button>
      </div>
      <p className="muted">上传须在 30 秒内传完（hub 的请求读取时限）：8 MiB 的包约需 3 Mbit/s 的上行。</p>
    </form>
    {notice != null && <p role="status">{notice}</p>}
    {error != null && <p role="alert" className="error">{errorText(error)}</p>}
    {preview.data?.url && <p><a href={preview.data.url} target="_blank" rel="noreferrer">打开沙箱预览</a>（只预览所选产物，不改变公开首页；链接会过期。）</p>}
    {themes.length === 0 ? <EmptyState title="还没有主题。" /> : <div className="table-scroll" role="region" aria-label="主题管理" tabIndex={0}>
      <table className="nodes"><thead><tr><th>预览</th><th>名称</th><th>id</th><th>版本 / 摘要</th><th>来源</th><th>安装于</th><th>状态</th><th>操作</th></tr></thead><tbody>
        {themes.map((t, index) => {
          const executable = t.sdk === 1, protectedVersion = t.enabled || t.previous, label = versionLabel(t);
          return <tr key={`${t.id}/${t.digest}`}>
            <td><Preview theme={t} /></td><td>{t.name}</td><td><code>{t.id}</code></td>
            <td>{t.version}<br /><code title={t.digest}>{t.digest.slice(0, 12)}</code></td>
            <td>{t.repository ? `${t.repository} · ${t.release} · ${t.asset}` : "本地上传"}</td>
            <td className="muted">{dateTime(t.uploadedAt)}</td>
            <td>{themeStatus(t)}</td>
            <td>
              <button type="button" className="link" aria-label={`下载原包 ${label}`} disabled={!t.digest || busy} onClick={() => archive.mutate({ id: t.id, digest: t.digest })}>下载原包</button>{" "}
              <button type="button" className="link" aria-label={`预览 ${label}`} disabled={!executable || busy || preview.isPending} onClick={() => { preview.reset(); preview.mutate({ id: t.id, digest: t.digest }); }}>预览</button>{" "}
              {!t.enabled && <button type="button" className="link" aria-label={`${t.previous ? "回滚" : "启用"} ${label}`} disabled={!executable || publicDir || busy} onClick={() => enable.mutate({ id: t.id, digest: t.digest })}>{t.previous ? "回滚" : "启用"}</button>}{" "}
              {protectedVersion ? <button type="button" className="link" aria-label={`删除 ${label}`} disabled title="当前和上一版本受保护，不能删除">删除</button> : <ConfirmDelete label={`删除 ${label}`} confirm={`确认删除 ${label}`} pending={busy} onDelete={() => remove.mutate({ id: t.id, digest: t.digest })} />}
              {themes.findIndex((other) => other.id === t.id) === index && <>{" "}<ConfirmDelete verb="卸载全部版本" label={`卸载全部版本 ${t.name}（${t.id}）`} confirm={`确认卸载全部版本 ${t.name}（${t.id}）`} note="将删除该主题的全部版本；若正在启用则回落到内置页。此操作无法撤销。" pending={busy} onDelete={() => uninstall.mutate({ id: t.id })} /></>}
            </td>
          </tr>;
        })}
      </tbody></table>
    </div>}
  </section>;
}
