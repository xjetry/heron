// 实时快照由页面按 POLL_MS 轮询，不依赖历史刷出；管理与公开入口共用此周期。读数与最近上报随 agent 上报变化（hub 下发的
// report_interval_ms，默认 10 秒），在线判定在距最近上报超过 hub 的 TTL 时按读取时刻翻转；5 秒让这两类变化至多晚一个周期可见，
// 持续流量是 2 秒时的五分之二。
export const POLL_MS = 5000;

// 详情的周期量、诊断与对比页的 hub 时钟用较低频率读取，不依赖历史落盘刷出。
export const TRAFFIC_MS = 10_000;
