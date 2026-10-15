import { readdirSync, readFileSync } from "node:fs";
import { join, relative } from "node:path";
import { createRouterTransport } from "@connectrpc/connect";
import { TransportProvider } from "@connectrpc/connect-query";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { expect, it, vi } from "vitest";
import { AdminService, type ListNodesRequest, type ListNodesResponse } from "../gen/heron/v1/admin_pb";
import { useNodeList } from "./useNodeList";

type ListNodesReply = Partial<Pick<ListNodesResponse, "revision" | "unchanged">> & { nodes?: { id: bigint; name: string }[] };

function setup(listNodes: (request: ListNodesRequest) => Promise<ListNodesReply>) {
  const transport = createRouterTransport(({ service }) => { service(AdminService, { listNodes }); });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => <TransportProvider transport={transport}><QueryClientProvider client={client}>{children}</QueryClientProvider></TransportProvider>;
  return { client, wrapper };
}

it("带上一份的版本去问；hub 回未变时沿用上一份对象，变了才换", async () => {
  const node = { id: 1n, name: "a" };
  const known: string[] = [];
  const listNodes = vi.fn(async (request: ListNodesRequest): Promise<ListNodesReply> => {
    known.push(request.knownRevision);
    if (known.length === 1) return { nodes: [node], revision: "r1" };
    if (known.length === 2) return { revision: "r1", unchanged: true };
    return { nodes: [{ ...node, name: "b" }], revision: "r2" };
  });
  const { client, wrapper } = setup(listNodes);
  const { result } = renderHook(() => useNodeList({}), { wrapper });
  await waitFor(() => expect(result.current.data?.revision).toBe("r1"));
  const first = result.current.data;
  await client.refetchQueries();
  await waitFor(() => expect(listNodes).toHaveBeenCalledTimes(2));
  expect(result.current.data).toBe(first);
  await client.refetchQueries();
  await waitFor(() => expect(result.current.data?.revision).toBe("r2"));
  expect(result.current.data?.nodes[0].name).toBe("b");
  // 第一次没有上一份，无条件读取；之后每次都带上缓存里那一份的版本。
  expect(known).toEqual(["", "r1", "r1"]);
});

it("hub 回未变而本地没有上一份时报错，不把空列表当成没有节点", async () => {
  const { wrapper } = setup(async () => ({ revision: "r1", unchanged: true }));
  const { result } = renderHook(() => useNodeList({}), { wrapper });
  await waitFor(() => expect(result.current.error).not.toBeNull());
  expect(result.current.data).toBeUndefined();
});

// 同一个查询键上混用两种 queryFn，谁最后挂载谁的取数方式生效：页面一律经 useNodeList / nodeListQueryOptions 读 ListNodes，
// 只有失效（createConnectQueryKey 的 schema: 写法）可以直接点名这个方法。
it("ListNodes 只经 useNodeList 读取", () => {
  const root = join(import.meta.dirname, "..");
  const offenders: string[] = [];
  // 失效写法的命中数做冒烟：路径算错或扫描没读到源码时 offenders 恒为空，只看它会空过。
  let invalidations = 0;
  const walk = (dir: string) => {
    for (const entry of readdirSync(dir, { withFileTypes: true })) {
      const path = join(dir, entry.name);
      if (entry.isDirectory()) { if (entry.name !== "gen") walk(path); continue; }
      if (!/\.tsx?$/.test(entry.name) || /\.test\.tsx?$/.test(entry.name) || path.endsWith(join("api", "useNodeList.ts"))) continue;
      const text = readFileSync(path, "utf8");
      for (const match of text.matchAll(/(schema:\s*)?AdminService\.method\.listNodes/g)) {
        if (match[1] === undefined) offenders.push(relative(root, path));
        else invalidations++;
      }
    }
  };
  walk(root);
  expect(invalidations).toBeGreaterThan(0);
  expect(offenders).toEqual([]);
});
