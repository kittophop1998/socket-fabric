# API contract readiness and freeze baseline

Evidence date: 2026-10-17. The checked-in specs describe the current working
implementation and are now the compatibility baseline for frontend work. This
is not a statement about a Git tag or public release: this checkout has no
usable Git repository, tag history, or remote.

## Contract coverage

`api/openapi.yaml` contains 20 operations: 4 core infrastructure/realtime
routes, 14 Management operations, and 2 optional demo assets. The router
contract test compares registered method/path pairs against the specification
across Management and demo feature combinations. There is no `/metrics` HTTP
route. Disabled feature routes resolve to the normal JSON 404. Management
diagnostics report local-node data and do not aggregate a cluster.

`api/asyncapi.yaml` describes the 8 application JSON messages on `GET /ws`:
`auth`, `join`, `leave`, `authenticated`, `joined`, `left`, `event`, and
`error`. WebSocket control ping/pong frames are transport behavior, not JSON
messages. The first text frame authenticates with a short-lived client token;
each authorized room is joined explicitly. Reconnect requires fresh
authentication and re-subscription. Disconnected events can be lost.

| Contract surface | Result | Evidence and limit |
|---|---|---|
| Registered HTTP method/path coverage | PASS | `make contracts-test` runs Gin route comparison with optional feature modes and fails on missing/extra operations. |
| OpenAPI references, examples and TypeScript types | PASS | Local bundling/schema tests, operation checks and strict temporary TypeScript consumer passed. |
| AsyncAPI and shared WS frame schemas | PASS | AsyncAPI parser, shared schema validation, valid/invalid frames and live WS integration passed. |
| Management responses and RBAC | PASS | PostgreSQL-backed contract flow covers login, session, Viewer/Admin access, projects, credentials, diagnostics, audit and error cases. |
| Publish/error and project-room scope | PASS | Runtime contract and NATS/HTTP integration exercise accepted, invalid, unauthorized and isolated requests. |
| SDK/browser and CLI compatibility | PASS | `make integration-check` exercises independent Go/npm consumers, CLI, Chromium, and single/distributed flows. |
| Multiple released contract versions | NOT VERIFIED | No public tags or versioned compatibility matrix exist in this checkout. The tested baseline is the current source/spec/SDK tuple only. |

## Frontend flow evidence

| Flow | Contract and authority | Automated evidence | Readiness |
|---|---|---|---|
| Operator login → session/me → logout | `POST /admin/v1/auth/login` returns a one-time opaque Bearer session; `GET /auth/me` reads it; `POST /auth/logout` revokes it. Login failures are generic; sessions expire and have no refresh endpoint. | PostgreSQL Management test validates login, me, logout, revoked-session 401, invalid credentials 401 and throttling 429. | PASS; frontend must obtain a new session after expiry. |
| Browse and change projects | Viewer/Admin may list and inspect. Admin creates, updates and disables. Managed project IDs/status and pagination errors follow OpenAPI. | PostgreSQL Management test validates list/filter/pagination, detail, create conflicts, validation, role denial, update/disable, and disabled-project publish behavior. | PASS. |
| Manage publisher keys | Admin creates a project-scoped key, sees plaintext once, lists metadata, and revokes by project plus credential ID. | PostgreSQL Management test checks one-time secret, metadata redaction, key publication, revoke, post-revoke rejection, audit, and project scope. | PASS; UI must prevent accidental secret re-display/storage. |
| Diagnostics and audit | Viewer/Admin read local diagnostics; only Admin reads paginated audit logs. Cluster aggregation is explicitly unavailable. | PostgreSQL-backed response-schema tests validate all diagnostics and audit entries, roles, scope labels and secret absence. | PASS for documented local scope. |
| Obtain a client token | Core has no token-issuance endpoint. A trusted application backend verifies its own user and policy, then uses the existing Go SDK signer. | Independent example backend and external-consumer integration issue scoped tokens; unauthorized user/project/room requests are rejected. | PASS through the existing external backend pattern; no Core endpoint is required. |
| Connect, subscribe, receive, reconnect | `GET /ws` upgrades, first frame is `auth`, then explicit `join`/`leave`; event frames are best-effort. Fresh token and room re-authorization are required after reconnect. | Real Chromium and Gateway/NATS suite verifies event receipt, cross-node delivery, isolation, expiry, fresh-token recovery, re-subscribe and intentional disconnect. | PASS; no offline replay or processing acknowledgement. |
| Handle failures | HTTP error envelope/request ID; 401/403/404/409/413/415/429/503 as defined per operation. WS errors/close codes follow AsyncAPI. | Runtime contract, Management tests, CLI HTTP tests and Docker/NATS outage/recovery suite cover representative failures. | PASS for tested statuses; browser must handle timeout/outage and reauthenticate after expiry. |

## Browser hosting constraint

Management uses an `Authorization: Bearer` operator session, not a cookie, and
the Gateway does not implement CORS/preflight handling. A separately hosted
browser SPA therefore cannot call Management directly across origins. Direct
HTTP integrations must place the UI behind a same-origin reverse proxy or use a
Next.js BFF/server route that forwards requests to the Gateway. Keep the
operator session server-side when using a BFF; do not put it in URLs or
persistent browser storage. WSS client sessions still use the browser's
configured `WS_ALLOWED_ORIGINS` policy.

This is an integration/hosting requirement, not a missing backend endpoint. If a
future static cross-origin SPA is required, first review a narrow explicit CORS
policy, preflight contract, allowed origins and credential model; do not add a
wildcard or cookie authority as a convenience.

## Reproducible command

```sh
make contracts-test
```

For all SDK/CLI/browser and distributed integration evidence, run
`make integration-check`. For current Go/npm vulnerability findings, run
`make security-check`. The confirmed local runner results are recorded in
[Phase 5D report](PHASE_5D_REPORT.md); no hosted CI execution is claimed.
