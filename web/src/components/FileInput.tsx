import type { ChangeEvent, Ref } from "react";

// 文件选择：原生 file 输入的按钮文字与「未选择文件」由浏览器按界面语言绘制（英文浏览器下是 Choose File），这里换成自绘的
// 按钮与文件名，原生输入仍在 label 里，点按钮、键盘聚焦与 Playwright 的 setInputFiles 都落到它身上。
// 文件名由调用方给出而不是自己记：调用方会在选完后清空输入值（允许重选同一文件）或上传完成后复位，自己记的名字会和实际状态脱节。
export function FileInput({ label, caption, accept, disabled = false, fileName, inputRef, onChange }: {
  label: string; caption?: string; accept: string; disabled?: boolean; fileName?: string;
  inputRef?: Ref<HTMLInputElement>; onChange: (event: ChangeEvent<HTMLInputElement>) => void;
}) {
  return (
    <div className="file-field">
      {caption && <span className="field-caption">{caption}</span>}
      <label className="file-input" data-disabled={disabled || undefined}>
        <input ref={inputRef} type="file" className="file-native" aria-label={label} accept={accept} disabled={disabled} onChange={onChange} />
        <span className="file-button" aria-hidden="true">选择文件</span>
        <span className={fileName ? "file-name" : "file-name muted"}>{fileName || "未选择文件"}</span>
      </label>
    </div>
  );
}
