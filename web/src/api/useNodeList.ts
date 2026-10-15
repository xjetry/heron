import type { Transport } from "@connectrpc/connect";
import { callUnaryMethod, createQueryOptions, useTransport } from "@connectrpc/connect-query";
import { type QueryClient, type SkipToken, skipToken, useQuery, useQueryClient } from "@tanstack/react-query";
import { AdminService, type ListNodesResponse } from "../gen/heron/v1/admin_pb";

export type NodeListInput = { tags?: string[]; untagged?: boolean };

// nodeListQueryOptions 是 ListNodes 唯一的查询定义。查询键与 connect-query 相同（按方法与条件，不含 known_revision），
// 所以按方法失效、各页共享缓存都照旧；请求带上缓存里同键上一份响应的 revision，hub 回 unchanged 时把上一份原样作为
// 结果——对象引用不变，依赖它的 memo、排序会话与编辑草稿都不重算。上一份在发请求之前取定：等待期间缓存被回收也不影响。
export function nodeListQueryOptions(qc: QueryClient, transport: Transport, input: NodeListInput | SkipToken) {
  const base = createQueryOptions(AdminService.method.listNodes, input, { transport });
  if (input === skipToken) return base;
  return {
    ...base,
    queryFn: async ({ signal }: { signal: AbortSignal }): Promise<ListNodesResponse> => {
      const previous = qc.getQueryData<ListNodesResponse>(base.queryKey);
      const response = await callUnaryMethod(transport, AdminService.method.listNodes, { ...input, knownRevision: previous?.revision ?? "" }, { signal });
      if (!response.unchanged) return response;
      // 只有带着上一份的版本去问，hub 才会回 unchanged；没有上一份却收到它是协议错位，空列表不能冒充"没有节点"。
      if (previous === undefined) throw new Error("hub 回答节点资料未变，但本地没有上一份可沿用");
      return previous;
    },
  };
}

export function useNodeList(input: NodeListInput | SkipToken, options: { enabled?: boolean; refetchInterval?: number } = {}) {
  const qc = useQueryClient();
  const transport = useTransport();
  return useQuery({ ...nodeListQueryOptions(qc, transport, input), ...options });
}
