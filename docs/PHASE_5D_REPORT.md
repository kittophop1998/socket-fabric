# Phase 5D — Backend readiness gate and API freeze

Evidence date: **2026-10-17**. This phase audited current implementation and
contracts, ran contract/security/integration gates, documented the compatibility
baseline, and made a frontend-integration decision. No endpoint, protocol,
authentication, SDK, CLI or server runtime behavior changed.

## Decision

**GO for frontend integration, conditional on same-origin Management access via
a Next.js BFF/server route or same-origin reverse proxy.** No backend endpoint is
missing for the documented Dashboard flows. Management has Bearer sessions but
does not implement CORS/preflight or cookie sessions, so a separately hosted
browser SPA cannot call it cross-origin directly. The BFF should keep the
operator token server-side. The browser's WebSocket uses the current
`WS_ALLOWED_ORIGINS` policy and a fresh short-lived token issued by a trusted
application backend.

**NO-GO for production approval or public release.** Local readiness evidence
does not resolve the explicit deployment, capacity, recovery, provenance and
ownership blockers below. Frontend integration readiness and production
readiness are separate decisions.

## Baseline inventory

- `api/openapi.yaml`: 20 operations covering 4 infrastructure/realtime routes,
  14 Management operations and 2 optional demo assets. The Gin router does not
  expose `/metrics`. Management/demo routes are conditionally registered and
  return the ordinary JSON 404 when disabled.
- `api/asyncapi.yaml`: 8 application message types: `auth`, `join`, `leave`,
  `authenticated`, `joined`, `left`, `event`, and `error`. Ping/pong are
  WebSocket control frames, not application messages. Reconnect needs fresh
  authentication and explicit room joins; there is no ACK/replay.
- Go SDK exports `NewClient`, `Client.Publish`, typed publish/result/error
  types, and the trusted-backend `NewTokenSigner`/`IssueToken` surface.
- TypeScript SDK exports `SocketFabricClient`, typed event/options/state/error
  types, and connect/subscribe/unsubscribe/disconnect. Its reconnect flow
  requests fresh authentication and restores explicit membership.
- CLI commands are `version`, `status`, `ping`, `publish`, and `config validate`.
  It distinguishes HTTP connectivity and API acceptance from WebSocket receipt
  and does not retry publish automatically.
- Runtime source and tests confirm separate operator/publisher/client
  credentials, Admin/Viewer policy, one-time key disclosure, strict request
  validation, local-only diagnostics, disabled-feature 404 behavior and
  best-effort delivery semantics.

The compatibility and frontend-flow matrices are maintained in
[API Contract Readiness](API_CONTRACT_READINESS.md). The explicit freeze and
change policy is in [API Contracts](API_CONTRACTS.md#compatibility-baseline-and-change-policy).

## Compatibility matrix

| Component | Local result | External/public result |
|---|---|---|
| Core HTTP/OpenAPI | PASS — router/method alignment, shared schemas, runtime responses and generated TypeScript validated. | No tagged historical contract or multi-version gateway matrix: NOT VERIFIED. |
| WebSocket/AsyncAPI | PASS — parser, valid/invalid schemas, live event/auth/reconnect/isolation paths verified. | No negotiated protocol version or released compatibility tuple: NOT VERIFIED. |
| Go SDK | PASS — isolated consumer builds and publishes through real Gateway; no imports of Gin, NATS or server internals. | Public `go get` BLOCKED by provisional module path/repository identity; local copied-module/workspace verification is not registry proof. |
| TypeScript SDK | PASS — strict types/tests, packed artifact consumer, SSR/browser bundle and real browser flow. | npm publishing/registry ownership BLOCKED; package remains private. |
| CLI | PASS — standalone external-directory binary exercises version/config/status/ping/publish and failure codes. | Public binaries/tags BLOCKED; no publication or version change. |

## Executed validation

| Check | Status | Actual evidence |
|---|---|---|
| `make contracts-test` | PASS | Go contract tests passed; OpenAPI bundling and strict TypeScript generation passed; AsyncAPI parser passed; PostgreSQL-backed Management API contract smoke built the Docker image, bootstrapped/migrated the fixture, tested endpoints/CLI and clean shutdown with secret-safe logs. |
| `make integration-check` | PASS | Root, Go SDK and example modules passed tidy-diff/verify, vet, unit/race tests and builds; real NATS adapter/bootstrap race integration and PostgreSQL migration/RBAC/revocation/outage integration passed; npm installs, strict TS and 25 SDK tests, 9 demo lifecycle tests, independent Go/npm consumers, CLI, Docker and Chromium flows passed in single/distributed mode; documentation links passed. |
| `make security-check` | PASS within defined scan scope | Targeted source credential scan, four npm audit graphs and govulncheck for root/SDK/example passed. Root govulncheck found no vulnerable symbols or affected imported packages; it did report the module-level OpenPGP advisory described below. |
| `make pr-check` | PASS | Run after the readiness docs were added: Go formatting, root/nested Go tidy/verify/vet/unit/race/build, contract generation/tests, TypeScript SDK (25 tests), demo lifecycle (9 tests), and documentation link validation passed. |
| API runtime contract preservation | PASS | No HTTP route, request/response shape, WebSocket frame, credential or error-contract changes were made. Existing tests compare the real Gin router to OpenAPI and validate runtime payloads. |
| Git/CI/provider audit | BLOCKED | `git rev-parse`, `git remote`, branch and tag queries all report that this `.git` is not a repository. No `.github` or `.gitlab` workflow files are present. No remote or provider was guessed. |

`docker compose build` was not invoked as a standalone command in this phase;
both Compose modes were actually built and exercised by the integration runner.
Public registry installs, provider-hosted CI and version-to-version compatibility
were not executed.

## Phase 5C blocker reconciliation

| Finding | Phase 5D status | Evidence / remaining work |
|---|---|---|
| `GO-2026-5932`, `golang.org/x/crypto@v0.57.0` | PASS for current reachable code; residual dependency advisory remains | Current pinned `govulncheck v1.9.0` run found 0 vulnerable symbols and 0 affected imported packages, plus a module-level record covering all symbols in `golang.org/x/crypto/openpgp` and related packages, all versions, no known fixed version. Current SocketFabric code uses other `x/crypto` packages and does not import OpenPGP. Do not add OpenPGP; re-evaluate if use changes. See the [Go vulnerability report](https://pkg.go.dev/vuln/GO-2026-5932). |
| NATS and PostgreSQL outage/recovery | PASS in local integration | Real local broker/DB stop/restart and concurrent authorization/publish tests passed. Production network partitions and managed infrastructure recovery are NOT VERIFIED. |
| Credential expiry/revocation consistency | PASS for tested local semantics | PostgreSQL runtime tests cover expiration/revoke/use, project scope and post-commit rejection. In-flight authorized requests can finish; client JWTs have no individual revocation. Multi-region consistency is NOT VERIFIED. |
| Container OS image scan | NOT VERIFIED | `trivy`, `grype`, `gitleaks`, and `trufflehog` are unavailable in this environment. Runtime container hardening checks from Phase 5C do not scan OS packages. |
| Production TLS/reverse proxy | NOT VERIFIED | Local HTTPS/Origin tests exist, but no real deployment proxy, external DNS, certificates or production network policy was exercised. |
| Production capacity/soak | NOT VERIFIED | Phase 5C contains short, bounded loopback baselines only. No production limits or SLO are inferred. |
| Database backup/restore | NOT VERIFIED | Migration and outage/restart tests passed; backup restoration was not exercised. |
| Git history, remote, CI and provenance | BLOCKED | No usable Git repository and no provider workflow/configuration are available. Owner must restore the canonical checkout and select a CI provider. |
| Release ownership/license/security contact | BLOCKED | Existing Phase 4D owner decisions remain unresolved; Phase 5D does not select legal terms or create external resources. |

The OpenPGP advisory was checked against the official Go vulnerability
database. It describes the `openpgp` packages as unsafe by design, unmaintained,
and without a fixed version; its package scope is distinct from the other
`golang.org/x/crypto` packages used by the application. The scanner result is
therefore not reported as “zero dependency advisories.”

## API freeze decision

The current checked-in OpenAPI, AsyncAPI and shared JSON schemas form the API
freeze baseline for Phase 6 integration. `v1` and `admin/v1` are the HTTP path
families. Spec `info.version` is documentation metadata, not runtime
negotiation. The WebSocket protocol has no negotiated version. Operation IDs,
auth/RBAC, statuses/errors, fields/nullability, pagination, WS message ordering,
close behavior and delivery meaning are frozen for integration review.

Every contract-affecting change needs API-owner review, updated specs, generated
type checks, representative runtime tests, SDK regression tests and a
compatibility note. Request bodies use strict JSON decoding, so adding an
optional request property can still break an older Gateway; coordinate rollout
or add an explicit migration. Tolerant additive response fields may be
compatible. Security fixes remain allowed, with compatibility preserved where
possible and unavoidable behavior changes reviewed and documented.

For future publication, retain SemVer: root Core and CLI share a version line;
Go SDK and TypeScript SDK can version independently and must each state the
tested Gateway contract tuple. Before 1.0, incompatible changes require an
explicit migration and owner review. No public tag/version is chosen here.

## Before Phase 6 and before production

Frontend implementation can start against the frozen contracts if it:

1. Uses a same-origin Next.js BFF or reverse proxy for Management and keeps the
   opaque operator session server-side.
2. Uses the existing application-backend token issuance pattern for client
   authentication; the Dashboard must not receive signing or publisher secrets.
3. Displays one-time publisher keys only at creation and never persists them in
   browser storage.
4. Treats diagnostics as local, publish as best-effort acceptance, and reconnect
   as a fresh-auth/re-subscribe flow.
5. Runs `make contracts-test` on every contract change and the documented
   integration gate for compatibility-sensitive changes.

Before public release or production approval, the owner must provide a usable
canonical repository/remote and CI provider, choose license and release identity,
establish private security reporting, scan container OS packages, validate the
actual TLS/proxy/network setup, measure a workload-specific soak envelope, and
test database backup restoration. These are not blockers to beginning the
frontend against the local API freeze, but they remain release/operations gates.
