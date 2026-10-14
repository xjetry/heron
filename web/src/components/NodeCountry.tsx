import { CountrySource, type Node } from "../gen/heron/v1/admin_pb";
import { withId } from "../lib/ids";
import { CountryBadge } from "./CountryBadge";

export const lookupText = (node: Node) => `查得 ${node.countryLookup}（于 ${node.countryIp}）`;

export function NodeCountry({ node }: { node: Node }) {
  const source = node.countrySource === CountrySource.MANUAL
    ? ["手动指定", node.countryLookup && lookupText(node)].filter(Boolean).join("；")
    : node.countrySource === CountrySource.LOOKUP ? `查得于 ${node.countryIp}` : "尚无地区信息";
  return <span aria-label={`国家 / 地区 ${withId(node.name, node.id)}`} title={source}>
    {node.countrySource === CountrySource.UNSPECIFIED ? <span className="muted">地区未知</span> : <CountryBadge code={node.country} />}
  </span>;
}
