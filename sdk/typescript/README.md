# SocketFabric browser client

Browser-native TypeScript client for the existing SocketFabric WebSocket
protocol. ESM and declarations are built together, with no runtime dependencies.
The package is private and has not been published to npm.
The name/version are still provisional: npm ownership, repository identity and
project license must be confirmed before publication. See [release readiness](../../docs/RELEASE_READINESS.md)
for compatibility, packaging gates and manual approval steps. The current
`0.1.0` metadata is unchanged and is not a public release.

Follow the [external application quick start](../../docs/QUICKSTART.md) for a Go
business API and a plain TypeScript browser app using this exact packed package,
with no source aliases or imports from server internals.

```sh
cd sdk/typescript
npm ci
npm run typecheck
npm test
npm pack
# In another project:
npm install /absolute/path/socketfabric-client-0.1.0.tgz
```

The artifact contains `package.json`, this README, and the built `dist/*.js` and
`dist/*.d.ts` files. It exports ESM at `@socketfabric/client` with matching type
declarations; source/tests, ENV, credentials and build tooling are excluded. It
has no runtime dependencies. The example installs the tarball and bundles a bare
`@socketfabric/client` import with esbuild, without React/Next or Node polyfills.
The [integration runner](../../tests/sdk/README.md) installs the actual `.tgz`
into a disposable independent consumer, checks NodeNext typings and SSR-safe
import without a WebSocket global, then runs the bundled app in Chromium.

Packing/installation is not npm publication. Keep `private: true` until the
owning npm scope, license and release policy are decided; no registry ownership
or public installation is claimed.

Node 22 or newer is needed for the package build/tests. The runtime uses standard
browser `WebSocket`, `AbortController`, `TextEncoder`, and timers; importing it
does not require a browser or open a socket. There is no Node WebSocket adapter.

```ts
import { SocketFabricClient } from '@socketfabric/client';

const client = new SocketFabricClient<{ resourceId: string }>({
  url: 'wss://realtime.example/ws',
  tokenProvider: async ({ signal }) => {
    // Your authenticated backend checks the current user's project/room access
    // before issuing a short-lived token. Fetch runs again on every connection.
    const response = await fetch('/api/realtime-token', {
      method: 'POST', credentials: 'same-origin', signal,
    });
    if (!response.ok) throw new Error('Token request failed');
    const { token } = await response.json();
    return token;
  },
});

const off = client.on('event', event => {
  console.log(event.type, event.data.resourceId);
});
client.on('error', error => {
  if (error.code === 'unauthorized') {
    // Refresh your application's login/token access, then call connect again.
  }
});
client.on('state', state => console.log(state));

await client.connect();
await client.subscribe('resource-123');
// Later:
await client.unsubscribe('resource-123');
off();
client.disconnect();
```

Never put publisher keys or token signing secrets in the browser. This client
accepts only a short-lived client token from an application-controlled provider.
Token URLs, logging, local storage, signing and event publishing are absent.

## API

- `new SocketFabricClient<T = unknown>(options)` accepts `url`, `tokenProvider`,
  optional `connectTimeoutMs`, `operationTimeoutMs`, and `reconnect` settings.
  The URL must use `ws:`/`wss:` without credentials, query or fragment.
- `connect({ signal? })` resolves with `{ connectionId, projectId, userId }` after
  authentication. Calls during the same attempt coalesce. The initiating caller's
  abort cancels the attempt; a later coalesced caller's abort cancels only its wait.
  The signal is detached after success. A failed initial attempt never retries.
- `subscribe(roomId)` resolves after `joined`; repeated equivalent operations are
  idempotent. New subscriptions require an authenticated connection.
- `unsubscribe(roomId)` resolves after `left`, or removes remembered room intent
  immediately when offline. A pending unsubscribe is never restored after loss.
- `disconnect()` cancels timers, aborts token work, rejects pending operations,
  clears room intent, and requests a normal WebSocket close. It is idempotent.
- `on(name, listener)` returns a listener-removal function. Names are `event`
  (`EventEnvelope<T>`), `state` (`ConnectionState`), `error` (`SocketFabricError`),
  `subscribed` (room ID), and `unsubscribed` (room ID). Listener exceptions are
  isolated from the connection lifecycle.

`EventEnvelope<T>` retains `eventId`, `projectId`, `type`, `roomId`, `data`, and
`timestamp`. Timestamps remain the exact wire string, including nanoseconds.
`T` supplies compile-time payload typing only: validate application data with
your own schema before using it. Runtime checks validate protocol structure,
identifiers, timestamps and authenticated project scope. Malformed frames reset
the connection; incoming frames have a bounded 1 MiB UTF-8 limit.

## Reconnect and failure semantics

After the first successful authentication, unexpected disconnects reconnect with
fresh credentials, then independently re-authorize only rooms previously
acknowledged by the server. An unacknowledged join is rejected and forgotten.
Revoked rooms emit `error` and are removed from recovery while other room joins
continue. `connect()` resolves at authentication, before recovery joins complete;
observe `subscribed` for recovered-room acknowledgement.
If fresh authentication changes the project or user, remembered room intent is
cleared rather than carrying subscriptions from the previous identity.

Close code `1008` and `unauthorized` are terminal: automatic retries stop until
the application calls `connect()` explicitly. This lets the application repair
login/token access. Network loss, gateway shutdown, and overload retry safely.
Each connection attempt has a 10-second total budget covering the token provider,
previous socket closure, handshake and authentication. Token providers should
respect the supplied signal; late results are ignored even if they do not.
Only one token-provider invocation may be unresolved per client. A provider that
ignores abort blocks fresh token acquisition until it settles; retries still
time out without creating more provider calls or an accumulating waiter list.
Only one socket may remain active. A closing socket that never completes prevents
a replacement socket and causes that attempt to time out.

Defaults (all durations are positive integer milliseconds):

| Option | Default |
| --- | ---: |
| `connectTimeoutMs` | 10000 |
| `operationTimeoutMs` | 10000 |
| `reconnect.initialDelayMs` | 500 |
| `reconnect.overloadDelayMs` | 2000 |
| `reconnect.maxDelayMs` | 30000 |
| `reconnect.stableMs` | 30000 |

Retry delays use exponential backoff with independent 50–100% jitter below the
maximum; code `1013` uses the overload base. The retry counter resets only after
the stable duration. Duration options must not exceed the timer limit
2147483647; maximum delay must cover both initial bases.

Membership work is bounded to 64 rooms and 64 operations, with one command in
flight because server error frames lack an operation ID. An acknowledgement
timeout rejects pending operations and resets the connection before issuing any
more commands. There is no offline command or event buffer.

Errors are `SocketFabricError` instances with a `code`, safe `message` and
optional `requestId`. Server message text and raw token/provider errors are not
included. SDK error codes include `invalid_options`, `not_connected`,
`invalid_room`, `room_limit`, `operation_limit`, `connect_timeout`,
`operation_timeout`, `connection_lost`, `connection_failed`, `invalid_frame`,
`token_provider_failed`, `invalid_token`, `canceled`, and `disconnected`.
Server rejection codes such as `forbidden` and `unauthorized` are preserved.
Handle promise rejections as well as observing `error` for connection/recovery
failures.

Delivery remains best-effort. Events while disconnected or not yet re-subscribed
may be permanently missed. There is no replay, deduplication or guaranteed
delivery. An event already queued by the server can arrive after unsubscribe.
The SDK does not reinterpret browser event receipt as an application ACK.

## Tests

```sh
npm run build
npm run typecheck
npm test
```

Deterministic socket/timer tests cover fresh authentication, bounded serialized
subscriptions, idempotency, scoped frame validation, cancellation, stale
callbacks, backoff, policy close, partial recovery and ACK-timeout resets.
