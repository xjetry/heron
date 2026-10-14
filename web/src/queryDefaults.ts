import type { QueryClientConfig } from "@tanstack/react-query";
import { retryQuery } from "./retry";

type QueryDefaults = NonNullable<NonNullable<QueryClientConfig["defaultOptions"]>["queries"]>;

// 非轮询查询仍不因聚焦重取：历史依赖 hub 分钟换键，重复取会多耗公开限流令牌。
// 轮询在后台暂停，回前台立即补取才能避免等待下个周期；不改变认证失败不重试的约束。
// GetSite 不轮询；它的 staleTime: Infinity 还阻止断网重连与重新挂载触发重取。
export const queryDefaults: QueryDefaults = {
  retry: retryQuery,
  refetchOnWindowFocus: (query) => {
    const configured = (query.options as QueryDefaults).refetchInterval;
    const interval = typeof configured === "function" ? configured(query) : configured;
    return typeof interval === "number" && interval > 0 ? "always" : false;
  },
};
