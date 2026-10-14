import { useQuery } from "@connectrpc/connect-query";
import { errorBanner, queryGate } from "../api/queryGate";
import { AdminService, type BackupLayerStatus, type RestoreRecord } from "../gen/heron/v1/admin_pb";
import { dateTime } from "../lib/format";
import { EmptyState } from "./EmptyState";

const at = (unix: bigint) => dateTime(unix);

function failureAdvice(category: string): string {
  const advice: Record<string, string> = {
    upload: "检查对象存储连接、凭据和写入权限",
    theme_upload: "主题包上传失败，请检查对象存储连接、凭据和写入权限",
    list: "检查对象存储的列举权限", theme_list: "检查主题目录的列举权限",
    delete: "检查对象存储的删除权限", theme_delete: "检查主题对象的删除权限",
    settings: "检查备份设置中的值并重新保存",
    marker: "故障标记无法读取，请查看 hub 日志中的数据库错误",
    startup: "检查数据库目录的读写权限和磁盘空间",
    snapshot: "检查数据库和磁盘空间", cleanup: "检查暂存目录的删除权限",
    record: "检查数据库是否可写", theme_record: "检查数据库是否可写，上传标记保存失败",
    theme_read: "检查数据库读取错误", client: "检查对象存储地址和凭据配置",
    retention_config: "将备份保留份数设置为至少一份",
    unrecovered: "等待备份重新执行，或查看 hub 日志中的未恢复故障",
  };
  return advice[category.split("/")[0]] ?? "查看 hub 日志以定位故障";
}

// 故障按后端给的原样显示，不按 enabled 过滤：停用后后端已不再报旧故障（GetBackupStatusResponse.enabled），
// 而设置读不出时 enabled 同样为 false，配置层的 settings 故障仍须可见。
function Layer({ name, status }: { name: string; status: BackupLayerStatus | undefined }) {
  return <article className="card" aria-label={name}>
    <h3>{name}</h3>
    <p>上次成功：{status?.lastSuccessAt === undefined ? "从未成功" : at(status.lastSuccessAt)}</p>
    {status?.failure
      ? <p className="error">当前故障：{status.failure.category}{status.failure.statusCode ? `（HTTP ${status.failure.statusCode}）` : ""}；自 {at(status.failure.sinceAt)} 起</p>
      : <p className="muted">无当前故障</p>}
    {status?.failure && <p>{failureAdvice(status.failure.category)}</p>}
  </article>;
}

export function BackupStatus() {
  const status = useQuery(AdminService.method.getBackupStatus, {}, { refetchInterval: 5000 });
  const gate = queryGate(status);
  if (!gate.ready) return gate.loading ?? errorBanner(...gate.errors);
  return <section aria-label="备份状态">
    {gate.banner}
    <p>{gate.data.enabled ? "自动备份已启用" : "自动备份未启用"}</p>
    {gate.data.themesWithoutPackage.map((id) => <p key={id}>主题 {id} 未备份：请重新上传原包</p>)}
    <Layer name="配置与凭据" status={gate.data.config} />
    <Layer name="指标与探测历史" status={gate.data.metrics} />
  </section>;
}

// 恢复记录：离线恢复（heron-hub restore）写下的审计历史。恢复只在停机时发生，
// 这里打开时查一次即可，不轮询。指标快照缺席不是 0，是那次恢复没带指标层；
// 逐表明细放 title，表里只给总计数与份数。
export function RestoreRecords() {
  const records = useQuery(AdminService.method.listRestoreRecords, {});
  const gate = queryGate(records);
  if (!gate.ready) return <section aria-label="恢复记录">{gate.loading ?? errorBanner(...gate.errors)}</section>;
  const list = gate.data.records;
  return <section aria-label="恢复记录">
    {gate.banner}
    {list.length === 0
      ? <EmptyState title="没有恢复记录">hub 从未离线恢复过：记录由 heron-hub restore 在停机时写入，随配置层备份。</EmptyState>
      : <div className="table-scroll" role="region" aria-label="恢复记录表" tabIndex={0}>
        <table className="nodes">
          <thead><tr><th>恢复时间</th><th>配置快照时刻</th><th>指标快照时刻</th><th>清掉的孤儿行</th><th>主题</th></tr></thead>
          <tbody>
            {list.map((r) => <RestoreRecordRow key={r.id} r={r} />)}
          </tbody>
        </table>
      </div>}
  </section>;
}

function RestoreRecordRow({ r }: { r: RestoreRecord }) {
  const orphanTotal = r.orphans.reduce((sum, o) => sum + o.rows, 0n);
  const orphanDetail = r.orphans.map((o) => `${o.table}：${o.rows} 行`).join("；");
  const themes = r.themes ?? { restored: [], missing: [], ignored: [] };
  const themeDetail = [
    themes.restored.length > 0 ? `恢复：${themes.restored.join("、")}` : "",
    themes.missing.length > 0 ? `缺包：${themes.missing.join("、")}` : "",
    themes.ignored.length > 0 ? `忽略：${themes.ignored.join("、")}` : "",
  ].filter((part) => part !== "").join("；");
  return <tr>
    <td data-label="恢复时间" className="num">{dateTime(r.restoredAt)}</td>
    <td data-label="配置快照时刻" className="num">{dateTime(r.configTakenAt)}</td>
    <td data-label="指标快照时刻" className="num">{r.metricsTakenAt === undefined ? <span className="muted">未恢复指标层</span> : dateTime(r.metricsTakenAt)}</td>
    {/* 零计数的表也在内：恢复按节点从属清单全表登记，不省略没有孤儿的表。 */}
    <td data-label="清掉的孤儿行" title={orphanDetail}>{orphanTotal.toString()} 行</td>
    <td data-label="主题" title={themeDetail}>恢复 {themes.restored.length} / 缺包 {themes.missing.length} / 忽略 {themes.ignored.length}</td>
  </tr>;
}
