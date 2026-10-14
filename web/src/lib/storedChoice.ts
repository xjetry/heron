// 浏览器记住的界面选择（视图、分组、筛选的选择方式等）：一个键存一个枚举值。没选过、存储不可用或记住的值不在 choices 里时
// 用 fallback——存储是外部输入，旧版本写下的、手改的值都不能让页面进入枚举之外的状态。
export function readChoice<T extends string>(key: string, choices: readonly T[], fallback: T): T {
  try {
    const value = localStorage.getItem(key);
    return choices.includes(value as T) ? (value as T) : fallback;
  } catch {
    return fallback;
  }
}

// 写不进去只影响下次打开，不改变当前页面已切换的选择。
export function writeChoice(key: string, value: string): void {
  try {
    localStorage.setItem(key, value);
  } catch {
    // 持久化是尽力而为。
  }
}
