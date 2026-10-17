# SocketFabric

SocketFabric is a self-hosted realtime service for application backends. Authenticated clients subscribe to project rooms and trusted backends publish events over HTTP. Optional NATS Core fan-out connects gateways. Independent Go/TypeScript SDKs, a developer CLI and the Phase 4C external application example preserve the single-node default, core engine, and existing APIs.

Phase 5A adds an opt-in PostgreSQL control plane: operator Bearer sessions,
Admin/Viewer permissions, project/key lifecycle, transactional audit logs and
local-node diagnostics. Management is disabled by default. See the
[Management setup/API/security reference](docs/MANAGEMENT.md) and
[implementation evidence](docs/PHASE_5A_PLAN.md). No dashboard is included.

Events use best-effort delivery. A successful publish confirms server acceptance and local queueing, not browser receipt or processing. There is no event persistence, replay, offline inbox, Redis presence, or dashboard. Optional PostgreSQL stores management configuration and audit records only.

Start with the [external application quick start](docs/QUICKSTART.md) to run a Go business API, install the packed TypeScript SDK, and connect two browser sessions in single-node or distributed mode. It covers secure token issuance, authorization, reconnect, CLI diagnostics and cleanup without modifying Core. The following direct gateway/demo instructions remain available for protocol development.

The [Phase 4C plan and verification record](docs/PHASE_4C_PLAN.md) lists changed files, executed checks and remaining release blockers.

Phase 4D adds local distribution builds, manifests, checksums and a reproducible quality gate. See [release readiness](docs/RELEASE_READINESS.md), [contributing](CONTRIBUTING.md), [security](SECURITY.md) and the [Unreleased changelog](CHANGELOG.md). Public Git/module identity, npm ownership and the project license remain blocked decisions; nothing is published automatically.

| Capability | Current support |
| --- | --- |
| Single-node / distributed gateways | In-memory single mode or NATS Core Pub/Sub across nodes |
| Publishing / subscriptions | Authenticated HTTP publication and project/room-scoped WebSocket subscriptions |
| Recovery / backpressure | Fresh-auth reconnect, explicit authorized subscription recovery, bounded queues and slow-client disconnect |
| Developer integration | Independent Go/TypeScript SDKs, CLI and external Go/browser example; public distribution remains blocked |
| Delivery | Best-effort; acceptance is not browser receipt or processing |
| Management API | Optional PostgreSQL, operator auth/RBAC, project/key lifecycle, audits and local diagnostics |
| Event persistence / replay / ACK guarantees | Not implemented |
| Presence / dashboard | Not implemented |

## Run locally

Requirements: Go 1.27.2 or newer; OpenSSL for the example credentials; Docker with Compose for container commands. Node.js 24.3 or newer is needed only to rebuild or test the TypeScript demo; the checked-in JavaScript works without Node in Go and Docker builds.

Generate independent credentials in your shell. The publisher key belongs to your application backend; the signing secret is used only by trusted token issuers and SocketFabric.

```sh
export CLIENT_TOKEN_SECRET="$(openssl rand -hex 32)"
export PUBLISHER_KEY="$(openssl rand -hex 32)"
export PUBLISHER_KEYS_JSON="$(printf '{"project-a":"%s"}' "$PUBLISHER_KEY")"
go mod download
GIN_MODE=release DEMO_ENABLED=true go run ./cmd/server
```

The default address is `0.0.0.0:8080`. Override `HTTP_HOST` or `HTTP_PORT` to change it. Credentials remain required even when you only need health checks; missing, weak, or invalid credentials fail startup before the listener opens.

The application reads environment variables directly and does not load `.env` automatically. You can copy `.env.example` to `.env`, fill in independent generated credentials, and source a trusted file:

```sh
set -a
. ./.env
set +a
GIN_MODE=release go run ./cmd/server
```

The blank credentials in `.env.example` are intentionally invalid. `.env` is excluded from Git and the Docker build context. Sourcing a file executes shell commands, so only source a file you control.

## Two-client browser demo

With `DEMO_ENABLED=true`, open `http://localhost:8080/demo`. The page has two independent client panels. Create a client token from another terminal with the same exported credentials:

```sh
go run ./cmd/token --project project-a --user demo-user --rooms room-a --ttl 5m
```

Paste that token into both panels and click **Connect / reconnect**. Each connection authenticates and joins `room-a`; separate connections belonging to the same user are supported. Publish from your terminal:

```sh
curl -i http://localhost:8080/v1/events \
  -H "Authorization: Bearer $PUBLISHER_KEY" \
  -H 'Content-Type: application/json' \
  --data '{"projectId":"project-a","type":"resource.updated","roomId":"room-a","data":{"resourceId":"resource-42","status":"updated"}}'
```

Both panels display the same event envelope. Leave the room in one panel and publish again to check isolation, then join again. You can also use two browser tabs. Client tokens stay in page memory and clear on reload; the demo does not receive publisher keys or signing secrets. The page uses the same origin as the gateway.

The demo retries interrupted connections with exponential backoff and equal jitter (50–100% of the capped delay), up to 30 seconds. It authenticates each new connection and explicitly re-joins the rooms still selected by that panel. Leaving a room removes it from reconnect attempts. A stable authenticated connection resets the retry delay; slow-client closes use a longer initial delay. **Disconnect**, replacing a connection, and leaving the page cancel pending retries so a panel owns one active attempt or connection.

Authentication rejection or close code `1008` stops automatic retries. An expired token closes its connections; issue a fresh token and replace the rejected token before reconnecting. The demo never refreshes or issues tokens itself. The separate `cmd/token` helper is a development aid for trusted token issuance. A future application backend should authenticate its user and issue only the project and room permissions that user is allowed to access.

The minimal demo is TypeScript source with generated JavaScript checked in, not a production SDK. Rebuilding requires Node.js 24.3 or newer and strips TypeScript syntax without a static type check; Go and Docker builds use the checked-in JavaScript. To rebuild and run its reconnect tests:

```sh
node internal/adapters/inbound/http/demo-build.mjs
node internal/adapters/inbound/http/demo-build.mjs --check
node --test --test-isolation=none internal/adapters/inbound/http/demo.test.mjs
```

## Go and TypeScript SDKs

The independent packages in `sdk/go` and `sdk/typescript` support external projects without importing Gin, NATS, or gateway internals. The [standalone example](examples/sdk/README.md) adds an application-owned authenticated HTTP token endpoint and business API, plus a plain TypeScript browser app. It derives user project/room policy on the backend and uses the existing SDKs. Existing `/demo` remains a small direct-protocol development aid.

The Go module `socketfabric/sdk/go` provides a project-scoped HTTP publisher with cancellation, a 10-second default timeout, typed API errors, and no automatic publish retry. Its separate HS256 signer emits authorized short-lived client tokens. The signing secret grants token-issuance authority across configured projects; keep it in a central trusted issuer and enforce application permissions before signing.

The browser package `@socketfabric/client` provides typed events, acknowledged subscribe/unsubscribe, fresh authentication on reconnect, bounded membership operations, backoff/jitter, and cancellation. It has no runtime dependencies or publishing/signing API. Initial connect failure rejects within the 10-second default budget; automatic recovery begins only after a successful connection. Policy close `1008` stops retries. Offline joins reject; offline unsubscribe removes recovery intent. Every reconnect re-authorizes explicit room subscriptions. No offline messages are buffered or replayed.

The browser app installs an actual npm tarball and bundles a bare `@socketfabric/client` import with esbuild. Package exports, ESM, declarations and SSR-safe import are tested in an independent consumer. It fetches a fresh token on each connection/reconnection and offers explicit refresh, subscribe/unsubscribe and disconnect; Core policy expiry remains terminal until explicit reconnect.

Package release identity remains provisional. Go consumers can use a local workspace; existing checkout consumers retain local replacements. The runner builds a copied independent module with no consumer replacement, but that is local development proof: canonical public Go module URL/tag is still a release blocker. npm stays private and is consumed as a `.tgz`; license and npm scope ownership are undecided. No registry publication occurs. SDK/example code is not copied into the gateway Docker image.

## Developer CLI

Build an independent binary and run it from any project. It uses the existing Go SDK and HTTP API, with no server signing secret or direct broker access:

```sh
go run ./cmd/socketfabric --help
go build -trimpath -o bin/socketfabric ./cmd/socketfabric
export SOCKETFABRIC_API_KEY="$PUBLISHER_KEY"
export SOCKETFABRIC_PROJECT_ID=project-a
./bin/socketfabric status --url http://localhost:8080
./bin/socketfabric ping --json
./bin/socketfabric publish --room room-a --event resource.updated --data '{"status":"completed"}'
./bin/socketfabric config validate --json
```

Flags override `SOCKETFABRIC_URL` (default `http://localhost:8080`), `SOCKETFABRIC_TIMEOUT` (default `5s`) and project ENV. Keys are ENV-only. Status checks both liveness/readiness for one gateway; ping measures HTTP connectivity. Publish confirms acceptance and local queueing, not browser delivery, and never retries automatically. All result commands support `--json`; use `--data -` for bounded stdin input.

See the [CLI guide](docs/CLI.md) for local installation, Windows/macOS use, commands, CI, exit codes and security. Optional `make cli` / `make cli-cross` targets build the CLI; the existing Docker entrypoint remains the gateway server. The [Phase 4B plan](docs/PHASE_4B_PLAN.md) records implementation and verification scope.

For local distribution artifacts, `make cli-dist` builds six CLI targets and the private npm tarball, compares repeated builds and writes `bin/release/SHA256SUMS`, a manifest and third-party notices. `make release-verify` checks the files. `make release-check` runs the full root/nested Go, SDK, artifact and actual Docker/NATS/Chromium gate when the [documented prerequisites](CONTRIBUTING.md) are available. Outputs stay local and do not imply an approved public release.

## HTTP and WebSocket contracts

The implementation-backed OpenAPI, AsyncAPI, shared schemas, compatibility freeze policy, and frontend readiness decision are documented in [API Contracts](docs/API_CONTRACTS.md), [API Contract Readiness](docs/API_CONTRACT_READINESS.md), and [Backend Readiness](docs/BACKEND_READINESS.md). Phase 5D records a conditional GO for frontend work using same-origin/BFF Management access; it does not approve production or public release. Run `make contracts-test` when changing Gateway routes or WebSocket frames.

| Request | Behavior |
| --- | --- |
| `GET /healthz` | `200 {"status":"ok"}` |
| `GET /readyz` | `200 {"status":"ready"}` when serving and required broker is ready; otherwise `503` |
| `GET /ws` | WebSocket upgrade, followed by client authentication |
| `POST /v1/events` | Bearer publisher key; validate and route an event within its project |
| `GET /demo`, `GET /demo.js` | Browser demo and its module only when `DEMO_ENABLED=true` |
| Unknown route / unsupported method | JSON `404` / `405`; `Allow` header for supported methods |

`POST /v1/events` accepts exactly `projectId`, `type`, `roomId`, and `data`. The key must belong to that project and may publish to any room within it. Identifiers and event types contain 1–128 ASCII letters, digits, `.`, `_`, `:`, or `-`. `data` is required and may be any JSON value, including `null`. The JSON request body is limited to 64 KiB; server-generated metadata is not accepted as input.

The server adds `eventId` and a UTC `timestamp`, then returns `202`:

```json
{
  "event": {
    "eventId": "server-generated-id",
    "projectId": "project-a",
    "type": "resource.updated",
    "roomId": "room-a",
    "data": {"resourceId": "resource-42", "status": "updated"},
    "timestamp": "2026-10-14T03:00:00Z"
  },
  "queuedConnections": 2
}
```

`queuedConnections` counts only connections on the accepting gateway that accepted the event into their outbound queue. It is not a cluster-wide subscriber count. Zero is valid even when another gateway has subscribers. Slow or disconnected clients may miss an accepted event.

In distributed mode the gateway publishes once to NATS, flushes within `NATS_PUBLISH_TIMEOUT`, then delivers directly to its local subscribers. NATS `NoEcho` prevents that gateway's own broker subscription from receiving a duplicate. Flush success confirms broker processing of the publish, not browser delivery. A timeout, permission error, or disconnect can return `503` after a remote gateway has already received the event; retrying can produce duplicates. SocketFabric does not automatically retry a failed publish.

HTTP errors use one envelope, including authentication, permission, validation, payload-size, content-type, and unavailable errors:

```json
{"error":{"code":"internal_error","message":"Internal server error","requestId":"f33ad9f2638532fe6d3d96e4cceea3f9"}}
```

Every HTTP response carries `X-Request-ID`. Supplied IDs accept 1–128 ASCII letters, digits, `.`, `_`, or `-`; otherwise the service generates an ID. Internal error details are not returned.

Connect to `/ws` and send the auth frame first. Never put credentials in the WebSocket URL or query string. All application frames are JSON text:

| Client frame | Server frame |
| --- | --- |
| `{"op":"auth","token":"..."}` | `{"op":"authenticated","connectionId":"...","projectId":"project-a","userId":"demo-user"}` |
| `{"op":"join","roomId":"room-a"}` | `{"op":"joined","roomId":"room-a"}` |
| `{"op":"leave","roomId":"room-a"}` | `{"op":"left","roomId":"room-a"}` |
| Backend publishes | `{"op":"event","event":{...}}` with the six-field envelope above |
| Invalid / unauthorized command | `{"op":"error","error":{"code":"...","message":"...","requestId":"..."}}` |

Join and leave are idempotent. The project and user come from the verified token, and room permissions and token expiry are checked for each subscription. Client-selected project/user values are not trusted. Membership belongs to each connection independently and is removed on disconnect. A reconnect creates a new connection with no memberships: authenticate again, wait for `authenticated`, then explicitly join authorized rooms. Previous membership never grants access to the new connection.

Leave prevents subsequent room routing to that membership, but events already accepted into the connection queue may still arrive after `left`. Disconnects and gateway restarts discard memberships and pending events. Events published while a client is disconnected may be permanently missed; reconnect and re-subscription do not replay them.

Client tokens use JWT HS256 with `iss=socketfabric`, `aud=socketfabric-ws`, `sub` for the user, `projectId`, an explicit `rooms` list, `iat`, and `exp`. Tokens allow at most 64 rooms, have no wildcard permissions, and cannot live longer than 15 minutes. The trusted `cmd/token` helper defaults to 5 minutes. Signature, algorithm, claims, configured project, and expiry are validated. Token expiration disconnects an active client and removes its subscriptions.

Close codes include `1008` for authentication/expiry violations, `1003` for binary application messages, `1009` for oversized frames, `1013` for a full outbound queue, and `1001` during server shutdown or an enabled application idle timeout (`idle_timeout`). Optional Management also uses `1008` for disabled projects and `1013` when managed policy becomes stale. Read/write failures and missed heartbeat responses also trigger cleanup.

## Configuration and lifecycle

| Variable | Default | Validation |
| --- | --- | --- |
| `APP_ENV` | `development` | `development`, `test`, `production` |
| `SOCKETFABRIC_MODE` | `single` | `single` or `distributed` |
| `NODE_ID` | Empty | Optional identifier prefix, at most 64 characters using the identifier alphabet |
| `HTTP_HOST` | `0.0.0.0` | IP address or hostname |
| `HTTP_PORT` | `8080` | 1–65535 |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `HTTP_READ_HEADER_TIMEOUT` | `5s` | Positive Go duration |
| `HTTP_READ_TIMEOUT` | `15s` | Positive Go duration |
| `HTTP_WRITE_TIMEOUT` | `15s` | Positive Go duration |
| `HTTP_IDLE_TIMEOUT` | `60s` | Positive Go duration |
| `SHUTDOWN_TIMEOUT` | `10s` | Positive Go duration |
| `CLIENT_TOKEN_SECRET` | Required | At least 32 bytes; independent from publisher keys |
| `PUBLISHER_KEYS_JSON` | Required | JSON mapping project IDs to unique keys of at least 32 bytes; may be `{}` only when Management is enabled |
| `MANAGEMENT_ENABLED` | `false` | Optional PostgreSQL control plane; [additional settings](docs/MANAGEMENT.md#configuration-and-readiness) |
| `WS_QUEUE_CAPACITY` | `32` | 1–1024 messages per connection |
| `WS_MAX_CONNECTIONS` | `256` | 1–10000 active connections, including connections waiting for auth |
| `WS_MAX_MESSAGE_BYTES` | `16384` | 1–1048576 bytes per incoming client message |
| `WS_AUTH_TIMEOUT` | `5s` | Positive Go duration for the initial auth frame |
| `WS_WRITE_TIMEOUT` | `5s` | Positive Go duration per outbound write |
| `WS_PING_INTERVAL` | `30s` | Positive Go duration |
| `WS_PONG_TIMEOUT` | `10s` | Positive Go duration |
| `WS_IDLE_TIMEOUT` | `0s` | Nonnegative Go duration; zero disables application inactivity closure |
| `WS_ALLOWED_ORIGINS` | Unset: same-origin only | Comma-separated explicit HTTP/HTTPS origins; no paths, query, credentials, or wildcards |
| `DEMO_ENABLED` | `false` | `true` or `false` |
| `NATS_URL` | `nats://localhost:4222` | Distributed only: one `nats://` or `tls://` host:port URL without credentials, path, query, or fragment |
| `NATS_TOKEN` | Required in distributed mode | At least 32 bytes, distinct from client signing and publisher keys |
| `NATS_SUBJECT_PREFIX` | `socketfabric` | 1–128 ASCII letters, digits, `_`, `-`, and nonempty dot-separated tokens; no wildcards |
| `NATS_CONNECT_TIMEOUT` | `5s` | Distributed only: positive Go duration |
| `NATS_PUBLISH_TIMEOUT` | `2s` | Distributed only: positive Go duration |

Unset optional variables use defaults. Explicitly empty or invalid values fail startup, except the optional `NODE_ID`, NATS values ignored in single mode and management/DB values ignored while Management is disabled. Omit `WS_ALLOWED_ORIGINS` to keep the same-origin policy. For a separate browser app, for example, set `WS_ALLOWED_ORIGINS=https://app.example.com,http://localhost:3000`. Origin policy supplements authentication; it does not replace token authorization. `GIN_MODE=release` suppresses Gin's debug output. HTTP headers are limited to 1 MiB, and forwarded proxy headers are not trusted by default.

Single mode ignores every NATS environment variable and never connects to a broker. Distributed mode permits at most 128 ENV projects; managed projects use their own bounded configured policy limit. Each process appends a fresh cryptographic random suffix to `NODE_ID` (or `node` when omitted), so reusing a prefix does not create a routing identity collision. Mode and effective node ID are included in structured logs.

Distributed startup must connect, establish its project subscriptions (or the validated bounded wildcard in management mode), and flush without broker errors before becoming ready. Startup fails within the configured connection budget if this cannot complete. Publish permission failures are detected when publishing and return `503`. `/healthz` remains process liveness; `/readyz` also requires a connected, validated broker session. During a broker outage existing WebSockets and local memberships remain, but publishing and new WebSocket admission return `503`. After reconnect, readiness resumes only after subscriptions flush without broker errors. Existing clients can receive new events again without reconnecting; missed events are not replayed. Authentication, permission, and subscription-limit faults keep the node unready until configuration is corrected and the gateway restarted; transient transport faults use controlled reconnects.

JSON logs go to stdout. Request logs include method, route template, status, duration, and request ID. `WebSocket closing` and `WebSocket disconnected` records include connection/request IDs, close code and server-selected reason, `queue_capacity`, `queue_depth`, `queue_depth_at_close`, `queue_high_water`, `events_accepted`, `events_written`, `events_rejected`, `events_discarded`, `messages_rejected`, `messages_discarded`, and `slow_client_disconnect`. The message counters include control frames as well as events. Authenticated records also include project/user IDs. The closing record can have one event still being written; the disconnected record snapshots counters after the session workers stop. A previously admitted routing attempt can still race this snapshot and be rejected after close. This phase adds no metrics HTTP endpoint. Headers, query strings, bodies, JWTs, keys, secrets, panic values, and client-provided close reasons are excluded.

Outbound queues are bounded by `WS_QUEUE_CAPACITY`. Enqueue never waits for a slow socket: a full queue rejects the message and disconnects that client with `1013`, allowing other clients and publishers to continue. One writer serializes each connection's accepted control and event messages in queue order with `WS_WRITE_TIMEOUT` deadlines. Concurrent publishers have no global ordering guarantee, and acceptance does not guarantee a later socket write or browser receipt. Queues are discarded on close; reconnect never restores them.

`WS_PING_INTERVAL` and `WS_PONG_TIMEOUT` control heartbeat checks for unresponsive peers. After authentication, optional `WS_IDLE_TIMEOUT` measures inactivity since the last client application text frame, including join/leave or invalid text commands; heartbeat pongs and outbound events do not reset it. With a positive idle timeout, a subscriber that only receives events eventually closes with `1001` and reason `idle_timeout`. The default `0s` preserves receive-only subscriptions. Authentication expiry and write deadlines remain independent of heartbeat and idle activity.

On `SIGINT` or `SIGTERM`, readiness and new-connection admission stop first. The server drains HTTP, closes WebSocket sessions, and drains/closes its broker resources under `SHUTDOWN_TIMEOUT`, then forces remaining connections closed if the deadline expires. HTTP shutdown alone cannot drain upgraded connections, so WebSocket lifecycle is handled explicitly. Configuration, listener, server, and forced-shutdown failures exit unsuccessfully; normal shutdown exits successfully.

## Docker

The multi-stage image builds a static binary and runs as a non-root user with CA certificates. Using the credentials exported above:

```sh
docker build -t socketfabric:local .
docker run --rm --name socketfabric -p 8080:8080 \
  --env CLIENT_TOKEN_SECRET --env PUBLISHER_KEYS_JSON \
  --env DEMO_ENABLED=true socketfabric:local
```

Compose requires credentials from your shell or a filled local `.env` and checks `/readyz`:

```sh
DEMO_ENABLED=true docker compose up --build -d
docker compose ps
curl -fsS http://localhost:8080/healthz
curl -fsS http://localhost:8080/readyz
docker compose logs app
docker compose down
```

Compose interpolation/config output can contain credentials; keep that output private. Use `HTTP_HOST=0.0.0.0` inside the container so the published port is reachable. `HTTP_PORT` changes both the listener and the published port. Compose allows 15 seconds to stop; keep `SHUTDOWN_TIMEOUT` below that or increase `stop_grace_period`.

For a signal smoke check, connect both demo clients and run `docker stop --time 15 socketfabric` from another terminal. Clients should close and normal shutdown should exit successfully. For an explicit container exit-status check, run without `--rm`, stop it, inspect `.State.ExitCode`, then remove the stopped container.

## Two-gateway distributed demo

Use the independent client signing secret and publisher key generated above, then generate a separate broker token:

```sh
export NATS_TOKEN="$(openssl rand -hex 32)"
DEMO_ENABLED=true docker compose -f docker-compose.distributed.yml up --build -d
docker compose -f docker-compose.distributed.yml ps
curl -fsS http://localhost:8080/readyz
curl -fsS http://localhost:8081/readyz
go run ./cmd/token --project project-a --user demo-user --rooms room-a --ttl 5m
```

This standalone Compose file runs `nats:2.15.1-alpine` with token authentication and two gateways. NATS client port 4222 and monitoring port 8222 remain private to the Compose network; the broker healthcheck uses its internal `/healthz`. Gateway ports bind to `127.0.0.1:8080` and `127.0.0.1:8081`. Override `GATEWAY_A_PORT` and `GATEWAY_B_PORT` to use different host ports. All gateways must share publisher credentials, client signing secret, broker token, and subject prefix.

Open `/demo` on port 8080 in one browser window and on port 8081 in another. Paste the generated token, connect, and join `room-a` on each. Run the earlier curl publish command against either gateway. Each subscribed client receives the same event envelope once; `queuedConnections` still reports only the accepting gateway's local queues. Other projects and rooms remain isolated.

For an outage/reconnect check, leave the browser connections open:

```sh
docker compose -f docker-compose.distributed.yml stop nats
curl -i http://localhost:8080/healthz
curl -i http://localhost:8080/readyz
# Repeat the publish curl: expect 503 while disconnected.
docker compose -f docker-compose.distributed.yml start nats
# Wait for both /readyz endpoints to return 200, then publish again.
docker compose -f docker-compose.distributed.yml down
```

Liveness stays `200`, readiness becomes `503`, and the existing WebSockets remain connected during the outage. Once both gateways are ready again, publishing resumes on those same WebSockets without replaying missed events. Stopping one gateway closes its own clients with `1001` while the peer remains operational.

## Architecture

Dependencies follow `Adapters → Application → Domain`, with explicit constructor injection and no DI framework:

```text
cmd/server                         composition root
cmd/token                          trusted token issuance helper
cmd/socketfabric                   independent developer CLI using the public Go SDK
internal/domain                    pure-Go event and authorization rules
internal/application               use cases and ports owned by application needs
internal/adapters/inbound/http      Gin API, WebSocket sessions, browser demo and middleware
internal/adapters/outbound          JWT/credentials, in-memory connections/rooms, NATS event bus
internal/config                    environment parsing and validation
internal/bootstrap                 HTTP lifecycle, readiness and shutdown
sdk/go                             independent Go publisher and trusted token signer module
sdk/typescript                     independent browser SDK with exported types
examples/sdk                       external-style trusted backend and browser consumer
```

Domain and application packages do not depend on Gin, WebSocket libraries, or NATS. Application use cases authenticate clients, authorize room membership, and publish validated events through application-owned ports. The in-memory registry isolates memberships by `(projectId, roomId)`, snapshots recipients under a mutex, and checks membership again before enqueueing outside the lock. Each WebSocket adapter session owns its queue, transport I/O, heartbeat, expiry, idle timeout, and cancellation. Health checks remain operational handlers.

The outbound NATS adapter uses `nats.go v1.54.0`, owns one connection per gateway, and subscribes to exact subjects `<prefix>.v1.events.<base64url(projectId)>` for configured projects. Project IDs never become wildcard subject tokens. It uses ordinary Pub/Sub so every subscribed gateway receives events; there are no queue groups. Consumers validate the envelope and its project against the subject, then broadcast only to local authorized room members without republishing. Each project subscription has a fixed pending limit of 64 messages and 1 MiB; bus envelopes are limited to 128 KiB with 64 KiB event data, and up to 64 publishes may be in flight. These bounds limit memory rather than adding delivery guarantees.

The SDKs depend only on the public HTTP/WebSocket contracts and own their wire types; they do not import gateway `internal` packages. Core SDK code is not part of the gateway composition root. See [project context](docs/PROJECT_CONTEXT.md) for the broader roadmap. PostgreSQL, Redis, JetStream, and durable delivery remain outside this phase.

## Validation and limitations

```sh
go mod tidy
go fmt ./...
go vet ./...
go test ./...
go test -race ./...
go build -o bin/socketfabric-server ./cmd/server
go build -o bin/socketfabric ./cmd/socketfabric

# Optional demo development checks; Go/Docker use the checked-in module.
node internal/adapters/inbound/http/demo-build.mjs
node internal/adapters/inbound/http/demo-build.mjs --check
node --test --test-isolation=none internal/adapters/inbound/http/demo.test.mjs

# Install/provide the pinned nats-server 2.15.1 executable separately.
NATS_SERVER_BIN=/absolute/path/to/nats-server go test -tags integration ./...
NATS_SERVER_BIN=/absolute/path/to/nats-server go test -race -tags integration ./...

# Independent modules are not included in root go test ./....
(cd sdk/go && go mod tidy && go vet ./... && go test -race ./...)
(cd examples/sdk && go mod tidy && go fmt ./... && go vet ./... && go test ./... && go test -race ./... && go build -o /tmp/socketfabric-sdk-example .)
npm --prefix sdk/typescript ci
npm --prefix sdk/typescript run typecheck
npm --prefix sdk/typescript test
(cd sdk/typescript && npm pack)
```

The optional [external integration runner](tests/sdk/README.md) installs the SDKs into disposable external Go/npm consumers and checks the actual Go HTTP application and bundled browser UI against single-node and two-gateway Docker deployments:

```sh
npm --prefix tests/sdk ci
(cd tests/sdk && npx playwright install chromium)
node tests/sdk/run.mjs
# Add real CLI single/distributed smoke checks using the same isolated runner.
node tests/sdk/run.mjs --cli
```

Tests cover configuration/auth validation, HTTP contracts, room/project isolation, unauthorized commands, multiple connections per user, cleanup, concurrency, slow-client handling, deadlines, and shutdown. Demo tests exercise reconnect backoff and cancellation with controlled timers. Integration-tagged tests start a real external NATS server through `NATS_SERVER_BIN`; the server is not added as a Go module dependency. Existing health, logging, recovery, and HTTP lifecycle checks remain applicable. Race tests need a supported platform and C toolchain.

Phase 3 validation on 2026-10-15 passed formatting, vet, module tidiness, Go builds, unit/integration tests and race tests, including real NATS server 2.15.1. All nine deterministic demo tests and the generated JavaScript freshness check passed. Tests cover membership snapshots invalidated by leave/rejoin or reconnect, FIFO event writes, concurrent queue overflow, control-message rejection logs, heartbeat progress during a blocked write, idle expiry/reset, and explicit reconnect authorization.

Docker builds and single mode without valid NATS configuration passed. Chromium clients on two gateways received matching envelopes once, authenticated and re-subscribed after a gateway restart, cancelled pending retries during a network interruption, and stopped automatic retries after policy close `1008` until credentials changed. Broker outage/recovery retained existing sockets; shutdown closed clients with `1001` and gateways exited with code 0 while the peer remained operational. Runtime logs and demo DOM contained no publisher/broker/signing credentials. Temporary containers, networks, and generated credentials were removed. These checks are functional verification, not load or throughput benchmarks.

Phase 4A validation passed Core Go tests/vet and race tests with real NATS, Go SDK vet/race tests, and TypeScript strict type checking plus all 25 deterministic SDK tests. External consumer checks passed Go `replace`, npm tarball exports/declarations, and SSR-safe import. The Docker/Chromium runner verified SDK-signed tokens against the server, Go publishing through either gateway, identical events, project/room isolation, the actual browser example and safe text rendering, gateway restart with fresh authentication and room restoration, broker outage with explicit HTTP 503, token expiry with terminal authentication handling, offline unsubscribe/subscribe behavior, and normal gateway exits. Test containers, networks, and temporary credentials were removed; no registry publication or load benchmark was performed.

Phase 4C validation on 2026-10-16 passed root/SDK/example Go tidy, formatting, vet, tests, race tests and builds; real-NATS integration race tests; 25 TypeScript SDK tests; nine existing demo tests; and the strict example browser build. The external runner built an independent Go workspace consumer, installed the real npm artifact, verified exports/types/SSR, and exercised the Go business API and actual browser UIs in both Compose modes. Final CLI/browser checks covered authorization/isolation, fresh-token recovery, terminal expiry, one active socket, no routing-loop duplicates, outages, normal shutdown and explicit container/network cleanup. The quick-start ENV script and private file permissions were also checked. Public Go installation remains unverified pending a canonical origin/tag; see the [evidence and limitations](docs/PHASE_4C_PLAN.md).

Membership disappears on gateway restart. No delivery acknowledgment, ordering across concurrent publishers, client-JWT revocation/rotation API, offline replay, event durability, Redis presence, or guaranteed delivery is provided. Optional Management persists operators/projects/managed credentials/audits and adds local login throttling; it does not add a distributed event rate limiter. The external application can issue a fresh token; Core adds no client refresh endpoint. There is no global connection registry or subscriber count. Broker access is a trusted infrastructure boundary: holders of its server-only token can publish valid project events, so it remains private and separate from browser/publisher/operator credentials. Load testing and production deployment policy remain necessary.

Phase 5C adds pinned test-tool setup, fast/integration/security gates, repeated infrastructure failure tests, container safeguards and measured HTTP/WS benchmarks. See [implementation evidence](docs/PHASE_5C_REPORT.md), [security audit](docs/SECURITY_AUDIT.md), [performance baseline](docs/PERFORMANCE_BASELINE.md) and [production operations](docs/PRODUCTION_OPERATIONS.md). Use `make test-tools`, `make pr-check`, `make integration-check`, `make security-check` and `make benchmark` on a prepared machine. Full release verification remains `make release-check`.

Next step: review Phase 5D integration-readiness decisions using the contracts and measured evidence. Public identity/licensing and production deployment/load/retention decisions remain separate blockers.
