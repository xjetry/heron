import { expect, it } from "vitest";
import { snapshotDiff } from "./operationDiff";

it("只列出变了的字段，路径带表名与行号", () => {
  const before = JSON.stringify({ node: [{ id: 1, name: "a", note: "" }], tags: [{ name: "x" }] });
  const after = JSON.stringify({ node: [{ id: 1, name: "b", note: "" }], tags: [{ name: "x" }] });
  expect(snapshotDiff(before, after)).toEqual([{ path: "node[0].name", before: '"a"', after: '"b"' }]);
});

it("建出来的行是全部字段从无到有，删掉的行反过来", () => {
  const empty = JSON.stringify({ node: [] });
  const row = JSON.stringify({ node: [{ id: 3, name: "n" }] });
  expect(snapshotDiff(empty, row)).toEqual([{ path: "node[0].id", after: "3" }, { path: "node[0].name", after: '"n"' }]);
  expect(snapshotDiff(row, empty)).toEqual([{ path: "node[0].id", before: "3" }, { path: "node[0].name", before: '"n"' }]);
});

it("相同快照没有变化；读不出 JSON 时返回 null，不报成没有变化", () => {
  const same = JSON.stringify({ settings: [{ "site.title": "Heron" }] });
  expect(snapshotDiff(same, same)).toEqual([]);
  expect(snapshotDiff("", same)).toBeNull();
  expect(snapshotDiff(same, "not json")).toBeNull();
});
