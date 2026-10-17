export interface EventEnvelope<T = unknown> {
  eventId: string;
  projectId: string;
  type: string;
  roomId: string;
  data: T;
  timestamp: string;
}

export interface ConnectionInfo {
  connectionId: string;
  projectId: string;
  userId: string;
}

export type ConnectionState = "disconnected" | "connecting" | "authenticating" | "connected" | "reconnecting";

export interface ReconnectOptions {
  initialDelayMs?: number;
  overloadDelayMs?: number;
  maxDelayMs?: number;
  stableMs?: number;
}

export interface ClientOptions {
  url: string;
  tokenProvider: (context: { signal: AbortSignal }) => string | Promise<string>;
  connectTimeoutMs?: number;
  operationTimeoutMs?: number;
  reconnect?: ReconnectOptions;
}

/** Neither credentials nor untrusted server messages are included in message. */
export class SocketFabricError extends Error {
  readonly code: string;
  readonly requestId: string | undefined;

  constructor(code: string, message: string, requestId?: string) {
    super(message);
    this.name = "SocketFabricError";
    this.code = code;
    this.requestId = requestId;
  }
}

export interface ClientEvents<T> {
  event: EventEnvelope<T>;
  state: ConnectionState;
  error: SocketFabricError;
  subscribed: string;
  unsubscribed: string;
}
