# Changelog

No public version or release tag has been issued by this checkout. Versions and
component compatibility follow the proposal in [release readiness](docs/RELEASE_READINESS.md)
once the owner approves it.

## Unreleased

### Added

- Phase 5C pinned test tooling, fast/integration/security gates, real repeated
  broker/credential and Gateway crash regressions, bounded performance baselines
  with profiles, and production operations/security evidence.

- Opt-in PostgreSQL Management API, operator sessions/Admin-Viewer permissions,
  managed projects/publisher keys, transactional audits and local diagnostics.
- Explicit migrations/operator bootstrap tool and real PostgreSQL/NATS outage,
  authentication, authorization and realtime regression coverage.

- Single-node and optional NATS Core distributed gateways with authenticated,
  project/room-scoped event publishing and WebSocket subscriptions.
- Independent Go/TypeScript SDKs, standalone developer CLI and an authenticated
  external Go/browser example with reconnect and explicit subscription recovery.
- Local distribution preparation: six CLI targets, stable build metadata,
  repeated-build comparisons, npm artifact inspection, manifests, SHA-256
  checksums and third-party notices.
- Gateway OpenAPI 3.1 and AsyncAPI 3 contracts, shared JSON Schemas, Gin route
  and auth/RBAC drift checks, runtime payload validation, and generated
  TypeScript contract smoke checks.
- Provider-independent quality gates using existing Go/npm/Docker/browser
  tooling, supply-chain checks and release/contribution/security documentation.

### Compatibility

- Management is disabled by default. ENV project/key behavior remains independent
  of PostgreSQL; managed projects fail closed when authorization is unavailable.

- Phase 4D adds build/documentation tooling and Go API documentation comments;
  existing HTTP/WebSocket, SDK and CLI behavior stays unchanged.
- Phase 5B adds machine-readable API contracts and verification only; Gateway
  route, authentication, and delivery behavior remains unchanged.
- Delivery remains best-effort; no persistence, replay, guaranteed delivery or
  client ACK is added.

### Security

- Compose gateways use read-only roots, dropped capabilities, no privilege gain
  and bounded local log rotation; no public protocol/auth scheme changes.

- Require Go 1.27.2 and align the Docker builder with that security patch.
- Update indirect `golang.org/x/net` to v0.60.0 and `quic-go` to v0.59.1 for
  reported HTTP/2 and QPACK vulnerabilities; retain existing API/protocol behavior.

### Release blockers

- Canonical Git/module identity and provenance, npm scope ownership, project
  license and private security reporting channel require owner confirmation.
- Public installation and native non-host platform execution remain unverified.
