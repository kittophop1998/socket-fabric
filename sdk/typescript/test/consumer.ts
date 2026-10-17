import { SocketFabricClient, SocketFabricError } from "../src/index.js";
import type { EventEnvelope, ConnectionInfo, ConnectionState } from "../src/index.js";

const client = new SocketFabricClient<{ resourceId: string }>({
  url: "wss://gateway.example/ws",
  tokenProvider: async ({ signal }) => {
    const response = await fetch("/realtime-token", { signal });
    return (await response.json() as { token: string }).token;
  },
});
client.on("event", (event: EventEnvelope<{ resourceId: string }>) => { event.data.resourceId satisfies string; });
client.on("state", (state: ConnectionState) => { state satisfies string; });
client.on("error", (error: SocketFabricError) => { error.code satisfies string; });
client.on("subscribed", room => { room satisfies string; });
client.connect().then((info: ConnectionInfo) => { info.projectId satisfies string; });
client.subscribe("room").then(() => {});
client.unsubscribe("room").then(() => {});
// @ts-expect-error Publishing is intentionally absent from the browser SDK.
client.publish({});
// @ts-expect-error Tokens come from a provider rather than a browser signing secret.
new SocketFabricClient({ url: "wss://gateway.example/ws", secret: "forbidden" });
