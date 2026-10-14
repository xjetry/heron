import { create } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import { createConnectQueryKey, useMutation, useQuery } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { Link, useParams } from "react-router";
import { errorText } from "../api/auth";
import { errorBanner, queryGateAll } from "../api/queryGate";
import { AdminService, CertPinChangeSchema, SaveProbeTaskRequestSchema, type NodeProbeCertificate } from "../gen/heron/v1/admin_pb";
import { PresentedReason, ProbeTaskSchema } from "../gen/heron/v1/types_pb";
import { formatPin } from "../lib/certpin";
import { Timestamp } from "../components/Timestamp";

const reasonText: Record<number, string> = {
  [PresentedReason.CA_VERIFY_FAILED]: "证书校验失败",
  [PresentedReason.PIN_MISMATCH]: "指纹不符",
  [PresentedReason.OUTSIDE_VALIDITY]: "不在有效期内",
};

function when(seconds: bigint) {
  return <Timestamp at={seconds} />;
}

export function ProbeCerts() {
  const { id = "" } = useParams();
  const taskId = BigInt(id);
  const qc = useQueryClient();
  const certs = useQuery(AdminService.method.listProbeCertificates, { taskId });
  const nodes = useQuery(AdminService.method.listNodes, {});
  const save = useMutation(AdminService.method.saveProbeTask);
  const [notice, setNotice] = useState("");
  const refresh = () => {
    void qc.invalidateQueries({ queryKey: createConnectQueryKey({ schema: AdminService.method.listProbeCertificates, cardinality: "finite" }) });
    void qc.invalidateQueries({ queryKey: createConnectQueryKey({ schema: AdminService.method.listProbeTasks, cardinality: "finite" }) });
  };
  const gate = queryGateAll(certs, nodes);
  if (!gate.ready) return gate.loading ?? errorBanner(...gate.errors);
  const [data, nodeList] = gate.data;
  const nameOf = (nodeId: bigint) => nodeList.nodes.find((n) => n.id === nodeId)?.name ?? `#${nodeId}`;
  const trust = (node: NodeProbeCertificate) => {
    const spki = node.candidate?.spkiSha256;
    if (!spki || !data.configId) return;
    const req = create(SaveProbeTaskRequestSchema, {
      task: create(ProbeTaskSchema, { id: taskId }),
      certPin: create(CertPinChangeSchema, { action: { case: "setSpkiSha256", value: spki } }),
      expectedConfigId: data.configId,
    });
    setNotice("");
    save.mutate(req, {
      onSuccess: () => {
        refresh();
        setNotice(`${nameOf(node.nodeId)} 的指纹已信任，已重新读取证书观测。`);
      },
      onError: (err) => {
        // 配置已经变了就停在这一次，不带着旧身份再试；数据重新读，避免页面停在旧身份上。
        if (err instanceof ConnectError && err.code === Code.FailedPrecondition) {
          refresh();
          setNotice("配置已变化，已重新读取。不会自动重试。");
          return;
        }
        setNotice(errorText(err));
      },
    });
  };
  return (
    <section>
      <p><Link to="/probes">返回探测任务</Link></p>
      <h1>证书观测</h1>
      <p className="muted">请离线核对指纹后再信任。各节点指纹不一致时逐个确认，没有一次信任全部的操作。候选不会自动生效。</p>
      <p>当前指纹：{data.certSpkiSha256?.length ? formatPin(data.certSpkiSha256) : "未钉"}</p>
      {notice && <p role="alert">{notice}</p>}
      {data.nodes.map((node) => (
        <article key={String(node.nodeId)} className="card" aria-label={nameOf(node.nodeId)}>
          <h2>{nameOf(node.nodeId)}</h2>
          {node.current ? <p>当前证书到期 {when(node.current.notAfterS)}，观测于 {when(node.current.observedAt)}</p> : <p className="muted">当前身份下没有成功证书。</p>}
          {node.candidate && (
            <p>
              候选 {formatPin(node.candidate.spkiSha256)}，{reasonText[node.candidate.reason] ?? "未知原因"}，到期 {when(node.candidate.notAfterS)}，观测于 {when(node.candidate.observedAt)}。
              {" "}<button type="button" disabled={save.isPending} onClick={() => trust(node)}>信任此指纹</button>
            </p>
          )}
        </article>
      ))}
      {data.nodes.length === 0 && <p className="muted">没有分配到可见节点。</p>}
    </section>
  );
}
