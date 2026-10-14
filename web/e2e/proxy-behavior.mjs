import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { once } from 'node:events';
import { mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { createServer } from 'node:http';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';
import { connect } from 'node:tls';
import { createProxy } from './proxy.mjs';

// 默认关闭时刻是 keepAliveTimeout + keepAliveTimeoutBuffer（5s + 1s）。6s 落在这条默认超时把连接关掉之后
// （定时器在响应写完时起，客户端从读完响应起算，略晚于定时器）。keepAliveTimeout 为 0 时同一条 TLS 连接仍应得到应答。
const idlePastDefaultCloseMs = 6000;

function certificate() {
  const dir = mkdtempSync(join(tmpdir(), 'heron-proxy-'));
  const keyPath = join(dir, 'key.pem');
  const certPath = join(dir, 'cert.pem');
  const tls = spawnSync('openssl', ['req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-keyout', keyPath, '-out', certPath, '-days', '1', '-subj', '/CN=localhost'], { encoding: 'utf8' });
  if (tls.status !== 0) throw new Error(tls.stderr || 'openssl failed');
  return { dir, key: readFileSync(keyPath), cert: readFileSync(certPath) };
}

function readResponse(socket, label) {
  return new Promise((resolve, reject) => {
    const chunks = [];
    let settled = false;
    const timer = setTimeout(() => fail(new Error(`${label}: no response`)), 2000);
    function cleanup() {
      clearTimeout(timer);
      socket.off('data', onData);
      socket.off('error', onError);
      socket.off('end', onEnd);
    }
    function fail(err) {
      if (settled) return;
      settled = true;
      cleanup();
      reject(err);
    }
    function succeed(value) {
      if (settled) return;
      settled = true;
      cleanup();
      resolve(value);
    }
    function onError(err) {
      fail(new Error(`${label}: ${err.code ? err.code + ' ' : ''}${err.message}`));
    }
    function onEnd() {
      fail(new Error(`${label}: connection closed before a response`));
    }
    function onData(buf) {
      chunks.push(buf);
      const raw = Buffer.concat(chunks);
      const headerEnd = raw.indexOf('\r\n\r\n');
      if (headerEnd < 0) return;
      const headerText = raw.subarray(0, headerEnd).toString('latin1');
      const status = /^HTTP\/1\.[01] (\d{3})\b/.exec(headerText);
      if (!status) return fail(new Error(`${label}: malformed status line ${headerText.split('\r\n', 1)[0]}`));
      const length = /(?:^|\r\n)content-length: *(\d+)\r\n/i.exec(headerText + '\r\n');
      if (!length) return fail(new Error(`${label}: response has no content-length`));
      const bodyStart = headerEnd + 4;
      const size = Number(length[1]);
      if (raw.length < bodyStart + size) return;
      const extra = raw.length - (bodyStart + size);
      if (extra > 0) return fail(new Error(`${label}: ${extra} extra bytes after the response`));
      succeed({ status: Number(status[1]), body: raw.subarray(bodyStart).toString('utf8') });
    }
    socket.on('data', onData);
    socket.on('error', onError);
    socket.on('end', onEnd);
  });
}

function requestOn(socket, path, label) {
  const pending = readResponse(socket, label);
  try {
    socket.write(`GET ${path} HTTP/1.1\r\nHost: localhost\r\nConnection: keep-alive\r\n\r\n`);
  } catch (err) {
    socket.destroy(err instanceof Error ? err : new Error(String(err)));
  }
  return pending;
}

test('空闲超过默认 keep-alive 超时后同一条 TLS 连接仍得到应答，且 upstream 每个请求都是新连接', { timeout: 20_000 }, async () => {
  const cert = certificate();
  const sockets = [];
  const upstream = createServer((req, res) => {
    sockets.push(req.socket);
    res.setHeader('content-type', 'text/plain');
    res.end(`ok-${sockets.length}`);
  });
  // upstream 自己的空闲超时会在约 6s 关掉连接，代理就算复用全局 agent 也只能新建连接，断言就分不出 keepAlive:false。
  // 这里不设 upstream 的空闲超时，复用与否只由代理的 agent 决定。
  upstream.keepAliveTimeout = 0;
  upstream.listen(0, '127.0.0.1');
  await once(upstream, 'listening');
  const proxy = createProxy({
    key: cert.key, cert: cert.cert, upstream: { host: '127.0.0.1', port: upstream.address().port },
    onRestart: async () => { throw new Error('restart is not part of this test'); },
  });
  proxy.listen(0, '127.0.0.1');
  await once(proxy, 'listening');
  const socket = connect({ host: '127.0.0.1', port: proxy.address().port, rejectUnauthorized: false });
  // 空闲期间服务端可能关掉连接。这个监听只防止 error 在第二次请求的监听装上之前变成未处理异常；失败仍由第二次请求报出。
  const onIdleError = () => {};
  socket.on('error', onIdleError);
  try {
    await once(socket, 'secureConnect');
    const first = await requestOn(socket, '/first', 'first request');
    assert.equal(first.status, 200);
    assert.equal(first.body, 'ok-1');
    await new Promise((resolve) => setTimeout(resolve, idlePastDefaultCloseMs));
    const second = await requestOn(socket, '/second', 'second request');
    assert.equal(second.status, 200, 'second request status');
    assert.equal(second.body, 'ok-2', 'second request body');
    assert.equal(socket.destroyed, false);
    // 全局 agent 的 timeout 是 5s，隔 6s 的两个请求即使走全局 agent 也已经各自新建连接。紧接着再发一次，复用才会显形。
    const followUp = await requestOn(socket, '/follow-up', 'follow-up request');
    assert.equal(followUp.status, 200, 'follow-up request status');
    assert.equal(followUp.body, 'ok-3', 'follow-up request body');
    assert.equal(sockets.length, 3, 'upstream request count');
    assert.notStrictEqual(sockets[1], sockets[2], 'upstream reused one connection for both requests');
  } finally {
    socket.off('error', onIdleError);
    socket.destroy();
    const proxyClosed = once(proxy, 'close');
    const upstreamClosed = once(upstream, 'close');
    proxy.closeAllConnections();
    upstream.closeAllConnections();
    proxy.close();
    upstream.close();
    await Promise.all([proxyClosed, upstreamClosed]);
    rmSync(cert.dir, { recursive: true, force: true });
  }
});
