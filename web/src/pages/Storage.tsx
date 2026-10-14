import { useQuery } from "@connectrpc/connect-query";
import { PageHeader } from "../components/PageHeader";
import { errorBanner, queryGate } from "../api/queryGate";
import { AdminService, type GetStorageStatsResponse, type SeriesTableHealth } from "../gen/heron/v1/admin_pb";
import { bytes, dateTime, duration } from "../lib/format";
import { requireCleanupPending, requireSqlObservedAt } from "../lib/storageStats";
import { walObservationView, walOverLimitHint } from "../lib/wal";

const at = (unix: bigint) => dateTime(unix);

// 读数缺失的两种含义不同：表为空（没有最老桶），与本级没有水位（1m 级不经上卷写入）。
function Oldest({ h }: { h: SeriesTableHealth }) {
  if (h.oldestTs === undefined) return <span className="muted">空表</span>;
  // 是否标红只看 hub 给的结论：阈值在 hub 一处判定（store.SeriesHealth.Staleness），这里不重算。
  return h.oldestStale ? <span className="error">{at(h.oldestTs)}（超出保留期）</span> : <>{at(h.oldestTs)}</>;
}

function Watermark({ h }: { h: SeriesTableHealth }) {
  if (h.watermarkTs === undefined) return <span className="muted">—</span>;
  return h.watermarkStale ? <span className="error">{at(h.watermarkTs)}（上卷滞后）</span> : <>{at(h.watermarkTs)}</>;
}

// 完成时刻只在整轮成功时记下：缺失即从未成功跑过。
const finished = (unix: bigint | undefined) => (unix === undefined ? <span className="muted">从未成功</span> : at(unix));

// 显示长度、无文件、大小未知还是未知，只由 walObservationView 决定；超限提示是否出现、说什么，
// 只由 walOverLimitHint 决定。这里不把读失败补成 0，也不自己按长度另设阈值：它不是未检查点的数据量。
function WalObservation({ s }: { s: GetStorageStatsResponse }) {
  const wal = walObservationView(s.wal);
  const hint = walOverLimitHint(s.wal, s.walSizeLimitBytes);
  return (
    <>
      <p>WAL 文件：{wal.text}（观测于 {at(wal.observedAt)}）</p>
      {hint && <p className="warn">{hint}</p>}
      <p className="muted">这是 -wal 文件的实际长度，不是未检查点的数据量：WAL 重置后从文件开头复用，长度不缩小，单次读数不说明是否需要检查点。</p>
    </>
  );
}

// 迁移前备份的两种空：列举失败（schema_backups_error 非空）与确实没有备份。
// 失败照实显示错误，不能说成没有备份。wal_size_limit_bytes 不参与这一判断。
function SchemaBackups({ s }: { s: GetStorageStatsResponse }) {
  if (s.schemaBackupsError !== "") return <p className="error">无法读取迁移前备份：{s.schemaBackupsError}</p>;
  const note = (
    <p className="muted">
      hub 启动时发现要升级数据库 schema，先在库旁做一份迁移前备份再迁移，按文件名末尾的序号保留最近两份；新旧以序号为准，修改时间只作参考。回退步骤见项目文档 docs/operations.md 的「升级与回退」。
    </p>
  );
  if (s.schemaBackups.length === 0) {
    return (
      <>
        <p>迁移前备份：无</p>
        {note}
      </>
    );
  }
  return (
    <>
      {note}
      <div className="table-scroll" role="region" aria-label="迁移前备份" tabIndex={0}>
        <table className="nodes">
          <thead><tr><th>路径</th><th>schema 版本</th><th>大小</th><th>修改时间</th></tr></thead>
          <tbody>
            {s.schemaBackups.map((b) => (
              <tr key={b.path}>
                <td>{b.path}</td>
                <td>{b.schemaVersion}</td>
                <td>{bytes(b.bytes)}</td>
                <td>{at(b.modifiedAt)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </>
  );
}

function CleanupPending({ pending }: { pending: bigint }) {
  return (
    <>
      <p>待清理的已删除节点与任务：{String(pending)}</p>
      <p className="muted">
        删除节点或探测任务后，它们的历史行由维护循环每分钟分块删除一部分；删除一生效这些行就不再出现在任何查询里，只是还占着空间。满配节点约 9 分钟清完，长期不降是清理在反复失败（原因见 hub 日志）或积压。
      </p>
    </>
  );
}

export function Storage() {
  const stats = useQuery(AdminService.method.getStorageStats, {});
  const gate = queryGate(stats);
  if (!gate.ready) return gate.loading ?? errorBanner(...gate.errors);
  const s = gate.data;
  const sqlObservedAt = requireSqlObservedAt(s.sqlObservedAt);
  const cleanupPending = requireCleanupPending(s.cleanupPending);
  return (
    <section>
      {gate.banner}
      <PageHeader title="存储" />
      <p className="muted">统计于 {at(sqlObservedAt)}；同一份 SQL 统计在算出后 60 秒内复用。</p>
      <p>数据库逻辑大小：{bytes(s.dbBytes)}</p>
      <p className="muted">
        逻辑大小是 SQL 快照里的页数乘页大小，不含 -wal 与 -shm 文件；与 WAL 文件观测不是同一时刻读出的。
      </p>
      <WalObservation s={s} />
      <p>
        上次清理完成：{finished(s.lastPruneAt)}；上次上卷完成：{finished(s.lastRollupAt)}
      </p>
      <CleanupPending pending={cleanupPending} />
      <p className="muted">
        清理停了只表现为库慢慢变大，上卷停了只表现为长窗口的图变空。最老桶对照保留期，标红表示超期的行没有被清掉（细一级只清理已上卷的部分，上卷停了它也会标红）；水位对照当前时刻，标红表示上卷停了，新库第一轮上卷之前也会标红约一分钟。
      </p>
      <div className="table-scroll" role="region" aria-label="时序表健康" tabIndex={0}>
        <table className="nodes">
          <thead><tr><th>表</th><th>桶长</th><th>保留期</th><th>最老桶</th><th>上卷水位</th></tr></thead>
          <tbody>
            {s.series.map((h) => (
              <tr key={h.table}>
                <td>{h.table}</td>
                <td>{duration(h.bucketS)}</td>
                <td>{duration(h.retentionS)}</td>
                <td><Oldest h={h} /></td>
                <td><Watermark h={h} /></td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <SchemaBackups s={s} />
      <div className="table-scroll" role="region" aria-label="各表行数" tabIndex={0}>
        <table className="nodes">
          <thead><tr><th>表</th><th>行数</th></tr></thead>
          <tbody>
            {s.tables.map((t) => <tr key={t.name}><td>{t.name}</td><td>{String(t.rows)}</td></tr>)}
          </tbody>
        </table>
      </div>
    </section>
  );
}
