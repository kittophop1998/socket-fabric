# External Go backend and TypeScript browser example

This standalone application demonstrates:

```text
Go business HTTP API -> public Go SDK -> SocketFabric / NATS -> TypeScript SDK -> browser
```

Use the [copy-paste quick start](../../docs/QUICKSTART.md) for private configuration,
single/distributed Compose, npm packing/install, building this example, browser
login, CLI debugging and cleanup. The gateway has no dependency on this example
and no Core API changes are required.

## Backend and authorization

`go run . serve` starts a standard-library HTTP server with explicit timeouts,
bounded request bodies, cancellation and SIGINT/SIGTERM shutdown. It binds a
numeric loopback address. Build browser assets before starting it. It serves only
`public/index.html` and `public/app.js`; it never exposes source, modules or ENV.

| Application route | Contract |
| --- | --- |
| `GET /` and `/app.js` | Fixed built browser files |
| `GET /api/config` | Public configured WebSocket URLs only |
| `POST /api/realtime-token` | HTTP Basic login, no client-selected scope; returns a short-lived scoped token |
| `POST /api/resources/{id}/update` | HTTP Basic login, JSON `{roomId,status}`; publishes fixed `resource.updated` |

These are application routes, not SocketFabric Core endpoints. User policies
come from the trusted backend's ENV. The token endpoint derives project/user/
rooms from that policy, and the business API validates resource ID, `updated` or
`completed` status, and room permission before publishing once with the matching
project key. Unknown input fields, multiple JSON values and oversized bodies are
rejected. Resource actions are illustrative; there is no persistence/database.

`EXAMPLE_USERS_JSON` maps usernames to `{password,projectId,rooms}`. Usernames
follow identifier rules without `:` (HTTP Basic separator); passwords must be
32–256 bytes and distinct from Core credentials. Each policy has 1–64 unique
explicit rooms and a project configured in `PUBLISHER_KEYS_JSON`. The server
stores password hashes in memory and compares them in constant time. HTTP Basic
with generated high-entropy credentials is for this bounded local example; use
your application's existing identity service for production users.

| ENV | Default / requirement |
| --- | --- |
| `EXAMPLE_ADDR` | `127.0.0.1:3000`; numeric loopback IP and port |
| `EXAMPLE_ORIGIN` | `http://127.0.0.1:3000`; exact public origin without path |
| `EXAMPLE_PUBLIC_DIR` | `public`; built HTML/JS directory |
| `EXAMPLE_USERS_JSON` | Required server-owned policy; 1–128 users |
| `EXAMPLE_TOKEN_TTL` | `5m`; 1 second through 15 minutes |
| `SOCKETFABRIC_URL` | `http://localhost:8080`; backend publish gateway |
| `SOCKETFABRIC_WS_URLS` | `ws://localhost:8080/ws`; 1–8 comma-separated public gateway URLs |
| `SOCKETFABRIC_TIMEOUT` | `5s`; positive backend publish timeout |
| `PUBLISHER_KEYS_JSON` | Required project-to-publisher-key map, shared with gateway |
| `CLIENT_TOKEN_SECRET` | Required server-only signing secret, shared with gateway |

The process reads ENV directly; `.env.example` is a reference and has intentionally
invalid blank credentials. JSON configurations are limited to 1 MiB each. Asset
files are limited to 8 MiB each; resource requests to 8 KiB and token requests to
1 KiB. Credentials stay in trusted server ENV. Errors/logs never print them or
raw upstream payloads. Origin/Host checks, CSP and `no-store` protect responses.
Cross-origin application API requests are rejected. Production requires HTTPS
and WSS through a trusted TLS proxy; an HTTPS origin requires WSS gateway URLs.

## Browser lifecycle and package consumption

`app.ts` imports `@socketfabric/client` through its public export. Install the
actual npm tarball, then `npm run build` typechecks and bundles with esbuild into
`public/`. Run `npm ci` before installing the undeclared development tarball:

```sh
# From this directory, after packing the SDK as described in the quick start:
npm ci
npm install --ignore-scripts --no-save --no-package-lock \
  ../../bin/npm/socketfabric-client-0.1.0.tgz
npm run build
go run . serve
```

Choose a gateway, sign in and connect. Every connection/reconnect calls the
application token endpoint with the connection's AbortSignal, authenticates and
subscribes. The password field clears immediately; the current Basic credential
and client JWT remain in memory only. There are no cookies or storage writes.
Switching/replacing a session removes listeners and cancels old work. Page exit
and **Disconnect** stop retries, clear room intent and release credentials.

**Subscribe** and **Unsubscribe** are explicit and idempotent. **Refresh token /
reconnect** obtains a fresh token and re-subscribes explicitly. There is no token
refresh timer. Network recovery uses SDK backoff/jitter and restores acknowledged
room intent; policy close `1008`, including expiry, stops retries until explicit
refresh/connect. Two tabs can use the same user on different gateways.

The UI validates the generic resource event at runtime, renders text only and
keeps at most 100 log lines. SDK type parameters do not perform business-data
validation. See [browser SDK reference](../../sdk/typescript/README.md) for the
complete lifecycle and error contract.

## Existing trusted terminal helpers

The Phase 4A token/publish commands remain available. They are trusted developer
helpers, never browser code or an unauthenticated token service:

```sh
# Required server signing secret is read from ENV; prints a scoped client JWT.
SOCKETFABRIC_PROJECT_ID=project-a SOCKETFABRIC_USER_ID=user-42 \
  SOCKETFABRIC_ROOMS=room-a go run . token --ttl 5m

# Required project publisher key is read from PUBLISHER_KEY.
go run . publish --room room-a --type resource.updated \
  --data '{"resourceId":"resource-42","status":"updated"}'
SOCKETFABRIC_DATA_JSON=null go run . publish
```

The publish helper uses `SOCKETFABRIC_URL`, `SOCKETFABRIC_PROJECT_ID`,
`SOCKETFABRIC_ROOM_ID`, `SOCKETFABRIC_EVENT_TYPE`, `SOCKETFABRIC_DATA_JSON` and
optional `--request-id`; flags override matching ENV. It prints the public
`PublishResult`. Treat token output and payloads as sensitive. For ordinary
developer/CI publishing, prefer the [standalone CLI](../../docs/CLI.md).

## External Go module and release status

This directory has its own go.mod, imports only the public `socketfabric/sdk/go`
package and uses a local replacement for checkout development. Root Go checks
do not include this module. The integration runner copies the backend and SDK to
separate temporary directories and builds with a Go workspace, without a consumer
replacement or imports from server internals.

That is local development verification. The canonical Go VCS/module URL and tag
are still missing, so clean public installation without workspace/replacements
is not available. SDK APIs remain unchanged. See [Go SDK installation and error
handling](../../sdk/go/README.md). The npm package remains private, installed as
a `.tgz`; npm scope ownership and license policy are also release blockers. No
packages/binaries are published automatically.

## Checks and delivery limits

```sh
go mod tidy
go fmt ./...
go vet ./...
go test ./...
go test -race ./...
go build -o ../../bin/socketfabric-example .
npm run build
# From the repository root with integration prerequisites installed:
node tests/sdk/run.mjs --cli
```

See [integration coverage](../../tests/sdk/README.md) for actual Docker/NATS and
Chromium testing, independent packed-package consumption and cleanup.

Publication remains best-effort. HTTP 202 confirms API acceptance, not browser
receipt/processing. Local queue counts are not cluster-wide subscriber counts.
503/timeouts may follow remote delivery, so neither backend nor SDK retries a
publish. Reconnect re-authorizes subscriptions; disconnected messages may be
permanently missed. No replay, persistence, deduplication or client ACK is added.
