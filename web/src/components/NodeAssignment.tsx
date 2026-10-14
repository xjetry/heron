import { useQuery } from "@connectrpc/connect-query";
import { useState } from "react";
import { AdminService, type Node } from "../gen/heron/v1/admin_pb";
import { toggled, withId } from "../lib/ids";
import { literalPattern } from "../lib/fold";
import { sameTag } from "../lib/tags";
import { MultiSelect } from "./MultiSelect";

export type NodeSelection = { allNodes: boolean; nodeIds: Set<bigint>; selectorTags: string[]; dynamic: boolean };
type Mode = "all" | "dynamic" | "picked";
const modeOf = (v: NodeSelection): Mode => (v.allNodes ? "all" : v.dynamic ? "dynamic" : "picked");

const QUICK_TAGS = 10;

// 模式切换只修改 allNodes / dynamic，另一种形状的草稿保留给用户切回；提交时由调用方按模式取舍。
// 已选集合可能含当前列表里没有的 id，仍需展示并允许移除，不能让不可见的选择留在草稿里。
export function NodeAssignment({ nodes, value, onChange, legend, noun = "分配" }: {
  nodes: readonly Node[]; value: NodeSelection; onChange: (patch: Partial<NodeSelection>) => void; legend: string; noun?: "分配" | "作用域";
}) {
  const tags = useQuery(AdminService.method.listTags, {});
  const [search, setSearch] = useState("");
  const [allQuick, setAllQuick] = useState(false);
  const mode = modeOf(value);
  const known = [...new Set([...(tags.data?.tags.map((t) => t.name) ?? nodes.flatMap((n) => n.tags)), ...value.selectorTags])].sort();
  const options = known.map((name) => ({ value: name, label: name, count: nodes.filter((n) => n.tags.some((t) => sameTag(t, name))).length }));
  const matching = nodes.filter((n) => value.selectorTags.every((tag) => n.tags.some((t) => sameTag(t, tag))));
  const pattern = search === "" ? null : literalPattern(search, false);
  const listed = pattern ? nodes.filter((n) => pattern.test(n.name) || pattern.test(String(n.id))) : nodes;
  const missing = [...value.nodeIds].filter((id) => !nodes.some((n) => n.id === id));
  const setMode = (next: Mode) => onChange({ allNodes: next === "all", dynamic: next === "dynamic" });
  return (
    <fieldset className="assignment">
      <legend>{legend}</legend>
      <div className="assignment-modes" role="radiogroup" aria-label={`${noun}方式`}>
        <label className="assignment-card"><input type="radio" name={`${legend}-mode`} aria-label="全部节点" checked={mode === "all"} onChange={() => setMode("all")} /><span>全部节点</span><small>含以后新建的节点</small></label>
        <label className="assignment-card"><input type="radio" name={`${legend}-mode`} aria-label="动态标签选择器" checked={mode === "dynamic"} onChange={() => setMode("dynamic")} /><span>动态标签选择器</span><small>以后新节点或标签变更会自动改变覆盖，移除标签也会撤销覆盖</small></label>
        <label className="assignment-card"><input type="radio" name={`${legend}-mode`} aria-label="指定节点" checked={mode === "picked"} onChange={() => setMode("picked")} /><span>指定节点</span><small>只保存本次选中的节点，之后标签变化不会改变分配</small></label>
      </div>
      {mode === "dynamic" && <>
        <MultiSelect label="匹配标签" searchable options={options} selected={value.selectorTags} onChange={(selectorTags) => onChange({ selectorTags })} />
        <p className="muted">{value.selectorTags.length === 0 ? "至少选择一个标签" : `当前匹配 ${matching.length} 个节点`}</p>
      </>}
      {mode === "picked" && <>
        {/* 快选只是捷径：标签多时先列前 QUICK_TAGS 个，其余收在「全部 N 个」后面，不让一排胶囊占掉半个抽屉。 */}
        {known.length > 0 && <div className="assignment-quick"><span className="muted">按标签快选</span>
          {(allQuick ? known : known.slice(0, QUICK_TAGS)).map((name) => <button key={name} type="button" className="chip" aria-label={`按标签快选 ${name}`} onClick={() => onChange({ nodeIds: new Set([...value.nodeIds, ...nodes.filter((n) => n.tags.some((t) => sameTag(t, name))).map((n) => n.id)]) })}>{name}</button>)}
        {known.length > QUICK_TAGS && <button type="button" className="link" aria-expanded={allQuick} onClick={() => setAllQuick(!allQuick)}>{allQuick ? "收起" : `全部 ${known.length} 个`}</button>}
        </div>}
        <input type="search" aria-label="搜索节点" placeholder="名称或 #id" value={search} onChange={(event) => setSearch(event.target.value)} />
        <ul className="assignment-list">
          {listed.map((n) => <li key={String(n.id)}><label><input type="checkbox" aria-label={withId(n.name, n.id)} checked={value.nodeIds.has(n.id)} onChange={() => onChange({ nodeIds: toggled(value.nodeIds, n.id) })} />{withId(n.name, n.id)}</label></li>)}
          {missing.map((id) => <li key={String(id)} className="muted">#{String(id)}（已不存在）<button type="button" className="link" aria-label={`移除 #${id}`} onClick={() => onChange({ nodeIds: toggled(value.nodeIds, id) })}>移除</button></li>)}
          {listed.length === 0 && missing.length === 0 && <li className="muted">没有匹配的节点。</li>}
        </ul>
        <p className="muted">已选择 {value.nodeIds.size} 个节点 <button type="button" className="link" onClick={() => onChange({ nodeIds: new Set() })}>清空已选</button></p>
      </>}
    </fieldset>
  );
}

// 动态标签选择器没有标签就没有可匹配的节点：旧表单靠 select 的 required 挡住提交，这是同一道闸的显式形式；
// 三处分配 / 作用域表单在 checkValidity 之后都要先过它，不把空集合交给 hub 去拒绝。
export const assignmentValid = (value: NodeSelection): boolean => value.allNodes || !value.dynamic || value.selectorTags.length > 0;
