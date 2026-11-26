import { useMutation } from "@connectrpc/connect-query";
import { errorBanner } from "../api/queryGate";
import { AdminService } from "../gen/heron/v1/admin_pb";
import { withId } from "../lib/ids";
import { CREDENTIALS_DESCRIPTION, NodeCredentialsBody } from "./NodeCredentialsDrawer";
import { Drawer } from "./Modal";

// 节点详情的「安装命令」入口。hub 每个节点只存一份凭据哈希（auth.RotateToken 以新哈希替换旧哈希），命令里的凭据只能现场换发，
// 而换发同时作废节点现用的凭据：上报过的节点上那台 agent 从此通不过鉴权，直到在主机上用新命令重新注册。入口名字不像换 token
// 那样自带后果，所以先说明、经确认才换发；reported 由调用方按 neverReported 实时给出，确认前节点开始上报时说明随之改成警告。
// 换发出的是节点凭据，命令带 --re-register（与节点列表的换 token 同一理由：目标主机若留有旧配置，不重新注册就会沿用旧身份）。
// 明文只在这次换发响应里，抽屉关闭即丢弃；已出租的机器不装 agent，调用方不提供这个入口。
export function NodeInstallDrawer({ node, reported, hubVersion, boundAgentVersion, snapshotError, opener, onClose }: {
  node: { id: bigint; name: string }; reported: boolean; hubVersion?: string; boundAgentVersion?: string; snapshotError?: unknown; opener: HTMLElement; onClose: () => void;
}) {
  const rotate = useMutation(AdminService.method.rotateNodeToken);
  const label = withId(node.name, node.id);
  const token = rotate.data?.token;
  if (token !== undefined) {
    return <Drawer title="安装命令" description={CREDENTIALS_DESCRIPTION} opener={opener} onClose={onClose}>
      <NodeCredentialsBody secretLabel={`节点 ${label} 的新 token`} token={token} hubVersion={hubVersion} boundAgentVersion={boundAgentVersion} error={snapshotError} reRegister />
      <footer className="modal-footer"><button type="button" className="primary-button" onClick={onClose}>完成</button></footer>
    </Drawer>;
  }
  return <Drawer title="安装命令" description="hub 只存凭据哈希，安装命令里的 token 要现场换发，旧 token 随即作废。" opener={opener} onClose={onClose} busy={rotate.isPending}>
    <div className="modal-body">
      {reported
        ? <p className="error">{label} 上报过指标：换发后主机上正在运行的 agent 立即无法上报，直到在主机上用新命令重新安装。只是升级 agent 不需要换发，用在线更新即可。</p>
        : <p>{label} 从未上报，没有 agent 在用当前凭据，换发不影响任何主机。</p>}
      {errorBanner(rotate.error)}
    </div>
    <footer className="modal-footer">
      <button type="button" disabled={rotate.isPending} onClick={onClose}>取消</button>
      <button type="button" className={reported ? "danger" : "primary-button"} disabled={rotate.isPending} onClick={() => rotate.mutate({ id: node.id })}>换发 token 并生成安装命令</button>
    </footer>
  </Drawer>;
}
