// 计费时区输入用到的时区名判定与候选。hub 用 time.LoadLocation 判定能否保存，浏览器用 Intl 按这个名字显示周期与日期；
// 两边的时区库都取自 IANA tz 数据库，但版本可能不同。浏览器认不得的名字页面无法按它显示（Intl 抛 RangeError），所以编辑器
// 把"浏览器认得"作为保存前的检查，最终是否接受仍由 hub 判定。

// 空串不是时区名：它表示沿用 hub 的 --timezone，由调用方先行处理，这里按认不得返回。
export function knownTimeZone(name: string): boolean {
  if (name === "") return false;
  try {
    new Intl.DateTimeFormat("en-US", { timeZone: name });
    return true;
  } catch {
    return false;
  }
}

let names: readonly string[] | undefined;

// 浏览器支持的时区名，外加 UTC：V8 的 supportedValuesOf 只列地理时区，不含 UTC。列表按 CLDR 的规范名给出，其中一些是
// IANA 的旧名（Asia/Calcutta 而不是 Asia/Kolkata、Europe/Kiev 而不是 Europe/Kyiv）；两种写法 Intl 都认，hub 也都能加载
// （cmd/hub 嵌入了 time/tzdata，系统时区库缺旧名链接时用嵌入的），所以候选只是便于选取，手输 IANA 的现名同样可以保存。
// 只算一次。
export function timeZoneNames(): readonly string[] {
  names ??= [...new Set(["UTC", ...Intl.supportedValuesOf("timeZone")])];
  return names;
}
