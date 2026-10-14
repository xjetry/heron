import { useState } from "react";
import { Modal } from "./Modal";
// 脚本模板随面板打包进 hub 二进制，与 hub 同版本（与 SKILL.md 随 hub 下发同一理由：面板不另存一份）。
import template from "../assets/heron-quick-node.user.js?raw";

// 复制时替换两个占位符：hub 地址取浏览器当前 origin——脚本用它调管理接口并把 Origin 钉成它，必须是面板所在的
// 来源，所以面板安装命令区的"Hub 连接地址"覆盖（只改 agent 连 hub 的地址）不进脚本；脚本给出的安装命令也用这个
// 地址，agent 经另一地址访问 hub 时用户在脚本里改。token 只在刚创建、明文还在手上时填入；否则留空，脚本首次
// 运行会引导设置。
export function buildUserscript(origin: string, token: string): string {
  return template.replaceAll("__HERON_HUB__", origin).replaceAll("__HERON_TOKEN__", token);
}

// 复制油猴脚本到剪贴板；剪贴板不可用（权限、非安全上下文）时退回弹窗手动全选。
export function UserscriptButton({ token = "", label = "复制油猴脚本" }: { token?: string; label?: string }) {
  const [state, setState] = useState<"idle" | "copied">("idle");
  // 手动复制弹窗开着时是打开它的按钮，关上后焦点还给它；null 表示弹窗关着。
  const [opener, setOpener] = useState<HTMLButtonElement | null>(null);
  const source = buildUserscript(window.location.origin, token);
  const copy = async (button: HTMLButtonElement) => {
    try {
      await navigator.clipboard.writeText(source);
      setState("copied");
      setTimeout(() => setState("idle"), 1500);
    } catch {
      setOpener(button);
    }
  };
  return <>
    <button type="button" onClick={(event) => void copy(event.currentTarget)}>
      {state === "copied" ? "已复制" : label}
    </button>
    {opener && <Modal title={label} description="浏览器拒绝了剪贴板访问，请全选复制" onClose={() => setOpener(null)} opener={opener}>
      <textarea data-autofocus readOnly rows={12} value={source} aria-label="油猴脚本源码"
        onFocus={(event) => event.currentTarget.select()}
        style={{ width: "100%", fontFamily: "monospace", fontSize: "12px" }} />
    </Modal>}
  </>;
}
