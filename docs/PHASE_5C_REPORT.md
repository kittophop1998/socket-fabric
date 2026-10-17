# Phase 5C implementation and evidence

Scope: production hardening, security review, fault injection, reproducible local
benchmarks and operations. Existing HTTP/WS schemas, credential schemes, SDK/CLI
APIs and best-effort semantics remain in place. No frontend, Phase 5D API freeze,
new business endpoints, durable messaging or public release is included.

## Implementation

- [Plan](PHASE_5C_PLAN.md), [security audit](SECURITY_AUDIT.md),
  [performance baseline](PERFORMANCE_BASELINE.md), and
  [operations guidance](PRODUCTION_OPERATIONS.md).
- Existing Compose Gateways gain read-only roots, dropped capabilities,
  no-new-privileges and bounded local log rotation. No server endpoint/config
  system or architecture layer was added.
- `scripts/test-tools.mjs` provisions/resolves pinned external test tools.
  `scripts/check.mjs` supports fast/integration/security/full suites; Makefile
  targets expose these without inferring a hosting provider.
- Existing NATS fixtures now exercise three broker failure/recovery cycles with
  concurrent publishers, retained authorized readers, isolation, no routing
  echoes and joined worker/connection cleanup. PostgreSQL tests add concurrent
  revoke/use, project-scoped revocation and post-logout authorization checks.
- The existing Chromium/SDK/CLI runner adds real Gateway SIGKILL recovery and
  runtime assertions for the container safeguards. It continues checking broker
  outage, token refresh/re-subscription, isolation, terminal expiry and cleanup.
- Linux integration benchmarks use real HTTP/WS and real NATS with bounded
  clients, requests and sample storage. Profiles and metadata stay under `bin/`.
  No speculative production-path optimization was applied.
- Corrected existing contract prose about revocation, TLS failure statuses and
  WS handshake errors; no API/message shape changed.

## Evidence status

| Check | Status | Evidence |
| --- | --- | --- |
| Pinned test-tool provisioning | PASS | `make test-tools` installed NATS 2.15.1 and govulncheck 1.9.0 into ignored project cache. |
| Fast PR quality gate | PASS | Final `make pr-check` passed Go formatting/tidy/module verification/vet/unit/race/build checks, npm lock installs, contract parsing/type generation/tests, SDK/demo tests, and documentation links. |
| Root + nested Go modules | PASS | Integration gate ran tidy-diff, module verification, vet, unit/race tests and builds for root, `sdk/go`, `examples/sdk`. |
| Real NATS and repeated fault injection | PASS | Existing adapter/bootstrap race suite plus new three-cycle concurrent-publish/cleanup test executed against real NATS. |
| Current-source credential patterns and dependency audits | PASS within scan scope | Final `make security-check` passed the targeted source scan, four npm lock graphs and Go vulnerability checks in all three modules. Root `govulncheck` found zero vulnerable symbols/imported packages and one module-only advisory, `GO-2026-5932` (`golang.org/x/crypto@v0.57.0`, `openpgp`, no fixed version listed); see [security audit](SECURITY_AUDIT.md). |
| Real PostgreSQL migrations/RBAC/revocation/outage | PASS | Full integration gate executed migrations up/down, rollback, session/key expiry, concurrent revoke/use, isolation, DB stop/restart and managed/ENV failure behavior under race instrumentation. |
| OpenAPI/AsyncAPI and generated TypeScript | PASS | `make integration-check` and final `make contracts-test` passed local reference/example validation, Gin route/security alignment (including `/ws` 426 error body), runtime payload assertions, PostgreSQL-backed Management contract smoke, and strict temporary TypeScript generation. |
| Chromium, packed SDK, CLI and container crash recovery | PASS | `make integration-check` passed real Go/npm external consumers, native CLI, single/distributed browsers, fresh-token SIGKILL recovery, peer health, scope isolation and cleanup. |
| Local performance/profiling | PASS within workload boundaries | 200-event baseline, 1,000-event repeat and matched eight-client/32-KiB control recorded with CPU/heap profiles. No production saturation capacity claim. |
| Git history, remote/provider, full-history secret scan | BLOCKED | No usable Git repository/remote in this checkout. No provider CI workflow guessed. |
| Production capacity, deployment TLS/proxy, DB backup restore | NOT VERIFIED | Local host tests do not validate a production installation, capacity envelope or DR plan. |
| Public release ownership/license/provenance | BLOCKED | Existing release decisions remain unresolved; no publication or external mutation occurred. |

Detailed local evidence is generated under `bin/check-integration.json`,
`bin/check-security.json`, `bin/security/` and `bin/performance-*`. These files
are ignored generated outputs; this report records the results for source review.

## Remaining production blockers

Confirm repository/provider and establish CI on prepared runners; approve public
identity/license/security-reporting decisions separately. Complete deployment-
specific TLS/proxy and network policy, distributed edge rate limits, DB/session/
audit retention and backup restoration, alerting, and container OS/provenance
review. Measure an isolated multi-process soak/capacity envelope before selecting
production connection/queue/container limits. Per-client JWT revocation and
cluster aggregation are not implemented capabilities.

Phase 5D has since completed the contract audit and readiness decision. See
[Phase 5D report](PHASE_5D_REPORT.md) for the conditional frontend GO and the
remaining production/release blockers. No Dashboard was implemented here.
