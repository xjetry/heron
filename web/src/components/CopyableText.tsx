import { useEffect, useRef, useState } from "react";
import { Icon } from "./Icon";

export function CopyableText({ label, value, copyLabel, compact = false }: { label: string; value: string; copyLabel?: string; compact?: boolean }) {
  const code = useRef<HTMLElement>(null);
  const [result, setResult] = useState<"idle" | "copied" | "failed">("idle");
  // 反馈只属于产生它的那段文本：文本一换就回到 idle，在渲染期调整，不先画出一帧旧反馈。
  const [resultFor, setResultFor] = useState(value);
  if (resultFor !== value) {
    setResultFor(value);
    setResult("idle");
  }
  // 每次文本提交后加一。复制可能晚于文本切换结束，完成时代数已变说明结果过期，不能改变新文本的反馈或选区。
  const generation = useRef(0);
  useEffect(() => {
    generation.current++;
  }, [value]);
  const copy = async () => {
    const attempt = generation.current;
    let next: "copied" | "failed";
    try {
      if (!navigator.clipboard) throw new Error("clipboard unavailable");
      await navigator.clipboard.writeText(value);
      next = "copied";
    } catch {
      next = "failed";
    }
    if (attempt !== generation.current) return;
    setResult(next);
    if (next === "failed" && code.current) {
      const range = document.createRange();
      range.selectNodeContents(code.current);
      const selection = window.getSelection();
      selection?.removeAllRanges();
      selection?.addRange(range);
    }
  };
  return <div className={`copyable-text${compact ? " copyable-inline" : ""}`}>
    <code ref={code} className={compact ? undefined : "secret"} aria-label={label}>{value}</code>
    <p className="copyable-actions">
      <button type="button" className={compact ? "icon-button copy-button" : undefined} aria-label={copyLabel ?? (compact ? `复制 ${label}` : undefined)} title={result === "copied" ? "已复制" : "复制"} onClick={() => void copy()}>
        {compact ? <Icon name={result === "copied" ? "check" : "copy"} /> : result === "copied" ? "已复制" : "复制"}
      </button>
      <span role="status" className={compact && result !== "failed" ? "sr-only" : "copy-feedback"}>{result === "failed" ? "复制失败，请手动选择" : compact && result === "copied" ? `已复制 ${value}` : ""}</span>
    </p>
  </div>;
}
