import type { PublicNetworkInfo } from "../gen/heron/v1/public_pb";
import { AddressDetectionState } from "../gen/heron/v1/types_pb";

// 公开页的双栈标记：只为确认有公网出口（AVAILABLE）的地址族画一个小标。AVAILABLE 由 hub 按显示值给出：agent 探测
// 确认，或管理员手填了该族的公网地址，或 hub 在上报上看到该族公网来源（PublicAddressDetection.state）；这里不区分
// 三者。不支持、探测失败与还没探测都不画：探测失败可能只是这一轮外部服务不通，画成"没有 IPv6"会误导访客；
// hub 来源依赖 hub 的来源解析正确（--trusted-proxies），也只在管理端（NodeAddresses）点明。三者的区分只在管理端显示。
// 两个族都没有确认时不渲染。外观与国家徽章相同（styles.css）。
// addressFamilies 把判定暴露给 NodeCard：卡片系统行要不要为双栈标记留位，走同一份口径。
export function addressFamilies(network: PublicNetworkInfo | undefined): string[] {
  return ([["IPv4", network?.ipv4], ["IPv6", network?.ipv6]] as const)
    .filter(([, detection]) => detection?.state === AddressDetectionState.AVAILABLE)
    .map(([family]) => family);
}

export function AddressFamilies({ network }: { network: PublicNetworkInfo | undefined }) {
  const families = addressFamilies(network);
  if (families.length === 0) return null;
  return (
    <span className="address-families" role="group" aria-label="公网出口">
      {families.map((family) => <span key={family} className="address-family" title={`有 ${family} 公网出口`}>{family}</span>)}
    </span>
  );
}
