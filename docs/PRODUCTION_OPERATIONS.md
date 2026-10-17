# Production operations

SocketFabric retains best-effort realtime delivery. Passing local checks does
not establish production capacity or availability for a particular deployment.
Start with [Management setup](MANAGEMENT.md), [quickstart](QUICKSTART.md),
[security findings](SECURITY_AUDIT.md) and the [measured baseline](PERFORMANCE_BASELINE.md).

## Deployment and trust boundaries

The existing single-node and two-gateway Compose files build the same non-root
multi-stage image. Gateways run with a read-only root filesystem, no Linux
capabilities, no privilege gain and local logs rotated at 10 MiB across three
files per container. No writable application volume is required. Custom CA
material/configuration should be mounted read-only. Test fixtures use generated
credentials, separate Compose projects and disposable databases; they do not
alter an existing deployment.

The bundled NATS **Core** service has the same filesystem/capability/log guards
and explicit UID/GID 65532. No broker storage directory is required in Core mode.
If supplying your own TLS files, grant that UID read access through read-only
mounts. The optional development PostgreSQL image retains its upstream volume
initialization behavior; it is not a production TLS/database deployment recipe.

Use HTTPS for publisher/operator traffic and WSS for browser traffic. Put a
TLS-terminating reverse proxy on a private network with the Gateway; do not
expose the backend listener around that proxy. Preserve the public Host and
WebSocket Upgrade/Connection headers. Preserve the browser Origin and configure
`WS_ALLOWED_ORIGINS` to the exact permitted public origins (no wildcard).
Set proxy WS idle timeouts above the configured heartbeat/pong budget; the
default heartbeat is 30s with a 10s pong budget. A proxy-specific configuration
has not been validated by these local Docker tests.

For Management, trust only the immediate TLS proxy's narrow CIDRs through
`MANAGEMENT_TRUSTED_PROXY_CIDRS`. The proxy must overwrite forwarded protocol
and construct a trustworthy X-Forwarded-For chain. Public clients must not be
able to reach Gateway from a trusted source address directly. Production rejects
`MANAGEMENT_ALLOW_INSECURE_HTTP=true` and requires PostgreSQL `sslmode=verify-full`.
Limit `/admin/v1` at the edge; keep broker/DB ports and broker monitoring private.

## Capacity and limits

Choose limits from workload tests and monitor memory/CPU, file descriptors, queue
pressure and DB pool pressure. There are no measured production capacity claims.
The baseline includes the SDK load generator in its Go process; it is not an
isolated production Gateway resource budget.

Default admission is 256 connections per node, 32 outbound messages per socket,
16 KiB inbound WS messages and 64 KiB publish bodies. Queue bounds are message
counts, not a small guaranteed memory footprint: 256 × 32 × approximately 64 KiB
is about 512 MiB of queued payload alone in the worst case, before Go/socket/JSON
overhead. Raising both limits to their configured maxima can require far more
memory. Set container limits only after checking the chosen payload/fanout case.

Argon2id login work can additionally consume roughly four × 19 MiB for concurrent
hashes; the default PostgreSQL pool is eight connections (maximum configurable
64). NATS subscriber pending limits and publish admission are bounded; overload
can reject/drop work and must not be treated as durable buffering. Slow clients
receive 1013 and lose pending events. Apply independent edge DoS controls.

## Health, failure and recovery

| Condition | Liveness / readiness | Behavior |
| --- | --- | --- |
| Broker disconnected | `/healthz` 200; `/readyz` 503 | Existing WS sessions/memberships remain. New sockets and publish reject until NATS reconnect/validation succeeds. No replay. |
| Management DB unavailable with ENV projects | Process/data-plane readiness can remain healthy | ENV publishing remains independent. Management and managed-key publishing fail closed. Fresh bounded project snapshots may temporarily authorize managed WS traffic; stale snapshots close affected sessions with 1013. |
| DB unavailable, managed projects only | Process remains live; data-plane readiness eventually fails | Readiness requires usable policy. Recovery refreshes DB/schema/policy before accepting managed work. |
| Managed project disabled | Policy propagates with configured refresh/max-age | Managed publishers consult primary project status; WS admission/routing and session closure follow bounded policy refresh. Existing JWTs gain no new rights. |
| Gateway crash/restart | That node is unavailable; peers continue | Local memory/queues disappear. SDK obtains authentication again and re-subscribes. Events during disconnect may be missed. |

HTTP publish success confirms local/broker acceptance under current mode, not
browser receipt. A broker timeout may return 503 after a remote node received
the event; callers decide whether retry duplicates are acceptable. No automatic
publish retry, client ACK, persistence or global ordering is introduced.

## Startup, shutdown and rollback

Bring up NATS before distributed gateways. Management migrations and operator
bootstrap are explicit trusted CLI actions; inspect/apply them before enabling
Management. Never run destructive `migrate down` against live data as a routine
application rollback. Back up PostgreSQL and test restore procedures separately.

On SIGTERM the server marks unready, closes admission, drains HTTP, closes active
WS sessions with 1001 and drains broker workers within `SHUTDOWN_TIMEOUT` (10s
default). Compose provides 15s before force termination. Keep the external
orchestrator budget larger than the Gateway budget; allow clients to reconnect
with jitter to healthy nodes. SIGKILL discards memory immediately.

Roll back by restoring the reviewed compatible image/configuration, retaining
schema/data and independent secrets. Validate `/readyz`, authenticated publish
and a scoped WS subscriber on each node. Observe reconnect and error rates;
avoid treating the local diagnostics node list as a cluster inventory.

## Reproducible checks and CI

```sh
make test-tools                 # pinned NATS v2.15.1 + govulncheck v1.9.0
npm --prefix tests/sdk ci
(cd tests/sdk && npx playwright install chromium)
make pr-check                   # formatting, root/nested Go, SDK/demo/contracts; no Docker
make contracts-test             # includes isolated PostgreSQL/Docker contract smoke
make integration-check          # fast checks + NATS/PG faults, CLI/SDK and Chromium
make security-check             # current-source patterns + npm/Go vulnerability DBs
node scripts/benchmark.mjs --out bin/performance-new --iterations 1000
```

Dependency/tool installation and advisory refresh need network access; subsequent
contract validation resolves local references. Security checks fail when their
scanner is missing or reports reachable vulnerabilities. Performance results
are recorded without an arbitrary throughput pass threshold. Exact toolchain
versions, workload, profiles and evidence are stored under ignored `bin/`.

The Git placeholder reveals no hosting provider. Wire `make pr-check` into the
confirmed provider's PR pipeline and schedule the expensive integration/security/
benchmark suites on prepared runners. No GitHub/GitLab workflow, account, remote
resource or deployment has been created here. Release ownership/license/history
blockers still apply independently of these technical checks.
