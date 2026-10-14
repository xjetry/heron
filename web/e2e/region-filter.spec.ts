import { AdminService } from "../src/gen/heron/v1/admin_pb";
import { boxOf, expect, login, must, mustField, rpc, test } from "./fixtures";

test('公开页地区多选与家宽标签交集，标签匹配方式，未知和手机布局', async ({ page, browserName, hub }, testInfo) => {
  const ids: bigint[] = [];
  await page.goto('/admin/login');
  await login(page);
  must(await rpc(page, AdminService.method.updateSettings, { settings: { publicEnabled: true } }));
  const fixtures = [
    ['香港家宽', 'HK', '家宽'], ['日本家宽', 'JP', '家宽'], ['美国家宽', 'US', '家宽'],
    ['香港机房', 'HK', '机房'], ['日本机房', 'JP', '机房'], ['待探测', '', '家宽'],
  ];
  for (const [label, countryPin, tag] of fixtures) {
    const name = `${label}-${browserName}`;
    const id = mustField(await rpc(page, AdminService.method.createNode, { name }), 'node').id;
    ids.push(id);
    hub.deleteNodeAtEnd(id);
    must(await rpc(page, AdminService.method.updateNode, { id, name, public: true, countryPin, tags: [tag], trafficResetDay: 1, offlineGraceS: 0 }));
  }
  await page.goto('/');
  // 方块只在状态墙上：墙画出全部节点，含离线与从未上报。
  await page.getByRole('group', { name: '视图' }).getByRole('button', { name: '状态墙' }).click();
  const tiles = page.locator('.tile');
  await expect(tiles).toHaveCount(6);
  // 地区与标签是筛选行上的入口，点开后面板平铺带计数的选项；选择方式默认单选，切到多选后逐个勾选。
  const regionFacet = page.getByRole('button', { name: /^地区 / });
  const regions = page.getByRole('group', { name: '地区' });
  const chip = (group: typeof regions, name: string) => group.getByRole('list').getByRole('button', { name: new RegExp(`^${name}( \\d+)?$`) });
  await regionFacet.click();
  await expect(regions.locator('.facet-chip-label')).toHaveText(['香港', '日本', '美国', '未知']);
  await regions.getByRole('group', { name: '选择方式' }).getByRole('button', { name: '多选' }).click();
  await chip(regions, '香港').click();
  await chip(regions, '日本').click();
  await expect(regionFacet).toHaveAccessibleName('地区 已选 2 个');
  // Esc 收起面板并把焦点还给入口。
  await chip(regions, '日本').press('Escape');
  await expect(regions).toHaveCount(0);
  await expect(regionFacet).toBeFocused();
  const tagFacet = page.getByRole('button', { name: /^标签 / });
  const tags = page.getByRole('group', { name: '标签' });
  await tagFacet.click();
  await chip(tags, '家宽').click();
  await expect(tiles).toHaveCount(2);
  await expect(tiles.locator('.tile-name')).toHaveText([`香港家宽-${browserName}`, `日本家宽-${browserName}`]);
  // 标签多选默认同时满足：家宽与机房没有节点同时带，滤空；改成满足任一后两类都在。
  await tags.getByRole('group', { name: '选择方式' }).getByRole('button', { name: '多选' }).click();
  await chip(tags, '机房').click();
  await expect(tiles).toHaveCount(0);
  await tags.getByRole('group', { name: '匹配方式' }).getByRole('button', { name: '满足任一' }).click();
  await expect(tiles).toHaveCount(4);
  await page.setViewportSize({ width: 1440, height: 960 });
  // 面板在 DOM 里紧跟标签入口，视觉上却排在筛选行全部控件之后、独占一行（CSS order）。
  const below = async (name: RegExp) => {
    const [panel, control] = [await boxOf(tags), await boxOf(page.getByRole('button', { name }))];
    expect(panel.y).toBeGreaterThanOrEqual(control.y + control.height);
  };
  await below(/^只看在线$/);
  await below(/^着色依据 /);
  await page.screenshot({ path: testInfo.outputPath('regions-desktop.png'), fullPage: true });
  await page.setViewportSize({ width: 375, height: 812 });
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBe(375);
  await page.screenshot({ path: testInfo.outputPath('regions-mobile.png'), fullPage: true });
  await page.setViewportSize({ width: 1440, height: 960 });
  // 选择方式与匹配方式记在浏览器里，刷新沿用；选择本身不记。
  await page.reload();
  await expect(tiles).toHaveCount(6);
  await tagFacet.click();
  await expect(tags.getByRole('group', { name: '选择方式' }).getByRole('button', { name: '多选' })).toHaveAttribute('aria-pressed', 'true');
  await expect(tags.getByRole('group', { name: '匹配方式' }).getByRole('button', { name: '满足任一' })).toHaveAttribute('aria-pressed', 'true');
  await chip(tags, '全部').press('Escape');
  await regionFacet.click();
  await regions.getByRole('group', { name: '选择方式' }).getByRole('button', { name: '单选' }).click();
  await chip(regions, '未知').click();
  await expect(tiles).toHaveCount(1);
  await expect(tiles.locator('.tile-name')).toHaveText([`待探测-${browserName}`]);
  await chip(regions, '未知').click();
  await expect(tiles).toHaveCount(6);
});
