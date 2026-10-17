import { SocketFabricClient, SocketFabricError } from '@socketfabric/client';
import type { ConnectionState, EventEnvelope } from '@socketfabric/client';

type Resource = { resourceId: string; status: 'updated' | 'completed' };
type TokenResponse = { token: string; projectId: string; userId: string; rooms: string[]; expiresIn: number };
type Active = { client: SocketFabricClient<Resource>; authorization: string; off: Array<() => void> };
class ApplicationError extends Error {
  constructor(readonly code: string) { super('Application request failed'); }
}
const element = <T extends HTMLElement>(id: string): T => document.getElementById(id) as T;
const username = element<HTMLInputElement>('username');
const password = element<HTMLInputElement>('password');
const gateway = element<HTMLSelectElement>('gateway');
const room = element<HTMLInputElement>('room');
const resource = element<HTMLInputElement>('resource');
const status = element<HTMLSelectElement>('resource-status');
const connect = element<HTMLButtonElement>('connect');
const refresh = element<HTMLButtonElement>('refresh');
const subscribe = element<HTMLButtonElement>('subscribe');
const unsubscribe = element<HTMLButtonElement>('unsubscribe');
const disconnect = element<HTMLButtonElement>('disconnect');
const publish = element<HTMLButtonElement>('publish');
const events = element<HTMLPreElement>('events');
const lines: string[] = [];
const lifetime = new AbortController();
let active: Active | undefined;
let businessRequest: AbortController | undefined;
let generation = 0;
let refreshing = false;

function log(value: unknown): void {
  lines.push(JSON.stringify(value));
  if (lines.length > 100) lines.shift();
  events.textContent = lines.join('\n');
  events.scrollTop = events.scrollHeight;
}

function report(error: unknown): void {
  const code = error instanceof SocketFabricError || error instanceof ApplicationError ? error.code : 'application_request_failed';
  log({ error: code });
}

function updateState(value: ConnectionState): void {
  element('state').textContent = value;
  subscribe.disabled = value !== 'connected';
  unsubscribe.disabled = !active;
  refresh.disabled = !active || refreshing || value === 'connecting' || value === 'authenticating';
  disconnect.disabled = !active;
  publish.disabled = !active || Boolean(businessRequest);
}

async function jsonRequest(path: string, init: RequestInit, signal: AbortSignal): Promise<unknown> {
  const response = await fetch(path, { ...init, credentials: 'omit', cache: 'no-store', signal: AbortSignal.any([signal, lifetime.signal, AbortSignal.timeout(5000)]) });
  if (!response.ok) throw new ApplicationError('http_' + response.status);
  return response.json();
}

function isToken(value: unknown): value is TokenResponse {
  if (!value || typeof value !== 'object') return false;
  const token = value as Partial<TokenResponse>;
  return typeof token.token === 'string' && typeof token.projectId === 'string' && typeof token.userId === 'string' && Array.isArray(token.rooms) && token.rooms.every(room => typeof room === 'string') && typeof token.expiresIn === 'number';
}

function renderEvent(event: EventEnvelope<Resource>): void {
  const data: unknown = event.data;
  if (event.type !== 'resource.updated' || !data || typeof data !== 'object') return;
  const value = data as Partial<Resource>;
  if (typeof value.resourceId !== 'string' || !/^[A-Za-z0-9._:-]{1,128}$/.test(value.resourceId) || (value.status !== 'updated' && value.status !== 'completed')) return;
  element('resource-state').textContent = `${value.resourceId}: ${value.status}`;
  log({ eventId: event.eventId, projectId: event.projectId, roomId: event.roomId, type: event.type, data: { resourceId: value.resourceId, status: value.status } });
}

function cleanup(): void {
  ++generation;
  refreshing = false;
  businessRequest?.abort();
  businessRequest = undefined;
  const previous = active;
  active = undefined;
  previous?.off.forEach(off => off());
  previous?.client.disconnect();
}

async function start(authorization: string, url: string): Promise<void> {
  cleanup();
  const current = generation;
  connect.disabled = true;
  const client = new SocketFabricClient<Resource>({
    url,
    tokenProvider: async ({ signal }) => {
      const value = await jsonRequest('/api/realtime-token', { method: 'POST', headers: { Authorization: authorization } }, signal);
      if (!isToken(value)) throw new ApplicationError('invalid_token_response');
      if (current === generation) {
        element('identity').textContent = `${value.userId} / ${value.projectId}; rooms: ${value.rooms.join(', ')}; token TTL: ${value.expiresIn}s`;
        log({ tokenRefreshed: true, projectId: value.projectId });
      }
      return value.token;
    },
  });
  const off = [
    client.on('state', state => { if (current === generation) updateState(state); }),
    client.on('event', event => { if (current === generation) renderEvent(event); }),
    client.on('subscribed', roomId => { if (current === generation) log({ subscribed: roomId }); }),
    client.on('unsubscribed', roomId => { if (current === generation) log({ unsubscribed: roomId }); }),
    client.on('error', error => { if (current === generation) report(error); }),
  ];
  active = { client, authorization, off };
  updateState('connecting');
  try {
    await client.connect();
    if (current === generation) await client.subscribe(room.value);
  } catch (error) {
    if (current === generation) report(error);
  } finally {
    if (current === generation) connect.disabled = false;
  }
}

connect.addEventListener('click', () => {
  if (!username.value || !password.value || !gateway.value) { report(new ApplicationError('credentials_required')); return; }
  const credentials = new TextEncoder().encode(username.value + ':' + password.value);
  const authorization = 'Basic ' + btoa(String.fromCharCode(...credentials));
  password.value = '';
  void start(authorization, gateway.value);
});
refresh.addEventListener('click', async () => {
  if (!active || refreshing) return;
  const previous = active;
  const current = generation;
  refreshing = true;
  refresh.disabled = true;
  // Reuse this instance so its connection budget includes actual socket closure.
  previous.client.disconnect();
  try {
    await previous.client.connect();
    if (current === generation) await previous.client.subscribe(room.value);
  } catch (error) {
    if (current === generation) report(error);
  } finally {
    if (current === generation) {
      refreshing = false;
      refresh.disabled = !active;
    }
  }
});
subscribe.addEventListener('click', () => { active?.client.subscribe(room.value).catch(report); });
unsubscribe.addEventListener('click', () => { active?.client.unsubscribe(room.value).catch(report); });
disconnect.addEventListener('click', () => {
  cleanup();
  connect.disabled = false;
  password.value = '';
  element('identity').textContent = '';
  updateState('disconnected');
});
publish.addEventListener('click', async () => {
  if (!active || businessRequest) return;
  const current = generation;
  const controller = new AbortController();
  businessRequest = controller;
  publish.disabled = true;
  try {
    const value = await jsonRequest('/api/resources/' + encodeURIComponent(resource.value) + '/update', {
      method: 'POST', headers: { Authorization: active.authorization, 'Content-Type': 'application/json' },
      body: JSON.stringify({ roomId: room.value, status: status.value }),
    }, controller.signal);
    if (current === generation && value && typeof value === 'object') {
      const result = value as { event?: { eventId?: unknown }; queuedConnections?: unknown };
      log({ accepted: true, eventId: typeof result.event?.eventId === 'string' ? result.event.eventId : undefined, localQueues: result.queuedConnections, deliveryConfirmed: false });
    }
  } catch (error) {
    if (current === generation && !controller.signal.aborted) report(error);
  } finally {
    if (businessRequest === controller) businessRequest = undefined;
    if (current === generation) publish.disabled = !active;
  }
});

window.addEventListener('pagehide', () => { cleanup(); lifetime.abort(); password.value = ''; });
void (async () => {
  try {
    const value = await jsonRequest('/api/config', {}, lifetime.signal) as { gateways?: unknown };
    if (!Array.isArray(value.gateways) || value.gateways.length === 0 || value.gateways.some(url => typeof url !== 'string')) throw new ApplicationError('invalid_config');
    for (const [index, url] of (value.gateways as string[]).entries()) {
      const option = document.createElement('option');
      option.value = url;
      option.textContent = `Gateway ${index + 1}: ${url}`;
      gateway.append(option);
    }
    connect.disabled = false;
  } catch (error) { report(error); }
})();
