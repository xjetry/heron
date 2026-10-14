import { create } from "@bufbuild/protobuf";
import { useQuery } from "@connectrpc/connect-query";
import { useEffect, useState } from "react";
import { Link, Outlet } from "react-router";
import { errorBanner } from "../api/queryGate";
import { PublicService, PublicSiteSchema } from "../gen/heron/v1/public_pb";
import { applySite, DEFAULT_TITLE } from "./site";
import { HeronMark } from "../components/HeronMark";
import { PUBLIC_SCHEME_KEY, readSchemeChoice, writeSchemeChoice } from "../lib/scheme";
import { POLL_MS } from "../lib/poll";
import { GitHubLink } from "../components/GitHubLink";
import { ThemeToggle } from "../components/ThemeToggle";

// 外观只在页面加载时取，之后不再重取：已打开的页面刷新后才看到改动，刷新时浏览器还可能再用最多 5 分钟的缓存
// （hub 对 GetSite 下发 max-age=300）。断网重连与重新挂载默认只重取已过期查询，staleTime: Infinity 阻止这两类重取。
// GetSite 没有轮询，queryDefaults 的聚焦策略对它返回 false；只有轮询查询才在回前台时立即补取。
//
// 没取到站点设置（加载中、失败或被限流）与设置全部为空是同一个状态：文档标题与页头都按内置外观，从同一份值得出。
// PublicLayout 只装配站点外观与导航：顶栏是站点标识、轮询间隔、仓库链接、明暗切换和登录入口，
// 没有告警铃铛或手动刷新。实时节点数据由子路由按公开快照读取，移动端只改变排布，不增加管理端字段来源。
export function PublicLayout() {
  const site = useQuery(PublicService.method.getSite, {}, { staleTime: Infinity });
  // 访客选择由 forcedTheme 优先于站点设置合成；存储不可用时仍由组件状态驱动当前页面。
  const [choice, setChoice] = useState(() => readSchemeChoice(PUBLIC_SCHEME_KEY));
  useEffect(() => applySite(site.data ?? create(PublicSiteSchema), choice), [site.data, choice]);
  return (
    <div className="layout public-layout">
      <header className="public-header">
        <Link to="/" className="brand">
          {site.data?.logo ? <img src={site.data.logo} alt="" className="logo" /> : <HeronMark />}
          {site.data?.title || DEFAULT_TITLE}
        </Link>
        {/* Overview 按 POLL_MS 自动刷新快照，顶栏仅说明刷新周期。 */}
        <span className="live muted">实时 · 每 {POLL_MS / 1000} 秒</span>
        <GitHubLink />
        <ThemeToggle choice={choice} onChange={(next) => { writeSchemeChoice(PUBLIC_SCHEME_KEY, next); setChoice(next); }} />
        {/* 面板是另一个前端入口，用普通链接整页跳转；只接受 hub 给出的面板路径。 */}
        {site.data?.adminPath && <a href={site.data.adminPath} className="admin">登录</a>}
      </header>
      <main className="main">
        {errorBanner(site.error)}
        <Outlet />
      </main>
    </div>
  );
}

export function NotFound() {
  return <p className="muted">页面不存在。<Link to="/">返回总览</Link></p>;
}
