import { create } from "@bufbuild/protobuf";
import { createConnectQueryKey } from "@connectrpc/connect-query";
import { useIsMutating, useQueryClient } from "@tanstack/react-query";
import { AdminService, GetSettingsResponseSchema, type Settings } from "../gen/heron/v1/admin_pb";

// SAVE_SETTINGS 是全部 UpdateSettings 表单共用的 mutationKey，做保存互斥。消费者：外观页的外观表单、国家 / 地区查询表单
// 与心跳表单（pages/Appearance.tsx、pages/HeartbeatSettings.tsx）、备份表单（components/BackupSettingsForm.tsx）、通知页的
// 登录通知表单与流量报告表单（pages/Channels.tsx、pages/TrafficReportSettings.tsx）。六者的 useMutation 都带这个键，并用 useSettingsSaving 在任一个在途时禁用自己的编辑
// 与提交。新增的设置表单同样要带上它，否则互斥对它不成立。
//
// 为什么互斥：每个表单保存成功后都把 hub 的回显整份写进 getSettings 的缓存（useAdoptSavedSettings）。回显是那次提交
// 之后的库，只有它是最后一次提交时，写进缓存的才是库的现状。两个保存同时在途时，runWriter 按先后提交，两个响应却各走
// 各的请求，可以按与提交相反的顺序到达：后写进缓存的是先提交的那份回显，缺了另一次保存的改动，要等重新拉取成功才纠正。
// 在重新拉取成功之前，从缓存初始化的表单会把较早那份回显里的旧值再提交一次（再保存即写回），另一次保存的改动就此丢失。
// 键上有在途的保存时，其余表单都不能提交；各表单的 onSuccess 返回 useAdoptSavedSettings 的 promise，在途一直持续到
// 重新拉取结束，同一时刻至多一份回显在写缓存。
export const SAVE_SETTINGS = ["heron.v1.AdminService/UpdateSettings"] as const;

export const useSettingsSaving = (): boolean => useIsMutating({ mutationKey: SAVE_SETTINGS }) > 0;

// 设置表单保存成功后都经它：先把 hub 的回显写进 getSettings 的缓存，再失效。回显就是库里的已保存值（外观、总闸、国家
// 查询两项、备份与登录通知渠道都由 SaveSettings 在同一个写事务里读回），缓存据此更新，不依赖刷新成功。只失效时，刷新
// 一旦失败，缓存就停在保存前的值：外观表单没动过的总闸开关显示缓存值，停在保存前；重新进入页面时各表单显示的也是保存
// 前的值。写与失效用同一个键，作用在同一组查询上。写进缓存的回显必须是库的现状：在重新拉取成功之前，从缓存初始化的
// 表单会把较早那份回显里的旧值再提交一次（再保存即写回），所以它依赖 SAVE_SETTINGS 的互斥保证同一时刻至多一份回显在
// 写缓存。
export function useAdoptSavedSettings() {
  const qc = useQueryClient();
  return (settings: Settings | undefined) => {
    const queryKey = createConnectQueryKey({ schema: AdminService.method.getSettings, cardinality: "finite" });
    qc.setQueriesData({ queryKey }, () => create(GetSettingsResponseSchema, { settings }));
    return qc.invalidateQueries({ queryKey });
  };
}
