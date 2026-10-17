# External application quick start

Run an independent Go business API and a bundled TypeScript browser consumer:

```text
Browser --application login--> Go example --Go SDK--> Gateway A
   |                              |                     |
   +--short-lived token-----------+                  NATS Core
   |                                                    |
   +--TypeScript SDK / authorized room--> Gateway A or Gateway B
```

The example owns user authentication, token issuance and resource validation.
SocketFabric owns connections and event routing. No Core source changes or new
Core APIs are needed. The resource action emits an event; it does not persist a
resource or implement a business database.

Requirements: a checkout of this repository, Go **1.27.2 or newer**, Node.js
**22 or newer**, npm, Docker with Compose, and a browser. The older built-in
`/demo` development checks require Node 24.3 or newer. Start in the repository
root. Use your team's repository URL; a canonical public Go repository/module
URL and release tag have not been selected. About ten minutes on a prepared
machine is an onboarding target, not a measured setup time.

## 1. Prepare private configuration

These Bash commands prompt for an application password, generate separate Core
credentials, and write a private, ignored `.env.integration`. Enter a unique
32–256 byte password and keep it in your password manager for the browser
login. Do not reuse a publisher key or signing secret as the application password.

```bash
umask 077
read -r -s -p 'Application password for alice (32–256 bytes): ' EXAMPLE_LOGIN_PASSWORD
printf '\n'
export EXAMPLE_LOGIN_PASSWORD
node --input-type=module <<'NODE' > .env.integration
import { randomBytes } from 'node:crypto';
const password = process.env.EXAMPLE_LOGIN_PASSWORD;
if (!password || Buffer.byteLength(password) < 32 || Buffer.byteLength(password) > 256 || /['\r\n]/.test(password)) {
  throw new Error('Use a 32–256 byte application password without single quotes or newlines');
}
const secret = () => randomBytes(32).toString('hex');
const publisher = secret();
const values = {
  APP_ENV: 'development', SOCKETFABRIC_MODE: 'single', HTTP_HOST: '0.0.0.0', HTTP_PORT: '8080',
  CLIENT_TOKEN_SECRET: secret(), PUBLISHER_KEY: publisher,
  PUBLISHER_KEYS_JSON: JSON.stringify({ 'project-a': publisher }), NATS_TOKEN: secret(),
  WS_ALLOWED_ORIGINS: 'http://127.0.0.1:3000', DEMO_ENABLED: 'false',
  SOCKETFABRIC_URL: 'http://127.0.0.1:8080', SOCKETFABRIC_TIMEOUT: '5s',
  SOCKETFABRIC_WS_URLS: 'ws://127.0.0.1:8080/ws',
  EXAMPLE_ADDR: '127.0.0.1:3000', EXAMPLE_ORIGIN: 'http://127.0.0.1:3000',
  EXAMPLE_TOKEN_TTL: '5m',
  EXAMPLE_USERS_JSON: JSON.stringify({ alice: { password, projectId: 'project-a', rooms: ['room-a'] } }),
};
for (const [name, value] of Object.entries(values)) console.log(`${name}='${value}'`);
NODE
unset EXAMPLE_LOGIN_PASSWORD
set -a
. ./.env.integration
set +a
```

The generated file contains secrets. Keep it private and never commit it, print
it to CI logs, serve the checkout with a generic static server, or share Compose
configuration output. Only source files you control: sourcing executes shell
code. The application and gateway read ENV directly; neither loads this file
automatically. Compose receives it explicitly below. Every additional trusted
terminal must source it using the three `set`/`.` commands above.

## 2. Start a single gateway

```bash
docker compose --env-file .env.integration -p socketfabric-onboarding up --build -d --wait
curl -fsS http://127.0.0.1:8080/healthz
curl -fsS http://127.0.0.1:8080/readyz
```

The Compose port is published on the host; use a trusted local machine/firewall
for this HTTP development setup. The application itself binds loopback only.
Single mode requires no NATS and ignores broker ENV.

## 3. Pack and install the actual browser SDK

```bash
mkdir -p bin/npm
npm --prefix sdk/typescript ci
npm --prefix sdk/typescript run typecheck
(cd sdk/typescript && npm pack --pack-destination ../../bin/npm)
npm --prefix examples/sdk ci
npm --prefix examples/sdk install --ignore-scripts --no-save --no-package-lock \
  "$PWD/bin/npm/socketfabric-client-0.1.0.tgz"
npm --prefix examples/sdk run build
```

The example imports `@socketfabric/client` through its package exports. esbuild
produces `examples/sdk/public/app.js`; Go serves only that file and the copied
HTML, never source, modules or ENV. No source alias or monorepo import is used.
Run the tarball install again after `npm ci`, which removes undeclared packages.
The SDK stays private; `npm pack` does not publish it.

## 4. Start the application backend

```bash
(cd examples/sdk && go mod tidy && go build -o ../../bin/socketfabric-example .)
EXAMPLE_PUBLIC_DIR="$PWD/examples/sdk/public" ./bin/socketfabric-example serve
```

This is a separate Go module using the existing Go SDK. Its checked-in local
`replace` supports checkout development. The integration runner additionally
builds a copied consumer outside the checkout with a temporary Go workspace and
no replacement in the consumer module. Neither proves public Go installation:
`socketfabric/sdk/go` is provisional and has no canonical VCS origin/version.
See [Go SDK installation](../sdk/go/README.md).

## 5. Connect two browser sessions

Open **http://127.0.0.1:3000** in two tabs/windows. Use that exact origin;
`localhost` is a different allowed origin.

1. Enter `alice` and your application password in both sessions.
2. Keep `room-a`, select Gateway 1, and click **Connect and subscribe**.
3. Click **Publish resource.updated** in one session. Both show
   `resource-42: completed` and the same event ID.
4. Click **Unsubscribe** in one session, publish a different resource ID, and
   confirm only the subscribed session updates. **Subscribe** joins again.
5. Enter `room-b` and subscribe: authorization rejects it. Set `room-a` again.
6. **Refresh token / reconnect** fetches a new token and explicitly subscribes.
   **Disconnect** closes the socket, clears room intent and in-memory credentials,
   and stops retries. Re-enter the password before another explicit connect.

The browser receives its own application login credential and a scoped client
JWT. It never receives a publisher API key, signing secret or broker token.
Nothing is stored in cookies, localStorage or sessionStorage. The backend rejects
client-selected token scopes and derives project/rooms from `EXAMPLE_USERS_JSON`.
Basic authentication is a small local example policy, not a full account system.
Use your existing application identity/authorization service for a deployed app.

To publish through the application API with curl from a trusted terminal, curl
prompts for the application password instead of putting it in process arguments:

```bash
curl --fail-with-body --user alice http://127.0.0.1:3000/api/resources/resource-43/update \
  -H 'Content-Type: application/json' \
  --data '{"roomId":"room-a","status":"updated"}'
```

HTTP 202 confirms API acceptance. The example does not retry a failed publish.
Timeouts/503 can have an unknown delivery outcome; blindly retrying may duplicate
events. Neither SDK provides persistence, replay, client ACK or delivery guarantees.

## 6. Repeat across two gateways and NATS

Stop the application with Ctrl+C, then run from the same configured root terminal:

```bash
docker compose --env-file .env.integration -p socketfabric-onboarding down
docker compose -f docker-compose.distributed.yml --env-file .env.integration \
  -p socketfabric-onboarding-distributed up --build -d --wait
export SOCKETFABRIC_WS_URLS=ws://127.0.0.1:8080/ws,ws://127.0.0.1:8081/ws
EXAMPLE_PUBLIC_DIR="$PWD/examples/sdk/public" ./bin/socketfabric-example serve
```

Reload both tabs. Select Gateway 1 in the first and Gateway 2 in the second,
authenticate and subscribe to `room-a`, then publish from either tab. The Go
backend publishes through Gateway A; NATS fans out to both gateways. NATS ports
remain private to Compose. Each gateway owns only local connections.

Unexpected gateway loss triggers SDK backoff/jitter, a fresh authenticated token
request, and re-subscription to acknowledged rooms. Tokens expire after five
minutes by default. Expiry/policy close `1008` stops automatic retries; use
**Refresh token / reconnect** explicitly. There is no refresh timer. Messages sent
while disconnected or before subscription recovery may be permanently missed.

## Diagnostics, verification and cleanup

In another trusted terminal, source `.env.integration` and build/use the CLI:

```bash
go build -o bin/socketfabric ./cmd/socketfabric
./bin/socketfabric status --url http://127.0.0.1:8080
./bin/socketfabric status --url http://127.0.0.1:8081 --json
./bin/socketfabric ping --url http://127.0.0.1:8080
SOCKETFABRIC_API_KEY="$PUBLISHER_KEY" ./bin/socketfabric publish --project project-a \
  --room room-a --event resource.updated \
  --data '{"resourceId":"cli-resource","status":"completed"}'
```

Status describes one gateway, never whole-cluster health. Ping proves HTTP
connectivity, not WebSocket delivery. CLI publication confirms acceptance only.
See [CLI reference](CLI.md) for CI, stdin and exit codes.

The [integration runner](../tests/sdk/README.md) repeats single/distributed browser
flows, three application user policies, project/room isolation, authorization
failures, expiry, fresh authentication, gateway/broker outages and cleanup. It
also installs a real npm artifact into a separate consumer and checks its SSR,
exports, declarations and browser bundle.

Stop the backend with Ctrl+C and remove the relevant Compose project:

```bash
docker compose --env-file .env.integration -p socketfabric-onboarding down
docker compose -f docker-compose.distributed.yml --env-file .env.integration \
  -p socketfabric-onboarding-distributed down
```

Keep/delete your private `.env.integration` according to your local secret policy.
Do not reuse generated development credentials in production.

| Symptom | Check |
| --- | --- |
| Application cannot start | Build assets first; supply valid distinct credentials, user policy and a loopback `EXAMPLE_ADDR` |
| Browser cannot authenticate | Application username/password, then token endpoint; do not enter a publisher key |
| `forbidden` subscription | Room must be in that user's server-owned token scope |
| WebSocket handshake rejected | Exact `EXAMPLE_ORIGIN` must appear in gateway `WS_ALLOWED_ORIGINS` |
| Connected but no event | Same project/room, acknowledged subscription, correct gateway URLs, `/readyz`; no offline replay |
| Health 200, readiness 503 | Distributed gateway's broker is unavailable; check private NATS connectivity |
| Token expiry stops reconnect | Expected policy `1008`; explicitly refresh, re-authenticate and subscribe |
| Go public install fails | Canonical public module URL/tag is still a release blocker; use documented local workspace/checkout workflow |

Use **HTTPS** for application/publish traffic and **WSS** for browser connections
across trust boundaries in production. Terminate TLS at a trusted proxy, retain
origin checks, protect the globally privileged signing secret, and apply your
application's account, rate-limit and secret-rotation policies. This phase adds
no rate limiter, production identity service or new Core feature. Public Go
origin/tag, package license and npm scope ownership remain release decisions;
no registry packages or binaries are published by these commands.
