# Management API (Phase 5A)

The optional control plane adds operators, project configuration and publisher
credentials to the existing gateway. It shares the Gin listener and lifecycle;
PostgreSQL never participates in individual WebSocket writes or room broadcasts.
Delivery remains best-effort. There is no dashboard, registration, replay or ACK.

## Local setup

Requirements: existing Go 1.27.2/Docker Compose tooling, Node for the shell JSON
helpers below, and PostgreSQL 18. The optional Compose file is for development;
its DB owner is not an appropriate production runtime account.

From the checkout, use a fresh development environment. These commands generate
secrets in memory; they do not write a filled ENV file or embed credentials in images.

```bash
export APP_ENV=development MANAGEMENT_ENABLED=true MANAGEMENT_ALLOW_INSECURE_HTTP=true
export CLIENT_TOKEN_SECRET="$(openssl rand -hex 32)"
export POSTGRES_PASSWORD="$(openssl rand -hex 32)"
export PUBLISHER_KEYS_JSON='{}'
export DATABASE_URL="postgres://socketfabric_owner:${POSTGRES_PASSWORD}@postgres:5432/socketfabric?sslmode=disable"

docker compose -f docker-compose.yml -f docker-compose.management.yml up -d --wait postgres
docker compose -f docker-compose.yml -f docker-compose.management.yml build app
docker compose -f docker-compose.yml -f docker-compose.management.yml run --rm --no-deps \
  --entrypoint /app/socketfabric-manage app migrate up

read -r -s -p 'Initial admin password (15–128 characters): ' MANAGEMENT_PASSWORD
printf '\n'
printf '%s' "$MANAGEMENT_PASSWORD" | docker compose -f docker-compose.yml \
  -f docker-compose.management.yml run --rm --no-deps -T \
  --entrypoint /app/socketfabric-manage app operator bootstrap-admin \
  --email admin@example.com --password-stdin

docker compose -f docker-compose.yml -f docker-compose.management.yml up -d app
```

Keep the generated database/signing secrets for subsequent runs in your secret
manager. Do not regenerate them against an existing database by repeating this
snippet. An occupied host DB port can be changed with `MANAGEMENT_DB_PORT`.
For native Go commands replace the DSN hostname `postgres` with `127.0.0.1` and
the published database port; `go run ./cmd/manage --help` shows the trusted commands.

Login with the password on stdin, capturing the session without printing it:

```bash
OPERATOR_TOKEN=$(printf '%s' "$MANAGEMENT_PASSWORD" |
  node --input-type=module -e 'import{readFileSync}from"node:fs";process.stdout.write(JSON.stringify({email:"admin@example.com",password:readFileSync(0,"utf8")}))' |
  curl --fail-with-body --silent --show-error http://localhost:8080/admin/v1/auth/login \
    --header 'Content-Type: application/json' --data-binary @- |
  node --input-type=module -e 'import{readFileSync}from"node:fs";process.stdout.write(JSON.parse(readFileSync(0,"utf8")).token)')
unset MANAGEMENT_PASSWORD

# Send Authorization through curl stdin instead of its process arguments.
admin_curl() {
  printf 'header = "Authorization: Bearer %s"\n' "$OPERATOR_TOKEN" |
    curl --fail-with-body --silent --show-error --config - "$@"
}
admin_curl http://localhost:8080/admin/v1/auth/me
admin_curl http://localhost:8080/admin/v1/projects \
  --header 'Content-Type: application/json' --data '{"id":"managed-a","name":"Managed A"}'
MANAGED_PUBLISHER_KEY=$(admin_curl http://localhost:8080/admin/v1/projects/managed-a/keys \
  --header 'Content-Type: application/json' --data '{}' |
  node --input-type=module -e 'import{readFileSync}from"node:fs";process.stdout.write(JSON.parse(readFileSync(0,"utf8")).key)')
admin_curl http://localhost:8080/admin/v1/projects/managed-a/keys
```

Store the one-time publisher secret in the application's secret manager. Its
identifier/metadata remain listable, but the plaintext cannot be retrieved again.
If a creation response is lost, list and revoke the orphan key, then create another.
After project-policy propagation, the existing CLI/Go SDK accept this key:

```bash
SOCKETFABRIC_API_KEY="$MANAGED_PUBLISHER_KEY" SOCKETFABRIC_PROJECT_ID=managed-a \
  go run ./cmd/socketfabric publish --room room-123 --event resource.updated \
  --data '{"status":"completed"}'
admin_curl http://localhost:8080/admin/v1/diagnostics/overview
admin_curl http://localhost:8080/admin/v1/auth/logout --request POST
unset OPERATOR_TOKEN MANAGED_PUBLISHER_KEY
```

The application backend still authenticates its own users and issues scoped
client JWTs with the existing [Go SDK signer](../sdk/go/README.md). The JWT
issuer/audience/rooms/lifetime and WebSocket protocol are unchanged. The trusted
`cmd/token` helper continues to issue tokens only for ENV projects; use the SDK
signer in an authorized backend for DB projects. An operator session or publisher
key cannot sign client tokens and is not accepted as a WebSocket credential.

For distributed mode keep the same secrets and database for both gateways:

```bash
export NATS_TOKEN="$(openssl rand -hex 32)"
docker compose -f docker-compose.distributed.yml -f docker-compose.management.yml build
docker compose -f docker-compose.distributed.yml -f docker-compose.management.yml up -d
```

Stop the single-node `app` first if it occupies gateway A's port. Migrations and
initial bootstrap run only once per database; the existing distributed ports are
8080/8081. All gateways serving managed projects must enable Management and use
the same DB, signing secret, reserved ENV project IDs and policy settings.

## Configuration and readiness

| Variable | Default / rule |
| --- | --- |
| `MANAGEMENT_ENABLED` | `false`; no management routes/DB dependency when disabled |
| `DATABASE_URL` | Required PostgreSQL URL when enabled; never logged |
| `DATABASE_MAX_CONNS` | `8`, range 1–64 |
| `DATABASE_CONNECT_TIMEOUT` / `DATABASE_QUERY_TIMEOUT` | `2s` each; query timeout below policy max age |
| `MANAGEMENT_SESSION_TTL` | `15m`, positive and at most `1h`; no refresh endpoint |
| `MANAGEMENT_MAX_PROJECTS` | `128`, range 1–10,000, counts enabled and disabled DB projects |
| `MANAGEMENT_POLICY_REFRESH_INTERVAL` | `2s`, below max age |
| `MANAGEMENT_POLICY_MAX_AGE` | `5s`, positive and at most `5s` |
| `MANAGEMENT_ALLOW_INSECURE_HTTP` | `false`; explicit development/test opt-in only |
| `MANAGEMENT_TRUSTED_PROXY_CIDRS` | Empty; explicit TLS-terminating proxies only, no `/0` |

Invalid enabled configuration prevents startup. An unreachable DB, wrong DB
credentials, or an absent/incompatible schema disables Management at runtime and
is retried without stopping a configured ENV data plane. Startup never migrates.
Readiness is verified with queries, not merely pool construction.

- `/healthz` remains process liveness.
- `/readyz` still requires NATS in distributed mode. With ENV projects it does
  not depend on DB availability. Managed-only deployments additionally require
  a current successful DB/schema/policy check.
- `/admin/v1` returns `503` while its DB/schema/policy is unavailable. Even an
  existing operator token cannot bypass this check. After recovery the original
  unexpired/unrevoked session works again; there is no offline session cache.
- `socketfabric status`/`ping` inspect the data-plane endpoints, not control-plane
  or cluster-wide health. An authenticated `/admin/v1/auth/me` request checks
  control-plane availability and operator authentication.

## Authentication and permissions

No default operator exists. Bootstrap is an explicit trusted DB action and is
allowed only before any operator exists. Subsequent `operator create --role
admin|viewer`, `set-password`, `disable` and `enable` use the same trusted command.
There is no HTTP role mutation or public self-registration. DB access itself is
administrative authority; restrict this command and its provisioning credentials.

Passwords require 15–128 Unicode characters and use Argon2id (19 MiB, two passes,
one lane, random 16-byte salt). Four hashes can run concurrently per process.
Session/managed-key secrets use 256 random bits; only SHA-256 secret verifiers
are persisted. Tokens have different `sfop_`/`sfpk_` namespaces. Sessions are
checked against the DB and current operator status/role for each request. Password
changes and operator status changes revoke all that operator's sessions.

| Capability | Admin | Viewer |
| --- | --- | --- |
| Login, me, logout own session | Yes | Yes |
| List/detail DB projects | Yes | Yes |
| Create/update/enable/disable projects | Yes | No |
| List/create/revoke credential metadata | Yes | No |
| Local-node diagnostics | Yes | Yes |
| Audit log listing | Yes | No |

Login allows ten attempts/IP/minute and five attempts/account/15 minutes, with
generic credential errors and bounded limiter state. Limits count all attempts,
including successes, and apply per node. Use trusted edge rate limits for a
shared cluster budget; no distributed limiter or MFA is supplied here.

## API reference

All management routes are under `/admin/v1`. Authentication is
`Authorization: Bearer <operator-session>`; query parameters and cookies do not
authenticate. Responses use `Cache-Control: no-store`. JSON bodies are limited
to 16 KiB and reject unknown fields. Errors keep the existing
`{"error":{"code":"invalid_request","message":"Invalid request","requestId":"..."}}` structure.

| Method/path | Request / response |
| --- | --- |
| `POST /auth/login` | `{email,password}` → `{token,tokenType:"Bearer",expiresAt,operator}` |
| `POST /auth/logout` | Revoke current session; `204` |
| `GET /auth/me` | Current operator metadata; never password hash |
| `GET /projects` | `limit`, `cursor`, `status=enabled|disabled`, literal case-insensitive `name` filter |
| `POST /projects` | `{id,name,status?}`; default enabled; `201` |
| `GET /projects/:id` | Project metadata |
| `PATCH /projects/:id` | `{name?,status?}`; at least one field, ID immutable |
| `GET /projects/:id/keys` | Credential metadata list |
| `POST /projects/:id/keys` | `{expiresAt?: RFC3339 timestamp}` → `{credential,key}`; `201`, one-time key |
| `DELETE /projects/:id/keys/:keyId` | Revoke within that project; repeat is idempotent, `204` |
| `GET /diagnostics/overview` | Local node, broker, DB/policy and current metrics |
| `GET /diagnostics/nodes` | Exactly this node; cluster visibility unavailable |
| `GET /diagnostics/metrics` | Aggregate current-session/registry gauges and counters |
| `GET /audit-logs` | List, optional exact `action` / `actorId` filters |

Lists return `{items,nextCursor?}`. Default limit 25, maximum 100. Cursors are
literal stable IDs in ascending order; a full final page may return a cursor
whose next page is empty. They are not a global chronological ordering guarantee.
No key hashes/plaintexts, session verifiers or event payloads appear in listings.
Key status is `active`, `revoked` or derived `expired`. A disabled project can
have credentials prepared/rotated, but they cannot publish until re-enabled.

Errors: `400` validation, `401` invalid/expired/revoked auth, `403` role/project
denial or HTTPS requirement, `404` missing resource, `409` duplicate/reserved ID
or project capacity, `413` body limit, `415` content type, `429` login/hash capacity
with Retry-After, `503` unavailable dependency. Publish API acceptance remains
`202` with the existing envelope and local queue count, never delivery confirmation.

## Project, key and failure semantics

ENV authentication wins first and stays in memory. ENV project IDs are reserved,
are not imported/listed as DB projects, and cannot be disabled/revoked through
Management. Rotate ENV keys by changing trusted deployment configuration and
restarting the affected nodes. An ENV/DB ID collision causes managed policy to
fail closed; it does not change the ENV project's authority.

Every new managed-key publish performs indexed DB verification of hash, expiry,
revocation and project status. There are no hidden retries or credential caches.
After a revocation transaction commits, subsequent authentication queries on
nodes using that primary reject the key. Requests already authorized/in flight
can still finish. No claim of instantaneous global revocation is made.

Project policy refresh runs outside realtime routing. Changes/new projects
propagate through each node's bounded snapshot; use at most the configured max
age measured from the start of its successful read. Fresh policy may still
authorize signed WebSocket clients during a brief DB interruption. Once stale,
managed admission/routing fails closed and session closure starts (`1013`, retryable).
Disabled projects initiate policy closure (`1008`); clients must explicitly
reconnect and re-subscribe after re-enabling. Previously issued JWTs are not
individually revoked and remain subject to their original expiry and fresh project
authorization. Credentials remain revoked across disable/enable cycles.

Closing drops queued messages through the existing close path and removes room
memberships immediately. A close handshake is bounded by the existing write
timeout; an event already admitted/in flight may arrive. Refresh timing does not
constitute a real-time OS scheduling guarantee. Messages during outages or while
disconnected are permanently missable. Recovery never replays events.

Management-enabled NATS nodes use one bounded single-token wildcard subscription
with strict subject/envelope/project validation, existing NoEcho and local-only
fan-out. Management-disabled nodes retain their static ENV subscriptions. Broker
credentials remain a private infrastructure trust boundary, never operator auth.

## Audit, diagnostics and operations

Operator bootstrap/provisioning/security changes, successful login/logout and
project/key mutations persist audit records atomically. Failed logins and role
denials are audited when the DB is available; DB failures produce sanitized log
categories instead, leaving a documented persistent-audit gap. Metadata is an
allowlist of request correlation IDs; no body, password, token or key is recorded.
Audits have no update/delete API. They are ordinary DB records, not a tamper-proof
ledger; production DB grants/backup/retention policy remain operator responsibilities.

Diagnostics are approximate snapshots of this process: connections, memberships,
rooms, queue occupancy and active-session event counters. Closed sessions are
observable in existing logs; historical totals and other nodes are explicitly
unavailable. No global presence, tracing or cluster aggregation is inferred.

Production requires HTTPS/WSS. By default forwarding headers are ignored. When
using TLS termination, configure only the actual proxy CIDRs, ensure it overwrites
`X-Forwarded-Proto`/`X-Forwarded-For`, and block direct public access to the backend
listener. Production DB URLs require `sslmode=verify-full`. Use separate migration/
provisioning and runtime DB users, least-privilege table grants, verified backups
and a private broker/database network. Never store operator Bearer tokens in
browser localStorage; a future dashboard must design its own secure session flow.

Rollback is explicit and destructive; stop managed traffic and back up first:

```sh
go run ./cmd/manage migrate down --confirm-data-loss
go run ./cmd/manage migrate up
```

Down removes operators/sessions/projects/keys/audits and leaves an empty migration
ledger. Up recreates the schema, not deleted data. Runtime only checks version and
checksum and never repairs a mismatched schema. Tests exercise down/up exclusively
against disposable schemas. Manage expired/revoked session and audit retention
with an owner-reviewed DB maintenance policy; this phase adds no automatic audit deletion.

## Verification

```sh
NATS_SERVER_BIN=/absolute/path/to/nats-server node tests/management/run.mjs
```

The runner creates an isolated PostgreSQL Compose project/volume, unique schemas,
generated credentials and real NATS processes. It runs `-race` migration/store/
management HTTP and multi-node tests including actual PostgreSQL stop/restart.
Cleanup removes only its own schemas, containers, networks and volumes. Existing
SDK/CLI/browser tests remain in [their runner](../tests/sdk/README.md). Actual
executed results and changed files are recorded in [the Phase 5A report](PHASE_5A_PLAN.md).
