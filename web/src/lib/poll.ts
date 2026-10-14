// 实时快照由页面按 POLL_MS 轮询，不依赖历史刷出；管理与公开入口共用此周期，让新上报尽快可见。
export const POLL_MS = 2000;

// 详情的周期量、诊断与对比页的 hub 时钟用较低频率读取，不依赖历史落盘刷出。
export const TRAFFIC_MS = 10_000;
