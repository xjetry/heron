// ==UserScript==
// @name         Heron 快速添加节点
// @namespace    https://github.com/xjetry/heron
// @version      1.4.0
// @description  任意站点右下角的悬浮按钮：把正在浏览的机器（名称、到期日、费用）一键添加为 Heron 监控节点，创建后直接给出安装凭据与安装命令。适配 Tampermonkey / Violentmonkey。
// @match        *://*/*
// @noframes
// @grant        GM_xmlhttpRequest
// @grant        GM_getValue
// @grant        GM_setValue
// @grant        GM_registerMenuCommand
// @connect      *
// ==/UserScript==

// 从 Heron 面板「API token」页复制的版本已经填好下面两个常量；从仓库直接拿的版本是占位符，
// 首次点悬浮按钮会引导设置，之后也可以随时用油猴菜单命令「Heron：…」改。菜单里的设置优先于常量。
// token 需要一个勾选了「创建节点」预授权的 API token（面板 → 设置 → API token）。

(function () {
  "use strict";

  const BUILTIN_HUB = "__HERON_HUB__";
  const BUILTIN_TOKEN = "__HERON_TOKEN__";

  // 占位符没替换时视为未配置，不能把 "__HERON_HUB__" 当 URL 发出去。
  const unfold = (v) => (typeof v === "string" && !v.startsWith("__HERON_") ? v.trim() : "");

  const gm = {
    xhr: typeof GM_xmlhttpRequest === "function" ? GM_xmlhttpRequest
      : (typeof GM !== "undefined" && GM && typeof GM.xmlHttpRequest === "function") ? GM.xmlHttpRequest.bind(GM) : null,
    async get(key) {
      if (typeof GM_getValue === "function") return GM_getValue(key, "");
      if (typeof GM !== "undefined" && GM && typeof GM.getValue === "function") return GM.getValue(key, "");
      return "";
    },
    async set(key, value) {
      if (typeof GM_setValue === "function") { GM_setValue(key, value); return; }
      if (typeof GM !== "undefined" && GM && typeof GM.setValue === "function") { await GM.setValue(key, value); }
    },
    menu(text, fn) {
      if (typeof GM_registerMenuCommand === "function") GM_registerMenuCommand(text, fn);
      else if (typeof GM !== "undefined" && GM && typeof GM.registerMenuCommand === "function") GM.registerMenuCommand(text, fn);
    },
  };

  const cfg = { hub: "", token: "", proxyPort: "7890" };

  async function loadConfig() {
    const [hub, token, proxyPort] = await Promise.all([gm.get("hub_url"), gm.get("api_token"), gm.get("proxy_port")]);
    cfg.hub = unfold(hub).replace(/\/+$/, "") || unfold(BUILTIN_HUB).replace(/\/+$/, "");
    cfg.token = unfold(token) || unfold(BUILTIN_TOKEN);
    if (parseProxyPort(String(proxyPort || "")) !== null) cfg.proxyPort = String(proxyPort);
  }

  async function askConfig() {
    const hub = window.prompt("Heron hub 地址（面板所在的 origin，如 https://hub.example.com）", cfg.hub || "");
    if (hub !== null) { cfg.hub = hub.trim().replace(/\/+$/, ""); await gm.set("hub_url", cfg.hub); }
    const token = window.prompt("Heron API token（需勾选「创建节点」预授权）", cfg.token || "");
    if (token !== null) { cfg.token = token.trim(); await gm.set("api_token", cfg.token); }
  }

  // ---- hub API（connect JSON；写操作一律经 ExecuteChange，预授权 token 的唯一写入口）----

  class ApiError extends Error {
    constructor(status, body, fallback) {
      const code = body && typeof body.code === "string" ? body.code : "";
      super((body && typeof body.message === "string" && body.message) || fallback || `HTTP ${status}`);
      this.code = code;
      this.status = status;
    }
  }

  function rpc(method, body) {
    return new Promise((resolve, reject) => {
      if (!gm.xhr) { reject(new ApiError(0, null, "脚本管理器不支持 GM_xmlhttpRequest，请用 Tampermonkey 或 Violentmonkey")); return; }
      gm.xhr({
        method: "POST",
        url: `${cfg.hub}/heron.v1.AdminService/${method}`,
        headers: {
          "Content-Type": "application/json",
          Authorization: `Bearer ${cfg.token}`,
          // hub 拒绝来源不匹配的浏览器请求；把 Origin 钉成 hub 自身来源，与面板发出的请求同口径。
          Origin: cfg.hub,
        },
        data: JSON.stringify(body),
        timeout: 20000,
        onload(res) {
          let data = null;
          try { data = JSON.parse(res.responseText); } catch { /* 非 JSON 按无正文处理 */ }
          if (res.status >= 200 && res.status < 300 && data !== null && typeof data === "object") { resolve(data); return; }
          reject(new ApiError(res.status, data));
        },
        onerror() { reject(new ApiError(0, null, `连不上 ${cfg.hub}（网络错误或地址不对）`)); },
        ontimeout() { reject(new ApiError(0, null, "请求超时")); },
      });
    });
  }

  // 一次执行 = 预览取 expected_version，再带同一个 request_id 执行。request_id 在一次表单填写期间保持不变：
  // 执行响应丢失后重试会命中幂等回执而不是重复建节点；任一字段变动或创建成功后换新。
  let requestId = "";
  function freshRequestId() {
    requestId = `qn-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`;
  }

  async function executeCreateNode(createNode) {
    for (let attempt = 0; ; attempt++) {
      const preview = await rpc("ExecuteChange", { preview: true, createNode });
      const version = preview.expectedVersion;
      try {
        return await rpc("ExecuteChange", { requestId, expectedVersion: version, createNode });
      } catch (err) {
        // 预览与执行之间 hub 状态变了：回执未写入，request_id 未被消耗，重取版本再来一次。
        if (err instanceof ApiError && err.code === "aborted" && attempt === 0) continue;
        throw err;
      }
    }
  }

  // ---- 安装命令：与面板注册窗口同口径 ----

  const SEMVER = /^v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-(?:0|[1-9]\d*|\d*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9]\d*|\d*[A-Za-z-][0-9A-Za-z-]*))*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$/;
  let hubVersion = "";
  // hub 绑定的 agent 版本（GetSnapshot 下发）：同版本 release 里的 install.sh 装的是它，不是 hub 自己的版本——
  // 只发 hub 的 release 原样带着绑定版本的安装脚本。
  let boundAgentVersion = "";

  function isLoopback(hostname) {
    if (hostname === "[::1]") return true;
    const m = /^127\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec(hostname);
    return m !== null && m.slice(1).every((o) => Number(o) <= 255);
  }

  function installCommand(token, domestic) {
    const script = SEMVER.test(hubVersion)
      ? `https://github.com/xjetry/heron/releases/download/${hubVersion}/install.sh`
      : "https://github.com/xjetry/heron/releases/latest/download/install.sh";
    let insecure = "";
    try {
      const url = new URL(cfg.hub);
      if (url.protocol === "http:" && !isLoopback(url.hostname)) insecure = " --insecure-http";
    } catch { /* hub 地址不合法时先给命令，报错在请求时已经提示过 */ }
    return `curl -fsSL ${script} | sh -s -- --hub ${cfg.hub} --key ${token}${insecure}${domestic ? " --update-source hub" : ""}`;
  }

  // 国内主机首次安装经 SSH 反代借用本机代理，拼法与面板 web/src/lib/installProxy.ts 相同（本脚本独立分发、
  // 不能引用面板代码），由 heron-quick-node.test.ts 逐字对照。端口拼进可复制的 shell 命令，只认不带前导零的
  // 十进制 1–65535，其余不出命令。
  function parseProxyPort(text) {
    if (!/^[1-9]\d{0,4}$/.test(text)) return null;
    const port = Number(text);
    return port <= 65535 ? port : null;
  }

  function sshProxyArgs(port) {
    const at = `127.0.0.1:${port}`;
    return `-t -R ${at}:${at} 'export http_proxy=http://${at}; export https_proxy=http://${at}; export all_proxy=socks5h://${at}; exec $SHELL -l'`;
  }

  // ---- 悬浮按钮与表单（shadow DOM，与页面样式隔离）----

  const CSS = `
    :host { all: initial; }
    * { box-sizing: border-box; font: 14px/1.5 -apple-system, "PingFang SC", "Microsoft YaHei", sans-serif; }
    .fab { position: fixed; right: 18px; bottom: 18px; z-index: 2147483646; width: 44px; height: 44px; border-radius: 50%;
      display: grid; place-items: center; padding: 0; border: 1px solid #d1d5db; cursor: pointer; background: #fff;
      box-shadow: 0 4px 14px rgba(0,0,0,.3); }
    .fab:hover { border-color: #214e57; }
    .fab svg { width: 30px; height: 30px; display: block; }
    .fab .bird { fill: #214e57; }
    .fab .legs { stroke: #214e57; }
    @media (prefers-color-scheme: dark) {
      .fab { background: #11161d; border-color: #2f3646; }
      .fab:hover { border-color: #a9d4d6; }
      .fab .bird { fill: #a9d4d6; }
      .fab .legs { stroke: #a9d4d6; }
    }
    .panel { position: fixed; right: 18px; bottom: 72px; z-index: 2147483646; width: 340px; max-height: 82vh; overflow: auto;
      background: #fff; color: #111; border-radius: 10px; box-shadow: 0 8px 30px rgba(0,0,0,.28); padding: 14px 16px; }
    @media (prefers-color-scheme: dark) { .panel { background: #1f2430; color: #e5e7eb; } .panel input, .panel select { background: #111827; color: #e5e7eb; border-color: #4b5563; } }
    h2 { font-size: 15px; margin: 0 0 10px; display: flex; justify-content: space-between; align-items: center; }
    h2 button { border: none; background: none; cursor: pointer; font-size: 16px; color: inherit; padding: 0 2px; }
    label { display: block; margin: 8px 0 2px; font-size: 12px; opacity: .75; }
    input, select { width: 100%; padding: 6px 8px; border: 1px solid #cbd5e1; border-radius: 6px; background: #fff; color: #111; }
    .row { display: flex; gap: 8px; } .row > div { flex: 1; min-width: 0; }
    .check { display: flex; gap: 6px; align-items: center; margin-top: 10px; font-size: 13px; }
    .check input { width: auto; }
    .actions { display: flex; gap: 8px; margin-top: 12px; }
    .actions button { flex: 1; padding: 7px 0; border-radius: 6px; border: 1px solid #cbd5e1; cursor: pointer; background: none; color: inherit; }
    .actions .primary { background: #4f46e5; border-color: #4f46e5; color: #fff; }
    .actions button:disabled { opacity: .55; cursor: default; }
    .note { margin: 10px 0 0; font-size: 12px; opacity: .75; word-break: break-all; }
    .error { margin: 10px 0 0; font-size: 12px; color: #dc2626; word-break: break-all; }
    .ok { color: #059669; }
    pre { background: rgba(127,127,127,.12); padding: 8px; border-radius: 6px; font-size: 12px; white-space: pre-wrap; word-break: break-all; margin: 6px 0 0; }
    .copyline { display: flex; gap: 6px; margin-top: 6px; }
    .copyline input { flex: 1; font-size: 12px; }
    .copyline button { border: 1px solid #cbd5e1; background: none; color: inherit; border-radius: 6px; padding: 0 10px; cursor: pointer; font-size: 12px; }
  `;

  const CURRENCIES = ["", "CNY", "USD", "HKD", "TWD", "JPY", "EUR", "GBP", "KRW", "SGD"];
  const CYCLES = [
    ["", "无（一次性 / 不填）"],
    ["BILLING_CYCLE_MONTHLY", "每月"],
    ["BILLING_CYCLE_QUARTERLY", "每季度"],
    ["BILLING_CYCLE_SEMIANNUAL", "每半年"],
    ["BILLING_CYCLE_YEARLY", "每年"],
    ["BILLING_CYCLE_BIENNIAL", "每两年"],
    ["BILLING_CYCLE_TRIENNIAL", "每三年"],
    ["BILLING_CYCLE_QUINQUENNIAL", "每五年"],
  ];

  const host = document.createElement("div");
  document.documentElement.appendChild(host);
  const root = host.attachShadow({ mode: "open" });
  const style = document.createElement("style");
  style.textContent = CSS;
  root.appendChild(style);

  // Heron 标志：与面板的 web/src/assets/heron.svg 同一图形与配色（UserscriptButton.test 核对两边一致，改一边须同改另一边）。
  // 脚本跑在任意第三方页面里，图形用 createElementNS 逐个建：启用 Trusted Types 的页面会拒绝 innerHTML 与 DOMParser 解析
  // 字符串，createElementNS 不受影响。
  const MARK = {
    viewBox: "0 0 64 64",
    parts: [
      ["path", { class: "bird", d: "M7 43c4-10 11-17 21-19l5-1c4-1 5-3 3-6l-3-4c-3-5 0-10 5-10 4 0 6 2 7 5l12 4-14 2c-2-1-3-2-3-4-2 0-3 1-2 3l4 5c4 6 1 11-5 14-2 10-10 15-20 13L7 43Z" }],
      ["path", { d: "M12 40c5-7 11-10 19-11-3 7-9 11-19 11Z", fill: "#fff", "fill-opacity": ".24" }],
      ["path", { class: "legs", d: "m24 45-2 13h-7m17-14 4 8-5 6h7", "stroke-width": "2.5", "stroke-linecap": "round", "stroke-linejoin": "round" }],
      ["circle", { cx: "40", cy: "7.5", r: "1.5", fill: "#e5ad64" }],
    ],
  };
  function heronMark() {
    const ns = "http://www.w3.org/2000/svg";
    const svg = document.createElementNS(ns, "svg");
    svg.setAttribute("viewBox", MARK.viewBox);
    svg.setAttribute("fill", "none");
    svg.setAttribute("aria-hidden", "true");
    for (const [tag, attrs] of MARK.parts) {
      const el = document.createElementNS(ns, tag);
      for (const [name, value] of Object.entries(attrs)) el.setAttribute(name, value);
      svg.appendChild(el);
    }
    return svg;
  }

  const fab = document.createElement("button");
  fab.className = "fab";
  fab.type = "button";
  fab.appendChild(heronMark());
  fab.title = "添加 Heron 节点";
  fab.setAttribute("aria-label", "添加 Heron 节点");
  root.appendChild(fab);

  let panel = null;
  let busy = false;

  function closePanel() { if (panel) { panel.remove(); panel = null; } }

  function copyText(text, button) {
    const done = () => { const old = button.textContent; button.textContent = "已复制"; setTimeout(() => { button.textContent = old; }, 1500); };
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(done, () => window.prompt("复制失败，请手动复制", text));
    } else {
      window.prompt("请手动复制", text);
    }
  }

  function copyRow(value, aria) {
    const row = document.createElement("div");
    row.className = "copyline";
    const input = document.createElement("input");
    input.readOnly = true;
    input.value = value;
    input.setAttribute("aria-label", aria);
    input.addEventListener("focus", () => input.select());
    const button = document.createElement("button");
    button.type = "button";
    button.textContent = "复制";
    button.addEventListener("click", () => copyText(value, button));
    row.append(input, button);
    return row;
  }

  async function openPanel() {
    if (panel) { closePanel(); return; }
    if (!cfg.hub || !cfg.token) await askConfig();
    if (!cfg.hub || !cfg.token) return;
    // 在 hub 自己的管理页上悬浮按钮没有意义。
    try { if (new URL(cfg.hub).origin === location.origin && location.pathname.startsWith("/admin")) return; } catch { /* 地址不合法时照常打开，错误交给请求 */ }
    freshRequestId();
    buildPanel();
    void preflight();
  }

  function setStatus(text, kind) {
    const el = panel && panel.querySelector(".status");
    if (!el) return;
    el.textContent = text;
    el.className = `status note ${kind || ""}`;
  }

  async function preflight() {
    setStatus("连接 hub 中…");
    try {
      const snap = await rpc("GetSnapshot", {});
      hubVersion = typeof snap.hubVersion === "string" ? snap.hubVersion : "";
      boundAgentVersion = typeof snap.boundAgentVersion === "string" ? snap.boundAgentVersion : "";
      setStatus(hubVersion ? `已连接 · hub ${hubVersion}` : "已连接");
    } catch (err) {
      setStatus(err instanceof ApiError ? err.message : String(err), "error");
    }
  }

  function buildPanel() {
    closePanel();
    panel = document.createElement("div");
    panel.className = "panel";

    const h2 = document.createElement("h2");
    h2.textContent = "添加 Heron 节点";
    const x = document.createElement("button");
    x.type = "button";
    x.textContent = "×";
    x.setAttribute("aria-label", "关闭");
    x.addEventListener("click", closePanel);
    h2.appendChild(x);
    panel.appendChild(h2);

    const fields = {};
    const addField = (key, labelText, input) => {
      const label = document.createElement("label");
      label.textContent = labelText;
      label.appendChild(input);
      panel.appendChild(label);
      input.addEventListener("input", freshRequestId);
      fields[key] = input;
    };
    const text = (value) => { const i = document.createElement("input"); i.value = value; return i; };
    const select = (options) => {
      const s = document.createElement("select");
      for (const [value, label] of options) {
        const o = document.createElement("option");
        o.value = value;
        o.textContent = label || value || "不填";
        s.appendChild(o);
      }
      return s;
    };

    // 名称预填当前页标题：在 IDC 商品页打开时通常就是机型名。
    addField("name", "名称", text((document.title || "").trim().slice(0, 64)));
    addField("expires", "到期日", (() => { const i = text(""); i.type = "date"; return i; })());

    const row = document.createElement("div");
    row.className = "row";
    const priceBox = document.createElement("div");
    const currencyBox = document.createElement("div");
    row.append(priceBox, currencyBox);
    panel.appendChild(row);
    const addInto = (box, key, labelText, input) => {
      const label = document.createElement("label");
      label.textContent = labelText;
      label.appendChild(input);
      box.appendChild(label);
      input.addEventListener("input", freshRequestId);
      fields[key] = input;
    };
    addInto(priceBox, "price", "价格", (() => { const i = text(""); i.inputMode = "decimal"; i.placeholder = "12.50"; return i; })());
    addInto(currencyBox, "currency", "币种", select(CURRENCIES.map((c) => [c, c])));
    addField("cycle", "付款周期", select(CYCLES));

    const check = document.createElement("label");
    check.className = "check";
    const autoRenew = document.createElement("input");
    autoRenew.type = "checkbox";
    check.append(autoRenew, document.createTextNode("自动续期（到期日随周期自动推后）"));
    panel.appendChild(check);
    autoRenew.addEventListener("input", freshRequestId);
    fields.autoRenew = autoRenew;

    const status = document.createElement("p");
    status.className = "status note";
    panel.appendChild(status);

    const actions = document.createElement("div");
    actions.className = "actions";
    const submit = document.createElement("button");
    submit.type = "button";
    submit.className = "primary";
    submit.textContent = "添加节点";
    submit.addEventListener("click", () => void onSubmit(fields, submit));
    const settings = document.createElement("button");
    settings.type = "button";
    settings.textContent = "设置";
    settings.addEventListener("click", async () => { await askConfig(); void preflight(); });
    actions.append(submit, settings);
    panel.appendChild(actions);

    root.appendChild(panel);
  }

  async function onSubmit(fields, submit) {
    if (busy) return;
    const name = fields.name.value.trim();
    const price = fields.price.value.trim();
    const currency = fields.currency.value;
    const cycle = fields.cycle.value;
    const expires = fields.expires.value;
    const autoRenew = fields.autoRenew.checked;
    if (!name) { setStatus("名称必填", "error"); fields.name.focus(); return; }
    if (price && !/^\d{1,9}(\.\d{1,2})?$/.test(price)) { setStatus("价格格式：最多 9 位整数、2 位小数", "error"); fields.price.focus(); return; }
    if (price && !currency) { setStatus("填了价格时币种必填", "error"); fields.currency.focus(); return; }
    if (autoRenew && (!cycle || !expires)) { setStatus("自动续期要求付款周期与到期日都填", "error"); return; }

    const billing = {};
    if (price) billing.price = price;
    if (currency) billing.currency = currency;
    if (cycle) billing.billingCycle = cycle;
    if (expires) billing.expiresOn = expires;
    if (autoRenew) billing.autoRenew = true;
    const createNode = { name };
    if (Object.keys(billing).length > 0) createNode.billing = billing;

    busy = true;
    submit.disabled = true;
    submit.textContent = "添加中…";
    try {
      const res = await executeCreateNode(createNode);
      const created = res.result || {};
      const token = typeof created.token === "string" ? created.token : "";
      const node = created.node || {};
      showResult(node, token);
    } catch (err) {
      if (err instanceof ApiError && err.code === "unauthenticated") {
        // SKILL.md 的口径：无效 token 与没有预授权写权限的 token 都报 unauthenticated，原文照显再补一句指路。
        setStatus(`${err.message}（token 无效、已吊销，或未勾选任何预授权；点「设置」可换）`, "error");
      } else if (err instanceof ApiError && err.code === "permission_denied" && /来源/.test(err.message)) {
        setStatus("hub 认为请求来源不匹配：确认 Hub 地址与浏览器访问面板用的地址一致；一致仍报错则是脚本管理器改写了 Origin 头", "error");
      } else if (err instanceof ApiError && err.code === "permission_denied") {
        setStatus(`${err.message}（token 需要勾选「创建节点」预授权）`, "error");
      } else {
        setStatus(err instanceof ApiError ? err.message : String(err), "error");
      }
    } finally {
      busy = false;
      submit.disabled = false;
      submit.textContent = "添加节点";
    }
  }

  function showResult(node, token) {
    if (!panel) return;
    const id = node.id !== undefined ? String(node.id) : "?";
    const name = typeof node.name === "string" ? node.name : "";
    const b = node.billing && typeof node.billing === "object" ? node.billing : null;
    const hasDays = b !== null && (typeof b.daysLeft === "string" || typeof b.daysLeft === "number");
    const days = hasDays ? Number(b.daysLeft) : null;

    panel.innerHTML = "";
    const h2 = document.createElement("h2");
    h2.textContent = `已添加 #${id} ${name}`;
    const x = document.createElement("button");
    x.type = "button";
    x.textContent = "×";
    x.setAttribute("aria-label", "关闭");
    x.addEventListener("click", closePanel);
    h2.appendChild(x);
    panel.appendChild(h2);

    const summary = document.createElement("p");
    summary.className = "note ok";
    summary.textContent = days === null ? "创建成功。" : days >= 0 ? `创建成功，距到期还有 ${days} 天。` : `创建成功，已过期 ${-days} 天。`;
    panel.appendChild(summary);

    if (token) {
      const t = document.createElement("p");
      t.className = "note";
      t.textContent = "安装凭据（只显示这一次）：";
      panel.appendChild(t);
      panel.appendChild(copyRow(token, "安装凭据"));

      // 国内主机只改变下面生成的命令，每次默认关闭；复制行按当前状态整段重建，复制拿到的总是屏幕上的命令。
      const check = document.createElement("label");
      check.className = "check";
      const domestic = document.createElement("input");
      domestic.type = "checkbox";
      domestic.setAttribute("aria-label", "国内主机");
      check.append(domestic, document.createTextNode("国内主机（连不上 GitHub 与 CDN）"));
      panel.appendChild(check);

      const proxy = document.createElement("div");
      const portLabel = document.createElement("label");
      portLabel.textContent = "本机代理端口";
      const port = document.createElement("input");
      port.inputMode = "numeric";
      port.value = cfg.proxyPort;
      port.setAttribute("aria-label", "本机代理端口");
      portLabel.appendChild(port);
      const sshBox = document.createElement("div");
      const howto = document.createElement("p");
      howto.className = "note";
      howto.textContent = "第一步：在开着代理的电脑上以 root 登录新节点，ssh root@主机地址 后面接上下面的参数（本机代理端口要同时接受 HTTP 与 SOCKS5，如 Clash 的混合端口；sudo 可能丢掉代理变量，请以 root 登录或用 sudo -E）。第二步：在登录后的 shell 里运行安装命令。命令带 --update-source hub，在线更新经 hub 中转；OpenRC 主机（如 Alpine）请删掉这个参数。";
      proxy.append(howto, portLabel, sshBox);
      panel.appendChild(proxy);

      const c = document.createElement("p");
      c.className = "note";
      c.textContent = SEMVER.test(hubVersion) ? `在新节点上运行（脚本取自 hub ${hubVersion} 的 release，安装 hub 绑定的 agent ${boundAgentVersion || "（未知）"}）：` : "在新节点上运行（hub 不是正式版本，将安装最新 release）：";
      panel.appendChild(c);
      const commandBox = document.createElement("div");
      panel.appendChild(commandBox);

      const render = () => {
        proxy.hidden = !domestic.checked;
        sshBox.replaceChildren();
        const parsed = parseProxyPort(port.value);
        if (parsed === null) {
          const err = document.createElement("p");
          err.className = "error proxy-error";
          err.textContent = "本机代理端口须为 1–65535 的整数";
          sshBox.appendChild(err);
        } else if (domestic.checked) {
          sshBox.appendChild(copyRow(sshProxyArgs(parsed), "SSH 反代参数"));
        }
        const command = installCommand(token, domestic.checked);
        const pre = document.createElement("pre");
        pre.textContent = command;
        commandBox.replaceChildren(pre, copyRow(command, "安装命令"));
      };
      domestic.addEventListener("change", render);
      port.addEventListener("input", () => {
        const parsed = parseProxyPort(port.value);
        if (parsed !== null) { cfg.proxyPort = String(parsed); void gm.set("proxy_port", cfg.proxyPort); }
        render();
      });
      render();
    }

    const actions = document.createElement("div");
    actions.className = "actions";
    const again = document.createElement("button");
    again.type = "button";
    again.className = "primary";
    again.textContent = "再添加一台";
    again.addEventListener("click", () => { freshRequestId(); buildPanel(); });
    const close = document.createElement("button");
    close.type = "button";
    close.textContent = "关闭";
    close.addEventListener("click", closePanel);
    actions.append(again, close);
    panel.appendChild(actions);
  }

  fab.addEventListener("click", () => void openPanel());

  gm.menu("Heron：设置 Hub 地址与 API token", () => void askConfig());

  void loadConfig();
})();
