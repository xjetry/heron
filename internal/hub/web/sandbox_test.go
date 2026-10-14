package web

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// 主题桥接跑的是外壳脚本本身：SDK 的命名导出发出 call，外壳只对放行集合里的方法 POST 公开服务，
// 路由正则接受 /probes/N 与 /nodes/N 的同形路径。字符串包含证明不了“没放行就不会发请求”。
func TestThemeBridgePostsComparisonAndProbeRoutes(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "shell.js"), []byte(themeShellJS), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sdk.js"), []byte(themeSDKJS), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "harness.js"), []byte(themeBridgeHarness), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, "harness.js")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bridge harness: %v\n%s", err, out)
	}
	var got struct {
		Fetches []struct {
			URL    string `json:"url"`
			Method string `json:"method"`
		} `json:"fetches"`
		Navigations []string `json:"navigations"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("harness output: %v\n%s", err, out)
	}
	wantURL := map[string]bool{
		"/heron.v1.PublicService/ListProbeComparisonNodes": false,
		"/heron.v1.PublicService/QueryProbeComparison":     false,
		"/heron.v1.PublicService/QueryProbes":              false,
	}
	for _, fetch := range got.Fetches {
		if fetch.Method != "POST" {
			t.Fatalf("bridge fetch is not POST: %+v", fetch)
		}
		if _, ok := wantURL[fetch.URL]; ok {
			wantURL[fetch.URL] = true
		}
		if fetch.URL == "/heron.v1.PublicService/DeleteNode" {
			t.Fatalf("method outside the allowlist was posted: %+v", got.Fetches)
		}
	}
	for url, seen := range wantURL {
		if !seen {
			t.Fatalf("missing POST %s in %+v", url, got.Fetches)
		}
	}
	nav := map[string]bool{}
	for _, path := range got.Navigations {
		nav[path] = true
	}
	for _, path := range []string{"/probes/12", "/probes/12/", "/nodes/4", "/"} {
		if !nav[path] {
			t.Fatalf("route rejected %s: %v", path, got.Navigations)
		}
	}
	for _, path := range []string{"/probes/0", "/probes/", "/nodes/0", "/admin"} {
		if nav[path] {
			t.Fatalf("route accepted %s: %v", path, got.Navigations)
		}
	}
}

const themeBridgeHarness = `
const fs = require("node:fs");
const vm = require("node:vm");

const fetches = [];
const navigations = [];
let shellPort = null;

class CapturingChannel {
  constructor() {
    this.port1 = { onmessage: null, postMessage() {}, close() {} };
    this.port2 = {};
    shellPort = this.port1;
  }
}

const frameWindow = { postMessage() {} };
const shellListeners = [];
const shellContext = vm.createContext({
  document: { querySelector: () => ({ getAttribute: () => "/", contentWindow: frameWindow, addEventListener() {} }) },
  addEventListener: (type, fn) => { shellListeners.push({ type, fn }); },
  location: { pathname: "/" },
  history: {
    pushState(_state, _title, path) { navigations.push(path); },
    replaceState(_state, _title, path) { navigations.push(path); },
  },
  fetch: async (url, init) => {
    fetches.push({ url, method: init && init.method, body: init && init.body });
    return { ok: true, json: async () => ({ ok: true }) };
  },
  MessageChannel: CapturingChannel,
  AbortController,
  console,
});
vm.runInContext(fs.readFileSync("shell.js", "utf8"), shellContext);
const onShellMessage = shellListeners.find((entry) => entry.type === "message");
if (!onShellMessage) throw new Error("shell did not register a message handler");
onShellMessage.fn({ source: frameWindow, origin: "null", data: { type: "heron:ready", version: 1 } });
if (!shellPort || typeof shellPort.onmessage !== "function") throw new Error("shell did not attach the bridge port");

async function navigate(path) {
  const before = navigations.length;
  await shellPort.onmessage({ data: { type: "navigate", path } });
  return navigations.length > before;
}
async function callShell(method, args) {
  const before = fetches.length;
  await shellPort.onmessage({ data: { type: "call", id: 1, method, args } });
  return fetches.slice(before);
}

(async () => {
  for (const path of ["/probes/12", "/probes/12/", "/nodes/4", "/", "/probes/0", "/probes/", "/nodes/0", "/admin"]) {
    await navigate(path);
  }
  const listed = await callShell("ListProbeComparisonNodes", { taskId: "9" });
  const queried = await callShell("QueryProbeComparison", { taskId: "9", nodeIds: ["1", "2"] });
  const probes = await callShell("QueryProbes", { nodeId: "1" });
  const denied = await callShell("DeleteNode", {});
  if (listed.length !== 1 || queried.length !== 1 || probes.length !== 1 || denied.length !== 0) {
    throw new Error("allowlist mismatch " + JSON.stringify({ listed, queried, probes, denied }));
  }

  const posted = [];
  const parent = { postMessage(data) { posted.push(data); } };
  const sdkListeners = [];
  const sdkPort = { postMessage(data) { posted.push(data); }, onmessage: null };
  const sdkContext = vm.createContext({
    parent,
    addEventListener: (type, fn) => { sdkListeners.push({ type, fn }); },
    removeEventListener() {},
    document: { readyState: "complete" },
    setTimeout,
    clearTimeout,
    console,
  });
  const sdkSource = fs.readFileSync("sdk.js", "utf8").replace(/^export /gm, "") +
    "\nglobalThis.__sdk = { call, listProbeComparisonNodes, queryProbeComparison, queryProbes };\n";
  vm.runInContext(sdkSource, sdkContext);
  const sdk = sdkContext.__sdk;
  if (typeof sdk.listProbeComparisonNodes !== "function" || typeof sdk.queryProbeComparison !== "function") {
    throw new Error("named exports missing");
  }
  const connect = sdkListeners.find((entry) => entry.type === "message");
  if (!connect) throw new Error("sdk did not listen for the bridge");
  connect.fn({ source: parent, data: { type: "heron:connect", version: 1, path: "/" }, ports: [sdkPort] });
  const pending = sdk.listProbeComparisonNodes({ taskId: "3" });
  await new Promise((resolve) => setImmediate(resolve));
  const call = posted.find((data) => data && data.type === "call" && data.method === "ListProbeComparisonNodes");
  if (!call) throw new Error("sdk export did not call ListProbeComparisonNodes: " + JSON.stringify(posted));
  const bridged = await callShell(call.method, call.args);
  if (bridged.length !== 1 || bridged[0].method !== "POST" || bridged[0].url !== "/heron.v1.PublicService/ListProbeComparisonNodes") {
    throw new Error("sdk call was not posted: " + JSON.stringify(bridged));
  }
  sdkPort.onmessage({ data: { type: "result", id: call.id, ok: true, value: { nodeIds: ["1"], maxNodesPerQuery: 1 } } });
  await pending;
  process.stdout.write(JSON.stringify({ fetches, navigations }));
})().catch((err) => {
  console.error(err);
  process.exit(1);
});
`
