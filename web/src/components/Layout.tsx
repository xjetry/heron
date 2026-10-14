import { useMutation } from "@connectrpc/connect-query";
import { useEffect, useState } from "react";
import { NavLink, Outlet, useLocation } from "react-router";
import { AdminService } from "../gen/heron/v1/admin_pb";
import { errorText } from "../api/auth";
import { useLeaveSession } from "../api/useLeaveSession";
import { HeronMark } from "./HeronMark";
import { Icon, type IconName } from "./Icon";
import { Modal } from "./Modal";
import { GitHubLink } from "./GitHubLink";
import { ThemeToggle } from "./ThemeToggle";
import { QuickSearch } from "./QuickSearch";
import { ADMIN_SCHEME_KEY, readSchemeChoice, writeSchemeChoice } from "../lib/scheme";

const navigation: { label: string; items: { to: string; label: string; icon: IconName }[] }[] = [
  { label: "监控", items: [{ to: "/", label: "总览", icon: "overview" }, { to: "/nodes", label: "节点", icon: "server" }, { to: "/renewals", label: "续费日历", icon: "calendar" }, { to: "/register", label: "批量添加节点", icon: "plus" }, { to: "/probes", label: "探测任务", icon: "activity" }] },
  { label: "告警", items: [{ to: "/alerts", label: "告警规则", icon: "bell" }, { to: "/events", label: "告警事件", icon: "history" }, { to: "/silences", label: "维护静默", icon: "moon" }, { to: "/channels", label: "通知渠道", icon: "send" }] },
  { label: "系统", items: [{ to: "/appearance", label: "外观", icon: "palette" }, { to: "/themes", label: "主题", icon: "layers" }, { to: "/storage", label: "存储", icon: "database" }, { to: "/updates", label: "在线更新", icon: "activity" }, { to: "/security", label: "安全", icon: "shield" }, { to: "/tokens", label: "API token", icon: "key" }] },
];

function Navigation({ onNavigate }: { onNavigate?: () => void }) {
  return <nav className="panel-nav" aria-label="主导航">{navigation.map((group) => <div className="nav-group" key={group.label}>
    <p>{group.label}</p>{group.items.map((item) => <NavLink key={item.to} to={item.to} end={item.to === "/"} onClick={onNavigate}><Icon name={item.icon} /><span>{item.label}</span></NavLink>)}
  </div>)}</nav>;
}

export function Layout() {
  const leaveSession = useLeaveSession();
  const location = useLocation();
  const [menuOpener, setMenuOpener] = useState<HTMLElement | null>(null);
  const [choice, setChoice] = useState(() => readSchemeChoice(ADMIN_SCHEME_KEY));
  const current = navigation.flatMap((group) => group.items).find((item) => item.to === "/" ? location.pathname === "/" : location.pathname.startsWith(item.to))?.label ?? "工作台";
  useEffect(() => {
    const previous = document.documentElement.dataset.theme;
    if (choice === "auto") delete document.documentElement.dataset.theme;
    else document.documentElement.dataset.theme = choice;
    return () => {
      if (previous === undefined) delete document.documentElement.dataset.theme;
      else document.documentElement.dataset.theme = previous;
    };
  }, [choice]);
  const logout = useMutation(AdminService.method.logout, { onSuccess: leaveSession });
  const { reset } = logout;
  useEffect(() => { reset(); }, [location, reset]);
  return (
    <div className="admin-shell">
      <a href="#admin-main" className="skip-link">跳到主要内容</a>
      <aside className="admin-sidebar">
        <div className="sidebar-brand"><span className="brand"><HeronMark />Heron</span></div>
        <Navigation />
      </aside>
      <div className="admin-workspace">
        <header className="admin-topbar">
          <button type="button" className="icon-button mobile-menu" aria-label="打开导航" aria-expanded={menuOpener !== null} onClick={(event) => setMenuOpener(event.currentTarget)}><Icon name="menu" /></button>
          <nav className="admin-breadcrumb" aria-label="位置"><span>工作台</span><span aria-hidden="true">/</span><strong>{current}</strong></nav>
          <div className="topbar-actions">
            <QuickSearch />
            <a href="/" target="_blank" rel="noreferrer" className="public-page-link" aria-label="公开页 ↗"><Icon name="external" /><span>公开页 ↗</span></a>
            <GitHubLink />
            <ThemeToggle choice={choice} onChange={(next) => { writeSchemeChoice(ADMIN_SCHEME_KEY, next); setChoice(next); }} />
            <button type="button" className="icon-button" title="登出" aria-label="登出" onClick={() => logout.mutate({})} disabled={logout.isPending}><Icon name="logout" /></button>
          </div>
        </header>
      <main id="admin-main" className="admin-main" tabIndex={-1}>
        {logout.error && <p role="alert" className="error">{errorText(logout.error)}</p>}
        <Outlet />
      </main>
      </div>
      {menuOpener && <Modal title="导航" className="navigation-drawer" opener={menuOpener} onClose={() => setMenuOpener(null)}><Navigation onNavigate={() => setMenuOpener(null)} /></Modal>}
    </div>
  );
}
