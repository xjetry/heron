import type { ReactNode } from "react";

// 设置页的一个分区：左栏是标题与说明，右栏是表单与状态，窄屏合成一栏。说明放左栏而不是表单上方，
// 长段文字不再把字段推到屏幕下方，各分区的字段也落在同一条左边线上。不加区域语义：右栏里的组件自己可能就是带名字的区域。
export function SettingsSection({ title, description, children }: { title: string; description?: ReactNode; children: ReactNode }) {
  return (
    <div className="settings-section">
      <div className="settings-aside"><h2>{title}</h2>{description}</div>
      <div className="settings-main">{children}</div>
    </div>
  );
}
