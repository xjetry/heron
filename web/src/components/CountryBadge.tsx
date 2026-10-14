// 国家 / 地区徽章：只写国家码。区域指示符旗帜在 Windows 上不渲染、在部分系统上对个别地区显示成方框，
// 同一个徽章在不同访客那里长得不一样，所以不用 emoji。没有国家时不渲染。
export function CountryBadge({ code }: { code: string }) {
  if (code === "") return null;
  return (
    <span className="country-badge" title={`国家 / 地区 ${code}`}>
      {code}
    </span>
  );
}
