import type { ReactNode } from "react";

// 列表为空时代替整张表：只剩表头的空表像是没加载完，空态说明写进一张卡片。
// 筛选后为空随输入变化，传 status 让读屏播报；列表本身为空是页面加载时的状态，不设 live region。
export function EmptyState({ title, children, status = false }: { title: ReactNode; children?: ReactNode; status?: boolean }) {
  return (
    <div className="card empty-state" role={status ? "status" : undefined}>
      <p>{title}</p>
      {children && <p className="muted">{children}</p>}
    </div>
  );
}
