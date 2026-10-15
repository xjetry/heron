import { type Page } from "@playwright/test";
import { AdminService, DeliveryFailure } from "../src/gen/heron/v1/admin_pb";
import { AgentService } from "../src/gen/heron/v1/agent_pb";
import { AddressDetectionState, BillingCycle } from "../src/gen/heron/v1/types_pb";
import { boxOf, expect, fulfillRpc, login, must, mustField, rpc, rpcRoute, test } from "./fixtures";

async function setScheme(page: Page, want: 'light' | 'dark') {
  for (let i = 0; i < 3; i++) {
    if (await page.evaluate(() => document.documentElement.dataset.theme) === want) return;
    await page.getByRole('button', { name: '明暗切换' }).click();
  }
  await expect(page.locator('html')).toHaveAttribute('data-theme', want);
}

async function openRowAction(page: Page, label: string, action: string) {
  await page.getByRole('button', { name: `更多操作 ${label}`, exact: true }).click();
  await page.getByRole('menuitem', { name: `${action} ${label}`, exact: true }).click();
}

test('注册命令复制与移动端布局', async ({ page, context, browserName }, testInfo) => {
  await page.goto('/admin/login');
  await login(page);
  await page.goto('/admin/register');
  await page.getByRole('button', { name: '开启接入窗口' }).click();
  await page.getByRole('dialog').getByRole('button', { name: '开启', exact: true }).click();
  await expect(page.getByLabel('curl 安装命令')).toBeVisible();
  if (browserName === 'chromium') {
    await context.grantPermissions(['clipboard-read', 'clipboard-write']);
    for (const tool of ['curl', 'wget']) {
      const command = await page.getByLabel(`${tool} 安装命令`).textContent();
      await page.getByRole('button', { name: `复制 ${tool} 命令` }).click();
      await expect(page.getByRole('button', { name: `复制 ${tool} 命令` })).toHaveText('已复制');
      expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(command);
    }
  }
  await page.screenshot({ path: testInfo.outputPath('register-copy-desktop.png'), fullPage: true });
  await page.setViewportSize({ width: 375, height: 812 });
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBe(375);
  await expect(page.getByRole('button', { name: '复制 curl 命令' })).toBeVisible();
  await expect(page.getByRole('button', { name: '复制 wget 命令' })).toBeVisible();
  await page.screenshot({ path: testInfo.outputPath('register-copy-mobile.png'), fullPage: true });
  await page.getByRole('button', { name: '关闭接入窗口' }).click();
  await expect(page.getByRole('button', { name: '复制 curl 命令' })).toHaveCount(0);
  await expect(page.getByRole('button', { name: '复制 wget 命令' })).toHaveCount(0);
});

// 覆盖地址存在真实浏览器的 localStorage 里：重载页面后再开窗口仍沿用，不合法的输入不覆盖它，清空即回到当前域名。
// 注册 key 只在开启的响应里出现一次，重载后要关掉再开才重新看到命令；窗口是全库共享的状态，开着会让后面的用例
// 点不了"开启接入窗口"，所以由 hub fixture 在拆除阶段关掉，用例中途失败也不留下开着的窗口。
test('安装命令的 Hub 连接地址只存浏览器，重载后沿用', async ({ page, hub }) => {
  hub.atEnd('CloseRegisterWindow', async () => must(await hub.rpc(AdminService.method.closeRegisterWindow, {})));
  const openWindow = async () => {
    await page.getByRole('button', { name: '开启接入窗口' }).click();
    await page.getByRole('dialog').getByRole('button', { name: '开启', exact: true }).click();
    await expect(page.getByLabel('curl 安装命令')).toBeVisible();
  };
  await page.goto('/admin/login');
  await login(page);
  await page.goto('/admin/register');
  await openWindow();
  const origin = new URL(page.url()).origin;
  const hubInput = page.getByRole('textbox', { name: 'Hub 连接地址' });
  await expect(hubInput).toHaveAttribute('placeholder', origin);
  await expect(page.getByLabel('curl 安装命令')).toContainText(`--hub ${origin} --key `);
  await hubInput.fill('https://heron-panel.example:28080/');
  await expect(page.getByLabel('curl 安装命令')).toContainText('--hub https://heron-panel.example:28080 --key ');
  await expect(page.getByLabel('wget 安装命令')).toContainText('--hub https://heron-panel.example:28080 --key ');
  await hubInput.fill('https://heron-panel.example:28080/admin');
  await expect(page.getByRole('alert')).toContainText('Hub 连接地址须为');
  await expect(page.getByLabel('curl 安装命令')).toHaveCount(0);
  await expect(page.getByLabel('wget 安装命令')).toHaveCount(0);

  await page.reload();
  await page.getByRole('button', { name: '关闭接入窗口' }).click();
  await expect(page.getByText('当前没有开启的接入窗口。')).toBeVisible();
  await openWindow();
  await expect(hubInput).toHaveValue('https://heron-panel.example:28080');
  await expect(page.getByLabel('curl 安装命令')).toContainText('--hub https://heron-panel.example:28080 --key ');
  await hubInput.fill('');
  await expect(page.getByLabel('curl 安装命令')).toContainText(`--hub ${origin} --key `);
  expect(await page.evaluate(() => localStorage.getItem('heron-install-hub'))).toBeNull();
  await page.getByRole('button', { name: '关闭接入窗口' }).click();
  await expect(page.getByRole('button', { name: '复制 curl 命令' })).toHaveCount(0);
});

test('后台明暗、双栈、编辑与计费、移动导航和键盘交互', async ({ page, context, browserName, hub }, testInfo) => {
  const ids: bigint[] = [];
  await page.emulateMedia({ reducedMotion: 'reduce' });
  await page.goto('/admin/login');
  await login(page);
  for (const [index, name] of ['tokyo-edge', 'seattle-core', 'frankfurt-worker'].entries()) {
    const result = await rpc(page, AdminService.method.createNode, { name: `${name}-${browserName}` });
    const id = mustField(result, 'node').id;
    ids.push(id);
    hub.deleteNodeAtEnd(id);
    must(await rpc(page, AdminService.method.updateNode, { id, name: `${name}-${browserName}`, note: '生产节点 / 核心业务', public: true, trafficResetDay: 1, offlineGraceS: 0, countryPin: ['JP', 'US', 'DE'][index], tags: ['production', index === 0 ? 'edge' : 'compute'], billing: { price: String(12 + index * 8), currency: 'USD', billingCycle: BillingCycle.MONTHLY, expiresOn: '2027-10-01' } }));
    const { token } = must(await rpc(page, AgentService.method.register, { key: must(result).token }));
    const checkedAt = BigInt(Math.floor(Date.now() / 1000));
    must(await rpc(page, AgentService.method.report, {
      factsHash: 1n, metrics: { bootId: '0b7c3a1e-5d2f-4e6a-9c8b-1a2b3c4d5e6f', cpuPct: 12 + index * 15, memUsed: 536870912n, memTotal: 2147483648n, diskUsed: 2147483648n, diskTotal: 21474836480n, load1: 0.3, load5: 0.2, load15: 0.1, netRxBps: 524288n, netTxBps: 131072n, uptimeS: 90_061n },
      facts: { hostname: 'edge.internal', agentVersion: 'dev', network: {
        ipv4: { state: index === 2 ? AddressDetectionState.FAILED : AddressDetectionState.AVAILABLE, address: index === 2 ? '' : ['8.8.8.8', '1.1.1.1'][index], checkedAt },
        ipv6: index === 1 ? { state: AddressDetectionState.UNSUPPORTED, checkedAt } : { state: AddressDetectionState.AVAILABLE, address: '2606:4700:4700::1111', checkedAt },
      } },
    }, token));
  }
  await page.goto('/admin/nodes');
  await setScheme(page, 'dark');
  await page.setViewportSize({ width: 1440, height: 960 });
  await expect(page.getByText('2606:4700:4700::1111').first()).toBeVisible();
  await expect(page.getByText('不支持', { exact: true })).toBeVisible();
  await expect(page.getByText('探测失败', { exact: true })).toBeVisible();
  const copy4 = page.getByRole('button', { name: '复制 IPv4 8.8.8.8', exact: true });
  const copy6 = page.getByRole('button', { name: '复制 IPv6 2606:4700:4700::1111', exact: true }).first();
  await expect(copy4).toBeVisible();
  await expect(copy6).toBeVisible();
  if (browserName === 'chromium') {
    await context.grantPermissions(['clipboard-read', 'clipboard-write']);
    for (const [button, address] of [[copy4, '8.8.8.8'], [copy6, '2606:4700:4700::1111']] as const) {
      await button.click();
      await expect(button).toHaveAttribute('title', '已复制');
      expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(address);
    }
  }
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBe(1440);
  expect((await page.getByRole('row', { name: `tokyo-edge-${browserName}`, exact: true }).boundingBox())?.height).toBeLessThanOrEqual(53);
  await page.screenshot({ path: testInfo.outputPath('nodes-dark-desktop.png'), fullPage: true });
  await openRowAction(page, `tokyo-edge-${browserName}（#${ids[0]}）`, '编辑');
  const dialog = page.getByRole('dialog');
  await expect(dialog).toHaveClass(/drawer/);
  await expect(dialog.getByRole('button', { name: '复制 IPv4 8.8.8.8', exact: true })).toBeVisible();
  const name = dialog.getByLabel(`名称 tokyo-edge-${browserName}（#${ids[0]}）`, { exact: true });
  await expect(name).toBeFocused();
  await name.fill('tokyo-renamed');
  await page.screenshot({ path: testInfo.outputPath('node-editor-desktop.png'), fullPage: true });
  for (let i = 0; i < 22; i++) {
    await page.keyboard.press('Tab');
    // 原生 dialog 允许 Tab 进入浏览器工具栏，但不能进入背景页面的控件。
    expect(await page.evaluate(() => document.activeElement === document.body || !!document.activeElement?.closest('dialog'))).toBe(true);
  }
  await dialog.getByRole('button', { name: '保存', exact: true }).click();
  await expect(dialog).toHaveCount(0);
  await expect(page.getByRole('link', { name: `tokyo-renamed（#${ids[0]}）` })).toBeVisible();
  await openRowAction(page, `tokyo-renamed（#${ids[0]}）`, '编辑');
  const billing = dialog.getByRole('region', { name: '费用', exact: true });
  await billing.getByLabel(`价格 tokyo-renamed（#${ids[0]}）`).fill('29.50');
  const expiryLabel = `到期日 tokyo-renamed（#${ids[0]}）`;
  const year = billing.getByRole('textbox', { name: `${expiryLabel} 年`, exact: true });
  const month = billing.getByRole('textbox', { name: `${expiryLabel} 月`, exact: true });
  const day = billing.getByRole('textbox', { name: `${expiryLabel} 日`, exact: true });
  await year.fill('2031');
  await month.fill('02');
  await day.fill('29');
  await dialog.getByRole('button', { name: '保存', exact: true }).click();
  await expect(dialog).toBeVisible();
  const unchanged = must(await rpc(page, AdminService.method.listNodes, {}));
  expect(unchanged.nodes.find((node) => node.id === ids[0])?.billing?.expiresOn).toBe('2027-10-01');
  await year.fill('');
  await year.pressSequentially('2031');
  await expect(month).toBeFocused();
  await month.pressSequentially('12');
  await expect(day).toBeFocused();
  await day.pressSequentially('25');
  await expect(year).toHaveValue('2031');
  await expect(month).toHaveValue('12');
  await expect(day).toHaveValue('25');
  await page.screenshot({ path: testInfo.outputPath('billing-desktop.png'), fullPage: true });
  await dialog.getByRole('button', { name: '保存', exact: true }).click();
  await expect(dialog).toHaveCount(0);
  await expect(page.getByText('US$29.50 / 月')).toBeVisible();
  const updated = must(await rpc(page, AdminService.method.listNodes, {}));
  expect(updated.nodes.find((node) => node.id === ids[0])?.billing?.expiresOn).toBe('2031-12-25');
  const renamedMenu = page.getByRole('button', { name: `更多操作 tokyo-renamed（#${ids[0]}）`, exact: true });
  await openRowAction(page, `tokyo-renamed（#${ids[0]}）`, '编辑');
  await page.keyboard.press('Escape');
  await expect(dialog).toHaveCount(0);
  await expect(renamedMenu).toBeFocused();
  await setScheme(page, 'light');
  // 危险操作首击只武装，关闭菜单必须撤销武装且保留节点。
  await openRowAction(page, `tokyo-renamed（#${ids[0]}）`, '删除');
  await expect(page.getByRole('menuitem', { name: `确认删除 tokyo-renamed（#${ids[0]}）` })).toBeVisible();
  await page.screenshot({ path: testInfo.outputPath('nodes-row-menu-armed.png') });
  await page.keyboard.press('Escape');
  await expect(page.getByRole('menu')).toHaveCount(0);
  await expect(page.getByRole('link', { name: `tokyo-renamed（#${ids[0]}）` })).toBeVisible();
  // 卡上的数量与节点列表的到期筛选必须使用同一判定。
  await page.goto('/admin/');
  const expiring = page.getByRole('list', { name: '需要处理' }).getByRole('link').nth(1);
  const expiringCount = Number(await expiring.locator('strong').textContent());
  await expiring.click();
  // 卡链到列表时带上按到期排序，最紧的排最前。
  await expect(page).toHaveURL(/\/admin\/nodes\?expiring=1&sort=expiry$/);
  // 数据行数等于卡上的数；为 0 时节点表让位给空态（components/EmptyState.tsx），先等空态出现，免得在加载中把 0 行当成结果。
  if (expiringCount === 0) await expect(page.getByRole('status').filter({ hasText: '没有匹配的节点。' })).toBeVisible();
  await expect(page.locator('.node-management tbody > tr')).toHaveCount(expiringCount);
  // 排序不是筛选：「清除筛选」留下排序（结果为空时没有表头，表头断言放在这里），再点表头回到服务端全序。
  await page.getByRole('button', { name: '清除筛选' }).click();
  await expect(page).toHaveURL(/\/admin\/nodes\?sort=expiry$/);
  await expect(page.getByRole('columnheader', { name: '到期' })).toHaveAttribute('aria-sort', 'ascending');
  await page.getByRole('columnheader', { name: '到期' }).getByRole('button', { name: '到期' }).click();
  await expect(page).toHaveURL(/\/admin\/nodes$/);
  await expect(page.getByRole('columnheader', { name: '到期' })).toHaveAttribute('aria-sort', 'none');
  await page.getByRole('link', { name: `tokyo-renamed（#${ids[0]}）`, exact: true }).waitFor();
  await page.screenshot({ path: testInfo.outputPath('nodes-light-desktop.png'), fullPage: true });
  await page.setViewportSize({ width: 375, height: 812 });
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBe(375);
  // 手机上表头不可见：从到期卡进入排序后，筛选行的胶囊是退出排序的路径；「清除筛选」不清排序。
  await page.goto('/admin/');
  await page.getByRole('list', { name: '需要处理' }).getByRole('link').nth(1).click();
  await expect(page).toHaveURL(/\/admin\/nodes\?expiring=1&sort=expiry$/);
  await page.getByRole('button', { name: '取消按到期排序' }).click();
  await expect(page).toHaveURL(/\/admin\/nodes\?expiring=1$/);
  await page.getByRole('button', { name: '清除筛选' }).click();
  await expect(page).toHaveURL(/\/admin\/nodes$/);
  await page.getByRole('link', { name: `tokyo-renamed（#${ids[0]}）`, exact: true }).waitFor();
  await expect(page.getByRole('row', { name: 'tokyo-renamed', exact: true }).getByRole('button', { name: `更多操作 tokyo-renamed（#${ids[0]}）` })).toBeVisible();
  // 手机卡片按 data-column 指名放进 admin.css 的 grid-template-areas：运行时长在状态右侧、同一行、落在卡片之内。没有指名的列
  // 由网格自动放进模板里空着的「.」格，Chromium 实测落在状态左边 20px 的缩进列里，挤成一条窄缝。
  const tokyoRow = page.getByRole('row', { name: 'tokyo-renamed', exact: true });
  const uptimeCell = tokyoRow.locator('td[data-column="uptime"]');
  await expect(uptimeCell).toBeVisible();
  await expect(uptimeCell).toHaveText('1d 1h');
  const [rowBox, uptimeBox, statusBox] = await Promise.all([boxOf(tokyoRow), boxOf(uptimeCell), boxOf(tokyoRow.locator('td[data-column="status"]'))]);
  expect(uptimeBox.x).toBeGreaterThanOrEqual(rowBox.x);
  expect(uptimeBox.x + uptimeBox.width).toBeLessThanOrEqual(rowBox.x + rowBox.width);
  expect(uptimeBox.y).toBe(statusBox.y);
  expect(uptimeBox.x).toBeGreaterThan(statusBox.x);
  await page.screenshot({ path: testInfo.outputPath('nodes-mobile.png'), fullPage: true });
  await openRowAction(page, `tokyo-renamed（#${ids[0]}）`, '编辑');
  expect(await dialog.evaluate(el => el.scrollWidth <= el.clientWidth)).toBe(true);
  const drawerBox = await dialog.boundingBox();
  expect(drawerBox?.x).toBe(0);
  expect(drawerBox?.width).toBe(375);
  await page.screenshot({ path: testInfo.outputPath('node-editor-mobile.png'), fullPage: true });
  await page.keyboard.press('Escape');
  await openRowAction(page, `tokyo-renamed（#${ids[0]}）`, '编辑');
  await billing.scrollIntoViewIfNeeded();
  await page.screenshot({ path: testInfo.outputPath('billing-mobile.png'), fullPage: true });
  await page.keyboard.press('Escape');
  await page.getByRole('button', { name: '打开导航' }).click();
  await dialog.getByRole('link', { name: '总览', exact: true }).click();
  await expect(dialog).toHaveCount(0);
  await expect(page.getByRole('heading', { name: '总览', exact: true })).toBeVisible();
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBe(375);
  await page.screenshot({ path: testInfo.outputPath('overview-mobile.png'), fullPage: true });
  await page.setViewportSize({ width: 1440, height: 960 });
  await setScheme(page, 'dark');
  await expect(page.getByRole('list', { name: '需要处理' })).toBeVisible();
  await page.screenshot({ path: testInfo.outputPath('overview-desktop.png'), fullPage: true });
  await page.setViewportSize({ width: 375, height: 812 });
  for (const [route, heading] of [
    ['probes', '探测任务'], ['alerts', '告警规则'], ['events', '告警事件'], ['channels', '通知渠道'],
    ['appearance', '外观'], ['themes', '主题'], ['storage', '存储'], ['updates', '在线更新'], ['security', '安全'],
    ['security/credentials', '账户安全'], ['tokens', 'API token'], ['register', '批量添加节点'],
  ]) {
    await page.goto('/admin/' + route);
    await expect(page.getByRole('heading', { name: heading, exact: true, level: 1 })).toBeVisible();
    await expect(page.getByText('加载中…', { exact: true })).toHaveCount(0);
    expect(await page.evaluate(() => document.documentElement.scrollWidth), route).toBe(375);
    await page.screenshot({ path: testInfo.outputPath(route.replace('/', '-') + '-mobile.png'), fullPage: true });
    const create = { probes: '新建探测任务', alerts: '新建告警规则', channels: '新建通知渠道' }[route];
    if (create) {
      await page.getByRole('button', { name: create, exact: true }).click();
      await page.screenshot({ path: testInfo.outputPath(route + '-drawer-mobile.png'), fullPage: true });
      await page.keyboard.press('Escape');
    }
  }
});

test('节点拖拽、键盘和移动端菜单保存同一完整顺序', async ({ page, browserName, hub }, testInfo) => {
  await page.goto('/admin/login');
  await login(page);
  const ids: bigint[] = [];
  const label = (index: number) => `order-${index}-${browserName}（#${ids[index]}）`;
  const actual = async () => must(await rpc(page, AdminService.method.listNodes, {})).nodes.filter((node) => ids.includes(node.id)).map((node) => node.id);
  for (let i = 0; i < 3; i++) {
    const node = mustField(await rpc(page, AdminService.method.createNode, { name: `order-${i}-${browserName}` }), 'node');
    ids.push(node.id);
    hub.deleteNodeAtEnd(node.id);
  }
  await page.goto('/admin/nodes');
  await page.setViewportSize({ width: 1440, height: 960 });
  const handle = page.getByRole('button', { name: `调整顺序 ${label(0)}`, exact: true });
  const target = page.getByRole('link', { name: label(2), exact: true }).locator('xpath=ancestor::tr');
  await expect(handle).toBeEnabled();
  const box = await boxOf(target);
  await handle.dragTo(target, { targetPosition: { x: 30, y: box.height - 5 } });
  await expect(page.getByText('顺序已保存', { exact: true })).toBeVisible();
  await expect.poll(actual).toEqual([ids[1], ids[2], ids[0]]);
  await handle.focus();
  await handle.press('Home');
  await expect.poll(actual).toEqual([ids[0], ids[1], ids[2]]);
  await expect(handle).toBeFocused();
  await handle.press('ArrowDown');
  await expect(handle).toBeFocused();
  await page.keyboard.press('ArrowDown');
  await expect(handle).toBeFocused();
  await expect.poll(actual).toEqual([ids[1], ids[2], ids[0]]);
  await handle.press('Home');
  await expect.poll(actual).toEqual([ids[0], ids[1], ids[2]]);
  await handle.press('End');
  await expect(handle).toBeFocused();
  await expect.poll(actual).toEqual([ids[1], ids[2], ids[0]]);
  await handle.press('Home');
  await expect.poll(actual).toEqual([ids[0], ids[1], ids[2]]);
  await page.screenshot({ path: testInfo.outputPath('node-order-desktop.png'), fullPage: true });
  await page.getByRole('searchbox', { name: '搜索节点' }).fill(`order-0-${browserName}`);
  await expect(handle).toBeDisabled();
  // 过滤时拖动与上下移仍禁用（行菜单里的上移、下移、置顶、置底同样保存完整排列），但按全序名次的「移动到…」可用，菜单保持可用（不被禁用）。
  await page.getByRole('button', { name: `更多操作 ${label(0)}`, exact: true }).click();
  await expect(page.getByRole('menuitem', { name: `移动到… ${label(0)}`, exact: true })).toBeEnabled();
  for (const action of ['上移一位', '下移一位', '置顶', '置底']) await expect(page.getByRole('menuitem', { name: `${action} ${label(0)}`, exact: true })).toBeDisabled();
  await page.keyboard.press('Escape');
  await expect(page.getByText('筛选或按列排序时不能用拖动或上下移（它们保存完整排列）；可用行菜单的「移动到…」按全序名次移动，或清除筛选与排序后再调整。')).toBeVisible();
  await page.getByRole('searchbox', { name: '搜索节点' }).fill('');
  await openRowAction(page, label(0), '置底');
  await expect.poll(actual).toEqual([ids[1], ids[2], ids[0]]);
  await handle.press('Home');
  await expect.poll(actual).toEqual([ids[0], ids[1], ids[2]]);
  await page.setViewportSize({ width: 375, height: 812 });
  await openRowAction(page, label(0), '移动到…');
  const move = page.getByRole('dialog', { name: '移动节点', exact: true });
  const total = must(await rpc(page, AdminService.method.listNodes, {})).nodes.length;
  await move.getByRole('spinbutton').fill(String(total));
  await move.getByRole('button', { name: '移动', exact: true }).click();
  await expect.poll(actual).toEqual([ids[1], ids[2], ids[0]]);
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBe(375);
  await page.screenshot({ path: testInfo.outputPath('node-order-mobile.png'), fullPage: true });
});

test('节点按全序名次整体移动到指定位置', async ({ page, browserName, hub }, testInfo) => {
  await page.goto('/admin/login');
  await login(page);
  const ids: bigint[] = [];
  const label = (index: number) => `move-${index}-${browserName}（#${ids[index]}）`;
  // ListNodes 返回的是 hub 的完整列表：本用例只断言自己那批节点的相对顺序，名次则对完整列表验证（1 起且与行序一致）。
  const hubList = async () => must(await rpc(page, AdminService.method.listNodes, {})).nodes;
  const mineInOrder = async () => (await hubList()).filter((node) => ids.includes(node.id)).map((node) => node.id);
  const positionsMatchList = async () => {
    const all = await hubList();
    return all.every((node, index) => node.position === index + 1);
  };
  const positionOf = async (id: bigint) => {
    const node = (await hubList()).find((entry) => entry.id === id);
    if (!node) throw new Error(`ListNodes has no node ${id}`);
    return node.position;
  };
  for (let i = 0; i < 5; i++) {
    const node = mustField(await rpc(page, AdminService.method.createNode, { name: `move-${i}-${browserName}` }), 'node');
    ids.push(node.id);
    hub.deleteNodeAtEnd(node.id);
  }
  await page.goto('/admin/nodes');
  await page.setViewportSize({ width: 1440, height: 960 });
  const total = (await hubList()).length;
  // 本批 5 个节点刚连着创建，占全序中连续的名次：从 ids[0] 的名次推出整批的基准位次。
  const base = await positionOf(ids[0]);
  const handle = (index: number) => page.getByRole('button', { name: `调整顺序 ${label(index)}`, exact: true });
  // 多选 move-1、move-3（本批第 2、4 位）整体移到本批第 3 位：其余相对顺序不变。
  await page.getByRole('checkbox', { name: `选择 ${label(1)}`, exact: true }).check();
  await page.getByRole('checkbox', { name: `选择 ${label(3)}`, exact: true }).check();
  await expect(page.getByText('已选择 2 个节点')).toBeVisible();
  await page.getByRole('button', { name: '移动到…', exact: true }).click();
  const dialog = page.getByRole('dialog');
  await expect(dialog).toHaveAccessibleName('移动节点');
  await expect(dialog.getByText(`共 ${total} 个节点，将移动其中的 2 个；其余节点相对顺序不变。`)).toBeVisible();
  const input = dialog.getByLabel(`目标位置（1–${total - 1}）`, { exact: true });
  await expect(input).toHaveAttribute('max', String(total - 1));
  await expect(dialog.getByText('将 2 个节点移到第 1–2 位')).toBeVisible();
  await input.fill(String(base + 2));
  await expect(dialog.getByText(`将 2 个节点移到第 ${base + 2}–${base + 3} 位`)).toBeVisible();
  await page.screenshot({ path: testInfo.outputPath('node-move-modal.png'), fullPage: true });
  await dialog.getByRole('button', { name: '移动', exact: true }).click();
  await expect(dialog).toHaveCount(0);
  await expect(page.getByRole('toolbar', { name: '批量操作' })).toHaveCount(0);
  await expect.poll(mineInOrder).toEqual([ids[0], ids[2], ids[1], ids[3], ids[4]]);
  expect(await positionsMatchList()).toBe(true);
  // 序号列跟着新的全序名次走。
  await expect(handle(0)).toHaveText(String(base));
  await expect(handle(2)).toHaveText(String(base + 1));
  await expect(handle(1)).toHaveText(String(base + 2));
  await expect(handle(3)).toHaveText(String(base + 3));
  // 过滤到单个节点：拖动禁用，但行菜单可按全序名次移动这一个节点。
  await page.getByRole('searchbox', { name: '搜索节点' }).fill(`move-4-${browserName}`);
  await expect(page.getByRole('link', { name: label(4), exact: true })).toBeVisible();
  const positionOfLast = await positionOf(ids[4]);
  await expect(handle(4)).toHaveText(String(positionOfLast));
  await expect(handle(4)).toBeDisabled();
  await openRowAction(page, label(4), '移动到…');
  const single = page.getByRole('dialog');
  await expect(single).toHaveAccessibleName('移动节点');
  const singleInput = single.getByLabel(`目标位置（1–${total}）`, { exact: true });
  await expect(single.getByText('移到第 1 位')).toBeVisible();
  await singleInput.fill('2');
  await expect(single.getByText('移到第 2 位')).toBeVisible();
  await singleInput.fill('1');
  await expect(single.getByText('移到第 1 位')).toBeVisible();
  await single.getByRole('button', { name: '移动', exact: true }).click();
  await expect(single).toHaveCount(0);
  await page.getByRole('searchbox', { name: '搜索节点' }).fill('');
  await expect.poll(mineInOrder).toEqual([ids[4], ids[0], ids[2], ids[1], ids[3]]);
  expect(await positionsMatchList()).toBe(true);
  await expect(handle(4)).toHaveText('1');
  await expect(handle(3)).toHaveText(String(base + 4));
  await page.screenshot({ path: testInfo.outputPath('node-move-desktop.png'), fullPage: true });
});

test('已出租开关保存后同步节点状态与更新列表，关闭后恢复', async ({ page, browserName, hub }) => {
  await page.goto('/admin/login');
  await login(page);
  const name = `rented-toggle-${browserName}`;
  const node = mustField(await rpc(page, AdminService.method.createNode, { name }), 'node');
  hub.deleteNodeAtEnd(node.id);
  const label = `${name}（#${node.id}）`;
  const dialog = page.getByRole('dialog');
  const status = page.getByRole('row', { name, exact: true }).locator('[data-column="status"]');
  const updateRow = page.getByRole('row').filter({ has: page.getByRole('checkbox', { name: `选择 ${label}`, exact: true }) });

  await page.goto('/admin/nodes');
  await page.getByRole('row', { name, exact: true }).waitFor();
  await page.goto('/admin/updates');
  await updateRow.waitFor();
  await page.goto('/admin/nodes');
  await openRowAction(page, label, '编辑');
  await dialog.getByRole('checkbox', { name: `已出租 ${label}`, exact: true }).check();
  await dialog.getByRole('button', { name: '保存', exact: true }).click();
  await dialog.waitFor({ state: 'detached' });
  await expect(status).toHaveText('已出租');

  await page.goto('/admin/updates');
  await page.getByRole('heading', { name: '节点 Agent', exact: true }).waitFor();
  await expect(updateRow).toHaveCount(0);
  await page.goto('/admin/nodes');
  await openRowAction(page, label, '编辑');
  await dialog.getByRole('checkbox', { name: `已出租 ${label}`, exact: true }).uncheck();
  await dialog.getByRole('button', { name: '保存', exact: true }).click();
  await dialog.waitFor({ state: 'detached' });
  await expect(status).toHaveText('从未上报');
  await page.goto('/admin/updates');
  await updateRow.waitFor();
});

test('在线更新展示实际平台能力并禁止不支持的更新', async ({ page }, testInfo) => {
  await page.emulateMedia({ reducedMotion: 'reduce' });
  await page.goto('/admin/login');
  await login(page);
  await page.goto('/admin/updates');
  await expect(page.getByRole('heading', { name: '在线更新', exact: true })).toBeVisible();
  await expect(page.getByText(/不支持在线更新：/).first()).toBeVisible();
  await expect(page.getByRole('button', { name: '更新 Hub', exact: true })).toBeDisabled();
  await expect(page.getByRole('button', { name: '更新选中节点（0）', exact: true })).toBeDisabled();
  await page.setViewportSize({ width: 1440, height: 960 });
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBe(1440);
  await page.screenshot({ path: testInfo.outputPath('updates-desktop.png'), fullPage: true });
  await page.setViewportSize({ width: 375, height: 812 });
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBe(375);
  await page.screenshot({ path: testInfo.outputPath('updates-mobile.png'), fullPage: true });
});

// 手机上告警事件每条一张卡（admin.css 的 .events-table 移动端规则），不再横向滚动一张六列宽表。
// 一条事件可有多条投递，每条是块级元素；它们要与「未配置渠道」一样起在「投递」标签右侧，而不是掉到标签下一行。
test('手机上的告警事件排成卡片：不横向滚动，投递起在标签右侧', async ({ page }, testInfo) => {
  await page.goto('/admin/login');
  await login(page);
  const now = Math.floor(Date.now() / 1000);
  const delivery = (id: bigint, ok: boolean) => ({ id, channelId: id, attempts: ok ? 1 : 3, ok, done: true, ...(ok ? { deliveredAt: BigInt(now) } : { failure: DeliveryFailure.TRANSPORT }) });
  await page.route(rpcRoute(AdminService.method.listAlertEvents), (route) => fulfillRpc(route, AdminService.method.listAlertEvents, () => ({ events: [
    { id: 2n, ruleId: 1n, nodeId: 1n, transition: 'firing', at: BigInt(now), summary: '节点 tokyo 离线 39s', value: 39, deliveries: [delivery(11n, true), delivery(12n, false)] },
    { id: 1n, ruleId: 1n, nodeId: 1n, transition: 'recovered', at: BigInt(now - 60), summary: '节点 tokyo 已恢复', value: 0, deliveries: [] },
  ] })));
  await page.setViewportSize({ width: 375, height: 812 });
  await page.goto('/admin/events');
  const region = page.getByRole('region', { name: '告警事件' });
  const cells = region.locator('td[data-label="投递"]');
  await expect(cells).toHaveCount(2);
  await expect(cells.first().locator(':scope > *')).toHaveCount(2);
  await expect(cells.first().getByRole('button', { name: /^查看错误原文 / })).toBeVisible();
  expect(await region.evaluate((el) => el.scrollWidth <= el.clientWidth)).toBe(true);
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBe(375);
  for (const cell of await cells.all()) {
    const offsets = await cell.evaluate((td) => {
      const box = td.getBoundingClientRect();
      return [...td.children].map((el) => { const r = el.getBoundingClientRect(); return { left: r.left - box.left, top: r.top - box.top }; });
    });
    expect(offsets[0].top).toBeLessThan(2);
    expect(offsets[0].left).toBeGreaterThan(10);
    for (const o of offsets) expect(o.left).toBeCloseTo(offsets[0].left, 0);
  }
  // 「查看错误原文」是链接按钮，不能把它所在的投递行撑到控件高度（styles.css 的 button.link）。
  const line = await cells.first().evaluate((td) => {
    const row = td.querySelector('button')?.parentElement;
    if (!row) throw new Error('delivery cell has no line holding the error-text button');
    return { height: row.getBoundingClientRect().height, lineHeight: parseFloat(getComputedStyle(td).lineHeight) };
  });
  expect(line.height).toBeLessThanOrEqual(line.lineHeight + 2);
  await page.screenshot({ path: testInfo.outputPath('events-mobile.png'), fullPage: true });
});

// 表单行混排「标题在上、控件在下」的字段与链接按钮、勾选框（styles.css 的 form .row）：单行项的文字与字段控件里的文字齐平，
// 不因字段的下外边距沉下去，也不跟着字段标题浮上来。文字中心与控件中心比，同字号时等价于基线对齐。
test('表单行里的链接按钮与勾选框和字段控件齐平', async ({ page }) => {
  await page.goto('/admin/login');
  await login(page);
  await page.setViewportSize({ width: 1440, height: 960 });
  await page.goto('/admin/appearance');
  const form = page.getByRole('form', { name: '公开页外观' });
  await expect(form.getByRole('button', { name: '用内置配色' })).toBeVisible();
  const offsets = await form.evaluate((f) => {
    const center = (el: Element) => { const r = el.getBoundingClientRect(); return r.top + r.height / 2; };
    const required = <T>(what: string, value: T | null | undefined): T => {
      if (value === null || value === undefined) throw new Error(`appearance form has no ${what}`);
      return value;
    };
    const link = (text: string) => required(`link button ${text}`, [...f.querySelectorAll('button.link')].find((b) => b.textContent === text));
    const accent = required('accent color input', [...f.querySelectorAll('input')].find((i) => i.placeholder.startsWith('#rrggbb')));
    return { accent: center(link('用内置配色')) - center(accent), logo: center(link('移除 logo')) - center(required('.file-button', f.querySelector('.file-button'))) };
  });
  expect(Math.abs(offsets.accent), JSON.stringify(offsets)).toBeLessThan(1.5);
  expect(Math.abs(offsets.logo), JSON.stringify(offsets)).toBeLessThan(1.5);
  await page.goto('/admin/alerts');
  await page.getByRole('button', { name: '新建告警规则', exact: true }).click();
  const drawer = page.getByRole('dialog', { name: '新建告警规则' });
  await expect(drawer.getByRole('checkbox', { name: '启用' })).toBeVisible();
  const enabled = await drawer.evaluate((dialog) => {
    const required = <T>(what: string, value: T | null | undefined): T => {
      if (value === null || value === undefined) throw new Error(`alert rule drawer has no ${what}`);
      return value;
    };
    const labels = [...dialog.querySelectorAll('label')];
    const name = required('name input', labels.find((l) => l.textContent?.startsWith('名称'))?.querySelector('input'));
    const label = required('启用 label', labels.find((l) => l.textContent?.trim() === '启用'));
    const text = required('启用 label text', [...label.childNodes].find((n) => n.nodeType === Node.TEXT_NODE && n.textContent?.trim()));
    const range = document.createRange();
    range.selectNodeContents(text);
    const t = range.getBoundingClientRect(), n = name.getBoundingClientRect();
    return (t.top + t.height / 2) - (n.top + n.height / 2);
  });
  expect(Math.abs(enabled)).toBeLessThan(1.5);
});

// 管理端只有 GetSnapshot 与 ListNodes 由 hub 压缩（各自的理由见 internal/hub/api/service.go 的 Handler），其余响应
// 不压缩：总览同时读的 ListAlertRules 代表其余过程。从浏览器入口核对实际收到的响应头，而不是只看服务端单测。
test('总览读取的快照与节点资料压缩，其余管理响应不压缩', async ({ page }) => {
  await page.goto('/admin/login');
  await login(page);
  const encodings = new Map<string, string>();
  page.on('response', async (response) => {
    const method = new URL(response.url()).pathname.match(/^\/heron\.v1\.AdminService\/(GetSnapshot|ListNodes|ListAlertRules)$/)?.[1];
    if (method && response.ok()) encodings.set(method, (await response.allHeaders())['content-encoding'] ?? '');
  });
  await page.goto('/admin/');
  await expect(page.getByRole('list', { name: '需要处理' })).toBeVisible();
  await expect.poll(() => [encodings.get('GetSnapshot'), encodings.get('ListNodes'), encodings.get('ListAlertRules')]).toEqual(['gzip', 'gzip', '']);
});

test('节点列表筛选由 URL 持有：逐字输入与输入法组字不丢字，进详情再返回还原；/ 与 ⌘K 打开快速搜索', async ({ page, browserName, hub }) => {
  await page.goto('/admin/login');
  await login(page);
  const tag = `url-${browserName}`;
  const names = [`url-a-${browserName}`, `url-b-${browserName}`];
  const ids: bigint[] = [];
  for (const name of names) {
    const node = mustField(await rpc(page, AdminService.method.createNode, { name }), 'node');
    ids.push(node.id);
    hub.deleteNodeAtEnd(node.id);
  }
  hub.deleteAtEnd(AdminService.method.deleteTag, { name: tag });
  must(await rpc(page, AdminService.method.updateNode, { id: ids[0], name: names[0], tags: [tag], trafficResetDay: 1, offlineGraceS: 0 }));
  await page.goto('/admin/nodes');
  const box = page.getByRole('searchbox', { name: '搜索节点' });
  const row = (i: number) => page.getByRole('link', { name: `${names[i]}（#${ids[i]}）`, exact: true });
  const params = () => new URL(page.url()).searchParams;

  // 不留间隔的真实按键：导航异步落地时输入框也不丢字、不乱序。
  await box.click();
  await page.keyboard.type(names[0]);
  await expect(box).toHaveValue(names[0]);
  await expect.poll(() => params().get('q')).toBe(names[0]);
  await expect(row(0)).toBeVisible();
  await expect(row(1)).toHaveCount(0);

  if (browserName === 'chromium') {
    // 输入法组字：组字中途的拼音不留下，确认后只剩确认的字。
    await box.fill('');
    await box.focus();
    const cdp = await page.context().newCDPSession(page);
    await cdp.send('Input.imeSetComposition', { text: 'l', selectionStart: 1, selectionEnd: 1 });
    await cdp.send('Input.imeSetComposition', { text: 'lian', selectionStart: 4, selectionEnd: 4 });
    await cdp.send('Input.insertText', { text: '链' });
    await expect(box).toHaveValue('链');
    await expect.poll(() => params().get('q')).toBe('链');
    await box.fill(names[0]);
  }

  // 标签也进 URL；进详情（中途切 tab）再点返回，搜索词与标签都还原。
  await page.getByRole('button', { name: /^标签 / }).click();
  await page.getByRole('group', { name: '标签' }).getByRole('button', { name: new RegExp(`^${tag} \\d+$`) }).click();
  await page.keyboard.press('Escape');
  await expect.poll(() => params().getAll('tag')).toEqual([tag]);
  await row(0).click();
  await expect(page).toHaveURL(new RegExp(`/admin/nodes/${ids[0]}$`));
  await page.getByRole('tab', { name: 'Agent 诊断' }).click();
  await page.getByRole('link', { name: '返回节点列表' }).click();
  await expect(box).toHaveValue(names[0]);
  expect(params().get('q')).toBe(names[0]);
  expect(params().getAll('tag')).toEqual([tag]);
  await expect(page.getByRole('button', { name: `标签 ${tag}`, exact: true })).toBeVisible();

  // 快捷键：焦点不在输入框时 / 打开快速搜索；在输入框里 / 照常输入；⌘K / Ctrl+K 在输入框里也打开。
  const dialog = page.getByRole('dialog', { name: '搜索节点' });
  await page.getByRole('heading', { name: '节点', exact: true }).click();
  await page.keyboard.press('/');
  await expect(dialog).toBeVisible();
  await expect(dialog.getByRole('searchbox')).toBeFocused();
  await page.keyboard.press('Escape');
  await expect(dialog).toHaveCount(0);
  await box.focus();
  await page.keyboard.press('/');
  await expect(box).toHaveValue(`${names[0]}/`);
  await expect(dialog).toHaveCount(0);
  await page.keyboard.press('ControlOrMeta+k');
  await expect(dialog).toBeVisible();
  await page.keyboard.press('Escape');
});
