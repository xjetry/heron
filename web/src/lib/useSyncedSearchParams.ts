import { useEffect, useMemo, useRef, useState } from "react";
import { useLocation, useSearchParams } from "react-router";

// 页内筛选与 tab 的 URL 参数，代替直接用 useSearchParams。react-router 的导航异步完成：拿 useSearchParams 的值驱动受控控件时，
// 事件处理结束那一刻 URL 还是旧值，React 把控件恢复成旧状态——输入框丢字、输入法组字叠在旧值上，勾选框与下拉框闪回旧值
// （真实浏览器里实测过）。这里以本地副本为准、同步更新，再替换当前历史项的 URL；同一事件里的多次写入依次叠加在最新副本上。
//
// 每次写入带一个编号放进导航 state（SYNC_KEY）。位置每次变化交给 reconcile：state 里是自己写出、尚未落地的编号，就是自己的
// 写入落地，不动副本；否则是外部改动（侧栏链接、前进后退），以 URL 为准。按编号而不是按值识别：连续写入可能出现相同的值
// （a → 空 → a），导航还可能被合并、只落地最后一个，按值对不上是哪一次。
export type SetSearchParams = (next: URLSearchParams | ((current: URLSearchParams) => URLSearchParams), options?: { state?: object | null }) => void;

export const SYNC_KEY = "heronSyncedWrite";
// 编号带页面级随机前缀：历史记录里留着之前页面写下的编号，不能与本页的编号撞上。
const PAGE = Math.random().toString(36).slice(2);
let sequence = 0;

export function useSyncedSearchParams(): [URLSearchParams, SetSearchParams] {
  const [urlParams, setUrlParams] = useSearchParams();
  const location = useLocation();
  const url = urlParams.toString();
  const [local, setLocal] = useState(url);
  // latest 与 local 同值，供同一事件里的连续写入叠加（local 要到下次渲染才更新）；inFlight 是已写出、尚未落地的编号。
  const latest = useRef(url);
  const inFlight = useRef<string[]>([]);
  useEffect(() => {
    const result = reconcile(inFlight.current, writeIdOf(location.state));
    inFlight.current = result.inFlight;
    if (!result.external) return;
    latest.current = url;
    setLocal(url);
  }, [location, url]);
  const params = useMemo(() => new URLSearchParams(local), [local]);
  const set: SetSearchParams = (next, options) => {
    const value = (typeof next === "function" ? next(new URLSearchParams(latest.current)) : next).toString();
    // 没有变化就不写：省一次导航，也不留下一个等不到"变化"的编号。
    if (value === latest.current) return;
    const id = `${PAGE}:${++sequence}`;
    latest.current = value;
    inFlight.current = [...inFlight.current, id];
    setLocal(value);
    setUrlParams(new URLSearchParams(value), { replace: true, state: { ...options?.state, [SYNC_KEY]: id } });
  };
  return [params, set];
}

function writeIdOf(state: unknown): string | undefined {
  const id = typeof state === "object" && state !== null ? (state as Record<string, unknown>)[SYNC_KEY] : undefined;
  return typeof id === "string" ? id : undefined;
}

// 落地的是某个待落地编号：把它连同更早的一起划掉（导航被合并时更早的不会再落地）。不是待落地编号：外部改动，清空待落地。
export function reconcile(inFlight: readonly string[], landedId: string | undefined): { inFlight: string[]; external: boolean } {
  const index = landedId === undefined ? -1 : inFlight.indexOf(landedId);
  return index >= 0 ? { inFlight: inFlight.slice(index + 1), external: false } : { inFlight: [], external: true };
}
