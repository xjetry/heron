import { useState } from "react";
import { errorBanner } from "../api/queryGate";
import { MixedCheckbox } from "../components/MixedCheckbox";
import { Drawer } from "../components/Modal";
import type { Node, Tag } from "../gen/heron/v1/admin_pb";
import { sameTag, withTag } from "../lib/tags";

type TagState = boolean | "mixed";
function tagState(count: number, total: number): TagState {
  if (count === total) return true;
  return count === 0 ? false : "mixed";
}
function nextState(state: TagState, initial: TagState): TagState {
  if (state === "mixed") return true;
  if (state) return false;
  return initial === "mixed" ? "mixed" : true;
}
function stateText(state: TagState, change: boolean | undefined): string {
  if (change !== undefined) return change ? "全部添加" : "全部移除";
  if (state === "mixed") return "部分选中";
  return state ? "全部选中" : "未选中";
}

export function BatchNodeTagsEditor({ nodes, knownTags, opener, saving, error, onClose, onSave }: {
  nodes: readonly Node[]; knownTags: readonly Tag[]; opener: HTMLElement; saving: boolean; error: unknown;
  onClose: () => void; onSave: (changes: { nodeIds: bigint[]; addTags: string[]; removeTags: string[] }) => void;
}) {
  // 抽屉持有打开时的节点快照；轮询不重置草稿，保存仅提交显式增删，不回传旧标签全集。
  const [tags, setTags] = useState(() => [...knownTags.map((tag) => tag.name), ...nodes.flatMap((node) => node.tags)]
    .reduce<string[]>((all, name) => withTag(all, name), [])
    .map((name) => ({ name, count: nodes.filter((node) => node.tags.some((tag) => sameTag(tag, name))).length })));
  const [changes, setChanges] = useState(() => new Map<string, boolean>());
  const [input, setInput] = useState("");
  const setTag = (name: string, value: TagState) => setChanges((current) => {
    const next = new Map(current);
    const initial = tagState(tags.find((tag) => tag.name === name)?.count ?? 0, nodes.length);
    if (value === "mixed" || value === initial) next.delete(name);
    else next.set(name, value);
    return next;
  });
  const addTag = () => {
    const trimmed = withTag([], input)[0];
    if (!trimmed) return;
    const existing = tags.find((tag) => sameTag(tag.name, trimmed));
    const name = existing?.name ?? trimmed;
    if (!existing) setTags((current) => [...current, { name, count: 0 }]);
    setTag(name, true);
    setInput("");
  };
  const changed = changes.size > 0;
  return <Drawer title="批量编辑标签" description={`已选择 ${nodes.length} 个节点。未修改的标签保持原样，移除只解除这些节点的关联。`} busy={saving} opener={opener} onClose={onClose}>
    <form onSubmit={(event) => {
      event.preventDefault();
      if (saving || !changed) return;
      onSave({ nodeIds: nodes.map((node) => node.id), addTags: tags.filter(({ name }) => changes.get(name) === true).map(({ name }) => name), removeTags: tags.filter(({ name }) => changes.get(name) === false).map(({ name }) => name) });
    }}>
      <div className="modal-body">
        {errorBanner(error)}
        <p className="node-subtext">半选表示只有部分节点带此标签。点击依次切换为全部添加、全部移除、保持原样。</p>
        <div className="batch-tag-list">{tags.map(({ name, count }) => {
          const initial = tagState(count, nodes.length);
          const change = changes.get(name);
          const state = change ?? initial;
          return <label className="batch-tag-option" key={name}>
            <MixedCheckbox label={name} checked={state} disabled={saving} onChange={() => setTag(name, nextState(state, initial))} />
            <span className="batch-tag-name">{name}</span>
            <span className="muted">{count}/{nodes.length} 个节点</span>
            <span className="batch-tag-intent">{stateText(state, change)}</span>
          </label>;
        })}</div>
        <div className="batch-tag-add"><label>新标签<input value={input} disabled={saving} onChange={(event) => setInput(event.target.value)} onKeyDown={(event) => { if (event.key === "Enter") { event.preventDefault(); addTag(); } }} placeholder="输入标签名称" /></label>
          <button type="button" disabled={saving || withTag([], input).length === 0} onClick={addTag}>添加标签</button></div>
      </div>
      <footer className="modal-footer"><button type="button" disabled={saving} onClick={onClose}>取消</button><button type="submit" className="primary-button" disabled={saving || !changed}>{saving ? "正在保存…" : "保存"}</button></footer>
    </form>
  </Drawer>;
}
