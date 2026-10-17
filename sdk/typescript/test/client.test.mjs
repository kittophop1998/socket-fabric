import assert from 'node:assert/strict';
import test from 'node:test';
import { Client } from '../dist/client.js';
import { SocketFabricClient, SocketFabricError } from '../dist/index.js';

class Timers {
  now = 0;
  next = 0;
  tasks = new Map();
  callbacks = new Map();
  schedule = (callback, delay) => {
    const id = ++this.next;
    this.tasks.set(id, { at: this.now + delay, callback });
    this.callbacks.set(id, callback);
    return id;
  };
  cancel = id => this.tasks.delete(id);
  advance(delay) {
    const target = this.now + delay;
    for (let count = 0;; count++) {
      assert.ok(count < 1000, 'Timer loop');
      const entry = [...this.tasks].filter(([, task]) => task.at <= target)
        .sort((a, b) => a[1].at - b[1].at || a[0] - b[0])[0];
      if (!entry) break;
      this.tasks.delete(entry[0]);
      this.now = entry[1].at;
      entry[1].callback();
    }
    this.now = target;
  }
}

class Socket {
  readyState = 0;
  sent = [];
  listeners = new Map();
  closes = [];
  addEventListener(name, listener) {
    if (!this.listeners.has(name)) this.listeners.set(name, []);
    this.listeners.get(name).push(listener);
  }
  emit(name, event = {}) { this.listeners.get(name)?.forEach(listener => listener(event)); }
  open() { this.readyState = 1; this.emit('open'); }
  frame(value) { this.emit('message', { data: JSON.stringify(value) }); }
  send(body) { assert.equal(this.readyState, 1); this.sent.push(JSON.parse(body)); }
  close(code) { this.closes.push(code); this.readyState = 2; }
  finish(code = 1006) { this.readyState = 3; this.emit('close', { code }); }
}

const auth = { op: 'authenticated', connectionId: 'connection-1', projectId: 'project-a', userId: 'user-a' };
const envelope = { eventId: 'evt-1', projectId: 'project-a', roomId: 'room-a', type: 'changed', data: null, timestamp: '2026-10-15T12:34:56.123456789Z' };
const errorFrame = code => ({ op: 'error', error: { code, message: 'Untrusted message', requestId: 'request-1' } });
const tick = async () => { for (let i = 0; i < 8; i++) await Promise.resolve(); };
const observe = promise => promise.then(value => ({ value }), error => ({ error }));
async function code(promise, expected) { const result = await promise; assert.ok(result.error instanceof SocketFabricError); assert.equal(result.error.code, expected); }

function fixture(options = {}, dependencies = {}) {
  const timers = new Timers();
  const sockets = [];
  const signals = [];
  const errors = [];
  const events = [];
  const states = [];
  let token = 'client-token';
  const client = new Client({
    url: 'ws://localhost/ws',
    tokenProvider: ({ signal }) => { signals.push(signal); return token; },
    ...options,
  }, {
    socket: () => { const socket = new Socket(); sockets.push(socket); return socket; },
    schedule: timers.schedule, cancel: timers.cancel, random: () => 1,
    ...dependencies,
  });
  client.on('error', error => errors.push(error));
  client.on('event', event => events.push(event));
  client.on('state', state => states.push(state));
  const connected = async () => {
    const connecting = client.connect();
    await tick();
    const socket = sockets.at(-1);
    socket.open();
    socket.frame(auth);
    assert.deepEqual(await connecting, { connectionId: 'connection-1', projectId: 'project-a', userId: 'user-a' });
    return socket;
  };
  return { client, timers, sockets, signals, errors, events, states, connected, setToken: value => { token = value; } };
}

test('SSR-safe import and construction do not open WebSocket; options reject credential URLs', () => {
  assert.equal(typeof SocketFabricClient, 'function');
  assert.doesNotThrow(() => new SocketFabricClient({ url: 'wss://example.test/ws', tokenProvider: () => 'token' }));
  for (const url of ['https://example.test/ws', 'ws://user:secret@example.test/ws', 'ws://example.test/ws?token=secret', 'ws://example.test/ws#secret', 'bad']) {
    assert.throws(() => new SocketFabricClient({ url, tokenProvider: () => '' }), error => error.code === 'invalid_options' && !error.message.includes('secret'));
  }
  for (const connectTimeoutMs of [0, -1, NaN, Infinity, 1.2, 2 ** 31]) assert.throws(() => fixture({ connectTimeoutMs }));
});

test('connect resolves only after authentication and duplicate calls coalesce', async () => {
  const f = fixture();
  const first = f.client.connect();
  const second = f.client.connect();
  assert.equal(first, second);
  await tick();
  assert.equal(f.sockets.length, 1);
  const socket = f.sockets[0];
  socket.open();
  assert.deepEqual(socket.sent, [{ op: 'auth', token: 'client-token' }]);
  socket.frame(auth);
  await first;
  assert.deepEqual(f.states, ['connecting', 'authenticating', 'connected']);
  await f.client.connect();
  assert.equal(f.sockets.length, 1);
  f.client.disconnect();
  assert.equal(f.timers.tasks.size, 0);
});

test('initial failure is fail-fast and does not retry even after a previous successful lifecycle', async () => {
  const f = fixture();
  let promise = observe(f.client.connect());
  await tick();
  f.sockets[0].finish();
  await code(promise, 'connection_lost');
  f.timers.advance(60_000);
  assert.equal(f.sockets.length, 1);
  await f.connected();
  f.client.disconnect();
  f.sockets[1].finish(1000);
  promise = observe(f.client.connect());
  await tick();
  f.sockets[2].finish();
  await code(promise, 'connection_lost');
  f.timers.advance(60_000);
  assert.equal(f.sockets.length, 3);
});

test('attempt budget cancels ignored token providers and stale completion never opens a socket', async () => {
  let resolve;
  let signal;
  const f = fixture({ tokenProvider: context => { signal = context.signal; return new Promise(yes => { resolve = yes; }); } });
  const connecting = observe(f.client.connect());
  f.timers.advance(10_000);
  await code(connecting, 'connect_timeout');
  assert.equal(signal.aborted, true);
  resolve('late-token');
  await tick();
  assert.equal(f.sockets.length, 0);
  assert.equal(f.timers.tasks.size, 0);
});

test('abort cancels initial token/dial/auth but is detached after success; coalesced caller abort is isolated', async () => {
  const f = fixture();
  const owner = new AbortController();
  const promise = observe(f.client.connect({ signal: owner.signal }));
  await tick();
  owner.abort();
  await code(promise, 'canceled');
  assert.deepEqual(f.sockets[0].closes, [1000]);
  f.sockets[0].finish(1000);
  const nextOwner = new AbortController();
  const connecting = f.client.connect({ signal: nextOwner.signal });
  const observer = new AbortController();
  const watched = observe(f.client.connect({ signal: observer.signal }));
  observer.abort();
  await code(watched, 'canceled');
  await tick();
  f.sockets[1].open();
  f.sockets[1].frame(auth);
  await connecting;
  nextOwner.abort();
  assert.equal(f.sockets[1].readyState, 1);
  f.client.disconnect();
});

test('membership commands serialize, correlate errors and ACKs, and are idempotent', async () => {
  const f = fixture();
  const socket = await f.connected();
  const joined = [];
  const left = [];
  f.client.on('subscribed', room => joined.push(room));
  f.client.on('unsubscribed', room => left.push(room));
  const a = f.client.subscribe('room-a');
  assert.equal(f.client.subscribe('room-a'), a);
  const b = observe(f.client.subscribe('room-b'));
  assert.deepEqual(socket.sent.slice(1), [{ op: 'join', roomId: 'room-a' }]);
  socket.frame({ op: 'joined', roomId: 'room-a' });
  await a;
  await f.client.subscribe('room-a');
  assert.equal(socket.sent.length, 3);
  socket.frame(errorFrame('forbidden'));
  await code(b, 'forbidden');
  assert.equal(f.errors.at(-1).message, 'Server rejected the operation');
  const leave = f.client.unsubscribe('room-a');
  assert.equal(f.client.unsubscribe('room-a'), leave);
  socket.frame({ op: 'left', roomId: 'room-a' });
  await leave;
  await f.client.unsubscribe('room-a');
  assert.deepEqual(joined, ['room-a']);
  assert.deepEqual(left, ['room-a']);
  f.client.disconnect();
});

test('join then leave preserves order and pending unsubscribe never restores the room', async () => {
  const f = fixture();
  const first = await f.connected();
  const join = f.client.subscribe('room-a');
  const leave = observe(f.client.unsubscribe('room-a'));
  first.frame({ op: 'joined', roomId: 'room-a' });
  await join;
  assert.deepEqual(first.sent.slice(1), [{ op: 'join', roomId: 'room-a' }, { op: 'leave', roomId: 'room-a' }]);
  first.finish();
  await code(leave, 'connection_lost');
  f.timers.advance(500);
  await tick();
  f.sockets[1].open();
  f.sockets[1].frame(auth);
  assert.equal(f.sockets[1].sent.length, 1);
  f.client.disconnect();
});

test('fresh-token reconnect restores only acknowledged rooms and offline unsubscribe deletes intent', async () => {
  const f = fixture();
  const first = await f.connected();
  const a = f.client.subscribe('room-a');
  first.frame({ op: 'joined', roomId: 'room-a' });
  await a;
  const b = f.client.subscribe('room-b');
  first.frame({ op: 'joined', roomId: 'room-b' });
  await b;
  const pending = observe(f.client.subscribe('unacknowledged'));
  first.finish(1001);
  await code(pending, 'connection_lost');
  await code(observe(f.client.subscribe('offline-room')), 'not_connected');
  await f.client.unsubscribe('room-b');
  f.setToken('fresh-token');
  f.timers.advance(500);
  await tick();
  const second = f.sockets[1];
  second.open();
  assert.deepEqual(second.sent, [{ op: 'auth', token: 'fresh-token' }]);
  second.frame(auth);
  assert.deepEqual(second.sent.slice(1), [{ op: 'join', roomId: 'room-a' }]);
  second.frame({ op: 'joined', roomId: 'room-a' });
  f.client.disconnect();
});

test('recovery revalidates permissions independently and does not restore rejected rooms again', async () => {
  const f = fixture();
  let socket = await f.connected();
  for (const roomId of ['room-a', 'room-b']) {
    const join = f.client.subscribe(roomId);
    socket.frame({ op: 'joined', roomId });
    await join;
  }
  socket.finish();
  f.timers.advance(500);
  await tick();
  socket = f.sockets[1];
  socket.open(); socket.frame(auth);
  socket.frame(errorFrame('forbidden'));
  assert.deepEqual(socket.sent.at(-1), { op: 'join', roomId: 'room-b' });
  socket.frame({ op: 'joined', roomId: 'room-b' });
  await tick();
  socket.finish();
  f.timers.advance(1000);
  await tick();
  socket = f.sockets[2];
  socket.open(); socket.frame(auth);
  assert.deepEqual(socket.sent.slice(1), [{ op: 'join', roomId: 'room-b' }]);
  socket.frame({ op: 'joined', roomId: 'room-b' });
  f.client.disconnect();
});

test('room and operation bounds prevent unbounded work and rejected promises do not send', async () => {
  const f = fixture();
  const socket = await f.connected();
  const pending = Array.from({ length: 64 }, (_, i) => observe(f.client.subscribe('room-' + i)));
  await code(observe(f.client.subscribe('room-over-limit')), 'room_limit');
  await code(observe(f.client.unsubscribe('room-0')), 'operation_limit');
  assert.equal(socket.sent.length, 2);
  for (let i = 0; i < 64; i++) socket.frame({ op: 'joined', roomId: 'room-' + i });
  await Promise.all(pending);
  assert.equal(socket.sent.length, 65);
  await code(observe(f.client.subscribe('x'.repeat(129))), 'invalid_room');
  await code(observe(f.client.subscribe('*')), 'invalid_room');
  f.client.disconnect();
});

test('ACK timeout rejects all pending work and resets transport before another command', async () => {
  const f = fixture();
  const socket = await f.connected();
  const a = observe(f.client.subscribe('room-a'));
  const b = observe(f.client.subscribe('room-b'));
  f.timers.advance(10_000);
  await code(a, 'operation_timeout');
  await code(b, 'operation_timeout');
  assert.equal(socket.sent.length, 2);
  assert.deepEqual(socket.closes, [1000]);
  socket.frame({ op: 'joined', roomId: 'room-a' });
  assert.equal(socket.sent.length, 2);
  socket.finish(1000);
  f.timers.advance(500);
  await tick();
  f.sockets[1].open(); f.sockets[1].frame(auth);
  assert.equal(f.sockets[1].sent.length, 1);
  f.client.disconnect();
});

test('1008 and unauthorized stop retries until an explicit connect obtains fresh credentials', async () => {
  for (const method of ['close', 'error']) {
    const f = fixture();
    const socket = await f.connected();
    const pending = observe(f.client.subscribe('room-a'));
    if (method === 'error') socket.frame(errorFrame('unauthorized'));
    socket.finish(1008);
    await code(pending, 'unauthorized');
    f.timers.advance(60_000);
    assert.equal(f.sockets.length, 1);
    f.setToken('replacement-token');
    const connecting = f.client.connect();
    await tick();
    f.sockets[1].open();
    assert.equal(f.sockets[1].sent[0].token, 'replacement-token');
    f.sockets[1].frame(auth);
    await connecting;
    f.client.disconnect();
  }
});

test('retry jitter is bounded, overload starts slower, and flaps preserve exponential backoff', async () => {
  for (const random of [0, 0.5, 1]) {
    const f = fixture({}, { random: () => random });
    let socket = await f.connected();
    socket.finish(1013);
    let delay = Math.round(2000 * (0.5 + random * 0.5));
    f.timers.advance(delay - 1); await tick(); assert.equal(f.sockets.length, 1);
    f.timers.advance(1); await tick();
    socket = f.sockets[1]; socket.open(); socket.frame(auth); socket.finish();
    delay = Math.round(1000 * (0.5 + random * 0.5));
    f.timers.advance(delay - 1); await tick(); assert.equal(f.sockets.length, 2);
    f.timers.advance(1); await tick();
    socket = f.sockets[2]; socket.open(); socket.frame(auth);
    f.timers.advance(30_000); socket.finish();
    delay = Math.round(500 * (0.5 + random * 0.5));
    f.timers.advance(delay - 1); await tick(); assert.equal(f.sockets.length, 3);
    f.timers.advance(1); await tick(); assert.equal(f.sockets.length, 4);
    f.client.disconnect();
  }
});

test('manual disconnect cancels retries and stale callbacks; replacement waits for actual socket close', async () => {
  const f = fixture();
  const first = await f.connected();
  first.finish();
  const staleRetry = f.timers.callbacks.get(f.timers.next);
  f.client.disconnect();
  staleRetry(); f.timers.advance(60_000); await tick();
  assert.equal(f.sockets.length, 1);
  const connecting = f.client.connect(); await tick();
  const old = f.sockets[1]; old.open();
  f.client.disconnect();
  await code(observe(connecting), 'disconnected');
  const replacement = f.client.connect(); await tick();
  assert.equal(f.sockets.length, 2);
  old.frame(auth);
  old.finish(1000); await tick();
  assert.equal(f.sockets.length, 3);
  const current = f.sockets[2]; current.open(); current.frame(auth); await replacement;
  old.emit('close', { code: 1008 }); old.frame(errorFrame('unauthorized'));
  assert.equal(current.readyState, 1);
  f.client.disconnect();
  assert.equal(f.timers.tasks.size, 0);
});

test('stuck closing socket consumes connection budget without opening a second active connection', async () => {
  const f = fixture();
  await f.connected();
  f.client.disconnect();
  const connecting = observe(f.client.connect());
  await tick();
  f.timers.advance(10_000);
  await code(connecting, 'connect_timeout');
  assert.equal(f.sockets.length, 1);
});

test('strict runtime frames preserve nanosecond timestamps and JSON null; callback exceptions are isolated', async () => {
  const f = fixture();
  const socket = await f.connected();
  f.client.on('event', () => { throw new Error('application'); });
  const received = [];
  const off = f.client.on('event', event => received.push(event));
  socket.frame({ op: 'event', event: envelope });
  assert.deepEqual(received, [envelope]);
  off();
  const large = { ...envelope, data: '<>&'.repeat(20_000) };
  const escaped = JSON.stringify({ op: 'event', event: large }).replaceAll('<', '\\u003c').replaceAll('>', '\\u003e').replaceAll('&', '\\u0026');
  assert.ok(escaped.length > 256 * 1024);
  socket.emit('message', { data: escaped });
  assert.deepEqual(f.events.at(-1), large);
  assert.equal(received.length, 1);
  assert.equal(socket.readyState, 1);
  f.client.disconnect();
});

test('malformed, unexpected and cross-project frames reset safely without dispatch', async () => {
  for (const invalid of [
    { op: 'event', event: { ...envelope, projectId: 'project-b' } },
    { op: 'event', event: { ...envelope, data: undefined } },
    { op: 'event', event: { ...envelope, timestamp: 'yesterday' } },
    { op: 'event', event: { ...envelope, extra: true } },
    { op: 'joined', roomId: 'not-pending' },
    { ...auth },
    { op: 'mystery' },
  ]) {
    const f = fixture();
    const socket = await f.connected();
    socket.frame(invalid);
    assert.equal(f.events.length, 0);
    assert.equal(f.errors.at(-1).code, 'invalid_frame');
    assert.equal(socket.readyState, 2);
    f.client.disconnect();
  }
  for (const data of ['{', 'x'.repeat(1024 * 1024 + 1), new Uint8Array([1, 2])]) {
    const f = fixture(); const socket = await f.connected();
    socket.emit('message', { data });
    assert.equal(f.errors.at(-1).code, 'invalid_frame');
    f.client.disconnect();
  }
});

test('token provider failures and invalid tokens omit untrusted error detail and credentials', async () => {
  for (const tokenProvider of [() => { throw new Error('secret-token'); }, () => '', () => 'secret token', () => 'x'.repeat(16 * 1024 + 1)]) {
    const f = fixture({ tokenProvider });
    const connecting = observe(f.client.connect()); await tick();
    const result = await connecting;
    assert.ok(result.error instanceof SocketFabricError);
    assert.equal(result.error.message.includes('secret'), false);
    assert.equal(f.sockets.length, 0);
  }
});

test('an ignored-abort provider stays bounded across automatic retries and stale settlement requests a fresh token', async () => {
  let calls = 0;
  let settle;
  const f = fixture({ tokenProvider: () => {
    calls++;
    if (calls === 1) return 'first-token';
    if (calls === 2) return new Promise(resolve => { settle = resolve; });
    return 'fresh-token';
  } });
  const first = await f.connected();
  first.finish();
  f.timers.advance(500); await tick();
  assert.equal(calls, 2);
  for (let i = 0; i < 8; i++) { f.timers.advance(40_000); await tick(); }
  assert.equal(calls, 2, 'No accumulated token calls during ignored cancellation');
  assert.equal(f.sockets.length, 1);
  const currentAttempt = f.client.connect();
  settle('stale-token'); await tick();
  assert.equal(calls, 3);
  const second = f.sockets[1];
  second.open();
  assert.deepEqual(second.sent, [{ op: 'auth', token: 'fresh-token' }]);
  second.frame(auth); await currentAttempt;
  f.client.disconnect();
});

test('state callbacks may cancel connect before its abort listener is attached', async () => {
  const f = fixture();
  const controller = new AbortController();
  let added = 0;
  const original = controller.signal.addEventListener.bind(controller.signal);
  controller.signal.addEventListener = (...args) => { added++; return original(...args); };
  f.client.on('state', state => { if (state === 'connecting') f.client.disconnect(); });
  await code(observe(f.client.connect({ signal: controller.signal })), 'disconnected');
  assert.equal(added, 0);
  assert.equal(f.timers.tasks.size, 0);
  await tick(); assert.equal(f.sockets.length, 0);
});

test('provider abort callbacks can start a replacement without old failure disabling it', async () => {
  let calls = 0;
  let replacement;
  const f = fixture({ tokenProvider: ({ signal }) => {
    if (++calls === 1) {
      signal.addEventListener('abort', () => { replacement = f.client.connect(); });
      return new Promise(() => {});
    }
    return 'replacement';
  } });
  const old = observe(f.client.connect());
  f.timers.advance(10_000);
  await code(old, 'connect_timeout');
  assert.ok(replacement instanceof Promise);
  assert.equal(f.states.at(-1), 'connecting');
  // The old provider ignores abort, so replacement stays bounded and times out.
  const waiting = observe(replacement);
  f.timers.advance(10_000);
  await code(waiting, 'connect_timeout');
  assert.equal(calls, 1);
  f.client.disconnect();
});

test('stale stable timer cannot clear a new timer or reset flapping connection backoff', async () => {
  const f = fixture();
  let socket = await f.connected();
  const staleStable = f.timers.callbacks.get(f.timers.next);
  socket.finish();
  f.timers.advance(500); await tick();
  socket = f.sockets[1]; socket.open(); socket.frame(auth);
  staleStable();
  socket.finish();
  f.timers.advance(999); await tick(); assert.equal(f.sockets.length, 2);
  f.timers.advance(1); await tick(); assert.equal(f.sockets.length, 3);
  f.sockets[2].open(); f.sockets[2].frame(auth);
  staleStable();
  f.client.disconnect();
  assert.equal(f.timers.tasks.size, 0);
});

test('a closed-readyState reset clears old joined state even before the close event arrives', async () => {
  const f = fixture();
  const first = await f.connected();
  const joined = f.client.subscribe('room-a');
  first.frame({ op: 'joined', roomId: 'room-a' }); await joined;
  first.readyState = 3;
  first.emit('message', { data: '{' });
  await f.client.unsubscribe('room-a');
  f.timers.advance(500); await tick();
  const second = f.sockets[1]; second.open(); second.frame(auth);
  const rejoin = f.client.subscribe('room-a');
  assert.deepEqual(second.sent.slice(1), [{ op: 'join', roomId: 'room-a' }]);
  second.frame({ op: 'joined', roomId: 'room-a' }); await rejoin;
  first.emit('close', { code: 1008 });
  assert.equal(second.readyState, 1);
  f.client.disconnect();
});

test('fresh authentication with a changed user or project discards previous room intent', async () => {
  for (const changed of [{ userId: 'user-b' }, { projectId: 'project-b' }]) {
    const f = fixture();
    const first = await f.connected();
    const joined = f.client.subscribe('room-a');
    first.frame({ op: 'joined', roomId: 'room-a' }); await joined;
    first.finish(); f.timers.advance(500); await tick();
    const second = f.sockets[1]; second.open(); second.frame({ ...auth, ...changed });
    assert.deepEqual(second.sent.map(frame => frame.op), ['auth']);
    f.client.disconnect();
  }
});

test('reset error callbacks may start a replacement without old reset overwriting connection state', async () => {
  const f = fixture();
  const first = await f.connected();
  let replacement;
  const off = f.client.on('error', error => {
    if (error.code !== 'invalid_frame') return;
    f.client.disconnect();
    replacement = f.client.connect();
  });
  first.emit('message', { data: '{' });
  await tick();
  assert.equal(f.states.at(-1), 'connecting');
  first.finish(1000); await tick();
  const second = f.sockets[1]; second.open(); second.frame(auth);
  await replacement;
  off(); f.client.disconnect();
});
