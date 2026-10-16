import { getOption, type DescMethod } from "@bufbuild/protobuf";
import { MethodOptions_IdempotencyLevel } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError, type Interceptor } from "@connectrpc/connect";
import { access, Access } from "../gen/heron/v1/access_pb";

// 这是客户端等待预算，不是服务端合法请求的总耗时上界：超过预算的慢读也会取消。
// GetUpdates 的 Latest 外呼预算 15s（api/updates.go），Linux 本地更新器 HTTP 调用至多 3s
// （internal/update/protocol.go）；文件检查与 SQL 无硬上界。两端
// QueryMetrics/QueryProbes/QueryProbeComparison 排队至多 5s（api/history_gate.go），执行无硬上界。
// SQLite 争锁 busy_timeout=5s（store/store.go），并不约束 SQL 执行总时间。
// 30s 高于已知显式子阶段预算，不能据此声称任何合法 RPC 都能在 30s 内完成。
// 其余读取同样无总时限，细分路径如下（均在 internal/hub 下）：
// ListOperations: api/changes.go -> store/change.go；ListNotifyChannelRefs: api/changes.go 内存配置；
// ListNodes/GetRegisterWindow/ListTags: api/nodes.go、tags.go -> store；
// GetSnapshot: api/data.go、public.go -> store/live；GetTraffic: api/traffic.go -> traffic；
// ListProbeTasks/ListProbeCertificates: api/probes.go -> store/探测内存态；
// ListProbeComparisonNodes: api/comparison.go -> store 读事务（两端）；
// ListAlertRules/ListAlertEvents/ListSilences: api/alerts.go、silences.go -> store/告警内存态；
// GetSettings/GetHeartbeatStatus: api/settings.go -> store/心跳内存态；GetBackupStatus: api/backup_status.go -> store/备份态；
// ListRestoreRecords: api/restore_records.go -> store/restore_record.go；
// GetStorageStats: api/settings.go -> store/stats.go 全表 COUNT 与文件 stat；
// GetApiReference: api/reference.go 嵌入文件；GetCaller: api/tokens.go 取入口鉴权读到的主体，不另读库；
// GetSite: api/public.go -> store.SiteSettings。
// 上述方法的本地锁、SQL、文件读取与序列化均无硬上界，不能把无外呼当作零耗时。
// 两个 QueryClient 共用 retryQuery（再试两次）及 TanStack 默认退避 1s、2s：连续挂住
// 在 3 * 30s + 1s + 2s = 93s 后最终报错。前提是页面前台、在线、事件循环未冻结；
// 后台轮询/重试暂停与离线暂停不受这条墙钟保证覆盖。直接调用的对比分块没有查询重试。
export const READ_DEADLINE_MS = 30_000;
export const READ_DEADLINE_MESSAGE = `请求超过 ${READ_DEADLINE_MS / 1000} 秒等待预算`;

function isRead(method: DescMethod): boolean {
  return method.idempotency === MethodOptions_IdempotencyLevel.NO_SIDE_EFFECTS || getOption(method, access) === Access.READ;
}

// 拦截器读描述符声明，不靠名字或手写名单；缺声明默认不加截止时间，避免取消有副作用的调用。
export const readDeadline: Interceptor = (next) => async (req) => {
  if (!isRead(req.method)) return next(req);
  const controller = new AbortController();
  const timeout = new ConnectError(READ_DEADLINE_MESSAGE, Code.DeadlineExceeded);
  const existing = req.header.get("Connect-Timeout-Ms");
  req.header.set("Connect-Timeout-Ms", String(existing === null ? READ_DEADLINE_MS : Math.min(Number(existing), READ_DEADLINE_MS)));
  let cancel!: () => void;
  let timer: ReturnType<typeof setTimeout> | undefined;
  const stopped = new Promise<never>((_, reject) => {
    cancel = () => {
      const error = ConnectError.from(req.signal.reason, Code.Canceled);
      reject(error);
      controller.abort(error);
    };
    if (req.signal.aborted) cancel();
    else {
      req.signal.addEventListener("abort", cancel, { once: true });
      timer = setTimeout(() => {
        reject(timeout);
        controller.abort(timeout);
      }, READ_DEADLINE_MS);
    }
  });
  try {
    // fetch 与响应体读取共用 signal；race 也为未响应取消的下游提供有界完成保证。
    return await Promise.race([stopped, next({ ...req, signal: controller.signal })]);
  } finally {
    clearTimeout(timer);
    req.signal.removeEventListener("abort", cancel);
  }
};
