import { CollectionComponent, NetStructureReason, type AgentDiagnostics as Diagnostics } from "../gen/heron/v1/types_pb";
import { Timestamp } from "./Timestamp";

const COLLECTORS: Record<CollectionComponent, string> = {
  [CollectionComponent.UNSPECIFIED]: "未知类别",
  [CollectionComponent.BOOT_ID]: "启动标识",
  [CollectionComponent.CPU]: "CPU",
  [CollectionComponent.MEMORY]: "内存",
  [CollectionComponent.SWAP]: "交换空间",
  [CollectionComponent.DISK]: "磁盘",
  [CollectionComponent.LOAD]: "负载",
  [CollectionComponent.PROCS]: "进程数",
  [CollectionComponent.UPTIME]: "运行时间",
  [CollectionComponent.CONNS]: "连接数",
  [CollectionComponent.NET]: "网络",
  [CollectionComponent.DISK_IO]: "磁盘 I/O",
  [CollectionComponent.TCP_STATS]: "TCP 计数",
};

const STRUCTURE_REASONS: Record<NetStructureReason, string> = {
  [NetStructureReason.UNSPECIFIED]: "未知原因",
  [NetStructureReason.STACKED]: "叠加设备",
  [NetStructureReason.BRIDGE_PORT]: "网桥虚拟口",
  [NetStructureReason.OVERLAY]: "overlay",
  [NetStructureReason.TUNNEL]: "三层隧道",
};

export function AgentDiagnostics({ diagnostics, updatedAt }: { diagnostics?: Diagnostics; updatedAt?: bigint }) {
  const networkFailed = diagnostics?.failedCollectors.includes(CollectionComponent.NET);
  return <section className="card" aria-label="Agent 运行诊断">
    <h2>Agent 运行诊断</h2>
    {!diagnostics ? <p className="muted">Agent 未提供诊断信息，请更新 Agent 后等待上报。</p> : <>
      <p className="muted">最近保存的诊断，不保证实时健康。生效参数只读。</p>
      <dl className="facts">
        <dt>诊断信息更新时间</dt><dd>{updatedAt && updatedAt > 0n ? <Timestamp at={updatedAt} /> : "未知"}</dd>
        <dt>生效上报间隔</dt><dd>{diagnostics.reportIntervalMs > 0 ? <>{diagnostics.reportIntervalMs} ms <span className="muted">（不含请求耗时和失败退避）</span></> : "未知"}</dd>
        <dt>包含规则</dt><dd>{diagnostics.netInclude.length ? <code>{diagnostics.netInclude.join(" ")}</code> : "全部接口"}</dd>
        <dt>排除规则</dt><dd>{diagnostics.netExclude.length ? <code>{diagnostics.netExclude.join(" ")}</code> : "无"}</dd>
        <dt>实际计入接口</dt><dd>{networkFailed ? <span className="warn">本次网络采集失败，接口清单不可用</span> : diagnostics.netInterfacesTotal === 0 ? "没有计入的接口" : <>
          <div className="muted">共 {diagnostics.netInterfacesTotal} 个{diagnostics.netInterfacesTotal > diagnostics.netInterfaces.length && `，仅展示前 ${diagnostics.netInterfaces.length} 个`}</div>
          {diagnostics.netInterfaces.map((name) => <code className="tag" key={name}>{name}</code>)}
        </>}</dd>
        {/* agent 的网络采集失败时清空结构排除列表与退回标志，空列表不能证明没有接口被排除；
            本组件先检查 failedCollectors 的 NET 标记，再解释结构结果，避免把失败显示成“没有被排除的接口”。 */}
        <dt>结构规则</dt><dd>{!diagnostics.netStructureRules ? "未启用（显式网卡规则或非 Linux）" : networkFailed ? <span className="warn">本次网络采集失败，结构判定不可用</span> : diagnostics.netStructureFallback
          ? <span className="warn">结构规则会排除全部网卡，本次只按名字规则计入</span>
          : diagnostics.netStructureExcludedTotal === 0 ? "已启用，没有被排除的接口" : <>
            <div className="muted">已启用，排除 {diagnostics.netStructureExcludedTotal} 个{diagnostics.netStructureExcludedTotal > diagnostics.netStructureExcluded.length && `，仅展示前 ${diagnostics.netStructureExcluded.length} 个`}</div>
            {diagnostics.netStructureExcluded.map((e) => <code className="tag" key={e.name}>{e.name} · {STRUCTURE_REASONS[e.reason] ?? "未知原因"}</code>)}
          </>}</dd>
        <dt>采集失败项</dt><dd>{diagnostics.failedCollectors.length ? <span className="warn">{diagnostics.failedCollectors.map((component) => COLLECTORS[component] ?? "未知类别").join("、")}</span> : "最近采集未报告失败"}</dd>
      </dl>
    </>}
  </section>;
}
