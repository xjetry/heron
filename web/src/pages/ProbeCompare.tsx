import { useQuery } from "@connectrpc/connect-query";
import { useMemo } from "react";
import { Link, useParams } from "react-router";
import { errorBanner, queryGateAll } from "../api/queryGate";
import { ProbeComparison, type ProbeComparisonMethods } from "../components/ProbeComparison";
import { AdminService } from "../gen/heron/v1/admin_pb";
import { taskIdParam } from "../lib/probeComparison";
import { TRAFFIC_MS } from "../lib/poll";

const METHODS: ProbeComparisonMethods = {
  list: AdminService.method.listProbeComparisonNodes,
  query: AdminService.method.queryProbeComparison,
};

export function ProbeCompare() {
  const { id } = useParams();
  const taskId = taskIdParam(id);
  const nodes = useQuery(AdminService.method.listNodes, {}, { enabled: taskId !== undefined });
  const snap = useQuery(AdminService.method.getSnapshot, {}, { enabled: taskId !== undefined, refetchInterval: TRAFFIC_MS });
  const named = useMemo(() => (nodes.data?.nodes ?? []).map((node) => ({ id: node.id, name: node.name })), [nodes.data]);
  if (taskId === undefined) return <p role="alert" className="error">任务编号无效。</p>;
  const gate = queryGateAll(nodes, snap);
  if (!gate.ready) return gate.loading ?? errorBanner(...gate.errors);
  return (
    <>
      {gate.banner}
      <p><Link to="/probes">返回探测任务</Link></p>
      <ProbeComparison taskId={taskId} methods={METHODS} nodes={named} now={Number(gate.data[1].now)} />
    </>
  );
}
