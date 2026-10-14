import { type Page } from "@playwright/test";
import { readFile } from "node:fs/promises";
import { AdminService } from "../src/gen/heron/v1/admin_pb";
import { expect, login as loginSession, must, mustField, rpc, test } from "./fixtures";

// 固定测试密码只用于每次临时创建的本机数据库，服务器退出后整个目录删除。
const password = "local-browser-test-password";
function zip(files: Record<string, string>): Buffer {
  const parts: Buffer[] = [], directory: Buffer[] = [];
  let offset = 0;
  for (const [name, content] of Object.entries(files)) {
    const path = Buffer.from(name), bytes = Buffer.from(content);
    let crc = 0xffffffff;
    for (const byte of bytes) { crc ^= byte; for (let i = 0; i < 8; i++) crc = crc & 1 ? (crc >>> 1) ^ 0xedb88320 : crc >>> 1; }
    crc = (crc ^ 0xffffffff) >>> 0;
    const header = Buffer.alloc(30); header.writeUInt32LE(0x04034b50); header.writeUInt16LE(20, 4); header.writeUInt32LE(crc, 14); header.writeUInt32LE(bytes.length, 18); header.writeUInt32LE(bytes.length, 22); header.writeUInt16LE(path.length, 26);
    const central = Buffer.alloc(46); central.writeUInt32LE(0x02014b50); central.writeUInt16LE(20, 4); central.writeUInt16LE(20, 6); central.writeUInt32LE(crc, 16); central.writeUInt32LE(bytes.length, 20); central.writeUInt32LE(bytes.length, 24); central.writeUInt16LE(path.length, 28); central.writeUInt32LE(offset, 42);
    parts.push(header, path, bytes); directory.push(central, path); offset += header.length + path.length + bytes.length;
  }
  const central = Buffer.concat(directory), end = Buffer.alloc(22);
  end.writeUInt32LE(0x06054b50); end.writeUInt16LE(directory.length / 2, 8); end.writeUInt16LE(directory.length / 2, 10); end.writeUInt32LE(central.length, 12); end.writeUInt32LE(offset, 16);
  return Buffer.concat([...parts, central, end]);
}

async function login(page: Page) {
  await page.goto('/admin/login');
  await loginSession(page);
}

test('安装、预览、启用、旧资源及直接文档都保持权限隔离', async ({ page, context, browserName, hub }, testInfo) => {
  await login(page);
  must(await rpc(page, AdminService.method.updateSettings, { settings: { publicEnabled: true } }));
  const id = 'browser-' + browserName;
  // 用例中途会关闸、启用这个主题：两者都是整个 hub 共用的状态，收尾时卸载主题、恢复总闸（fixtures.ts），不留给后面的公开页用例。
  hub.atEnd('恢复公开页总闸', async () => must(await hub.rpc(AdminService.method.updateSettings, { settings: { publicEnabled: true } })));
  hub.deleteAtEnd(AdminService.method.deleteTheme, { id });
  const pkg = zip({
    'theme.json': JSON.stringify({ id, name: 'Browser Theme', version: '1', sdk: 1 }),
    'index.html': '<!doctype html><meta charset="utf-8"><link rel="stylesheet" href="./style.css"><div id="result">loading</div><button id="route">节点</button><script type="module" src="./app.js"></script>',
    'style.css': 'body{margin:0;background:rgb(12, 34, 56);color:white}#result{overflow-wrap:anywhere}',
    'dynamic.js': 'export default "dynamic-ok";',
    'view.svg': '<svg xmlns="http://www.w3.org/2000/svg"><script>try{document.cookie;document.documentElement.setAttribute("unsafe","yes")}catch(e){document.documentElement.setAttribute("isolated","yes")}</script></svg>',
    'app.js': `import {getSite,getSnapshot,navigate,onRoute,call} from '/_heron/theme-sdk.js';
const result={};
for(const [key,action] of Object.entries({parent:()=>parent.document.body, cookie:()=>document.cookie,storage:()=>localStorage.getItem('x')})) {try{action();result[key]=false}catch(e){result[key]=true}}
try{await fetch('/heron.v1.AdminService/CreateNode',{method:'POST',credentials:'include',headers:{'Content-Type':'application/json'},body:'{"name":"stolen"}'});result.fetch=false}catch(e){result.fetch=true}
try{await navigator.serviceWorker.register('./worker.js');result.worker=false}catch(e){result.worker=true}
try{await navigator.credentials.get({publicKey:{challenge:new Uint8Array(32),timeout:50}});result.passkey=false}catch(e){result.passkey=true}
result.dynamic=(await import('./dynamic.js')).default;
result.css=getComputedStyle(document.body).backgroundColor;
if(parent!==window){const site=await getSite();result.site=typeof site==='object';result.adminPath=site.adminPath??null;result.snapshot=!!(await getSnapshot());await onRoute(path=>{document.body.dataset.route=path});document.querySelector('#route').onclick=()=>navigate('/nodes/1')}
if(parent!==window){try{await call('QueryMetrics',{nodeId:'999',from:'1',to:'2'})}catch(error){result.errorCode=error.code}}
document.querySelector('#result').textContent=JSON.stringify(result);`,
  });
  await page.goto('/admin/themes');
  await page.getByLabel('主题包（zip，至多 8 MiB）').setInputFiles({ name: 'theme.zip', mimeType: 'application/zip', buffer: pkg });
  await page.getByRole('button', { name: '上传', exact: true }).click();
  await expect(page.getByText(/已安装 Browser Theme/)).toBeVisible();
  const downloadEvent = page.waitForEvent('download');
  await page.getByRole('button', { name: new RegExp('^下载原包 Browser Theme（' + id + '）') }).click();
  const downloaded = await downloadEvent;
  expect(await readFile((await downloaded.path()))).toEqual(pkg);
  await page.screenshot({ path: testInfo.outputPath('themes-desktop.png'), fullPage: true });
  await page.setViewportSize({ width: 375, height: 760 });
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBe(375);
  await page.screenshot({ path: testInfo.outputPath('themes-mobile.png'), fullPage: true });
  await page.setViewportSize({ width: 1280, height: 720 });
  const installed = must(await rpc(page, AdminService.method.listThemes, {})).themes.find((t) => t.id === id);
  if (!installed) throw new Error(`theme ${id} not listed`);
  expect(installed.enabled).toBe(false);
  // 原包经 bytes 字段（JSON 里是 base64）回读，与上传的字节逐一相同。
  expect(must(await rpc(page, AdminService.method.getThemePackage, { id, digest: installed.digest })).package).toEqual(new Uint8Array(pkg));
  const file = `/_heron/themes/${id}/${installed.digest}/index.html`;
  expect((await context.request.get(file)).status()).toBe(404);
  const preview = must(await rpc(page, AdminService.method.previewTheme, { id, digest: installed.digest }));
  const sandbox = await context.newPage();
  await sandbox.goto(preview.url);
  const frame = sandbox.frameLocator('iframe');
  await expect(frame.locator('#result')).toContainText('dynamic-ok');
  const result: unknown = JSON.parse(await frame.locator('#result').innerText());
  expect(result).toMatchObject({ parent: true, cookie: true, storage: true, fetch: true, worker: true, passkey: true, dynamic: 'dynamic-ok', css: 'rgb(12, 34, 56)', site: true, snapshot: true, adminPath: null, errorCode: 'not_found' });
  await frame.locator('#route').click();
  await expect(frame.locator('body')).toHaveAttribute('data-route', '/nodes/1');
  expect(new URL(sandbox.url()).pathname).toBe(preview.url);
  must(await rpc(page, AdminService.method.enableTheme, { id, digest: installed.digest }));
  await sandbox.goto('/nodes/2');
  await expect(sandbox.frameLocator('iframe').locator('body')).toHaveAttribute('data-route', '/nodes/2');
  await sandbox.frameLocator('iframe').locator('#route').click();
  await expect(sandbox).toHaveURL(/\/nodes\/1$/);
  await sandbox.goBack();
  await expect(sandbox.frameLocator('iframe').locator('body')).toHaveAttribute('data-route', '/nodes/2');
  await sandbox.setViewportSize({ width: 375, height: 760 });
  expect(await sandbox.locator('iframe').evaluate(el => el.getBoundingClientRect().width)).toBe(375);
  await sandbox.goto(file);
  await expect(sandbox.locator('#result')).toContainText('dynamic-ok');
  expect(JSON.parse(await sandbox.locator('#result').innerText())).toMatchObject({ cookie: true, storage: true, fetch: true, worker: true, passkey: true });
  await sandbox.goto(file.replace('index.html', 'view.svg'));
  await expect(sandbox.locator('svg')).toHaveAttribute('isolated', 'yes');
  // 沙箱里伪造的 CreateNode 没有成功：只看它要建的那个名字，不假定 hub 里没有别的用例建的节点。
  const { nodes } = must(await rpc(page, AdminService.method.listNodes, {}));
  expect(nodes.map((node) => node.name)).not.toContain('stolen');
  must(await rpc(page, AdminService.method.enableTheme, {}));
  expect((await context.request.get(file)).status()).toBe(200);
  must(await rpc(page, AdminService.method.updateSettings, { settings: { publicEnabled: false } }));
  expect(await (await context.request.get(file)).text()).toContain('公开页已关闭');
  await sandbox.close();
});

test('Passkey 在 HTTPS 注册、重启登录并迁移到新域名', async ({ page, context, browserName }) => {
  test.skip(browserName !== 'chromium', '虚拟认证器由 Chromium CDP 提供');
  const cdp = await context.newCDPSession(page);
  await cdp.send('WebAuthn.enable');
  await cdp.send('WebAuthn.addVirtualAuthenticator', { options: { protocol: 'ctap2', transport: 'internal', hasResidentKey: true, hasUserVerification: true, isUserVerified: true, automaticPresenceSimulation: true } });
  await login(page);
  await page.goto('/admin/security');
  await page.getByRole('article', { name: 'TOTP' }).getByRole('link', { name: '管理 TOTP、恢复码与 Passkey' }).click();
  await page.getByLabel('管理员密码').fill(password);
  await page.getByLabel('认证器名称').fill('Browser key');
  await page.getByRole('button', { name: '添加 Passkey', exact: true }).click();
  await expect(page.getByRole('heading', { name: '认证方式已更新' })).toBeVisible();
  await page.goto('/admin/login');
  await page.getByRole('button', { name: /Passkey/ }).click();
  await expect(page).not.toHaveURL(/\/login$/);
  const info = must(await rpc(page, AdminService.method.getSecurity, {}));
  expect(info.origin).toBe('https://localhost:18988');
  expect(info.passkeys).toHaveLength(1);
  expect((await context.request.post('/__e2e/restart')).ok()).toBe(true);
  await expect.poll(async () => {
    // 重启期间请求可能连不上（fetch 在页面里抛错），也可能回非 200；两种都按"还没回来"处理。
    try {
      const security = await rpc(page, AdminService.method.getSecurity, {});
      return security.ok ? security.message.origin : '';
    } catch {
      return '';
    }
  }).toBe('https://localhost:18988');
  must(await rpc(page, AdminService.method.logout, {}));
  await page.goto('/admin/login');
  await page.getByRole('button', { name: /Passkey/ }).click();
  await expect(page).not.toHaveURL(/\/login$/);
  await page.goto('https://other.localhost:18988/admin/login');
  await loginSession(page);
  await page.goto('https://other.localhost:18988/admin/security/credentials');
  await expect(page.getByText('当前访问域名与已绑定域名不同')).toBeVisible();
  await page.getByLabel('管理员密码').fill(password);
  await page.getByLabel('认证器名称').fill('Replacement key');
  await page.getByRole('button', { name: '重新绑定并添加 Passkey', exact: true }).click();
  await expect(page.getByRole('heading', { name: '认证方式已更新' })).toBeVisible();
  await page.goto('https://other.localhost:18988/admin/login');
  await page.getByRole('button', { name: /Passkey/ }).click();
  await expect(page).not.toHaveURL(/\/login$/);
  const rebound = must(await rpc(page, AdminService.method.getSecurity, {}));
  expect(rebound.origin).toBe('https://other.localhost:18988');
  expect(rebound.passkeys).toHaveLength(1);
  expect(rebound.passkeys[0].name).toBe('Replacement key');
});

// 面板下载的主题开发指南里的最小主题，原样打包（只把 id 换成本用例专用的），在 hub 的预览里必须能跑：
// 读到站点标题、列出公开节点、收到当前路由。指南的示例改坏了，这里先红。
test('主题开发指南里的最小主题原样可用', async ({ page, context, browserName, hub }) => {
  const guide = await readFile(new URL('../src/assets/heron-theme-skill.md', import.meta.url), 'utf8');
  const block = (file: string, lang: string) => {
    const match = guide.match(new RegExp('`' + file.replace('.', '\\.') + '`\\n\\n```' + lang + '\\n([\\s\\S]*?)\\n```'));
    if (!match) throw new Error(`指南里没有 ${file} 的 ${lang} 代码块`);
    return match[1];
  };
  const id = 'skill-' + browserName;
  const manifest: unknown = JSON.parse(block('theme.json', 'json'));
  expect(manifest).toEqual({ id: 'my-theme', name: 'My theme', version: '1.0.0', sdk: 1 });
  if (typeof manifest !== 'object' || manifest === null) throw new Error('指南里的 theme.json 不是对象');
  await login(page);
  must(await rpc(page, AdminService.method.updateSettings, { settings: { publicEnabled: true } }));
  hub.atEnd('恢复公开页总闸', async () => must(await hub.rpc(AdminService.method.updateSettings, { settings: { publicEnabled: true } })));
  hub.deleteAtEnd(AdminService.method.deleteTheme, { id });
  const name = `skill-node-${browserName}`;
  const node = mustField(await rpc(page, AdminService.method.createNode, { name }), 'node').id;
  hub.deleteNodeAtEnd(node);
  must(await rpc(page, AdminService.method.updateNode, { id: node, name, public: true, trafficResetDay: 1, offlineGraceS: 0 }));
  const pkg = zip({ 'theme.json': JSON.stringify({ ...manifest, id }), 'index.html': block('index.html', 'html') });
  await page.goto('/admin/themes');
  await page.getByLabel('主题包（zip，至多 8 MiB）').setInputFiles({ name: 'theme.zip', mimeType: 'application/zip', buffer: pkg });
  await page.getByRole('button', { name: '上传', exact: true }).click();
  await expect(page.getByText(/已安装 My theme/)).toBeVisible();
  const installed = must(await rpc(page, AdminService.method.listThemes, {})).themes.find((t) => t.id === id);
  if (!installed) throw new Error(`theme ${id} not listed`);
  const preview = must(await rpc(page, AdminService.method.previewTheme, { id, digest: installed.digest }));
  const sandbox = await context.newPage();
  const logs: string[] = [];
  sandbox.on('console', (message) => logs.push(message.text()));
  await sandbox.goto(preview.url);
  const frame = sandbox.frameLocator('iframe');
  await expect(frame.locator('h1#title')).not.toBeEmpty();
  await expect(frame.locator('#nodes li', { hasText: name })).toHaveText(`${name} 离线`);
  await expect.poll(() => logs).toContain('当前路由 /');
  await sandbox.close();
});
