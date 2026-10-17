// Real external application fixtures shared by the single/distributed scenarios.
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';

const pause = milliseconds => new Promise(resolve => setTimeout(resolve, milliseconds));
export async function eventually(check, message, timeout = 15_000) {
  const deadline = Date.now() + timeout;
  while (Date.now() < deadline) {
    if (await check()) return;
    await pause(40);
  }
  throw new Error(message);
}

export function startApplication({ executable, cwd, publicDir, origin, gatewayURL, gatewayURLs, users, signingSecret, publisherKeys, tokenTTL = '5m', rememberCredential }) {
  const cleanEnvironment = Object.fromEntries(Object.entries(process.env).filter(([name]) => !/^(EXAMPLE_|SOCKETFABRIC_|CLIENT_TOKEN_SECRET|PUBLISHER_|NATS_)/.test(name)));
  const child = spawn(executable, ['serve'], {
    cwd, stdio: ['ignore', 'pipe', 'pipe'],
    env: { ...cleanEnvironment, EXAMPLE_ADDR: new URL(origin).host, EXAMPLE_ORIGIN: origin, EXAMPLE_PUBLIC_DIR: publicDir,
      SOCKETFABRIC_URL: gatewayURL, SOCKETFABRIC_WS_URLS: gatewayURLs.join(','), SOCKETFABRIC_TIMEOUT: '3s',
      CLIENT_TOKEN_SECRET: signingSecret, PUBLISHER_KEYS_JSON: JSON.stringify(publisherKeys), EXAMPLE_USERS_JSON: JSON.stringify(users), EXAMPLE_TOKEN_TTL: tokenTTL },
  });
  let logs = '', closed = false;
  const sessions = [];
  for (const stream of [child.stdout, child.stderr]) stream.on('data', chunk => { logs = (logs + chunk).slice(-65_536); });
  const exit = new Promise(resolve => {
    child.once('error', () => { closed = true; resolve({ code: null, signal: 'spawn_error' }); });
    child.once('close', (code, signal) => { closed = true; resolve({ code, signal }); });
  });
  const authorization = username => 'Basic ' + Buffer.from(username + ':' + (users[username]?.password ?? 'incorrect')).toString('base64');
  const request = (path, { username, body, headers, method = 'POST' } = {}) => fetch(origin + path, {
    method, headers: { ...(username ? { Authorization: authorization(username) } : {}), ...(body !== undefined ? { 'Content-Type': 'application/json' } : {}), ...headers },
    ...(body !== undefined ? { body: JSON.stringify(body) } : {}), signal: AbortSignal.timeout(5000),
  });
  return {
    origin, request, sessions,
    get logs() { return logs; },
    async ready() {
      await eventually(async () => {
        if (closed) throw new Error('External Go application exited before becoming ready');
        try { return (await request('/api/config', { method: 'GET' })).status === 200; } catch { return false; }
      }, 'External Go application did not start');
    },
    async session(browser, username, gatewayIndex = 0, room = 'room-a') {
      const page = await browser.newPage();
      const session = { page, frames: [], exposures: [], errors: [], tokenCalls: 0, tokenResponses: [], sockets: 0, peakSockets: 0 };
      sessions.push(session);
      page.on('pageerror', () => session.errors.push('Unexpected browser script error'));
      page.on('request', request => {
        session.exposures.push(request.url() + JSON.stringify(request.headers()) + (request.postData() ?? ''));
        if (new URL(request.url()).pathname === '/api/realtime-token') ++session.tokenCalls;
      });
      page.on('response', response => {
        if (new URL(response.url()).pathname !== '/api/realtime-token' || response.status() !== 200) return;
        session.tokenResponses.push(response.json().then(body => { rememberCredential(body.token); return body; }).catch(() => undefined));
      });
      page.on('websocket', socket => {
        ++session.sockets;
        session.peakSockets = Math.max(session.peakSockets, session.sockets);
        socket.on('close', () => { --session.sockets; });
        socket.on('framesent', ({ payload }) => session.exposures.push(String(payload)));
        socket.on('framereceived', ({ payload }) => {
          session.exposures.push(String(payload));
          try { session.frames.push(JSON.parse(String(payload))); } catch { session.errors.push('Invalid gateway frame'); }
        });
      });
      await page.goto(origin);
      await page.waitForFunction(() => !document.querySelector('#connect').disabled);
      await page.fill('#username', username);
      await page.fill('#password', users[username].password);
      await page.fill('#room', room);
      await page.selectOption('#gateway', gatewayURLs[gatewayIndex]);
      await page.click('#connect');
      await eventually(() => session.frames.some(frame => frame.op === 'joined' && frame.roomId === room), 'Application did not subscribe to authorized room');
      assert.equal(await page.inputValue('#password'), '');
      assert.equal(session.sockets, 1, 'Application opened duplicate active connections');
      session.received = async event => {
        await eventually(() => session.frames.some(frame => frame.op === 'event' && frame.event.eventId === event.eventId), 'Application browser did not receive published event');
        assert.deepEqual(session.frames.filter(frame => frame.op === 'event' && frame.event.eventId === event.eventId).map(frame => frame.event), [event]);
        await page.waitForFunction(id => document.querySelector('#events').textContent.includes(id), event.eventId);
      };
      session.refresh = async () => {
        const before = session.frames.filter(frame => frame.op === 'joined').length;
        const beforeCalls = session.tokenCalls;
        await page.click('#refresh');
        await eventually(() => session.tokenCalls === beforeCalls + 1 && session.frames.filter(frame => frame.op === 'joined').length === before + 1 && session.sockets === 1, 'Explicit refresh did not re-authenticate and subscribe exactly once');
      };
      session.disconnect = async () => {
        await page.click('#disconnect');
        await eventually(() => session.sockets === 0, 'Intentional disconnect did not close the socket');
        await page.waitForFunction(() => document.querySelector('#state').textContent === 'disconnected');
      };
      return session;
    },
    async verifySecurity(serverSecrets) {
      for (const session of sessions) {
        const tokens = (await Promise.all(session.tokenResponses)).filter(Boolean);
        assert.ok(tokens.length >= 1, 'No successful application token response');
        const identity = session.frames.find(frame => frame.op === 'authenticated');
        for (const token of tokens) {
          assert.equal(token.projectId, identity.projectId);
          assert.equal(token.userId, identity.userId);
          assert.deepEqual(token.rooms, users[token.userId].rooms);
        }
        const eventIds = session.frames.filter(frame => frame.op === 'event').map(frame => frame.event.eventId);
        assert.equal(new Set(eventIds).size, eventIds.length, 'A routing loop duplicated browser delivery');
        assert.equal(session.peakSockets, 1, 'Application overlapped active WebSocket connections');
        const content = session.exposures.join('\n') + await session.page.content();
        for (const secret of serverSecrets) assert.ok(!content.includes(secret), 'Server credential leaked to browser');
        assert.equal(await session.page.evaluate(() => localStorage.length + sessionStorage.length), 0);
        assert.deepEqual(session.errors, []);
      }
      const bundle = await (await request('/app.js', { method: 'GET' })).text();
      for (const secret of serverSecrets) assert.ok(!(logs + bundle).includes(secret), 'Server credential leaked to application logs/bundle');
    },
    async close() {
      if (!closed) child.kill('SIGTERM');
      const timer = setTimeout(() => { if (!closed) child.kill('SIGKILL'); }, 7000);
      const result = await exit;
      clearTimeout(timer);
      assert.equal(result.code, 0, 'External Go application did not shut down cleanly');
    },
  };
}
