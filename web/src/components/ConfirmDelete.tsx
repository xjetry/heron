import { useState } from "react";

// 危险操作两段式确认：首击只进入确认态，确认才执行；执行挂起时禁用确认，双击不会重复提交。
// 确认态随组件卸载撤销：调用方在编辑态不渲染本组件，已武装的确认不跨越别的操作继续有效。
// 首击的可见文字必须包含在可访问名里，调用方的 label 应以同一动词开头。
export function ConfirmDelete({ label, confirm, note, pending, onDelete, verb = "删除" }: {
  label: string; confirm: string; note?: string; pending: boolean; onDelete: () => void; verb?: string;
}) {
  const [confirming, setConfirming] = useState(false);
  if (!confirming) {
    return <button type="button" className="link danger" aria-label={label} onClick={() => setConfirming(true)}>{verb}</button>;
  }
  return (
    <>
      <button type="button" className="danger" disabled={pending} onClick={onDelete}>{confirm}</button>{" "}
      {note ? <><span className="muted">{note}</span>{" "}</> : null}
      <button type="button" className="link" aria-label={`取消${label}`} onClick={() => setConfirming(false)}>取消</button>
    </>
  );
}
