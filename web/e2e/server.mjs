import { spawn, spawnSync } from 'node:child_process';
import { mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { once } from 'node:events';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createProxy } from './proxy.mjs';

const dir = mkdtempSync(join(tmpdir(), 'heron-browser-'));
// 临时目录在本进程的 exit 事件里删：正常结束、process.exit 与启动阶段抛错都经过它。hub 子进程退出后本进程才
// process.exit，删的时候已没有人在写库。SIGKILL 不经过任何处理函数，所以 playwright.config.ts 的 webServer
// 必须配 gracefulShutdown，否则 Playwright 收尾时按默认 SIGKILL 进程组，每跑一次留下一个目录。
process.on('exit', () => rmSync(dir, { recursive: true, force: true }));
const db = join(dir, 'hub.db');
const binary = new URL('../../bin/heron-hub', import.meta.url).pathname;
const password = 'local-browser-test-password';
const key = join(dir, 'key.pem'), cert = join(dir, 'cert.pem');
const tls = spawnSync('openssl', ['req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-keyout', key, '-out', cert, '-days', '1', '-subj', '/CN=localhost'], { encoding: 'utf8' });
if (tls.status !== 0) throw new Error(tls.stderr);
const setup = spawnSync(binary, ['passwd', '--db', db], { input: password + '\n', encoding: 'utf8' });
if (setup.status !== 0) throw new Error(setup.stderr);
let closing = false, restarting = false;
function startHub() {
  const child = spawn(binary, ['serve', '--db', db, '--listen', '127.0.0.1:18987', '--trusted-proxies', '127.0.0.1/32'], { stdio: 'inherit' });
  child.on('exit', code => {
    if (restarting) return;
    process.exit(closing ? 0 : code || 1);
  });
  return child;
}
let hub = startHub();
const proxy = createProxy({
  key: readFileSync(key), cert: readFileSync(cert), upstream: { host: '127.0.0.1', port: 18987 },
  // 此控制口只在回环测试代理内存在，重启沿用数据库，不能通过重新建库伪造持久化验收。
  onRestart: async () => {
    restarting = true;
    const exited = once(hub, 'exit');
    hub.kill('SIGTERM');
    await exited;
    hub = startHub();
    restarting = false;
  },
});
proxy.listen(18988, '127.0.0.1');
for (const signal of ['SIGTERM', 'SIGINT']) process.on(signal, () => { closing = true; proxy.close(); hub.kill('SIGTERM'); });
