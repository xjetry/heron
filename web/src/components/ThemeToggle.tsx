import { nextSchemeChoice, SCHEME_LABEL, type SchemeChoice } from "../lib/scheme";
import { Icon, type IconName } from "./Icon";

// 三档各有图标。按钮按 跟随系统 → 浅色 → 深色 循环，系统是深色时「深色 → 跟随系统」这一步页面看不出变化，
// 图标是唯一能告诉访客切到了哪一档的东西；跟随系统也不能借用太阳或月亮，否则它与某一档显式选择无法区分。
const ICON: Record<SchemeChoice, IconName> = { auto: "system", light: "sun", dark: "moon" };

export function ThemeToggle({ choice, onChange }: { choice: SchemeChoice; onChange: (next: SchemeChoice) => void }) {
  const next = nextSchemeChoice(choice);
  return (
    <button type="button" className="icon-button theme-toggle" aria-label="明暗切换" title={`当前：${SCHEME_LABEL[choice]}，点击切到${SCHEME_LABEL[next]}`} onClick={() => onChange(next)}>
      <Icon name={ICON[choice]} />
    </button>
  );
}
