// hub 为每个节点记住答案的地址数（internal/hub/geo 的 answersPerNode，countryLimits.test.ts 对照）。面板文案按它写出
// "不再外呼"的上界：超过这个数的地址轮换时会再查，文案不能许诺无条件的"每地址一次"。
export const ANSWERS_PER_NODE = 4;
