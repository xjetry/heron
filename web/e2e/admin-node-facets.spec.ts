import { AdminService } from "../src/gen/heron/v1/admin_pb";
import { boxOf, expect, login, must, mustField, rpc, test } from "./fixtures";

// 管理端节点页的地区与标签筛选入口与公开页同一个组件（components/Facet.tsx）：这里从管理端入口核对单选→多选→匹配方式→
// 无标签与未知地区→手机布局→刷新与从详情返回后还原。库里可能有别的用例留下的节点，所以先用搜索框收窄到本用例的节点，
// 计数只对本用例独有的标签断言。
test('管理端节点页地区与标签筛选：单选、多选并集、标签匹配方式、无标签互斥、未知地区、手机布局、刷新与返回还原', async ({ page, browserName, hub }, testInfo) => {
  await page.goto('/admin/login');
  await login(page);
  const home = `家宽-${browserName}`, idc = `机房-${browserName}`;
  const prefix = `facet-${browserName}-`;
  const fixtures: [string, string, string[]][] = [
    ['东京家宽', 'JP', [home]], ['大阪机房', 'JP', [idc]], ['香港家宽', 'HK', [home]], ['香港两用', 'HK', [home, idc]], ['待探测', '', []],
  ];
  const ids = new Map<string, bigint>();
  for (const [label, countryPin, tags] of fixtures) {
    const name = `${prefix}${label}`;
    const id = mustField(await rpc(page, AdminService.method.createNode, { name }), 'node').id;
    ids.set(label, id);
    hub.deleteNodeAtEnd(id);
    must(await rpc(page, AdminService.method.updateNode, { id, name, countryPin, tags, trafficResetDay: 1, offlineGraceS: 0 }));
  }
  for (const tag of [home, idc]) hub.deleteAtEnd(AdminService.method.deleteTag, { name: tag });

  await page.setViewportSize({ width: 1440, height: 960 });
  await page.goto('/admin/nodes');
  const params = () => new URL(page.url()).searchParams;
  const rows = page.locator('table.node-management tbody tr');
  const shown = (...labels: string[]) =>
    expect.poll(() => rows.evaluateAll((trs) => trs.map((tr) => tr.getAttribute('aria-label')))).toEqual(labels.map((label) => `${prefix}${label}`));
  await page.getByRole('searchbox', { name: '搜索节点' }).fill(prefix);
  await expect(rows).toHaveCount(5);

  const regionFacet = page.getByRole('button', { name: /^地区 / });
  const tagFacet = page.getByRole('button', { name: /^标签 / });
  const regions = page.getByRole('group', { name: '地区' });
  const tags = page.getByRole('group', { name: '标签' });
  const chip = (group: typeof regions, name: string) => group.getByRole('list').getByRole('button', { name: new RegExp(`^${name}( \\d+)?$`) });

  // 单选：点一个只看它；地区进 URL。
  await expect(regionFacet).toHaveAccessibleName('地区 全部');
  await regionFacet.click();
  await chip(regions, '日本').click();
  await shown('东京家宽', '大阪机房');
  await expect(regionFacet).toHaveAccessibleName('地区 日本');
  await expect.poll(() => params().getAll('region')).toEqual(['JP']);
  // 多选：地区之间取并集，地区没有匹配方式。
  await regions.getByRole('group', { name: '选择方式' }).getByRole('button', { name: '多选' }).click();
  await expect(regions.getByRole('group', { name: '匹配方式' })).toHaveCount(0);
  await chip(regions, '香港').click();
  await shown('东京家宽', '大阪机房', '香港家宽', '香港两用');
  await expect(regionFacet).toHaveAccessibleName('地区 已选 2 个');
  // Esc 收起面板并把焦点还给入口。
  await chip(regions, '香港').press('Escape');
  await expect(regions).toHaveCount(0);
  await expect(regionFacet).toBeFocused();

  // 标签：计数按全部节点算（不随地区筛选变化），单选一个后切多选，默认同时满足，可改满足任一。
  await tagFacet.click();
  await expect(chip(tags, home)).toHaveAccessibleName(`${home} 3`);
  await expect(chip(tags, idc)).toHaveAccessibleName(`${idc} 2`);
  await chip(tags, home).click();
  await shown('东京家宽', '香港家宽', '香港两用');
  await tags.getByRole('group', { name: '选择方式' }).getByRole('button', { name: '多选' }).click();
  await chip(tags, idc).click();
  await shown('香港两用');
  await expect(tagFacet).toHaveAccessibleName('标签 已选 2 个');
  await tags.getByRole('group', { name: '匹配方式' }).getByRole('button', { name: '满足任一' }).click();
  await shown('东京家宽', '大阪机房', '香港家宽', '香港两用');
  await expect.poll(() => params().get('match')).toBe('any');
  await expect.poll(() => params().getAll('tag').sort()).toEqual([home, idc].sort());

  // 面板在 DOM 里紧跟标签入口，视觉上排在筛选行全部控件之后、独占一行（CSS order）。
  const below = async (control: Parameters<typeof boxOf>[0]) => {
    const [panel, box] = [await boxOf(tags), await boxOf(control)];
    expect(panel.y).toBeGreaterThanOrEqual(box.y + box.height);
  };
  await below(page.getByRole('button', { name: /^状态 / }));
  await below(page.getByRole('button', { name: '清除筛选' }));
  await page.screenshot({ path: testInfo.outputPath('admin-facets-desktop.png'), fullPage: true });

  // 「无标签」是标签面板里「全部」之后的第一个胶囊，与选标签互斥：选它清掉标签，再选标签清掉它。
  const untagged = tags.getByRole('list').getByRole('button', { name: /^无标签节点 \d+$/ });
  await expect(tags.getByRole('list').getByRole('button').nth(1)).toHaveAccessibleName(/^无标签节点 \d+$/);
  await untagged.click();
  await expect(tagFacet).toHaveAccessibleName('标签 无标签');
  await expect(chip(tags, home)).toHaveAttribute('aria-pressed', 'false');
  await expect(chip(tags, idc)).toHaveAttribute('aria-pressed', 'false');
  await expect.poll(() => [params().get('untagged'), params().getAll('tag')]).toEqual(['1', []]);
  // 无标签与地区取交集：待探测没有地区，日本、香港里没有无标签的节点。
  await shown();
  await chip(tags, idc).click();
  await expect(untagged).toHaveAttribute('aria-pressed', 'false');
  await shown('大阪机房', '香港两用');
  await untagged.click();
  // 未知地区是空码：URL 里写成空值。
  await regionFacet.click();
  await chip(regions, '全部').click();
  await chip(regions, '未知').click();
  await shown('待探测');
  await expect.poll(() => params().getAll('region')).toEqual(['']);
  await expect(regionFacet).toHaveAccessibleName('地区 未知');

  // 手机：面板在筛选行下方展开，胶囊按屏宽折行，不被管理端 900px 断点下的其它规则压扁、不撑出横向滚动。
  await tagFacet.click();
  await page.setViewportSize({ width: 375, height: 812 });
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBe(375);
  const panel = await boxOf(tags);
  const row = await boxOf(page.getByRole('group', { name: '筛选' }));
  expect(panel.y).toBeGreaterThanOrEqual((await boxOf(tagFacet)).y + (await boxOf(tagFacet)).height);
  expect(panel.x).toBeGreaterThanOrEqual(row.x);
  expect(Math.round(panel.width)).toBe(Math.round(row.width));
  const chips = await tags.getByRole('list').getByRole('button').evaluateAll((buttons) => buttons.map((b) => {
    const r = b.getBoundingClientRect();
    return { left: r.left, right: r.right, top: r.top, height: r.height };
  }));
  for (const c of chips) {
    expect(c.height).toBe(28);
    expect(c.left).toBeGreaterThanOrEqual(panel.x);
    expect(c.right).toBeLessThanOrEqual(panel.x + panel.width);
  }
  await page.screenshot({ path: testInfo.outputPath('admin-facets-mobile.png'), fullPage: true });
  await page.setViewportSize({ width: 1440, height: 960 });

  // 刷新：筛选从 URL 还原，选择方式从浏览器偏好还原（标签面板仍是多选）。
  await page.reload();
  await expect(regionFacet).toHaveAccessibleName('地区 未知');
  await expect(tagFacet).toHaveAccessibleName('标签 无标签');
  await shown('待探测');
  await tagFacet.click();
  await expect(tags.getByRole('group', { name: '选择方式' }).getByRole('button', { name: '多选' })).toHaveAttribute('aria-pressed', 'true');
  await expect(tags.getByRole('group', { name: '匹配方式' }).getByRole('button', { name: '满足任一' })).toHaveAttribute('aria-pressed', 'true');
  await page.keyboard.press('Escape');

  // 进详情再点「返回节点列表」：地区、无标签、匹配方式与搜索词原样还原。
  await page.getByRole('link', { name: `${prefix}待探测（#${ids.get('待探测')}）`, exact: true }).click();
  await expect(page).toHaveURL(new RegExp(`/admin/nodes/${ids.get('待探测')}$`));
  await page.getByRole('link', { name: '返回节点列表' }).click();
  await expect(regionFacet).toHaveAccessibleName('地区 未知');
  await expect(tagFacet).toHaveAccessibleName('标签 无标签');
  await shown('待探测');
  expect(params().get('match')).toBe('any');
  expect(params().get('q')).toBe(prefix);

  // 清除筛选：地区、标签、无标签与匹配方式一起回到缺省。
  await page.getByRole('button', { name: '清除筛选' }).click();
  await expect.poll(() => page.url()).toMatch(/\/admin\/nodes$/);
  await expect(regionFacet).toHaveAccessibleName('地区 全部');
  await expect(tagFacet).toHaveAccessibleName('标签 全部');
});
