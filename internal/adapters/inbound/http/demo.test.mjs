import assert from 'node:assert/strict';
import test from 'node:test';
import { ReconnectingClient, reconnectDelay } from './demo.js';

class Timers {
  now = 0;
  next = 0;
  tasks = new Map();
  callbacks = new Map();
  schedule = (callback, delay) => {
    const id = ++this.next;
    this.tasks.set(id, { callback, at: this.now + delay });
    this.callbacks.set(id, callback);
    return id;
  };
  cancel = id => this.tasks.delete(id);
  advance(delay) {
    const target = this.now + delay;
    for (let operations = 0; ; operations++) {
      assert.ok(operations < 1000, 'Unexpected timer loop');
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
  listeners = new Map();
  sent = [];
  closes = [];
  addEventListener(name, listener) {
    if (!this.listeners.has(name)) this.listeners.set(name, []);
    this.listeners.get(name).push(listener);
  }
  emit(name, event = {}) { this.listeners.get(name)?.forEach(listener => listener(event)); }
  open() { this.readyState = 1; this.emit('open'); }
  frame(frame) { this.emit('message', { data: JSON.stringify(frame) }); }
  send(body) { assert.equal(this.readyState, 1); this.sent.push(JSON.parse(body)); }
  close(code) { this.closes.push(code); this.readyState = 2; }
  finish(code = 1006) { this.readyState = 3; this.emit('close', { code }); }
}

function fixture() {
  const timers = new Timers();
  const sockets = [];
  const urls = [];
  const statuses = [];
  const logs = [];
  const frames = [];
  let token = 'first-client-token';
  const client = new ReconnectingClient({
    url: 'ws://localhost/ws', token: () => token,
    socket: url => { urls.push(url); const socket = new Socket(); sockets.push(socket); return socket; },
    schedule: timers.schedule, cancel: timers.cancel, random: () => 1,
    status: (message, view) => statuses.push({ message, ...view }),
    log: message => logs.push(message), frame: frame => frames.push(frame),
  });
  return { client, timers, sockets, urls, statuses, logs, frames, setToken: value => { token = value; } };
}

const authenticated = { op: 'authenticated', projectId: 'project-a', userId: 'user-a' };

test('backoff is exponential with bounded jitter and a hard maximum, including overload', () => {
  assert.deepEqual([0, 0.5, 1].map(random => reconnectDelay(0, random)), [250, 375, 500]);
  assert.equal(reconnectDelay(1, 1), 1000);
  assert.equal(reconnectDelay(0, 1, true), 2000);
  // The cap bounds the exponential ceiling before jitter; it must not flatten
  // half of the random inputs to an identical 30-second retry.
  assert.deepEqual([0, 0.2, 0.5, 1].map(random => reconnectDelay(100, random)),
    [15_000, 18_000, 22_500, 30_000]);
  assert.equal(reconnectDelay(100, -1), 15_000);
  assert.equal(reconnectDelay(100, 2), 30_000);
  for (let attempt = 0; attempt < 100; attempt++) {
    for (const random of [0, 0.2, 0.5, 1]) {
      for (const overloaded of [false, true]) {
        const delay = reconnectDelay(attempt, random, overloaded);
        assert.ok(delay >= 250 && delay <= 30_000);
      }
    }
  }
});

test('every reconnect sends current credentials and waits for fresh auth before idempotent joins', () => {
  const f = fixture();
  f.client.connect('room-a');
  const first = f.sockets[0];
  first.open();
  f.client.join('room-a');
  assert.deepEqual(first.sent, [{ op: 'auth', token: 'first-client-token' }]);
  first.frame(authenticated);
  first.frame(authenticated);
  f.client.join('room-a');
  assert.deepEqual(first.sent.slice(1), [{ op: 'join', roomId: 'room-a' }]);
  first.finish(1001);
  f.setToken('fresh-client-token');
  f.timers.advance(499);
  assert.equal(f.sockets.length, 1);
  f.timers.advance(1);
  const second = f.sockets[1];
  second.open();
  assert.deepEqual(second.sent, [{ op: 'auth', token: 'fresh-client-token' }]);
  second.frame(authenticated);
  assert.deepEqual(second.sent[1], { op: 'join', roomId: 'room-a' });
  assert.ok(f.urls.every(url => url === 'ws://localhost/ws'));
  assert.equal(f.logs.join('').includes('client-token'), false);
});

test('leave is idempotent, ordered after a pending join, and not restored after reconnect', () => {
  const f = fixture();
  f.client.connect('room-a');
  const first = f.sockets[0];
  first.open();
  first.frame(authenticated);
  f.client.leave('room-a');
  f.client.leave('room-a');
  assert.deepEqual(first.sent.slice(1), [{ op: 'join', roomId: 'room-a' }, { op: 'leave', roomId: 'room-a' }]);
  f.client.join('room-b');
  first.finish();
  f.client.leave('room-b');
  f.timers.advance(500);
  const second = f.sockets[1];
  second.open();
  second.frame(authenticated);
  assert.deepEqual(second.sent.map(command => command.op), ['auth']);
});

test('manual disconnect cancels retries and stale callbacks cannot restart or mutate new attempts', () => {
  const f = fixture();
  f.client.connect('room-a');
  const old = f.sockets[0];
  old.finish();
  const staleRetry = f.timers.callbacks.get(f.timers.next);
  f.client.disconnect();
  staleRetry();
  f.timers.advance(60_000);
  assert.equal(f.sockets.length, 1);
  f.client.connect('room-b');
  old.open();
  old.frame(authenticated);
  assert.deepEqual(old.sent, []);
  const current = f.sockets[1];
  current.finish();
  staleRetry();
  f.client.disconnect();
  f.timers.advance(60_000);
  assert.equal(f.sockets.length, 2);
  assert.equal(f.timers.tasks.size, 0);
});

test('replacing a connection waits for the old close and ignores stale auth and close events', () => {
  const f = fixture();
  f.client.connect('room-a');
  const old = f.sockets[0];
  old.open();
  f.client.connect('room-b');
  assert.equal(f.sockets.length, 1);
  assert.deepEqual(old.closes, [1000]);
  old.frame(authenticated);
  assert.equal(old.sent.length, 1);
  old.finish(1000);
  assert.equal(f.sockets.length, 2);
  const current = f.sockets[1];
  current.open();
  current.frame(authenticated);
  const before = f.statuses.length;
  old.emit('close', { code: 1008 });
  old.frame({ op: 'error', error: { code: 'unauthorized' } });
  assert.equal(f.statuses.length, before);
  assert.deepEqual(current.sent.map(command => command.op), ['auth', 'join', 'join']);
});

test('policy close and unauthorized error stop retries until a different token is supplied', () => {
  for (const method of ['policy', 'unauthorized']) {
    const f = fixture();
    f.client.connect('room-a');
    const old = f.sockets[0];
    old.open();
    if (method === 'policy') old.finish(1008);
    else {
      old.frame({ op: 'error', error: { code: 'unauthorized' } });
      assert.deepEqual(old.closes, [], 'Client must preserve the server policy close');
      old.finish(1008);
    }
    f.timers.advance(60_000);
    assert.equal(f.statuses.at(-1).active, false, 'Fresh-token Connect must be available after policy close');
    f.client.connect('room-a');
    assert.equal(f.sockets.length, 1);
    assert.match(f.statuses.at(-1).message, /Paste a fresh token/);
    f.setToken('replacement-client-token');
    f.client.connect('room-a');
    assert.equal(f.sockets.length, 2);
    f.sockets[1].open();
    f.sockets[1].frame(authenticated);
    assert.deepEqual(f.sockets[1].sent[1], { op: 'join', roomId: 'room-a' });
  }
});

test('overloaded clients retry more slowly and flapping connections do not reset backoff', () => {
  const f = fixture();
  f.client.connect('room-a');
  f.sockets[0].finish(1013);
  f.timers.advance(1999);
  assert.equal(f.sockets.length, 1);
  f.timers.advance(1);
  f.sockets[1].open();
  f.sockets[1].frame(authenticated);
  f.sockets[1].finish();
  f.timers.advance(999);
  assert.equal(f.sockets.length, 2);
  f.timers.advance(1);
  f.sockets[2].open();
  f.sockets[2].frame(authenticated);
  f.timers.advance(30_000);
  f.sockets[2].finish();
  f.timers.advance(499);
  assert.equal(f.sockets.length, 3);
  f.timers.advance(1);
  assert.equal(f.sockets.length, 4);
});

test('stale stable timers cannot reset a new connection retry or strand its timer', () => {
  const f = fixture();
  f.client.connect('room-a');
  f.sockets[0].open();
  f.sockets[0].frame(authenticated);
  const staleStable = f.timers.callbacks.get(f.timers.next);
  f.sockets[0].finish();
  f.timers.advance(500);
  f.sockets[1].open();
  f.sockets[1].frame(authenticated);
  staleStable();
  f.client.disconnect();
  assert.equal(f.timers.tasks.size, 0);
});

test('demo desired subscriptions stay bounded', () => {
  const f = fixture();
  f.client.connect('room-a');
  f.sockets[0].open();
  f.sockets[0].frame(authenticated);
  for (let i = 0; i < 200; i++) f.client.join('room-' + i);
  f.client.join('x'.repeat(129));
  assert.equal(f.sockets[0].sent.filter(command => command.op === 'join').length, 128);
  assert.match(f.logs.at(-1), /128 characters/);
});
