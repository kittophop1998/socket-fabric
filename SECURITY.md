# Security and vulnerability reporting

No public release or support window has been verified for this checkout. Public repository
ownership and a verified private reporting contact/channel are **BLOCKED** release
decisions; no email address or hosting provider is invented here. Before public
release, the owner must publish a monitored private reporting route and define
supported versions, response expectations and disclosure coordination.

Do not post signing secrets, publisher keys, broker tokens, user credentials,
private event data or exploit details in public issues. Use the owner-confirmed
private channel when established. This document does not send reports or change
external accounts. A bug report can describe affected versions, expected/actual
behavior and a minimal sanitized reproduction without including live credentials.

## Trust boundaries

- Publisher API keys are project-scoped backend/CI credentials. Browsers receive
  only short-lived scoped client tokens, never publisher/signing/broker secrets.
- The client signing secret is installation-wide issuance authority. A trusted
  backend must authenticate its user and authorize project/rooms before signing.
  SDK constructors do not replace application authorization.
- NATS is trusted private infrastructure. Its server-only credential can submit
  broker events; never expose it to browsers or untrusted applications.
- Operator Bearer sessions are separate from client JWTs and publisher keys.
  PostgreSQL contains password hashes and secret verifiers, not reusable plaintext
  session/key secrets. Protect DB backups and explicit provisioning/migration
  commands. See [Management security/failure semantics](docs/MANAGEMENT.md) for
  TLS/proxy trust, permissions, audit gaps and the five-second policy age limit.
- Preserve origin checks, session expiry, heartbeat, bounded queues, connection
  limits and slow-client disconnects. Diagnostic HTTP success is not complete
  cluster health or browser delivery confirmation.
- Use HTTPS for application/publish traffic and WSS for browser connections
  across trust boundaries. Protect trusted TLS termination, server ENV and the
  application identity system. The local Basic/ENV example is not a production
  account service or rate limiter.
- Reconnect authenticates again and re-authorizes subscriptions. Messages while
  disconnected may be missed. There is no durable delivery, replay or client ACK.

## Supply chain and artifacts

Run the [release quality gate](CONTRIBUTING.md) before distribution. It verifies
Go modules, npm audit results, optional pinned Go vulnerability scans, bounded
npm tarball paths/contents, checksums, targeted credential patterns and real
external consumers. The fallback pattern scanner does not prove that arbitrary
secrets are absent. Full history cannot be audited in a checkout without Git.
Use a reviewed history scanner once the real repository is available.

`SHA256SUMS` detects changes relative to a trusted checksum list. It does not
authenticate the publisher when both files and checksums come from an untrusted
source. Obtain expected hashes from the owner-confirmed release channel and
verify before installation. No signature or provenance attestation is created
by local preparation. See [release findings](docs/RELEASE_READINESS.md).

The generated third-party notice file preserves existing Go/JWT notices; it does
not license SocketFabric itself. Project licensing and dependency/license review
remain owner responsibilities. Never automatically rotate production credentials
or alter dependency/account ownership in response to a scanner finding; review
impact and apply the smallest authorized fix with regression checks.
