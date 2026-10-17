# Phase 5C security audit

Scope: local Gateway, operator/publisher/client authentication, PostgreSQL control
plane, NATS Core routing, HTTP/WS input, SDK/CLI boundaries, build artifacts and
Docker. This is a source review and regression exercise, not a penetration test
of a deployed installation or a production-readiness certification.

## Findings and remediation

| Finding | Severity / impact | Evidence and disposition |
| --- | --- | --- |
| Realtime containers lacked enforced filesystem/capability/log controls; broker image had no explicit non-root user metadata | Operational hardening; increases post-compromise and disk-exhaustion impact | Both Gateway Compose definitions and NATS Core now use read-only roots, dropped capabilities, no-new-privileges and 10m/3-file log rotation. The Gateway image already runs as non-root; Core NATS explicitly runs as UID/GID 65532. Runtime assertions inspect these controls without printing container ENV. |
| Phase 5B NATS regression prerequisite was not configured | Verification gap | Existing v2.15.1 binary was discovered and version checked. `make test-tools` now provisions the pinned NATS/server scanner into ignored `.cache/test-tools`; quality gates resolve that path or an explicit ENV override and reject a mismatched NATS version. |
| Contract prose conflated key revocation with cached project propagation | Low; could mislead credential-rotation procedures | Corrected prose: managed keys are checked on every new publish against the primary; only WS/project policy uses a bounded snapshot. In-flight authorization is not cancelled globally. Corrected existing `/ws` 426/error-header documentation without changing behavior. |
| `GO-2026-5932` in `golang.org/x/crypto@v0.57.0` | Residual, module-only; current code does not import the affected `openpgp` package | `govulncheck` reports zero vulnerable symbols and zero vulnerable imported packages in the root module, plus this one module-level advisory. The advisory has no listed fixed version. The security gate passes its documented reachability policy; this is not a clean dependency graph or a claim that the package is safe if introduced. Re-run the pinned scanner after dependency changes and review upstream remediation before release. |
| Git history/provenance unavailable | BLOCKED | `.git` is a placeholder with no usable history/remote. Full-history secret scanning and CI-provider identification cannot be verified here. No provider workflow or repository identity was invented. |

No verified auth/RBAC/project isolation bypass was found in the reviewed flows.
No core authentication redesign or speculative dependency update was applied.
See [Phase 5C evidence](PHASE_5C_REPORT.md) for executed checks and their limits.

## Reviewed controls

- Client JWTs require HS256, issuer, audience, issued-at, expiry, valid project,
  user and explicit allowed rooms; maximum lifetime is 15 minutes. Sessions
  reauthorize on connect/join and clear room membership on disconnect.
- Operator tokens and publisher keys use separate opaque namespaces and random
  256-bit verifiers. PostgreSQL stores SHA-256 verifier hashes, not plaintext.
  Operator passwords use the fixed Argon2id parameters already implemented;
  expensive work is capped at four concurrent hashes. Malformed stored
  parameters cannot select arbitrary hash memory/CPU cost.
- Authentication reads session/key expiry and revocation from the primary.
  Admin/Viewer permissions are checked in handlers and use cases; Viewer does
  not list keys or audit logs. Project-scoped revoke queries cannot affect keys
  belonging to another project. Mutations and audits share a transaction.
- Login limiting uses per-IP and normalized-account buckets with bounded
  memory; trusted forwarding applies only to configured proxy CIDRs. Bearer
  credentials are not cookies; there is no cookie session or permissive CORS
  middleware that would introduce cookie/CSRF authority.
- WS Origin is checked at upgrade. HTTP header/body/read deadlines and WS
  frame/auth/connection limits are in place. Binary/oversized/malformed frames
  have existing controlled error/close paths. One data writer owns each socket;
  overflow drops queued data and closes only the slow client.
- SQL values are parameterized. Dynamic SQL suffixes and table names are fixed
  internal strings; migrations use an advisory transaction lock/checksum and
  never run implicitly at server startup.
- HTTP logs use registered route templates rather than raw paths/query values;
  broker/DB errors are sanitized. Request bodies, API keys, JWTs and password
  hashes are not included in metrics/errors. Existing browser tests scan traffic,
  bundles, DOM and logs for server credentials using synthetic test secrets.

## Remaining deployment responsibilities

Login limits are local-node controls, not cluster-wide brute-force protection.
Apply edge connection/request limits and protect Management behind operator
network access. No distributed publish rate limiter was added. High-entropy
ENV credentials and TLS/WSS termination remain deployment responsibilities.

NATS is a trusted private transport: its shared server credential has authority
to submit valid project events. Keep it inaccessible to browsers/untrusted apps;
use NATS TLS and reviewed server permissions across trust boundaries. DB
credentials, backups and administrative CLI access are privileged authority.

ENV keys rotate by changing trusted configuration and restarting gateways;
managed keys rotate by creating a replacement, updating trusted publishers,
then revoking the old key. Revocation does not retract already authorized events.
Existing client JWTs cannot be individually revoked and installation signing-key
rotation requires coordinated gateway/application rollout.

Audit/session retention, disk/DB alerting, backup restoration, container OS
vulnerability scanning, artifact provenance/signing and a monitored private
security contact remain operator/release work. Current-source pattern scanning
is bounded evidence, not proof that arbitrary credentials never existed.
No `trivy`, `grype`, `gitleaks` or `trufflehog` executable was available in this
environment; image OS and comprehensive history scans remain NOT VERIFIED.
