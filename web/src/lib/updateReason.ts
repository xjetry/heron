// UpdateStatus.reason 是写给程序读的英文短句，由 agent 与 hub 各自给出（internal/update/protocol.go 的 Client.Status、
// internal/hub/updates/manager.go、internal/hub/api/updates.go、internal/agent/client/update.go 里的 Reason 字面量）。
// 新旧版本的 agent 都会发来这些原文，所以界面按原文映射成中文，而不是要求协议改成编码。
// 不在表里的原文（例如更新引擎把 err.Error() 直接放进 reason）没有译文，调用方原样显示。
// 改动上述 Go 字面量时同步改这张表：updateReason.test.ts 从 Go 源码双向对照。
export const UPDATE_REASONS: Readonly<Record<string, string>> = {
  "agent has not reported online update support": "agent 尚未上报在线更新能力（版本过旧，或刚连上 hub）",
  "online updates require Linux systemd": "需要 Linux systemd",
  "online updates require Linux systemd; use the platform installer": "需要 Linux systemd，请用对应平台的安装方式更新",
  "container deployments must update their image; online updates are unsupported": "容器部署请更新镜像，不支持在线更新",
  "local updater unavailable; install with the current systemd installer": "本机更新器不可用，请用当前的 systemd 安装器重新安装",
  "local updater protocol is incompatible": "本机更新器协议不兼容",
};

// 未登记的原文返回 undefined，由调用方原样显示。
export function updateReasonText(reason: string): string | undefined {
  return Object.hasOwn(UPDATE_REASONS, reason) ? UPDATE_REASONS[reason] : undefined;
}
