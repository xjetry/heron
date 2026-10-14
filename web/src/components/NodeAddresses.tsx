import { AddressSource, type NodeAddress, type NodeNetwork } from "../gen/heron/v1/admin_pb";
import { AddressDetectionState, type AddressDetection, type NetworkInfo } from "../gen/heron/v1/types_pb";
import { CopyableText } from "./CopyableText";
import { dateTime } from "../lib/format";
import { Timestamp } from "./Timestamp";

// 显示值（Node.network）没有地址时的文案。DISABLED 出现在显示值里只有一种情形：手填已清空（来源不是手填），agent 还
// 没收到恢复探测的应答，所以写"等待探测"；agent 按手填停用时显示值是手填地址本身。
const stateLabel = (d: NodeAddress | undefined): string => d?.state === AddressDetectionState.UNSUPPORTED ? "不支持" : d?.state === AddressDetectionState.FAILED ? "探测失败" : d?.state === AddressDetectionState.DISABLED ? "等待探测" : "等待上报";

// 一行摘要（节点详情页头）：有地址写地址，否则与地址格同一套文案。
export const addressText = (d: NodeAddress | undefined): string => d?.state === AddressDetectionState.AVAILABLE ? d.address : d?.source === AddressSource.UNSPECIFIED || d === undefined ? "—" : stateLabel(d);

// agent 原报（facts.network）的文案：手填或 hub 来源时在详细视图里对照显示，看得出 agent 是否已按手填停用该族探测、
// 或 agent 没探到而 hub 看到了什么。
const reportedText = (r: AddressDetection | undefined): string => r?.state === AddressDetectionState.AVAILABLE ? `探测到 ${r.address}` : r?.state === AddressDetectionState.UNSUPPORTED ? "不支持" : r?.state === AddressDetectionState.FAILED ? "探测失败" : r?.state === AddressDetectionState.DISABLED ? "已停用探测" : "尚未上报";

function Address({ family, display, reported, detailed }: { family: string; display?: NodeAddress; reported?: AddressDetection; detailed: boolean }) {
  const available = display?.state === AddressDetectionState.AVAILABLE && !!display.address;
  const manual = display?.source === AddressSource.MANUAL;
  const hubSeen = display?.source === AddressSource.HUB_SEEN;
  // checked_at 是 agent 当前这个结果首次得到的时刻（结果不变时 agent 沿用原值，见 AddressDetection.checked_at），
  // 不是最近一次探测的时刻，所以文案一律写成"自某时起"。
  const checked = reported && reported.checkedAt > 0n ? reported.checkedAt : undefined;
  // 手填与 hub 来源的 title 同形：先说显示值是什么，再附 agent 原报。hub 来源不是 agent 的自述，也不是运维的断言，
  // 是 hub 在上报连接上看到的对端，所以开头点明"不是 agent 探测"。
  const agentPart = `agent：${reportedText(reported)}${checked ? `（自 ${dateTime(checked)} 起）` : ""}`;
  const title = manual ? `管理员手填；${agentPart}`
    : hubSeen ? `hub 在上报连接上看到的来源地址，不是 agent 探测；${agentPart}`
    : checked ? `结果自 ${dateTime(checked)} 起未变${display?.state === AddressDetectionState.UNSUPPORTED ? "；无可用接口地址或路由" : ""}` : "需要支持双栈探测的 agent 上报结果";
  return <div className="node-address" aria-label={family} title={title}>
    <span className="address-family">{family}</span>
    {available ? <CopyableText compact label={`${family} 地址`} value={display.address} copyLabel={`复制 ${family} ${display.address}`} /> : <span className={`address-state${display?.state === AddressDetectionState.FAILED ? " failed" : ""}`}>{stateLabel(display)}</span>}
    {manual && <span className="address-source">手填</span>}
    {hubSeen && <span className="address-source">上报来源</span>}
    {detailed && (manual || hubSeen) && <span className="address-state">agent：{reportedText(reported)}</span>}
    {/* HUB_SEEN 不渲染独立的"自 … 起"：checkedAt 只属于 agent 原报，只能出现在上面的 agent 原报文案里；单独一行会让它
        看起来像 hub 来源地址的起始时刻——一个 FAILED 很久的节点刚换了上报来源，新地址与旧失败时间是两个事实。 */}
    {detailed && checked && !hubSeen && <span className="address-since">自 <Timestamp at={checked} /> 起</span>}
  </div>;
}

// network 是 hub 算好的显示值（手填、agent 探测、hub 看到的来源依次取先看得到的）；reported 是 agent 原报，只用于当前
// 结果的起始时刻与手填、hub 来源时的对照。
export function NodeAddresses({ network, reported, detailed = false }: { network?: NodeNetwork; reported?: NetworkInfo; detailed?: boolean }) {
  return <div className="node-addresses">
    <Address family="IPv4" display={network?.ipv4} reported={reported?.ipv4} detailed={detailed} />
    <Address family="IPv6" display={network?.ipv6} reported={reported?.ipv6} detailed={detailed} />
  </div>;
}
