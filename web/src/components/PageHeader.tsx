import type { ReactNode } from "react";

// 列表页统一为页头、筛选行、表格；页头只有标题、一句可选说明与主按钮。
export function PageHeader({ title, description, actions }: { title: string; description?: ReactNode; actions?: ReactNode }) {
  return (
    <header className="page-header">
      <div><h1>{title}</h1>{description && <p className="muted">{description}</p>}</div>
      {actions && <div className="page-header-actions">{actions}</div>}
    </header>
  );
}
