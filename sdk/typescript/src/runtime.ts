export interface SocketEvents {
  open: object;
  message: { data: unknown };
  close: { code: number };
  error: object;
}

export interface Socket {
  readonly readyState: number;
  send(data: string): void;
  close(code?: number): void;
  addEventListener<K extends keyof SocketEvents>(name: K, listener: (event: SocketEvents[K]) => void): void;
}

// Kept outside the public package exports so tests can control time and sockets.
export interface Runtime {
  socket(url: string): Socket;
  schedule(callback: () => void, delay: number): number;
  cancel(timer: number): void;
  random(): number;
}

export const browserRuntime: Runtime = {
  socket: url => new WebSocket(url),
  schedule: (callback, delay) => globalThis.setTimeout(callback, delay),
  cancel: timer => globalThis.clearTimeout(timer),
  random: () => Math.random(),
};
