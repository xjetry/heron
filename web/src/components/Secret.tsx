import { CopyableText } from "./CopyableText";

// token 与注册 key 只在创建那一次返回；hub 只存哈希，离开这个页面就再也看不到。
export function Secret({ label, value }: { label: string; value: string }) {
  return (
    <div className="card">
      <p><strong>{label}</strong> —— 只显示这一次，hub 不保存明文。</p>
      <CopyableText label={label} value={value} />
    </div>
  );
}
