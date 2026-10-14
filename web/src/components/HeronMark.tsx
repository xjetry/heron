import heron from "../assets/heron.svg";

// 字标或站点标题由调用方提供，图形不重复读屏名称。
export function HeronMark() {
  return <img src={heron} alt="" className="heron-mark" width="32" height="32" />;
}
