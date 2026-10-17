# Phase 5A — Management API, RBAC and PostgreSQL

## Agreed design

- Management is opt-in. Preserve HTTP/WebSocket, SDK, CLI and ENV publisher behavior.
- Opaque operator Bearer sessions; local trusted operator provisioning, no registration.
- ENV project IDs remain reserved and unmanaged. DB projects use managed keys.
- PostgreSQL failure leaves the ENV data plane running. Managed authentication fails closed.
- Refresh bounded project policy every two seconds; stop managed admission and initiate
  session closure when the last successful read is five seconds old. Disabled projects
  close existing sessions. Already admitted/in-flight messages cannot be recalled.
- No dashboard, durable messaging, new public releases or protocol changes.

## Implementation

1. Add pgx/v5 persistence, versioned explicit migrations, operators/sessions/projects/
   credentials/audits, transactional audit writes, pool deadlines and lifecycle cleanup.
2. Add Argon2id passwords, 256-bit hashed opaque sessions/keys, login throttling,
   server-side RBAC and HTTPS/trusted-proxy checks under `/admin/v1`.
3. Add project CRUD/status, cursor pagination, one-time key creation, metadata listing,
   expiry/revocation and safe local-node diagnostics. Check managed keys directly in DB.
4. Integrate project policy in realtime use cases/local routing. Support DB-only projects
   and bounded validated NATS wildcard subscriptions in management mode; retain NoEcho.
5. Extend existing build/Compose/quality tooling and docs; add real isolated PostgreSQL,
   NATS and management integration tests without changing existing regression contracts.

## Validation

Run root and nested Go tidy/format/vet/unit/race/build; explicit migration up/down/up;
real PostgreSQL transactions/auth/RBAC/key lifecycle/outage recovery; single/distributed
delivery/isolation/disable/stale-policy tests; existing SDK/CLI/npm/browser/NATS suites;
Docker builds/startup/shutdown and fixture cleanup. Record only actually executed checks.

## Evidence

Verified on 2026-10-16 with Go 1.27.2, Docker Compose 5.2.0, an isolated
`postgres:18-bookworm` container and real NATS 2.15.1. Previous phase records are
not used as evidence for the new management implementation.

| Check | Actual result |
| --- | --- |
| Existing and new root unit tests | PASS |
| Real PostgreSQL migrations up/idempotent up/down/up, constraints and transaction rollback | PASS with `-race` |
| Operator bootstrap/no registration, login/logout/expiry/password-reset/disable revocation | PASS with real PostgreSQL |
| Admin/Viewer checks, body/content-type limits, project validation/pagination, safe errors | PASS |
| Credential hash verification, one-time output, expiry/revocation, concurrent creation and audit | PASS |
| Actual Go SDK -> existing Publish API -> authorized WebSockets on separate NATS gateways | PASS; includes multiple connections/user, room/project isolation, malformed broker envelope and no routing-loop echoes |
| Project disable, NATS outage/recovery and PostgreSQL outage/recovery | PASS with `-race` |
| ENV publishing during DB failure; startup with absent DB; managed-only readiness; fresh reconnect after recovery | PASS |
| Direct HTTPS login, rejection of untrusted forwarding headers and credential namespace separation | PASS |
| Actual Docker image: explicit migrations/bootstrap, API/CLI status/ping/config/publish, bad-key rejection, exit-0 shutdown and secret-safe logs | PASS |
| Fixture cleanup of schemas/processes/containers/volumes/networks | PASS |
| Complete root/nested/SDK/CLI/browser/distribution quality gate | PASS — 21 local checks |
| Root, Go SDK and example tidy/verify/vet/unit/race/build | PASS on final code |
| TypeScript strict checks/tests, existing demo lifecycle tests | PASS — 25 SDK tests and 9 demo tests |
| npm audit (all three lock graphs), Go vulnerability checks (all three modules) | PASS; no reachable Go findings; previous unimported OpenPGP advisory is not a claim of a clean module graph |
| Six CLI cross-builds/repeated hashes, npm pack/independent installation, checksums | PASS; native Linux execution only, foreign-platform runtime NOT VERIFIED |
| Actual CLI/npm artifacts in external Go/browser integration, both Compose modes | PASS, includes unchanged SDK/CLI contracts, reconnect/expiry/isolation/outage behavior |
| Documentation relative links and targeted source/artifact credential scans | PASS within documented scanner coverage |
| Git provenance/history audit, ownership/license/public installation | BLOCKED as before; two existing provenance/distribution gate records |

Final command:

```sh
NATS_SERVER_BIN=/tmp/socketfabric-nats-server-2.15.1 \
GOVULNCHECK_BIN=/tmp/socketfabric-phase4d-tools/govulncheck \
  node scripts/check.mjs --artifacts bin/release-phase5a
```

Those two tool paths identify this machine's actual evidence, not portable setup
paths. The reproducible prerequisite instructions remain in CONTRIBUTING/README.
The machine-readable result is the ignored `bin/release-verification.json`
(21 PASS, two existing BLOCKED records); local distribution/checksum evidence is
under ignored `bin/release-phase5a/`. No package, tag, binary or release was published.

The test runner initially exposed two fixture issues (quoted schema search paths
and Docker-assigned ports changing on restart); both were corrected. The final
real-container runs above passed after those fixes. These are functional checks,
not throughput/load benchmarks or externally hosted deployment verification.

## Changed files

- `cmd/manage`: separate trusted migration/operator executable and parsing tests.
- `internal/domain/management.go`, application management ports/use cases and
  project-policy worker: entities, permissions, transactional orchestration,
  bounded policy, credential/session verification and audit rules.
- Outbound `postgres` adapter/embedded SQL migrations and `auth/operator.go`:
  pgx pooling/parameterized SQL, schema verification, explicit migrations,
  Argon2id passwords, opaque verifier storage and focused tests.
- Existing auth/realtime/NATS/memory adapters and application service: optional
  policy integration, DB-only projects, bounded validated wildcard consumption,
  existing ENV fast path, unchanged event envelope and NoEcho semantics.
- Existing inbound HTTP/router/gateway/session and bootstrap/config: admin APIs,
  HTTPS/proxy policy, local login throttling, RBAC, snapshots, failure mapping,
  readiness and DB/worker lifecycle; management-disabled defaults remain intact.
- Dockerfile, both existing Compose files and optional management overlay:
  management executable and opt-in development PostgreSQL.
- `tests/management`, PostgreSQL test support and integration tests: isolated real
  database/NATS/outage/Docker/CLI checks with generated credentials and cleanup.
- Existing release/check scripts, tests and Makefile: include management gates,
  strip DB settings from build environments and scan new credential formats.
- ENV example, README, PROJECT_CONTEXT, Management/CLI/release/contribution/
  security docs and changelog: actual contracts, setup, permissions and limitations.

## Remaining limits and recommendation

- Direct indexed DB reads are intentional for managed publisher auth; no
  credential cache or production throughput claim. Existing admitted requests
  may complete after revocation. Policy cutoff/close handshakes are documented.
- Integration used local development PostgreSQL transport and a direct test HTTPS
  server. A production CA/proxy/PostgreSQL TLS deployment, load tests and native
  non-host execution are NOT VERIFIED by these local checks.
- Diagnostics and rate limits are local-node only. Session/audit retention, edge
  rate limits, least-privilege DB grants, verified TLS/proxy deployment and load
  testing require operator policy. Audits are not a tamper-proof ledger.
- No individual client JWT revocation/refresh API or browser storage policy is
  introduced. Dashboard, durable messaging, presence and public releases remain
  out of scope. Existing ownership/license/public-install blockers are unchanged.
- Recommend Phase 5B: a separately scoped Dashboard using these APIs, including
  a reviewed browser/BFF session and CSRF design before selecting frontend auth.
