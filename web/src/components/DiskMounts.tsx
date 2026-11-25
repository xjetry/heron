import type { GetNodeDisksResponse } from "../gen/heron/v1/admin_pb";
import { bytes, percent } from "../lib/format";
import { Bar, usage } from "./Bar";
import { Timestamp } from "./Timestamp";

// 逐盘容量只在 hub 内存里（GetNodeDisks）：hub 重启后到节点再次上报前没有读数，和"这次上报没有逐盘读数"是两种情形，
// 前者看 sampledAt 是否缺席，后者看 total 是否为 0，分开说明，不显示成空表。列表已按使用率降序排好（hub 准入时复核），
// 这里不重排，首行就是最满的盘。
export function DiskMounts({ data }: { data?: GetNodeDisksResponse }) {
  const truncated = data !== undefined && data.total > data.mounts.length;
  return <section className="card" aria-label="本地磁盘">
    <h2>本地磁盘</h2>
    {data === undefined ? <p className="muted">加载中…</p>
      : data.sampledAt === undefined ? <p className="muted">hub 启动后尚未收到该节点的上报。</p>
        : data.total === 0 ? <p className="muted">最近一次上报没有逐盘读数（采集失败见「Agent 诊断」，或 Agent 版本不提供）。</p>
          : <>
            <p className="muted">
              {data.online ? "最近一次上报" : "离线前最后一次上报"}（<Timestamp at={data.sampledAt} />），共 {data.total} 块{truncated && `，仅展示最满的 ${data.mounts.length} 块`}。共享同一块设备或存储池的挂载点只列一个。
            </p>
            <table className="nodes">
              <thead><tr><th scope="col">挂载路径</th><th scope="col">已用 / 容量</th><th scope="col">使用率</th></tr></thead>
              <tbody>
                {data.mounts.map((m) => {
                  const u = usage(m.used, m.total);
                  return <tr key={m.path}>
                    <td><code>{m.path}</code></td>
                    <td className="num">{bytes(m.used)} / {bytes(m.total)}</td>
                    <td>{u && <Bar value={u.pct} label={percent(u.pct)} thin />}</td>
                  </tr>;
                })}
              </tbody>
            </table>
          </>}
  </section>;
}
