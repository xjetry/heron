package web

import (
	"html/template"
	"io"
	"net/http"
)

// SandboxHeaders 必须覆盖主题文件的全部响应，包括直接导航、错误和缓存验证；iframe 属性不能保护独立打开的文档。
func SandboxHeaders(h http.Header) {
	h.Set("Content-Security-Policy", "sandbox allow-scripts; default-src 'none'; script-src http: https: 'unsafe-inline'; style-src http: https: 'unsafe-inline'; img-src http: https: data:; font-src http: https: data:; connect-src 'none'; worker-src 'none'; frame-src 'none'; object-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'self'")
	h.Set("Permissions-Policy", "publickey-credentials-create=(), publickey-credentials-get=(), camera=(), microphone=(), geolocation=()")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
}

// ThemeShell 只序列化受信的运行参数，不把包内 HTML 注入父文档。主题代码始终由不透明来源的子文档执行。
func ThemeShell(w http.ResponseWriter, frameURL string) {
	builtinHeaders(w.Header())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	io.WriteString(w, `<!doctype html><html lang="zh"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Heron</title><style>html,body,iframe{margin:0;width:100%;height:100%;border:0}body{overflow:hidden}#error{position:fixed;top:0;left:0;background:#fff;color:#900;padding:1rem}</style><iframe title="公开页主题" sandbox="allow-scripts" referrerpolicy="no-referrer" src="`+template.HTMLEscapeString(frameURL)+`"></iframe><p id="error" hidden></p><script src="/_heron/theme-shell.js"></script></html>`)
}

// ThemeRuntimeHandler 只提供项目维护的脚本，不从主题包接收脚本内容。
func ThemeRuntimeHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		builtinHeaders(w.Header())
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		switch r.URL.Path {
		case "/_heron/theme-shell.js":
			io.WriteString(w, themeShellJS)
		case "/_heron/theme-sdk.js":
			w.Header().Set("Access-Control-Allow-Origin", "*")
			io.WriteString(w, themeSDKJS)
		default:
			http.NotFound(w, r)
		}
	})
}

const themeShellJS = `(() => {
const frame = document.querySelector('iframe');
const methods = new Set(['GetSite','GetSnapshot','QueryMetrics','QueryProbes','ListProbeComparisonNodes','QueryProbeComparison']);
const preview = frame.getAttribute('src').startsWith('/_heron/preview/');
let currentPath = preview ? '/' : location.pathname;
let port, generation = 0, pending = new Set(), count = 0, since = Date.now();
function close() { generation++; port?.close(); for(const controller of pending) controller.abort(); pending.clear(); }
function route(path, replace = false) {
  if(typeof path !== 'string' || !/^\/(?:(?:nodes|probes)\/[1-9][0-9]*)?\/?$/.test(path)) return;
  if(!preview) history[replace ? 'replaceState' : 'pushState'](null, '', path);
  currentPath = path;
  port?.postMessage({type:'route', path});
}
addEventListener('popstate', () => { currentPath = location.pathname; port?.postMessage({type:'route',path:currentPath}); });
addEventListener('pagehide', close);
frame.addEventListener('load', close);
addEventListener('message', event => {
  if(event.source !== frame.contentWindow || event.origin !== 'null' || event.data?.type !== 'heron:ready' || event.data.version !== 1) return;
  close(); const current = generation, channel = new MessageChannel(); port = channel.port1;
  port.onmessage = async ({data}) => {
    if(current !== generation || !data || typeof data !== 'object') return;
    if(Date.now() - since >= 1000) { since = Date.now(); count = 0; }
    let size; try { size = JSON.stringify(data).length; } catch (_) { return; }
    if(++count > 30 || size > 16384) return;
    if(data.type === 'navigate') { route(data.path, data.replace === true); return; }
    if(data.type !== 'call' || !Number.isSafeInteger(data.id) || !methods.has(data.method) || pending.size >= 8) return;
    if(data.args == null || typeof data.args !== 'object' || Array.isArray(data.args)) return;
    const controller = new AbortController(); pending.add(controller);
    try {
      const response = await fetch('/heron.v1.PublicService/' + data.method, {method:'POST',credentials:'omit',redirect:'error',headers:{'Content-Type':'application/json'},body:JSON.stringify(data.args),signal:controller.signal});
      const value = await response.json();
      if(data.method === 'GetSite') {
        delete value.adminPath; delete value.admin_path;
        if(typeof value.title === 'string') document.title = value.title;
      }
      if(current === generation) port.postMessage({type:'result',id:data.id,ok:response.ok,value});
    } catch (_) { if(current === generation) port.postMessage({type:'result',id:data.id,ok:false,value:{message:'公开数据请求失败'}}); }
    finally { pending.delete(controller); }
  };
  frame.contentWindow.postMessage({type:'heron:connect',version:1,path:currentPath}, '*', [channel.port2]);
});
})();`

const themeSDKJS = `let port, sequence = 0, path = '/', listeners = new Set(), calls = new Map();
const ready = new Promise(resolve => {
  const receive = event => {
    if(event.source !== parent || event.data?.type !== 'heron:connect' || event.data.version !== 1 || event.ports.length !== 1) return;
    removeEventListener('message', receive); port = event.ports[0]; path = event.data.path;
    port.onmessage = ({data}) => {
      if(data?.type === 'route') { path = data.path; for(const listener of listeners) listener(path); return; }
      if(data?.type !== 'result') return;
      const call = calls.get(data.id); if(!call) return; calls.delete(data.id); clearTimeout(call.timer);
      if(data.ok) call.resolve(data.value); else call.reject(Object.assign(new Error(data.value?.message || '公开接口调用失败'), {code:data.value?.code || 'unavailable'}));
    }; resolve();
  };
  addEventListener('message', receive);
  const announce = () => parent.postMessage({type:'heron:ready',version:1}, '*');
  if(document.readyState === 'complete') announce(); else addEventListener('load', announce, {once:true});
});
export async function call(method, args = {}) {
  await ready;
  return new Promise((resolve,reject) => {
    const id = ++sequence, timer = setTimeout(() => { calls.delete(id); reject(new Error('公开接口调用超时')); }, 15000);
    calls.set(id,{resolve,reject,timer}); port.postMessage({type:'call',id,method,args});
  });
}
export const getSite = () => call('GetSite');
export const getSnapshot = () => call('GetSnapshot');
export const queryMetrics = args => call('QueryMetrics',args);
export const queryProbes = args => call('QueryProbes',args);
export const listProbeComparisonNodes = args => call('ListProbeComparisonNodes',args);
export const queryProbeComparison = args => call('QueryProbeComparison',args);
export async function navigate(path, replace = false) { await ready; port.postMessage({type:'navigate',path,replace}); }
export async function onRoute(listener) { await ready; listeners.add(listener); listener(path); return () => listeners.delete(listener); }
`
