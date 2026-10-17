import type { Runtime, Socket } from "./runtime.js";
import { SocketFabricError } from "./types.js";
import type { ClientEvents, ClientOptions, ConnectionInfo, ConnectionState, EventEnvelope } from "./types.js";

const maxRooms = 64;
const maxOperations = 64;
const maxFrameBytes = 1024 * 1024;
const validID = (value: unknown): value is string => typeof value === "string" && /^[A-Za-z0-9._:-]{1,128}$/.test(value);
const object = (value: unknown): value is Record<string, unknown> => Boolean(value) && typeof value === "object" && !Array.isArray(value);
const exactKeys = (value: Record<string, unknown>, keys: string[]) => Object.keys(value).length === keys.length && keys.every(key => Object.hasOwn(value, key));
const failure = (code: string, message: string) => new SocketFabricError(code, message);

type Deferred<T> = { promise: Promise<T>; resolve: (value: T) => void; reject: (error: SocketFabricError) => void };
function deferred<T>(): Deferred<T> {
  let resolve!: Deferred<T>["resolve"];
  let reject!: Deferred<T>["reject"];
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

type Attempt = Deferred<ConnectionInfo> & {
  generation: number;
  controller: AbortController;
  timer: number;
  automatic: boolean;
  token?: string;
  removeAbort?: () => void;
};
type Operation = Deferred<void> & { kind: "join" | "leave"; room: string; timer?: number };
type Transport = { socket: Socket; generation: number };
type Frame =
  | ({ op: "authenticated" } & ConnectionInfo)
  | { op: "joined" | "left"; roomId: string }
  | { op: "error"; error: { code: string; message: string; requestId: string } }
  | { op: "event"; event: EventEnvelope };

function frameFrom(data: unknown): Frame {
  if (typeof data !== "string" || data.length > maxFrameBytes || new TextEncoder().encode(data).length > maxFrameBytes) throw failure("invalid_frame", "Invalid server frame");
  let value: unknown;
  try { value = JSON.parse(data); } catch { throw failure("invalid_frame", "Invalid server frame"); }
  if (!object(value)) throw failure("invalid_frame", "Invalid server frame");
  if (value.op === "authenticated" && exactKeys(value, ["op", "connectionId", "projectId", "userId"]) && validID(value.connectionId) && validID(value.projectId) && validID(value.userId)) return value as Frame;
  if ((value.op === "joined" || value.op === "left") && exactKeys(value, ["op", "roomId"]) && validID(value.roomId)) return value as Frame;
  if (value.op === "error" && exactKeys(value, ["op", "error"]) && object(value.error) && exactKeys(value.error, ["code", "message", "requestId"]) && validID(value.error.code) && typeof value.error.message === "string" && value.error.message.length <= 1024 && validID(value.error.requestId)) return value as Frame;
  if (value.op === "event" && exactKeys(value, ["op", "event"]) && object(value.event)) {
    const event = value.event;
    if (exactKeys(event, ["eventId", "projectId", "type", "roomId", "data", "timestamp"]) && validID(event.eventId) && validID(event.projectId) && validID(event.type) && validID(event.roomId) && typeof event.timestamp === "string" && event.timestamp.length <= 64 && /^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d{1,9})?(?:Z|[+-]\d\d:\d\d)$/.test(event.timestamp) && Number.isFinite(Date.parse(event.timestamp))) return value as Frame;
  }
  throw failure("invalid_frame", "Invalid server frame");
}

/** Internal implementation: public construction uses only ClientOptions. */
export class Client<T = unknown> {
  private readonly url: string;
  private readonly tokenProvider: ClientOptions["tokenProvider"];
  private readonly connectTimeout: number;
  private readonly operationTimeout: number;
  private readonly reconnect: { initialDelayMs: number; overloadDelayMs: number; maxDelayMs: number; stableMs: number };
  private readonly listeners = new Map<keyof ClientEvents<T>, Set<(value: never) => void>>();
  private readonly desired = new Set<string>();
  private readonly joined = new Set<string>();
  private operations: Operation[] = [];
  private inFlight: Operation | undefined;
  private transport: Transport | undefined;
  private attempt: Attempt | undefined;
  private tokenWork: Attempt | undefined;
  private info: ConnectionInfo | undefined;
  private identity: { projectId: string; userId: string } | undefined;
  private retry: number | undefined;
  private stable: number | undefined;
  private generation = 0;
  private retries = 0;
  private enabled = false;
  private everAuthenticated = false;
  private currentState: ConnectionState = "disconnected";

  constructor(options: ClientOptions, private readonly runtime: Runtime) {
    let url: URL;
    try { url = new URL(options.url); } catch { throw failure("invalid_options", "A valid WebSocket URL is required"); }
    if (!["ws:", "wss:"].includes(url.protocol) || url.username || url.password || url.hash || url.search) throw failure("invalid_options", "WebSocket URL must use ws or wss without credentials, query or fragment");
    if (typeof options.tokenProvider !== "function") throw failure("invalid_options", "A token provider is required");
    this.url = url.href;
    this.tokenProvider = options.tokenProvider;
    this.connectTimeout = this.duration(options.connectTimeoutMs ?? 10_000);
    this.operationTimeout = this.duration(options.operationTimeoutMs ?? 10_000);
    this.reconnect = {
      initialDelayMs: this.duration(options.reconnect?.initialDelayMs ?? 500),
      overloadDelayMs: this.duration(options.reconnect?.overloadDelayMs ?? 2_000),
      maxDelayMs: this.duration(options.reconnect?.maxDelayMs ?? 30_000),
      stableMs: this.duration(options.reconnect?.stableMs ?? 30_000),
    };
    if (this.reconnect.maxDelayMs < Math.max(this.reconnect.initialDelayMs, this.reconnect.overloadDelayMs)) throw failure("invalid_options", "Reconnect maximum must cover initial and overload delays");
  }

  on<K extends keyof ClientEvents<T>>(name: K, listener: (value: ClientEvents<T>[K]) => void): () => void {
    let listeners = this.listeners.get(name);
    if (!listeners) { listeners = new Set(); this.listeners.set(name, listeners); }
    const callback = listener as (value: never) => void;
    listeners.add(callback);
    return () => { listeners.delete(callback); };
  }

  connect(options: { signal?: AbortSignal } = {}): Promise<ConnectionInfo> {
    if (options.signal?.aborted) return Promise.reject(failure("canceled", "Connection canceled"));
    if (this.info) return Promise.resolve({ ...this.info });
    if (this.attempt) return this.waitForAttempt(this.attempt.promise, options.signal);
    this.cancelRetry();
    this.enabled = true;
    this.retries = 0;
    const attempt = this.beginAttempt(false);
    if (options.signal && this.isCurrentAttempt(attempt)) {
      const signal = options.signal;
      const cancel = () => { if (this.attempt === attempt) this.stop(failure("canceled", "Connection canceled"), false); };
      signal.addEventListener("abort", cancel, { once: true });
      attempt.removeAbort = () => signal.removeEventListener("abort", cancel);
      if (signal.aborted) cancel();
    }
    return attempt.promise;
  }

  subscribe(roomId: string): Promise<void> {
    if (!validID(roomId)) return Promise.reject(failure("invalid_room", "Invalid room ID"));
    if (!this.info) return Promise.reject(failure("not_connected", "Connect before subscribing"));
    const last = this.lastOperation(roomId);
    if (last?.kind === "join") return last.promise;
    if (!last && this.joined.has(roomId)) return Promise.resolve();
    const rooms = new Set([...this.desired, ...this.joined, ...this.operations.filter(op => op.kind === "join").map(op => op.room)]);
    if (this.inFlight?.kind === "join") rooms.add(this.inFlight.room);
    if (rooms.size >= maxRooms && !rooms.has(roomId)) return Promise.reject(failure("room_limit", "Subscription limit reached"));
    return this.enqueue("join", roomId);
  }

  unsubscribe(roomId: string): Promise<void> {
    if (!validID(roomId)) return Promise.reject(failure("invalid_room", "Invalid room ID"));
    if (!this.info) { this.desired.delete(roomId); return Promise.resolve(); }
    const last = this.lastOperation(roomId);
    if (last?.kind === "leave") return last.promise;
    if (!last && !this.joined.has(roomId)) return Promise.resolve();
    if (this.operations.length + Number(Boolean(this.inFlight)) >= maxOperations) return Promise.reject(failure("operation_limit", "Subscription operation limit reached"));
    this.desired.delete(roomId);
    return this.enqueue("leave", roomId);
  }

  disconnect(): void {
    this.stop(failure("disconnected", "Client disconnected"), true);
  }

  private duration(value: number): number {
    if (!Number.isSafeInteger(value) || value <= 0 || value > 2_147_483_647) throw failure("invalid_options", "Timeouts and reconnect delays must be positive integer milliseconds");
    return value;
  }

  private isCurrentAttempt(attempt: Attempt): boolean { return this.attempt === attempt; }

  private emit<K extends keyof ClientEvents<T>>(name: K, value: ClientEvents<T>[K]): void {
    for (const callback of [...(this.listeners.get(name) ?? [])]) {
      try { callback(value as never); } catch { /* Application callbacks cannot corrupt connection state. */ }
    }
  }

  private state(state: ConnectionState): void {
    if (state === this.currentState) return;
    this.currentState = state;
    this.emit("state", state);
  }

  private beginAttempt(automatic: boolean): Attempt {
    const attempt: Attempt = {
      ...deferred<ConnectionInfo>(), generation: ++this.generation, controller: new AbortController(),
      timer: 0, automatic,
    };
    this.attempt = attempt;
    attempt.timer = this.runtime.schedule(() => {
      if (this.attempt === attempt) this.failAttempt(failure("connect_timeout", "Connection timed out"));
    }, this.connectTimeout);
    // Automatic attempts have no caller, but their failures still emit errors.
    if (automatic) void attempt.promise.catch(() => {});
    this.state(automatic ? "reconnecting" : "connecting");
    if (this.attempt !== attempt) return attempt;
    this.closeTransport();
    void this.fetchToken(attempt);
    return attempt;
  }

  private async fetchToken(attempt: Attempt): Promise<void> {
    // A provider can ignore abort. Never accumulate unresolved invocations or a
    // waiter list across retries: let the current attempt retain its own budget
    // and start its fresh provider only after the old invocation settles.
    if (this.tokenWork) return;
    this.tokenWork = attempt;
    try {
      let token: unknown;
      try { token = await this.tokenProvider({ signal: attempt.controller.signal }); }
      catch {
        if (this.attempt === attempt) this.failAttempt(failure("token_provider_failed", "Token provider failed"));
        return;
      }
      if (this.attempt !== attempt || !this.enabled) return;
      if (typeof token !== "string" || !token || token.length > 16 * 1024 || /\s/.test(token)) {
        this.failAttempt(failure("invalid_token", "Token provider returned an invalid token"));
        return;
      }
      attempt.token = token;
      this.dial(attempt);
    } finally {
      if (this.tokenWork === attempt) this.tokenWork = undefined;
      const next = this.attempt;
      if (next && next !== attempt && this.enabled) void this.fetchToken(next);
    }
  }

  private dial(attempt: Attempt): void {
    if (this.attempt !== attempt || !attempt.token || !this.enabled) return;
    if (this.transport && this.transport.socket.readyState !== 3) return;
    let socket: Socket;
    try { socket = this.runtime.socket(this.url); }
    catch { this.failAttempt(failure("connection_failed", "Unable to open WebSocket")); return; }
    const transport = { socket, generation: attempt.generation };
    this.transport = transport;
    const current = () => this.enabled && this.transport === transport && transport.generation === this.generation;
    socket.addEventListener("open", () => {
      if (!current() || this.attempt !== attempt) return;
      this.state("authenticating");
      if (current()) this.send({ op: "auth", token: attempt.token });
      delete attempt.token;
    });
    socket.addEventListener("message", event => {
      if (!current()) return;
      try { this.handleFrame(frameFrom(event.data)); }
      catch (error) { this.reset(error instanceof SocketFabricError ? error : failure("invalid_frame", "Invalid server frame")); }
    });
    // Native WebSocket error carries no safe, useful server detail. Close or the
    // attempt deadline drives recovery, avoiding duplicate error notifications.
    socket.addEventListener("error", () => {});
    socket.addEventListener("close", event => this.closed(transport, event.code));
  }

  private handleFrame(frame: Frame): void {
    if (frame.op === "authenticated") {
      const attempt = this.attempt;
      if (!attempt || this.info) throw failure("invalid_frame", "Unexpected authentication frame");
      if (this.identity && (this.identity.projectId !== frame.projectId || this.identity.userId !== frame.userId)) this.desired.clear();
      this.identity = { projectId: frame.projectId, userId: frame.userId };
      this.info = { connectionId: frame.connectionId, projectId: frame.projectId, userId: frame.userId };
      this.everAuthenticated = true;
      this.finishAttempt(attempt);
      attempt.resolve({ ...this.info });
      const generation = this.generation;
      const stable = this.runtime.schedule(() => {
        if (this.stable !== stable || generation !== this.generation) return;
        this.stable = undefined;
        if (this.info) this.retries = 0;
      }, this.reconnect.stableMs);
      this.stable = stable;
      // Re-authorize every remembered room through the normal serialized path.
      for (const room of this.desired) void this.enqueue("join", room).catch(() => {});
      this.state("connected");
      return;
    }
    if (frame.op === "error") {
      const error = new SocketFabricError(frame.error.code, "Server rejected the operation", frame.error.requestId);
      if (frame.error.code === "unauthorized") {
        // The gateway follows this frame with its policy close. Preserve 1008
        // rather than replacing it with a client-initiated normal close.
        this.stop(error, false, false);
        this.emit("error", error);
        return;
      }
      if (!this.info) { this.failAttempt(error); return; }
      if (!this.inFlight) throw failure("invalid_frame", "Unexpected server error");
      const operation = this.takeInFlight();
      if (operation.kind === "join") this.desired.delete(operation.room);
      operation.reject(error);
      this.emit("error", error);
      this.pump();
      return;
    }
    if (!this.info) throw failure("invalid_frame", "Server frame preceded authentication");
    if (frame.op === "event") {
      if (frame.event.projectId !== this.info.projectId) throw failure("invalid_frame", "Event project differs from authenticated project");
      this.emit("event", frame.event as EventEnvelope<T>);
      return;
    }
    const operation = this.inFlight;
    if (!operation || operation.room !== frame.roomId || (operation.kind === "join" ? "joined" : "left") !== frame.op) throw failure("invalid_frame", "Unexpected subscription acknowledgement");
    this.takeInFlight();
    if (operation.kind === "join") {
      this.joined.add(operation.room);
      if (!this.operations.some(op => op.room === operation.room && op.kind === "leave")) this.desired.add(operation.room);
      this.emit("subscribed", operation.room);
    } else {
      this.joined.delete(operation.room);
      this.desired.delete(operation.room);
      this.emit("unsubscribed", operation.room);
    }
    operation.resolve();
    this.pump();
  }

  private finishAttempt(attempt: Attempt): void {
    this.runtime.cancel(attempt.timer);
    attempt.removeAbort?.();
    delete attempt.token;
    if (this.attempt === attempt) this.attempt = undefined;
  }

  private failAttempt(error: SocketFabricError): void {
    const attempt = this.attempt;
    if (!attempt) return;
    this.finishAttempt(attempt);
    attempt.reject(error);
    this.info = undefined;
    this.joined.clear();
    if (!attempt.automatic) this.enabled = false;
    this.closeTransport();
    attempt.controller.abort();
    if (this.generation !== attempt.generation) return;
    this.emit("error", error);
    if (this.generation !== attempt.generation) return;
    if (this.enabled && this.everAuthenticated && !this.transport) this.scheduleRetry(false);
    else if (!this.enabled) this.state("disconnected");
  }

  private closed(transport: Transport, code: number): void {
    if (this.transport !== transport) return;
    this.transport = undefined;
    this.info = undefined;
    this.joined.clear();
    this.cancelStable();
    const attempt = this.attempt;
    if (attempt && attempt.generation !== transport.generation) { this.dial(attempt); return; }
    if (code === 1008 && this.enabled) {
      const error = failure("unauthorized", "Connection rejected or authentication expired");
      this.stop(error, false);
      this.emit("error", error);
      return;
    }
    this.rejectOperations(failure("connection_lost", "Connection closed before acknowledgement"));
    if (attempt) { this.failAttempt(failure("connection_lost", "Connection closed before authentication")); return; }
    if (this.enabled && this.everAuthenticated) this.scheduleRetry(code === 1013);
    else this.state("disconnected");
  }

  private scheduleRetry(overloaded: boolean): void {
    if (!this.enabled || this.retry !== undefined || this.attempt || this.transport) return;
    const base = overloaded ? this.reconnect.overloadDelayMs : this.reconnect.initialDelayMs;
    const ceiling = Math.min(this.reconnect.maxDelayMs, base * 2 ** Math.min(this.retries++, 30));
    const random = Math.max(0, Math.min(1, this.runtime.random()));
    const generation = this.generation;
    const timer = this.runtime.schedule(() => {
      if (this.retry !== timer || generation !== this.generation || !this.enabled) return;
      this.retry = undefined;
      this.beginAttempt(true);
    }, Math.round(ceiling * (0.5 + random * 0.5)));
    this.retry = timer;
    this.state("reconnecting");
  }

  private reset(error: SocketFabricError): void {
    if (this.attempt) { this.failAttempt(error); return; }
    this.info = undefined;
    this.joined.clear();
    const generation = ++this.generation;
    this.cancelStable();
    this.rejectOperations(error);
    this.closeTransport();
    this.emit("error", error);
    if (this.generation !== generation) return;
    if (this.enabled) {
      if (!this.transport) this.scheduleRetry(false);
      else this.state("reconnecting");
    }
  }

  private stop(error: SocketFabricError, clearRooms: boolean, closeSocket = true): void {
    this.enabled = false;
    const generation = ++this.generation;
    this.info = undefined;
    this.joined.clear();
    this.cancelRetry();
    this.cancelStable();
    const attempt = this.attempt;
    if (attempt) { this.finishAttempt(attempt); attempt.reject(error); }
    this.rejectOperations(error);
    if (clearRooms) { this.desired.clear(); this.everAuthenticated = false; this.identity = undefined; }
    if (closeSocket) this.closeTransport();
    attempt?.controller.abort();
    if (this.generation === generation) this.state("disconnected");
  }

  private closeTransport(): void {
    const socket = this.transport?.socket;
    if (!socket) return;
    if (socket.readyState === 3) { this.transport = undefined; return; }
    if (socket.readyState >= 2) return;
    try { socket.close(1000); } catch { /* Wait for close or the next attempt deadline. */ }
  }

  private send(command: Record<string, unknown>): boolean {
    if (!this.transport || this.transport.socket.readyState !== 1 || !this.enabled) return false;
    try { this.transport.socket.send(JSON.stringify(command)); return true; }
    catch { this.reset(failure("connection_lost", "Unable to write to WebSocket")); return false; }
  }

  private enqueue(kind: Operation["kind"], room: string): Promise<void> {
    if (this.operations.length + Number(Boolean(this.inFlight)) >= maxOperations) return Promise.reject(failure("operation_limit", "Subscription operation limit reached"));
    const operation = { ...deferred<void>(), kind, room };
    this.operations.push(operation);
    this.pump();
    return operation.promise;
  }

  private pump(): void {
    if (this.inFlight || !this.info || !this.enabled) return;
    const operation = this.operations.shift();
    if (!operation) return;
    this.inFlight = operation;
    operation.timer = this.runtime.schedule(() => {
      if (this.inFlight === operation) this.reset(failure("operation_timeout", "Subscription acknowledgement timed out"));
    }, this.operationTimeout);
    this.send({ op: operation.kind, roomId: operation.room });
  }

  private lastOperation(room: string): Operation | undefined {
    return [...this.operations].reverse().find(op => op.room === room) ?? (this.inFlight?.room === room ? this.inFlight : undefined);
  }

  private takeInFlight(): Operation {
    const operation = this.inFlight!;
    this.inFlight = undefined;
    if (operation.timer !== undefined) this.runtime.cancel(operation.timer);
    return operation;
  }

  private rejectOperations(error: SocketFabricError): void {
    if (this.inFlight) this.takeInFlight().reject(error);
    for (const operation of this.operations.splice(0)) operation.reject(error);
  }

  private cancelStable(): void {
    if (this.stable !== undefined) this.runtime.cancel(this.stable);
    this.stable = undefined;
  }

  private cancelRetry(): void {
    if (this.retry !== undefined) this.runtime.cancel(this.retry);
    this.retry = undefined;
  }

  private waitForAttempt(promise: Promise<ConnectionInfo>, signal?: AbortSignal): Promise<ConnectionInfo> {
    if (!signal) return promise;
    return new Promise((resolve, reject) => {
      const abort = () => reject(failure("canceled", "Connection canceled"));
      signal.addEventListener("abort", abort, { once: true });
      void promise.then(resolve, reject).finally(() => signal.removeEventListener("abort", abort));
      if (signal.aborted) abort();
    });
  }
}
