import type { PublicFacts } from "../gen/heron/v1/public_pb";

// PublicNode.facts 在节点从未上报、但管理员手填了出口地址时只带 network（public.proto）：主机信息各项都是零值。
// 系统信息行与"系统"一栏按"没有主机信息"处理、整行不画，只画双栈标记；判定只在这里，节点页与详情面板共用。
export const hasHostFacts = (f: PublicFacts | undefined): f is PublicFacts => !!f && !!(f.os || f.arch || f.cpuModel || f.virtualization || f.cpuCores);
