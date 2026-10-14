import type { DragEventHandler } from "react";
import type { OrderMove } from "../api/useOrder";
import { Icon } from "./Icon";

// position 是行首序号：显示顺序就是服务端全序（未收窄、未按列排序）时用期望排列的位次，否则用服务端
// position（全序名次：过滤、按列排序与沿用旧结果都不重排）。排序列只放这一个手柄：拖动或聚焦后按方向键 / Home / End；
// 不拖动的鼠标用户用行 ⋯ 菜单里的上移、下移、置顶、置底（pages/Nodes.tsx），与这里同走 ReorderNodes 的完整排列。
export function NodeOrderControl({ label, count, position, reorderDisabled, onMove, onDragStart, onDragEnd }: {
  label: string; count: number; position: number; reorderDisabled: boolean;
  onMove: (move: OrderMove) => void;
  onDragStart: DragEventHandler<HTMLButtonElement>; onDragEnd: DragEventHandler<HTMLButtonElement>;
}) {
  const locked = reorderDisabled || count < 2;
  return <div className="node-order-control">
    <button type="button" className="icon-button order-handle" aria-label={`调整顺序 ${label}`} aria-describedby="node-order-help"
      title="拖动调整顺序；方向键上移/下移，Home 置顶，End 置底" disabled={locked} draggable={!locked}
      onDragStart={onDragStart} onDragEnd={onDragEnd}
      onKeyDown={(event) => {
        if (locked) return;
        const move = { ArrowUp: -1, ArrowDown: 1, Home: "first", End: "last" }[event.key] as OrderMove | undefined;
        if (move !== undefined) { event.preventDefault(); onMove(move); }
      }}><Icon name="grip" /><span className="order-position">{position}</span></button>
  </div>;
}
