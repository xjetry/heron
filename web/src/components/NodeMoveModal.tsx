import { type FormEvent, useState } from "react";
import type { Node } from "../gen/heron/v1/admin_pb";
import { errorBanner } from "../api/queryGate";
import { Modal } from "./Modal";

// 「移动到…」的弹窗：nodes 是要移动的节点（batch 多选或行菜单单个），total 是全部节点总数 N（未过滤）。
// 目标位置的合法区间按 N-k+1 计算，输入与预览都只用这个区间，越界在提交前就拦下。
export function NodeMoveModal({ nodes, total, pending, error, opener, onClose, onConfirm }: {
  nodes: readonly Node[]; total: number; pending: boolean; error: unknown; opener: HTMLElement;
  onClose: () => void; onConfirm: (position: number) => void;
}) {
  const k = nodes.length;
  const max = Math.max(total - k + 1, 1);
  const [value, setValue] = useState("1");
  const position = Number(value);
  const valid = Number.isInteger(position) && position >= 1 && position <= max;
  const preview = k === 1
    ? `移到第 ${valid ? position : "…"} 位`
    : `将 ${k} 个节点移到第 ${valid ? position : "…"}–${valid ? position + k - 1 : "…"} 位`;
  const onSubmit = (event: FormEvent) => {
    event.preventDefault();
    if (!pending && valid) onConfirm(position);
  };
  return <Modal title="移动节点" description={`共 ${total} 个节点，将移动其中的 ${k} 个；其余节点相对顺序不变。`} busy={pending} opener={opener} onClose={onClose}>
    <form onSubmit={onSubmit}>
      <div className="modal-body">
        {errorBanner(error)}
        <label>目标位置（1–{max}）
          <input data-autofocus type="number" min={1} max={max} step={1} inputMode="numeric" value={value}
            disabled={pending} onChange={(event) => setValue(event.target.value)} />
        </label>
        <p className="node-subtext">{valid ? preview : `目标位置必须是 1–${max} 之间的整数。`}</p>
      </div>
      <footer className="modal-footer">
        <button type="button" disabled={pending} onClick={onClose}>取消</button>
        <button type="submit" className="primary-button" disabled={pending || !valid}>移动</button>
      </footer>
    </form>
  </Modal>;
}
