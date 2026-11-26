import { useQuery } from "@connectrpc/connect-query";
import { AdminService } from "../gen/heron/v1/admin_pb";

// 面板按日历切时段（探测的晚高峰）用 hub 实际生效的时区，取自 GetSettings.timezone——与静默窗口、报告投递时刻同一个
// --timezone，不用浏览器的本地时区。流量周期与到期剩余天数按各节点的计费时区，不读这里。它只随 hub 重启改变，这个读者不重取（staleTime: Infinity）；设置页对同一查询键的
// 读者按自己的选项重取，互不影响。取不到时返回 undefined，调用方据此只提供全时段。
export function useHubTimeZone(): string | undefined {
  const settings = useQuery(AdminService.method.getSettings, {}, { staleTime: Infinity });
  return settings.data?.timezone || undefined;
}
