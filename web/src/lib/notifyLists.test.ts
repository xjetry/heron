// @vitest-environment node
import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { expect, it } from "vitest";
import { NOTIFY_LISTS } from "./alerts";

const notifyListGo = readFileSync(resolve(dirname(fileURLToPath(import.meta.url)), "../../../internal/hub/store/notify_list.go"), "utf8");

// store.NotifyLists 的每一行写成 `{List: X, …}`，X 是 `Name NotifyList = "key"` 形式的常量或 NotifyList("key") 字面值。
// 取出登记表里每个列表的键，与面板的 NOTIFY_LISTS 双向逐值比对：hub 新增的列表面板没写，删掉它唯一的接收渠道时确认
// 不提示；面板留着 hub 已删的列表，说明表与 hub 已经脱节。
it("NOTIFY_LISTS 与 hub 的 store.NotifyLists 逐个列表一致", () => {
  const consts = new Map([...notifyListGo.matchAll(/\b(\w+)\s+NotifyList\s*=\s*"([^"]+)"/g)].map((m) => [m[1], m[2]]));
  const table = notifyListGo.match(/\bvar NotifyLists = \[\]NotifyListSpec\{\n([\s\S]*?)\n\}/);
  if (!table) throw new Error("var NotifyLists not found in notify_list.go");
  const keys = [...table[1].matchAll(/\bList:\s*(?:NotifyList\("([^"]+)"\)|(\w+))/g)].map((m) => {
    const key = m[1] ?? consts.get(m[2]);
    if (key === undefined) throw new Error(`NotifyLists row names ${m[2]}, which is not a NotifyList constant in notify_list.go`);
    return key;
  });
  expect(keys).toContain("notify.login_channels");
  expect(NOTIFY_LISTS.map((l) => l.key).sort()).toEqual([...keys].sort());
});
