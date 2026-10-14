import { createBrowserRouter } from "react-router";
import { Layout } from "./components/Layout";
import { Login } from "./pages/Login";
import { Overview } from "./pages/Overview";
import { NodeDetail } from "./pages/NodeDetail";
import { Nodes } from "./pages/Nodes";
import { RegisterWindow } from "./pages/RegisterWindow";
import { Renewals } from "./pages/Renewals";
import { ProbeTasks } from "./pages/ProbeTasks";
import { ProbeCerts } from "./pages/ProbeCerts";
import { ProbeCompare } from "./pages/ProbeCompare";
import { AlertRules } from "./pages/AlertRules";
import { AlertEvents } from "./pages/AlertEvents";
import { Silences } from "./pages/Silences";
import { Channels } from "./pages/Channels";
import { ApiTokens } from "./pages/ApiTokens";
import { Appearance } from "./pages/Appearance";
import { Themes } from "./pages/Themes";
import { Storage } from "./pages/Storage";
import { Sessions } from "./pages/Sessions";
import { Security } from "./pages/Security";
import { Updates } from "./pages/Updates";

// basename 与 hub 的挂载路径一致。路由不做鉴权判断：谁都能打开任何页面，
// 页面里的第一次请求得到 Unauthenticated 就会被数据层送去登录。
export const router = createBrowserRouter(
  [
    { path: "/login", Component: Login },
    {
      path: "/",
      Component: Layout,
      children: [
        { index: true, Component: Overview },
        { path: "nodes/:id", Component: NodeDetail },
        { path: "nodes", Component: Nodes },
        { path: "renewals", Component: Renewals },
        { path: "probes", Component: ProbeTasks },
        { path: "probes/:id/compare", Component: ProbeCompare },
        { path: "probes/:id/certs", Component: ProbeCerts },
        { path: "alerts", Component: AlertRules },
        { path: "events", Component: AlertEvents },
        { path: "silences", Component: Silences },
        { path: "channels", Component: Channels },
        { path: "tokens", Component: ApiTokens },
        { path: "appearance", Component: Appearance },
        { path: "themes", Component: Themes },
        { path: "storage", Component: Storage },
        { path: "updates", Component: Updates },
        { path: "security", Component: Sessions },
        { path: "security/credentials", Component: Security },
        { path: "register", Component: RegisterWindow },
      ],
    },
  ],
  { basename: "/admin" },
);
