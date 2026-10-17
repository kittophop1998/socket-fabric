# Backend readiness for frontend integration

Evidence date: 2026-10-17. This report evaluates integration with the current
Gateway contracts. It is separate from public-release and production-readiness
approval.

## Decision

**GO for frontend integration, with a same-origin/BFF requirement for Management.**
A Next.js application can use the existing Management, diagnostics, audit,
publish and WebSocket contracts without a Core endpoint change. Its server-side
BFF or a same-origin reverse proxy must call Management because the Gateway has
Bearer sessions but no CORS/preflight or cookie-session implementation. Keep
operator session tokens server-side in the BFF. Browser WebSocket connections
use the existing exact `WS_ALLOWED_ORIGINS` policy and a short-lived token from
the trusted application backend.

This GO does not approve production deployment or public release. Production
TLS/proxy configuration, soak capacity, database restore, image OS scanning,
Git provenance, hosted CI, repository ownership, license and release security
contact remain unresolved or unverified; see [Phase 5D report](PHASE_5D_REPORT.md).

## Compatibility matrix

| Surface | Current contract | Verification | Status |
|---|---|---|---|
| Gateway HTTP | 20 documented operations; `/healthz`, `/readyz`, `/ws`, `/v1/events`, 14 Management operations and 2 optional demo assets. No `/metrics`. | Gin route/method alignment over Management/demo feature modes, runtime schemas and PostgreSQL Management flow in `make contracts-test`. | PASS |
| WebSocket | 8 application messages; initial token frame, explicit room join/leave, event/error envelopes, transport heartbeat and documented close codes. | AsyncAPI parse/schema validation plus live browser, reconnect, expiry, isolation and NATS tests in `make integration-check`. | PASS |
| Go SDK | `NewClient`, project-scoped `Publish(ctx, ...)`, typed API errors; `NewTokenSigner`/`IssueToken` for trusted backend token issuance. | Separate Go consumer built and exercised from outside the server package boundary. The fixture uses a local copied SDK/workspace for the provisional module path. | PASS locally; public `go get` NOT VERIFIED |
| TypeScript SDK | `SocketFabricClient` with typed event envelope, connect, subscribe, unsubscribe and disconnect; reconnect obtains a fresh token and restores explicit authorized memberships. | Strict typecheck, 25 unit tests, package installation in an independent consumer, browser bundle/SSR and real Chromium integration. | PASS locally; registry install BLOCKED by unresolved package ownership |
| CLI | `version`, `status`, `ping`, `publish`, `config validate`; stable documented input/remote/cancellation exit behavior. | Standalone external-directory CLI exercised against actual single/distributed gateways and HTTP failures by integration runner. | PASS locally; public binary release BLOCKED |
| Cross-version compatibility | Only current source/spec/SDK tuple has been exercised; no published tags or multi-version Gateway matrix. | No remote tags or usable Git provenance in this checkout. | NOT VERIFIED |

## Frontend flows

| Flow | API/auth contract | Evidence | Status / constraint |
|---|---|---|---|
| Login → current operator → logout | Login returns an opaque Bearer session once; `me` requires it; logout revokes it. Generic login failure, expiry, 401/403 and rate limiting are documented. There is no refresh route. | PostgreSQL Management runtime test covers login, current operator, logout/revocation, invalid credentials and rate limiting. | PASS; BFF must reauthenticate after expiry. |
| Project list/detail/create/update/disable | Viewer/Admin read; Admin mutates. Pagination, filters, validation, conflict and disabled behavior are in OpenAPI. | Contract integration covers list/filter, detail, create/collision, update/disable, role errors and disabled publishing. | PASS. |
| Credential create/list/revoke | Admin only; secret visible once, later list metadata only. Revoke is project-scoped; a committed revoke rejects later managed-key authentication on the primary. | Runtime tests cover key creation/publication, secret/hash absence, list, revocation, audit and subsequent rejection. | PASS; never persist plaintext in UI state/storage. |
| Diagnostics and audit | Viewer/Admin read local diagnostics; Admin reads audit pages. Nodes/metrics do not claim cluster aggregation. | PostgreSQL runtime schema tests cover all three diagnostics responses and audit pages/permissions. | PASS for local visibility only. |
| Trusted client token | Core offers no token issuer. Application backend authenticates its own user and authorizes allowed rooms before signing a short-lived scoped client token. | External Go backend and browser flow tests verify authorized issuance and project/room denial. | PASS through existing external example; Dashboard must not sign tokens. |
| WebSocket event/recovery | Client sends `auth`, waits for `authenticated`, joins authorized rooms, receives `event`; reconnect obtains fresh auth and re-joins. | Real Chromium tests exercise same-room delivery, cross-node delivery, room/project isolation, expiry and re-subscribe. | PASS; disconnected messages may be lost. |
| Error and outage handling | HTTP error envelope, request ID and operation-specific statuses; WS error frames/close codes; best-effort acceptance only. | Runtime contract, CLI, NATS and PostgreSQL fault tests cover representative authorization, validation, outage and recovery paths. | PASS for tested outcomes; no ACK/replay. |

## What the frontend must not assume

- `202` from publish means the Gateway accepted/routed the event in its current
  mode; it does not confirm browser write, receipt or processing.
- `queuedConnections` is local to the receiving Gateway. Diagnostics are also
  local; the Control Plane has no global node/room/presence view.
- Key revocation prevents new managed-key authentication after the primary
  transaction commits. It does not cancel a request already authorized.
- Management project policy propagates to WebSocket sessions through its
  bounded refresh behavior. Client JWTs cannot be individually revoked.
- A WebSocket reconnect does not restore trust in prior rooms. The client needs
  fresh auth and explicit authorized subscriptions.
- Direct browser-to-Gateway Management requests from another origin fail
  browser CORS checks. Use the same origin or a server-side BFF; do not add a
  permissive wildcard.

## Readiness checks

```sh
make contracts-test
make integration-check
make security-check
```

These are provider-neutral local commands. `make contracts-test` validates
schemas/routes and runs the PostgreSQL-backed contract smoke. The integration
gate covers all Go modules, real NATS/PostgreSQL faults, SDK/CLI consumers,
Docker and Chromium. The security gate uses the pinned scanner and current npm
audit graphs. Hosted CI is blocked until the owner confirms the repository and
provider; no provider workflow is assumed here.
