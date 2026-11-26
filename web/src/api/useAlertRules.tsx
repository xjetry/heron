import { useQuery } from "@connectrpc/connect-query";
import { createContext, useContext, type ReactNode } from "react";
import { AdminService } from "../gen/heron/v1/admin_pb";
import { ALERTS_MS } from "../lib/poll";

// Layout 挂载后由它独占 ListAlertRules 的定时轮询，并通过此上下文告知子页面：
// 同一查询键下不再额外开启第二条 refetchInterval，子页面切换或挂载时直接复用 ALERTS_MS 内的缓存，
// 变更（保存/删除规则）则经 invalidateQueries 强制失效重取；脱离 Layout 单独挂载的页面测试仍照常按 ALERTS_MS 轮询。
const AlertRulesPollingContext = createContext(false);

export function AlertRulesPollingProvider({ children }: { children: ReactNode }) {
  return <AlertRulesPollingContext value={true}>{children}</AlertRulesPollingContext>;
}

// 管理端常驻布局专用：标签页在后台时仍需按 ALERTS_MS 拉取触发中告警数以更新标题，因此开启后台轮询；
// 切回前台时 queryDefaults 的聚焦策略仍会立即补取。
export function useLayoutAlertRules() {
  return useQuery(AdminService.method.listAlertRules, {}, {
    staleTime: ALERTS_MS,
    refetchInterval: ALERTS_MS,
    refetchIntervalInBackground: true,
  });
}

// 各管理页面共用这条 ListAlertRules 查询：在 Layout 内复用常驻轮询与缓存，不因跨页导航或双订阅增加重复请求。
export function useAlertRules(options: { enabled?: boolean; poll?: boolean } = {}) {
  const managedByLayout = useContext(AlertRulesPollingContext);
  return useQuery(AdminService.method.listAlertRules, {}, {
    enabled: options.enabled,
    staleTime: ALERTS_MS,
    refetchInterval: options.poll && !managedByLayout ? ALERTS_MS : false,
  });
}
