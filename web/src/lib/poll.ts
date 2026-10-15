// 实时快照由页面按 POLL_MS 轮询，不依赖历史刷出；管理与公开入口共用此周期。快照只在 agent 上报时变化（hub 下发的
// report_interval_ms，默认 10 秒），5 秒让每次上报至多晚一个周期可见，持续流量是 2 秒时的五分之二。
export const POLL_MS = 5000;

// 详情的周期量、诊断与对比页的 hub 时钟用较低频率读取，不依赖历史落盘刷出。
export const TRAFFIC_MS = 10_000;
