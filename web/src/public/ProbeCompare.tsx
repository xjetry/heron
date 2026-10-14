import { useQuery } from "@connectrpc/connect-query";
import { useMemo } from "react";
import { useParams } from "react-router";
import { errorBanner, queryGate } from "../api/queryGate";
import { ProbeComparison, type ProbeComparisonMethods } from "../components/ProbeComparison";
import { PublicService } from "../gen/heron/v1/public_pb";
import { taskIdParam } from "../lib/probeComparison";
import { TRAFFIC_MS } from "../lib/poll";

const METHODS: ProbeComparisonMethods = {
  list: PublicService.method.listProbeComparisonNodes,
  query: PublicService.method.queryProbeComparison,
};

export function PublicProbeCompare() {
  const { id } = useParams();
  const taskId = taskIdParam(id);
  const snap = useQuery(PublicService.method.getSnapshot, {}, { enabled: taskId !== undefined, refetchInterval: TRAFFIC_MS });
  const named = useMemo(() => (snap.data?.nodes ?? []).map((node) => ({ id: node.id, name: node.name })), [snap.data]);
  if (taskId === undefined) return <p role="alert" className="error">任务编号无效。</p>;
  const gate = queryGate(snap);
  if (!gate.ready) return gate.loading ?? errorBanner(...gate.errors);
  return <>{gate.banner}<ProbeComparison taskId={taskId} methods={METHODS} nodes={named} now={Number(gate.data.now)} /></>;
}
