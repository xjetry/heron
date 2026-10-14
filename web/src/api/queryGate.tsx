import type { ReactNode } from "react";
import { errorText } from "./auth";

type QuerySlice = { data: unknown; error: unknown };
type NotReady = { ready: false; errors: unknown[]; loading: ReactNode };
type Ready<T> = { ready: true; data: T; banner: ReactNode };
type DataOf<S> = S extends { data: infer D } ? Exclude<D, undefined> : never;

// 多个查询可同时失败：不同错误都要可见，同文去重，顺序与输入顺序一致。
export function errorBanner(...errors: unknown[]): ReactNode {
  const texts = [...new Set(errors.flatMap((e) => (e != null ? [errorText(e)] : [])))];
  return texts.map((text) => <p key={text} role="alert" className="error">{text}</p>);
}

// 轮询与失效刷新随时可能失败，已渲染的内容与未保存的草稿不能因一次刷新失败被卸载：
// 有数据时的失败只加横幅。未就绪时错误与加载占位分开交给调用方：
// 全部已失败查询的错误进唯一的横幅槽位（经 errorBanner 合并去重），没有任何失败时才显示"加载中…"占位。
// 数据是否到达由类型表达：页面先处理未就绪分支才拿得到 data（排除 undefined），
// 不用空数组兜底把"还在加载"伪装成"没有数据"。
// 本函数面向页面必需的查询；可选或禁用的查询要在业务层先区分——未就绪不代表请求正在进行。
// 查询键随输入变化的查询，新键的请求失败时 data 为 undefined（挂起时也是，除非配了 placeholderData），本函数判成
// 未就绪；这类查询先经 useRetained 沿用上一份数据，上面"有数据时的失败只加横幅"才对它成立。
export function queryGateAll<Ss extends readonly QuerySlice[]>(...queries: Ss): NotReady | Ready<{ [K in keyof Ss]: DataOf<Ss[K]> }> {
  const errors = queries.flatMap((q) => (q.error != null ? [q.error] : []));
  if (queries.some((q) => q.data === undefined)) {
    return { ready: false, errors, loading: errors.length === 0 ? <p className="muted">加载中…</p> : null };
  }
  return {
    ready: true,
    data: queries.map((q) => q.data) as { [K in keyof Ss]: DataOf<Ss[K]> },
    banner: errorBanner(...queries.map((q) => q.error)),
  };
}

export function queryGate<T>(q: { data: T | undefined; error: unknown }): NotReady | Ready<T> {
  const gate = queryGateAll(q);
  return gate.ready ? { ready: true, data: gate.data[0], banner: gate.banner } : gate;
}
