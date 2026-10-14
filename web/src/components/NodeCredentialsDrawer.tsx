import { errorBanner } from "../api/queryGate";
import { InstallCommands } from "./InstallCommands";
import { Drawer } from "./Modal";
import { Secret } from "./Secret";

// 安装凭据明文只由创建或换发响应提供，hub 只存哈希；关闭弹窗后不能恢复，需要时重新换发。hubVersion 与
// boundAgentVersion 同属快照响应：hubVersion 未到时命令区不渲染，绑定版本也一样。
export function NodeCredentialsDrawer({ title, secretLabel, token, hubVersion, boundAgentVersion, error, reRegister = false, opener, onClose }: {
  title: string; secretLabel: string; token: string; hubVersion?: string; boundAgentVersion?: string; error?: unknown; reRegister?: boolean; opener: HTMLElement; onClose: () => void;
}) {
  return <Drawer title={title} description="这是仅用于注册的安装凭据，不能上报指标。关闭后无法再看到明文 token，hub 只存哈希。" opener={opener} onClose={onClose}>
    <div className="modal-body">
      <Secret label={secretLabel} value={token} />
      {reRegister && <p>此命令会重新注册，替换本机原有的节点身份；本地探测策略不会删除。</p>}
      <p className="muted">普通升级保留现有配置，不需要换发凭据或重新注册。</p>
      <p>在被监控的机器上以 root 执行（agent 若经其他地址访问 hub，在下面的 Hub 连接地址里填写）：</p>
      {errorBanner(error)}
      {hubVersion === undefined
        ? error == null && <p className="muted">正在读取 hub 版本，读到后即可复制安装命令…</p>
        : <InstallCommands hubVersion={hubVersion} boundAgentVersion={boundAgentVersion ?? ""} origin={window.location.origin} registerKey={token} reRegister={reRegister} />}
    </div>
    <footer className="modal-footer"><button type="button" className="primary-button" onClick={onClose}>完成</button></footer>
  </Drawer>;
}
