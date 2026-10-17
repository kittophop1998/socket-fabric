import { Client } from "./client.js";
import { browserRuntime } from "./runtime.js";
import type { ClientOptions } from "./types.js";

export { SocketFabricError } from "./types.js";
export type { ClientOptions, ClientEvents, ConnectionInfo, ConnectionState, EventEnvelope, ReconnectOptions } from "./types.js";

/** Browser-native, best-effort client. Importing this module never opens a socket. */
export class SocketFabricClient<T = unknown> extends Client<T> {
  constructor(options: ClientOptions) {
    super(options, browserRuntime);
  }
}
