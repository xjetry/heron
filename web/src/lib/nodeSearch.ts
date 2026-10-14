import { literalPattern } from "./fold";

// 地址按显示值（Node.network）搜：手填的地址能搜到，被手填取代的探测值不参与。
type SearchableNode = { name: string; note?: string; lastSource?: string; country?: string; network?: { ipv4?: { address: string }; ipv6?: { address: string } }; facts?: { hostname: string } };

export function filterNodes<T extends SearchableNode>(nodes: readonly T[], search: string): T[] {
  // 空输入表示不过滤，连名称与 facts 都为空的节点也必须保留。
  if (search === "") return nodes.slice();
  const pattern = literalPattern(search, false);
  return nodes.filter((node) => [node.name, node.note, node.facts?.hostname, node.lastSource, node.country, node.network?.ipv4?.address, node.network?.ipv6?.address]
    .some((field) => field !== undefined && pattern.test(field)));
}
