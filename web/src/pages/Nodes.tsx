import { createConnectQueryKey, createQueryOptions, useMutation, useQuery, useTransport } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { type DragEvent, type ReactNode, useRef, useState } from "react";
import { Link } from "react-router";
import { errorBanner, queryGate } from "../api/queryGate";
import { errorText } from "../api/auth";
import { useLatestError } from "../api/useLatestError";
import { type OrderMove, useOrder } from "../api/useOrder";
import { ConfirmDelete } from "../components/ConfirmDelete";
import { Expiry } from "../components/Expiry";
import { FacetFilters } from "../components/Facet";
import { MixedCheckbox } from "../components/MixedCheckbox";
import { Select } from "../components/Select";
import { NodeAddresses } from "../components/NodeAddresses";
import { NodeCountry } from "../components/NodeCountry";
import { NodeCreateDrawer } from "../components/NodeCreateDrawer";
import { NodeCredentialsDrawer } from "../components/NodeCredentialsDrawer";
import { NodeMoveModal } from "../components/NodeMoveModal";
import { NodeOrderControl } from "../components/NodeOrderControl";
import { AdminService, type Node, type NodeStatus, type Tag } from "../gen/heron/v1/admin_pb";
import { priceText } from "../lib/billing";
import { useFacetModes } from "../lib/facets";
import { duration } from "../lib/format";
import { installCommand, savedInstallOptions } from "../lib/installCommand";
import { withId } from "../lib/ids";
import { POLL_MS } from "../lib/poll";
import { EXPIRING_DAYS } from "../lib/status";
import { useSyncedSearchParams } from "../lib/useSyncedSearchParams";
import { withoutTag } from "../lib/tags";
import { laggingNode } from "../lib/lagging";
import { Missing } from "../components/Bar";
import { PageHeader } from "../components/PageHeader";
import { RowMenu } from "../components/RowMenu";
import { StatusBadge } from "../components/StatusBadge";
import { liveById, liveStatus } from "../lib/adminStatus";

import { applyScope, applySort, facetsFromParams, isFaceted, isScoped, NO_FACETS, NO_SCOPE, NODE_FACET_MODE_KEYS, nodeFacetOptions, nodeListReturnState, paramsWithFacets, paramsWithRegions, paramsWithScope, paramsWithSearch, paramsWithSort, paramsWithTagFilter, paramsWithTagMatch, scopeFromParams, searchFromParams, sortFromParams, STATUS_OPTIONS, tagFilterFromParams, type NodeSort, type ScopeFilters, type TagFilter } from "../lib/nodeFilters";
import type { TagMatch } from "../lib/tags";
import { NodeEditor } from "./NodeEditor";
import { BatchNodeTagsEditor } from "./BatchNodeTagsEditor";
import { trafficParts } from "../lib/traffic";
import { EmptyState } from "../components/EmptyState";

// 空串表示不按状态筛选（URL 里没有 status）。
const STATUS_FILTER_OPTIONS: readonly { value: NonNullable<ScopeFilters["status"]> | ""; label: string }[] = [{ value: "", label: "全部" }, ...STATUS_OPTIONS];

// Nodes 把筛选与排序持久化在 URL；排序只改变显示顺序，不改变筛选集合，
// 批量操作仍以当前已选节点的 ID 为准，不能把排序位置当作身份。
export function Nodes() {
  const qc = useQueryClient();
  const transport = useTransport();
  const { error, mutationOptions } = useLatestError();
  // 全部筛选（搜索词、地区、标签与匹配方式、状态、到期、落后）由 URL 持有（经 useSyncedSearchParams，控件不随异步导航闪回）：
  // 从详情返回列表时原样还原（nodeListReturnState）。切换任一筛选都清空选择，避免批量操作修改已经不可见的节点。
  const [params, setParams] = useSyncedSearchParams();
  const tags = useQuery(AdminService.method.listTags, {});
  const facets = facetsFromParams(params, tags.data?.tags.map((tag) => tag.name));
  // 地区与标签入口的单选 / 多选是这台浏览器的偏好，不进 URL。
  const [facetModes, chooseFacetMode] = useFacetModes(NODE_FACET_MODE_KEYS);
  const search = searchFromParams(params);
  // 进详情时带上列表此刻的查询串（本地副本，不等异步导航落地）。
  const listReturnState = nodeListReturnState(params.size > 0 ? `?${params}` : "");
  const scope = scopeFromParams(params);
  const sort = sortFromParams(params);
  // 只有一条不带条件的查询：全部筛选都在浏览器里按它过滤（applyScope），面板计数也按它算；它同时是全部节点总数 N 的来源——
  // 移动目标位置的合法区间 1..N-k+1 按 N 计算——且与 useOrder 的 reload 同一个键。查询键不随筛选变，刷新失败时
  // react-query 保留上一份数据，弹窗草稿独立于列表，刷新与排序不会卸载正在编辑的节点。
  const nodes = useQuery(AdminService.method.listNodes, { tags: [] }, { refetchInterval: 10_000 });
  const snapshot = useQuery(AdminService.method.getSnapshot, {}, { refetchInterval: POLL_MS });
  const hubVersion = snapshot.data?.hubVersion;
  // 落后标记以 GetSnapshot 的 boundAgentVersion 为目标，hubVersion 只表示 hub 自身版本，不能代替节点的目标版本。
  const boundAgentVersion = snapshot.data?.boundAgentVersion;
  const live = liveById(snapshot.data?.nodes);
  const refresh = (options?: { throwOnError: boolean }) => Promise.all([
    qc.invalidateQueries({ queryKey: createConnectQueryKey({ schema: AdminService.method.listNodes, cardinality: "finite" }) }, options),
    qc.invalidateQueries({ queryKey: createConnectQueryKey({ schema: AdminService.method.listTags, cardinality: "finite" }) }, options),
  ]);
  // 创建与轮换的响应是唯一明文来源，不能丢弃迟到响应；删除同一节点时同步清掉它的凭据弹窗。
  // opener 记下触发元素，弹窗关闭后焦点回到它；节点多了也不会把凭据顶出视口。
  const [secret, setSecret] = useState<{ id: bigint; title: string; label: string; value: string; reRegister: boolean; opener: HTMLElement } | null>(null);
  const lastOpener = useRef<HTMLElement | null>(null);
  const [creating, setCreating] = useState<HTMLElement | null>(null);
  const [drag, setDrag] = useState<{ id: bigint; members: string } | null>(null);
  const [drop, setDrop] = useState<{ target: bigint; edge: "before" | "after" } | null>(null);
  const [editor, setEditor] = useState<{ node: Node; opener: HTMLElement } | null>(null);
  const [selected, setSelected] = useState<bigint[]>([]);
  const [batchEditor, setBatchEditor] = useState<{ nodes: Node[]; tags: Tag[]; opener: HTMLElement } | null>(null);
  // 「移动到…」的目标：批量多选或行菜单单个节点；opener 是触发元素，弹窗关闭后焦点回到它。
  const [moveTarget, setMoveTarget] = useState<{ nodes: readonly Node[]; opener: HTMLElement } | null>(null);
  const create = useMutation(AdminService.method.createNode, {
    ...mutationOptions,
    onSuccess: (result) => {
      const node = result.node;
      setCreating(null);
      // 已出租的机器不装 agent，创建响应里的 token 没有安装对象，不打开凭据抽屉。
      if (node && !node.rented) setSecret({ title: "节点已创建", id: node.id, label: `节点 ${withId(node.name, node.id)} 的 token`, value: result.token, reRegister: false, opener: lastOpener.current ?? document.body });
      void refresh();
    },
  });
  // 单一弹窗承载当前目标；保存期间不可关闭或切换，回读完成后才解除编辑态。
  const update = useMutation(AdminService.method.updateNode, {
    ...mutationOptions,
    onSuccess: async () => {
      try { await refresh({ throwOnError: true }); }
      catch (error) { throw new Error(`已保存，但回读失败：${errorText(error)}`, { cause: error }); }
    },
  });
  const batchUpdate = useMutation(AdminService.method.batchUpdateNodeTags, {
    onSuccess: async () => {
      // 服务端已应用增删意图，旧快照不能继续作为草稿基线；回读失败在列表提示，不重开这份草稿。
      setBatchEditor(null);
      setSelected([]);
      try { await refresh({ throwOnError: true }); }
      catch (error) { throw new Error(`已保存，但回读失败：${errorText(error)}`, { cause: error }); }
    },
  });
  const remove = useMutation(AdminService.method.deleteNode, {
    ...mutationOptions,
    onSuccess: (_result, request) => { setSecret((current) => current?.id === request.id ? null : current); return refresh(); },
  });
  // 换发的 token 是节点凭据，抽屉里的命令带 --re-register：目标主机上若留有旧配置，不重新注册就会沿用旧身份、忽略新 token。
  const showRotated = (id: bigint, token: string, opener: HTMLElement) => {
    const name = nodes.data?.nodes.find((node) => node.id === id)?.name ?? String(id);
    setSecret({ title: "节点凭据", id, label: `节点 ${withId(name, id)} 的新 token`, value: token, reRegister: true, opener });
  };
  const rotate = useMutation(AdminService.method.rotateNodeToken, {
    ...mutationOptions,
    onSuccess: (result, request) => {
      if (request.id != null) showRotated(request.id, result.token, lastOpener.current ?? document.body);
      return refresh();
    },
  });
  // 「复制安装命令」只给从未上报、且未标已出租的节点：hub 不存 token 明文，要拿到命令里的凭据只能先换发，而从未上报的节点没有任何在运行的
  // agent 用着旧凭据，作废它没有影响。已出租的机器不装 agent，换发出来的命令没有安装对象。命令与凭据抽屉里显示的同一个拼法（lib/installCommand.ts）、读这台浏览器记住的安装参数，
  // 与换 token 一样带 --re-register；剪贴板写不进去（无权限、非安全上下文、换发等待期间用户手势已过期）时退回凭据抽屉，
  // 换发的明文不丢。换发失败只走 mutationOptions 的错误横幅，没有凭据可展示。
  const [copiedInstall, setCopiedInstall] = useState<string | null>(null);
  const rotateForCopy = useMutation(AdminService.method.rotateNodeToken, mutationOptions);
  const copyInstallCommand = async (node: Node, trigger: HTMLElement) => {
    if (hubVersion === undefined) return;
    setCopiedInstall(null);
    let token: string;
    try { token = (await rotateForCopy.mutateAsync({ id: node.id })).token; } catch { return; }
    void refresh();
    const command = installCommand({ hubVersion, boundAgentVersion: boundAgentVersion ?? "", registerKey: token, reRegister: true, options: savedInstallOptions(window.location.origin) });
    try {
      if (!navigator.clipboard) throw new Error("clipboard unavailable");
      await navigator.clipboard.writeText(command.curl);
      setCopiedInstall(withId(node.name, node.id));
    } catch {
      showRotated(node.id, token, trigger);
    }
  };
  const reorder = useMutation(AdminService.method.reorderNodes);
  const moveNodes = useMutation(AdminService.method.moveNodes, {
    onSuccess: () => {
      setMoveTarget(null);
      setSelected([]);
      // 按方法的键失效 listNodes（列表与总数同一条查询）；标签不受影响。
      return qc.invalidateQueries({ queryKey: createConnectQueryKey({ schema: AdminService.method.listNodes, cardinality: "finite" }) });
    },
  });
  const setFilters = (next: (current: URLSearchParams) => URLSearchParams) => { setParams(next); setSelected([]); };
  const setSearch = (value: string) => setFilters((current) => paramsWithSearch(current, value));
  const setRegions = (next: string[]) => setFilters((current) => paramsWithRegions(current, next));
  const setTagFilter = (filter: TagFilter) => setFilters((current) => paramsWithTagFilter(current, filter));
  const setTagMatch = (match: TagMatch) => setFilters((current) => paramsWithTagMatch(current, match));
  const setScope = (next: ScopeFilters) => setFilters((current) => paramsWithScope(current, next));
  const clearFilters = () => setFilters((current) => paramsWithScope(paramsWithFacets(paramsWithSearch(current, ""), NO_FACETS), NO_SCOPE));
  // 排序不收窄集合，已选节点仍可见，不经 setFilters 清空选择。
  const setSort = (next: NodeSort) => setParams((current) => paramsWithSort(current, next));
  // narrowed 表示显示的是全部节点的子集，决定空列表的文案。filtered 另含不收窄的非缺省条件（匹配方式），决定「清除筛选」
  // 是否出现；按列排序不是筛选，不在其中。reordered 表示显示顺序不是服务端全序（收窄或按列排序）：行首序号改用全序名次，
  // 拖动与上下移关闭（它们保存完整排列）。
  const narrowed = search !== "" || isFaceted(facets) || isScoped(scope);
  const filtered = narrowed || facets.tagMatch !== "all";
  const reordered = narrowed || sort !== null;
  const order = useOrder({
    items: nodes.data?.nodes ?? [], id: (node) => node.id, enabled: !reordered && nodes.data !== undefined,
    save: (ids) => reorder.mutateAsync({ ids }),
    reload: async () => {
      // 排序要求完整排列，回读与列表同一条不带条件的查询。
      const options = createQueryOptions(AdminService.method.listNodes, { tags: [] }, { transport });
      await qc.cancelQueries({ queryKey: options.queryKey, exact: true });
      return (await qc.fetchQuery({ ...options, staleTime: 0 })).nodes;
    },
  });
  const removeTag = useMutation(AdminService.method.deleteTag, {
    ...mutationOptions,
    // 删掉的标签必须从过滤里去掉：它已不在任何节点上，留下就是一个匹配不到任何节点、却仍显示为已选的条件（只选了它或按
    // 同时满足时列表为空）。无标签过滤下没有标签可选，状态不变。
    onSuccess: (_result, request) => {
      setParams((current) => {
        const filter = tagFilterFromParams(current);
        return filter.kind === "tags" ? paramsWithTagFilter(current, { kind: "tags", names: withoutTag(filter.names, request.name ?? "") }) : current;
      });
      return refresh();
    },
  });
  const gate = queryGate(nodes);
  const list = applySort(applyScope(order.items, live, boundAgentVersion, scope, search, facets), sort);
  const selectedIds = new Set(selected);
  const selectedNodes = list.filter((node) => selectedIds.has(node.id));
  // 创建和编辑由弹窗占用交互；换发响应前尚无弹窗，也要锁住同一批入口，避免并发响应覆盖唯一明文与返回焦点。
  const editing = editor !== null || batchEditor !== null || batchUpdate.isPending || creating !== null || rotate.isPending || rotateForCopy.isPending;
  // MoveNodes 在途时列表即将被重写，拖动 / 方向键的完整排列保存一并停下。
  const sortable = !reordered && !order.blocked && !editing && list.length > 1 && !moveNodes.isPending;
  // 「移动到…」按全序名次走 MoveNodes，过滤时也可用；编辑弹窗、MoveNodes 在途、排序会话未确认或被阻塞时
  // 关闭。N 来自不带条件的完整列表：未就绪或读取失败时入口关闭并说明原因，不给一个算不出区间的输入框。
  const moveTotal = nodes.data?.nodes.length;
  const moveReady = moveTotal !== undefined && nodes.error == null;
  const moveLocked = editing || moveNodes.isPending || order.pending || order.blocked || !moveReady;
  const members = list.map((node) => String(node.id)).sort().join(",");
  const dragging = sortable && drag?.members === members ? drag.id : null;
  const moveNode = (id: bigint, move: OrderMove) => {
    if (!sortable) return;
    order.move(id, move);
  };
  const endDrag = () => { setDrag(null); setDrop(null); };
  const dropPosition = (event: DragEvent<HTMLTableRowElement>, target: bigint): { target: bigint; edge: "before" | "after" } => {
    const bounds = event.currentTarget.getBoundingClientRect();
    return { target, edge: event.clientY < bounds.top + bounds.height / 2 ? "before" : "after" };
  };
  const openEditor = (node: Node, opener: HTMLElement) => {
    if (editing) return;
    update.reset();
    setEditor({ node, opener });
  };

  const allNodes = nodes.data?.nodes ?? [];
  const options = nodeFacetOptions(allNodes, tags.data?.tags.map((tag) => tag.name), facets);
  const untagged = facets.tags.kind === "untagged";
  // 标签入口与公开页同一规则（有标签时出现），另在已选无标签或节点上有标签时也出现：「无标签」不依赖 ListTags，清单加载中
  // 或失败时照常可选。
  const showTags = options.tags.length > 0 || untagged || allNodes.some((node) => node.tags.length > 0);
  return <section>
    <PageHeader title="节点" actions={<button type="button" className="primary-button" disabled={editing} onClick={(event) => { lastOpener.current = event.currentTarget; create.reset(); setCreating(event.currentTarget); }}>添加节点</button>} />
    {!editor && errorBanner(nodes.error)}
    {snapshot.error != null && <p role="alert" className="error">{boundAgentVersion === undefined ? "无法取得 hub 绑定的 agent 版本，落后标记不可用" : `刷新失败，落后标记按上次取得的绑定版本 ${boundAgentVersion || "空"} 判断`}；在线状态与流量可能不是最新值：{errorText(snapshot.error)}</p>}
    {secret && <NodeCredentialsDrawer title={secret.title} secretLabel={secret.label} token={secret.value} hubVersion={hubVersion} boundAgentVersion={boundAgentVersion} error={snapshot.error} reRegister={secret.reRegister} opener={secret.opener} onClose={() => setSecret(null)} />}
    <div className="filter-row" role="group" aria-label="筛选">
      <input type="search" aria-label="搜索节点" placeholder="名称、IP、地区、备注或主机名" value={search} onChange={(event) => setSearch(event.target.value)} />
      <FacetFilters modes={facetModes} onMode={chooseFacetMode}
        regions={{ options: options.regions, selected: facets.regions, onChange: setRegions }}
        tags={showTags ? {
          options: options.tags, selected: facets.tags.kind === "tags" ? facets.tags.names : [], onChange: (names) => setTagFilter({ kind: "tags", names }),
          match: facets.tagMatch, onMatch: setTagMatch,
          exclusive: { label: "无标签", name: "无标签节点", count: allNodes.filter((node) => node.tags.length === 0).length, on: untagged, onSelect: () => setTagFilter({ kind: "untagged" }) },
        } : undefined} />
      <Select label="状态" value={scope.status ?? ""} options={STATUS_FILTER_OPTIONS} onChange={(next) => setScope({ ...scope, status: next || null })} />
      <label className="check"><input type="checkbox" aria-label={`只看 ${EXPIRING_DAYS} 天内到期`} checked={scope.expiring} onChange={() => setScope({ ...scope, expiring: !scope.expiring })} />{EXPIRING_DAYS} 天内到期</label>
      <label className="check"><input type="checkbox" aria-label="只看 agent 版本落后" checked={scope.lagging} onChange={() => setScope({ ...scope, lagging: !scope.lagging })} />agent 版本落后</label>
      {/* 排序态在筛选行里有可见的指示与退出：窄屏下表头（含排序按钮）不可见，到期卡又带着排序链进来，没有它就退不出去。 */}
      {sort === "expiry" && <span className="chip">按到期排序<button type="button" aria-label="取消按到期排序" onClick={() => setSort(null)}>×</button></span>}
      {filtered && <button type="button" className="link" onClick={clearFilters}>清除筛选</button>}
      {tags.error != null && <span role="alert" className="error">无法取得标签清单：{errorText(tags.error)}</span>}
      <span className="muted num">{list.length} / {order.items.length}</span>
    </div>
    {!editing && errorBanner(error)}
    {!batchEditor && errorBanner(batchUpdate.error)}
    {!batchEditor && batchUpdate.isPending && <p role="status" className="muted">标签已保存，正在重新读取…</p>}
    {order.error != null && <p role="alert" className="error">排序未完成：{errorText(order.error)}</p>}
    {order.pending && <p role="status" className="muted">正在保存并确认排序…</p>}
    {order.confirmed && <p className="order-saved" aria-live="polite">顺序已保存</p>}
    {copiedInstall !== null && <p className="order-saved" role="status">已复制 {copiedInstall} 的 curl 安装命令（已换发新 token，旧 token 作废）</p>}
    {order.blocked && <button type="button" onClick={order.recover} disabled={order.pending}>重新读取排序</button>}
    {reordered && <p className="node-subtext">筛选或按列排序时不能用拖动或上下移（它们保存完整排列）；可用行菜单的「移动到…」按全序名次移动，或清除筛选与排序后再调整。</p>}
    {gate.ready ? <>
      {selectedNodes.length > 0 && <div className="batch-toolbar" role="toolbar" aria-label="批量操作">
        <span>已选择 {selectedNodes.length} 个节点</span>
        <button type="button" disabled={editing || nodes.error != null || remove.isPending || tags.data === undefined || tags.error != null} onClick={(event) => { batchUpdate.reset(); setBatchEditor({ nodes: selectedNodes, tags: tags.data?.tags ?? [], opener: event.currentTarget }); }}>批量编辑标签</button>
        <button type="button" disabled={moveLocked} onClick={(event) => { moveNodes.reset(); setMoveTarget({ nodes: selectedNodes, opener: event.currentTarget }); }}>移动到…</button>
        {!moveReady && nodes.error == null && <span className="muted">正在读取节点总数…</span>}
        {nodes.error != null && <span className="error">无法取得节点总数，「移动到…」不可用：{errorText(nodes.error)}</span>}
        <button type="button" className="link" disabled={editing} onClick={() => setSelected([])}>清除选择</button>
      </div>}
      <p className="node-subtext order-help" id="node-order-help">拖动手柄调整顺序，松开后自动保存。也可用行菜单（⋯）上移、下移、置顶、置底或「移动到…」指定位置，或聚焦手柄后按方向键、Home / End；勾选多个节点后可批量「移动到…」。</p>
      {list.length === 0
        ? scope.lagging && boundAgentVersion === undefined
          ? <EmptyState status title="无法取得 hub 绑定的 agent 版本，「agent 版本落后」筛选暂时没有结果。" />
          : narrowed ? <EmptyState status title="没有匹配的节点。" /> : <EmptyState title="还没有节点。">添加节点后安装 agent 即可开始监控。</EmptyState>
        : <div className="table-scroll" role="region" aria-label="节点管理" tabIndex={0}>
        <table className="nodes node-management"><thead><tr>
          <th data-column="select"><MixedCheckbox label="选择当前结果全部节点" checked={selectedNodes.length === 0 ? false : selectedNodes.length === list.length ? true : "mixed"} disabled={editing || list.length === 0} onChange={() => setSelected(selectedNodes.length === list.length ? [] : list.map((node) => node.id))} /></th>
          <th data-column="order"><span className="sr-only">排序</span></th><th data-column="name">节点</th><th data-column="addresses">IPv4 / IPv6</th><th data-column="status">状态</th><th data-column="uptime">运行时长</th><th data-column="traffic">本周期</th><th data-column="billing">费用</th><th data-column="expiry" aria-sort={sort === "expiry" ? "ascending" : "none"}><button type="button" className="link" onClick={() => setSort(sort === "expiry" ? null : "expiry")}>到期</button></th><th data-column="actions"><span className="sr-only">操作</span></th>
        </tr></thead>
          <tbody>{list.map((node, index) => {
            const label = withId(node.name, node.id);
            return <NodeRow key={String(node.id)} node={node} live={live.get(node.id)} boundAgentVersion={boundAgentVersion} detailState={listReturnState}
              selection={<MixedCheckbox label={`选择 ${label}`} checked={selectedIds.has(node.id)} disabled={editing} onChange={() => setSelected((current) => current.includes(node.id) ? current.filter((id) => id !== node.id) : [...current, node.id])} />}
              orderClass={dragging === node.id ? "is-dragging" : dragging !== null && drop?.target === node.id ? `drop-${drop.edge}` : undefined}
              onDragOver={(event) => { if (dragging === null || dragging === node.id) return; event.preventDefault(); event.dataTransfer.dropEffect = "move"; setDrop(dropPosition(event, node.id)); }}
              onDrop={(event) => { if (dragging !== null && dragging !== node.id) { event.preventDefault(); moveNode(dragging, dropPosition(event, node.id)); } endDrag(); }}
              orderControl={<NodeOrderControl label={label} count={list.length} position={reordered ? node.position : index + 1} reorderDisabled={!sortable}
                onMove={(move) => moveNode(node.id, move)} onDragEnd={endDrag}
                onDragStart={(event) => { if (!sortable) { event.preventDefault(); return; } event.dataTransfer.effectAllowed = "move"; event.dataTransfer.setData("text/plain", String(node.id)); setDrag({ id: node.id, members }); setDrop(null); }} />}
              menu={<RowMenu label={label} items={[
                { label: "编辑", disabled: editing, onSelect: (trigger) => openEditor(node, trigger) },
                { label: "查看详情", to: `/nodes/${node.id}`, state: listReturnState },
                // 上下移与置顶置底和排序手柄同一入口（完整排列），可用条件也相同：筛选或排序会话阻塞时整组禁用。
                { label: "上移一位", disabled: !sortable || index === 0, onSelect: () => moveNode(node.id, -1) },
                { label: "下移一位", disabled: !sortable || index === list.length - 1, onSelect: () => moveNode(node.id, 1) },
                { label: "置顶", disabled: !sortable || index === 0, onSelect: () => moveNode(node.id, "first") },
                { label: "置底", disabled: !sortable || index === list.length - 1, onSelect: () => moveNode(node.id, "last") },
                { label: "移动到…", disabled: moveLocked, onSelect: (trigger) => { moveNodes.reset(); setMoveTarget({ nodes: [node], opener: trigger }); } },
                ...(neverReported(node, live.get(node.id)) && !node.rented ? [{ label: "复制安装命令（会换发新 token）", disabled: editing || hubVersion === undefined, onSelect: (trigger: HTMLElement) => { void copyInstallCommand(node, trigger); } }] : []),
                { label: "换 token", disabled: rotate.isPending || editing, onSelect: (trigger) => { lastOpener.current = trigger; rotate.mutate({ id: node.id }); } },
                { label: "删除", danger: true, confirm: `确认删除 ${label}`, disabled: remove.isPending || editing, onSelect: () => remove.mutate({ id: node.id }) },
              ]}/>} />;
          })}</tbody>
        </table>
      </div>}
    </> : gate.loading}
    <TagManager tags={tags.data?.tags} pending={removeTag.isPending} onDelete={(name) => removeTag.mutate({ name })} />
    {batchEditor && <BatchNodeTagsEditor nodes={batchEditor.nodes} knownTags={batchEditor.tags} opener={batchEditor.opener} saving={batchUpdate.isPending} error={batchUpdate.error} onClose={() => setBatchEditor(null)}
      onSave={(changes) => batchUpdate.mutate(changes)} />}
    {moveTarget && <NodeMoveModal nodes={moveTarget.nodes} total={moveTotal ?? 0} pending={moveNodes.isPending} error={moveNodes.error} opener={moveTarget.opener}
      onClose={() => { setMoveTarget(null); moveNodes.reset(); }}
      onConfirm={(position) => moveNodes.mutate({ ids: moveTarget.nodes.map((node) => node.id), position })} />}
    {editor && <NodeEditor key={String(editor.node.id)} node={editor.node} opener={editor.opener} knownTags={tags.data?.tags ?? []}
      saving={update.isPending} error={update.error} listError={nodes.error} onClose={() => setEditor(null)}
      onSave={(patch) => update.mutate({ id: editor.node.id, ...patch }, { onSuccess: () => setEditor(null) })} />}
    {creating && <NodeCreateDrawer opener={creating} pending={create.isPending} error={create.error} onClose={() => setCreating(null)} onCreate={(request) => create.mutate(request)} />}
  </section>;
}

// 节点资料的 lastSeenAt 随 ListNodes（10 秒）刷新，快照条目随 POLL_MS 刷新，两边任一处有上报时间都算上报过；快照里没有
// 这个节点时只看节点资料。不经 nodeStatus：维护中的节点也可能从未上报，这里问的只是"有没有 agent 用过它的凭据"。
function neverReported(node: Node, live: { lastSeenAt?: bigint } | undefined): boolean {
  return node.lastSeenAt === undefined && live?.lastSeenAt === undefined;
}

function NodeRow({ node, live, boundAgentVersion, detailState, selection, orderControl, orderClass, onDragOver, onDrop, menu }: {
  node: Node; live: NodeStatus | undefined; boundAgentVersion?: string; detailState: unknown; selection: ReactNode; orderControl: ReactNode; menu: ReactNode;
  orderClass?: string; onDragOver: (event: DragEvent<HTMLTableRowElement>) => void; onDrop: (event: DragEvent<HTMLTableRowElement>) => void;
}) {
  const label = withId(node.name, node.id);
  const status = liveStatus(node, live);
  const lagging = laggingNode(node, boundAgentVersion);
  return <tr className={orderClass} aria-label={node.name} data-status={status ?? "unknown"} onDragOver={onDragOver} onDrop={onDrop}>
    <td data-column="select">{selection}</td>
    <td data-column="order" data-label="排序">{orderControl}</td>
    <td data-column="name" data-label="节点">
      <div className="node-name-line"><Link to={`/nodes/${node.id}`} state={detailState} aria-label={label}>{node.name}</Link><NodeCountry node={node} />
        {node.public && <span className="chip chip-public">公开</span>}</div>
      {(lagging || node.tags.length > 0 || node.note) && <div className="node-secondary">
        {lagging && <span className="badge-attention" title={`低于 hub 绑定的 agent 版本 ${boundAgentVersion}`}>agent 低于 {boundAgentVersion}</span>}
        {node.tags.length > 0 && <ul className="tag-chips" aria-label={`标签 ${label}`} title={node.tags.join("、")}>{node.tags.map((tag) => <li key={tag} className="chip">{tag}</li>)}</ul>}
        {node.note && <p className="node-note clip-text muted" title={node.note}>{node.note}</p>}
      </div>}
    </td>
    <td data-column="addresses" data-label="IPv4 / IPv6"><NodeAddresses network={node.network} reported={node.facts?.network} /></td>
    <td data-column="status" data-label="状态">{status ? <StatusBadge status={status} /> : <span className="muted">状态未知</span>}</td>
    <td data-column="uptime" data-label="运行时长">{live?.metrics?.uptimeS !== undefined ? <span className="num cell-main">{duration(live.metrics.uptimeS)}</span> : <Missing />}</td>
    <td data-column="traffic" data-label="本周期">{live?.traffic ? <TwoLine main={trafficParts(live.traffic).amount} note={trafficParts(live.traffic).percent} mono /> : <Missing />}</td>
    <td data-column="billing" data-label="费用"><TwoLine main={priceText(node.billing) || "—"} note={node.billing?.autoRenew ? "自动续期" : undefined} /></td>
    <td data-column="expiry" data-label="到期">{node.billing?.expiresOn ? <Expiry billing={node.billing} rented={node.rented} /> : "—"}</td>
    <td data-column="actions">{menu}</td>
  </tr>;
}

// 节点表的窄列固定写成两行：主读数不折行，附注（配额百分比、自动续期）另起一行，不在主读数中间随列宽断开。
// 两段之间的空格是给读屏与单元格文字用的分隔，视觉上由 .cell-note 换行。
function TwoLine({ main, note, mono = false }: { main: string; note?: string; mono?: boolean }) {
  return <><span className="num cell-main">{main}</span>{note && <>{" "}<span className={mono ? "num cell-note" : "cell-note"}>{note}</span></>}</>;
}

function TagManager({ tags, pending, onDelete }: { tags: readonly Tag[] | undefined; pending: boolean; onDelete: (name: string) => void }) {
  if (tags === undefined) return null;
  return <details className="tag-management" open><summary>标签管理 <span className="muted">{tags.length}</span></summary><section aria-label="标签">
    {tags.length === 0 ? <p className="node-subtext">还没有标签；在节点的编辑里添加。</p> : <ul className="tag-list">{tags.map((tag) => <li key={tag.name}><span className="tag">{tag.name}</span> <span className="muted">{tag.nodeCount} 个节点</span>{" "}<span className="tag-delete"><ConfirmDelete label={`删除标签 ${tag.name}`} confirm={`确认删除标签 ${tag.name}`} note="只从节点上解除，节点不受影响" pending={pending} onDelete={() => onDelete(tag.name)} /></span></li>)}</ul>}
  </section></details>;
}
