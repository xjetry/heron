import { useQuery } from "@connectrpc/connect-query";
import { skipToken } from "@tanstack/react-query";
import { type KeyboardEvent, useCallback, useRef, useState } from "react";
import { useNavigate } from "react-router";
import { errorBanner } from "../api/queryGate";
import { AdminService } from "../gen/heron/v1/admin_pb";
import { filterNodes } from "../lib/nodeSearch";
import { SEARCH_KEYSHORTCUTS, useSearchShortcut } from "../lib/useSearchShortcut";
import { Icon } from "./Icon";
import { Modal } from "./Modal";
import { SearchHint } from "./SearchHint";

const LIMIT = 8;

// 顶栏常驻各页，节点查询仅在打开搜索时启用；结果限制为前八条，由查询词进一步缩小范围。
export function QuickSearch() {
  const navigate = useNavigate();
  const [opener, setOpener] = useState<HTMLElement | null>(null);
  // 对话框关闭时焦点归还给打开它的元素；快捷键打开时这个元素是顶栏按钮，由 ref 直接指向，不按类名查 DOM。
  const trigger = useRef<HTMLButtonElement>(null);
  const [query, setQuery] = useState("");
  const [active, setActive] = useState(0);
  const nodes = useQuery(AdminService.method.listNodes, opener ? {} : skipToken);
  useSearchShortcut(useCallback(() => setOpener((current) => current ?? trigger.current ?? document.body), []));
  const close = () => { setOpener(null); setQuery(""); setActive(0); };
  const matches = nodes.data ? filterNodes(nodes.data.nodes, query).slice(0, LIMIT) : [];
  const onKeyDown = (event: KeyboardEvent<HTMLInputElement>) => {
    if (matches.length === 0) return;
    if (event.key === "ArrowDown") { event.preventDefault(); setActive((i) => Math.min(i + 1, matches.length - 1)); }
    else if (event.key === "ArrowUp") { event.preventDefault(); setActive((i) => Math.max(i - 1, 0)); }
    else if (event.key === "Enter" && matches[active]) { event.preventDefault(); const id = matches[active].id; close(); void navigate(`/nodes/${id}`); }
  };
  return (
    <>
      <button ref={trigger} type="button" className="quick-search-trigger" aria-label="搜索节点" aria-keyshortcuts={SEARCH_KEYSHORTCUTS} onClick={(event) => setOpener(event.currentTarget)}>
        <Icon name="search" /><SearchHint text="搜索节点" /><kbd>⌘K</kbd>
      </button>
      {opener && (
        <Modal title="搜索节点" opener={opener} onClose={close} className="quick-search">
          <div className="modal-body">
            <input data-autofocus type="search" aria-label="名称、IP、地区、备注或主机名" placeholder="名称、IP、地区、备注或主机名" value={query}
              onChange={(event) => { setQuery(event.target.value); setActive(0); }} onKeyDown={onKeyDown} />
            {errorBanner(nodes.error)}
            {nodes.data && matches.length === 0 && <p className="muted">没有匹配的节点。</p>}
            {matches.length > 0 && (
              <ul role="listbox" aria-label="匹配的节点">
                {matches.map((node, i) => (
                  <li key={String(node.id)} role="option" aria-selected={i === active} onPointerEnter={() => setActive(i)}>
                    <a href={`/admin/nodes/${node.id}`} onClick={(event) => { event.preventDefault(); close(); void navigate(`/nodes/${node.id}`); }}>{node.name}</a>
                  </li>
                ))}
              </ul>
            )}
          </div>
        </Modal>
      )}
    </>
  );
}
