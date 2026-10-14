import { useState } from "react";

// 输入会变的查询（换过滤条件即换查询键）在新键的请求挂起或失败时还没有自己的数据：react-query 的 placeholderData
// 只在挂起期间补位，请求一失败 data 就回到 undefined，queryGate 随之判成未就绪，已渲染的列表连同其中未保存的草稿
// 一起卸载。这里沿用本组件上一次取到的数据，直到当前键取到自己的数据为止，失败只由 error 表达。
// stale 标出显示的不是当前输入的结果：只对当前结果成立的操作（例如要求完整排列的排序）据它关闭。
// 沿用的范围默认是组件实例：同一个调用点跨渲染沿用。输入换成另一个对象（例如另一个节点）时不能沿用上一个
// 对象的数据——那会把它当成这一个对象的结果显示。对象的身份经 identity 参数显式声明：identity 相对上一次
// 渲染变化的那一帧连同之后，沿用值清空，直到当前 identity 下的查询自己取到数据为止；不传 identity 等于
// 调用点自己就是身份：输入变了，被查询的对象没变（例如同一个列表换了过滤条件）。
//
// identity 必须与沿用值一样记在 state 里，不能记在 ref 里：React 可能丢弃一次渲染再重试（例如挂起的
// transition），渲染期间的 state 更新随被丢弃的那次渲染一起作废，但 ref 的写入不会跟着回滚。如果 identity
// 记在 ref 里，丢弃重试后 ref 已经等于新 identity、状态却还没清空，"换对象"这一帧就漏判成"没换"，重试提交的
// 会是新对象套着上一个对象的数据。两者放进同一个 state 是实现选择，让它们一起回滚；各记一个 state 同样成立。
// identity 必须按值可比：原始值，或跨渲染稳定的引用。每次渲染都传一个新对象时 changedIdentity 挂载后每帧为真，
// 渲染期间的 setS 每帧都触发，React 以 Too many re-renders 中止。
export function useRetained<Q extends { data: unknown; error: unknown }>(q: Q, identity?: unknown): { data: Q["data"]; error: Q["error"]; stale: boolean } {
  const [s, setS] = useState({ identity, last: q.data });
  const changedIdentity = s.identity !== identity;
  // 上一次取到的数据在渲染期间比较并记进 state，这是 React 记住上一次渲染的值的写法；当帧直接用 q.data，
  // state 只在之后换键、q.data 变回 undefined 时派上用场。identity 变化的这一帧，新对象如果已经有自己的
  // 数据（例如缓存命中），直接记下——那本来就是新对象自己的数据，不是沿用；这一帧的返回值改用 retained
  // （当帧算出的有效沿用值，identity 变化时强制为 undefined），不直接读 s.last——沿用 useState 在渲染期间
  // 更新后要等下一次渲染才生效的写法，这一帧读 s.last 仍是变化前的值。
  if (changedIdentity) {
    setS({ identity, last: q.data });
  } else if (q.data !== undefined && q.data !== s.last) {
    setS({ identity, last: q.data });
  }
  const retained = changedIdentity ? undefined : s.last;
  return { data: q.data ?? retained, error: q.error, stale: q.data === undefined && retained !== undefined };
}
