import { createServer } from 'node:https';
import { Agent, request } from 'node:http';

// 实际终止 TLS 并转发协议，让浏览器与 hub 按部署时同一条可信代理链确定来源。
export function createProxy({ key, cert, upstream, onRestart }) {
  // hub 不设 IdleTimeout，Go 就用 ReadTimeout（cmd/hub/serve.go，30s）作为空闲 keep-alive 连接的读期限，到点关掉连接。
  // 全局 agent 默认 keep-alive，复用空闲 socket 会在 hub 已经关闭之后写入。这一跳是回环明文，每请求新连接的代价可忽略。
  const agent = new Agent({ keepAlive: false });
  const proxy = createServer({ key, cert }, async (req, res) => {
    if (req.method === 'POST' && req.url === '/__e2e/restart') {
      await onRestart();
      res.end('restarted');
      return;
    }
    const upstreamReq = request({
      hostname: upstream.host, port: upstream.port, method: req.method, path: req.url,
      headers: { ...req.headers, 'x-forwarded-proto': 'https' }, agent,
    }, (response) => {
      res.writeHead(response.statusCode, response.headers);
      response.pipe(res);
    });
    upstreamReq.on('error', () => { res.writeHead(502); res.end(); });
    req.pipe(upstreamReq);
  });
  // 空闲 keep-alive 连接只能由接下来不会再往里写的一方关闭。fixtures.ts 的 request.newContext 使用 Playwright 的
  // https.Agent({ keepAlive: true })（@playwright/test 1.63 的 APIRequestContext 不设 timeout，只在上下文销毁时
  // destroy），空闲 socket 没有自己的超时。响应结束之后 Node 把 socket 超时设为 keepAliveTimeout +
  // keepAliveTimeoutBuffer，默认 5s + 1s，到点由代理关掉浏览器还会写入的连接。0 即不设这项超时。代理随
  // server.mjs 收到的 SIGTERM 退出，这些 socket 不会在进程结束后继续累积。
  proxy.keepAliveTimeout = 0;
  proxy.on('close', () => agent.destroy());
  return proxy;
}
