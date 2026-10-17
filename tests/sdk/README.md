# External SDK, application and CLI integration

This optional runner installs a real private npm tarball into an independent
consumer, builds a separate Go application with the existing SDK, and exercises
single-node and distributed gateways with NATS and actual Chromium sessions.
It does not publish packages, add Core APIs or change delivery semantics.

Requirements: Go 1.27.2 or newer, Node.js 22 or newer, npm, Docker Compose, and
Chromium managed by Playwright. From the repository root:

```sh
npm --prefix sdk/typescript ci
npm --prefix tests/sdk ci
cd tests/sdk
npx playwright install chromium
cd ../..
node tests/sdk/run.mjs
# Include the existing standalone CLI conformance checks:
node tests/sdk/run.mjs --cli
# Exercise previously prepared distribution artifacts instead of rebuilding them:
node tests/sdk/run.mjs --cli \
  --cli-bin bin/release/cli/socketfabric-dev-linux-amd64 \
  --npm-tarball bin/release/npm/socketfabric-client-0.1.0.tgz
```

The runner copies the example's locked TypeScript build tooling into the
temporary consumer and installs it there; a separate example `npm ci` is not
required. It builds Core with the existing Dockerfile and both Compose files.
Artifact flags are optional: the default behavior remains the Phase 4C flow.
The tarball is inspected before installation; a supplied native CLI is used
directly from an external working directory. Select the host's OS/architecture.
The [release gate](../../CONTRIBUTING.md) selects these paths from the manifest.

## Evidence covered

- npm build/pack and artifact file allowlist; no source, tests, ENV or Node
  runtime dependencies; actual `.tgz` installation into an independent consumer.
- ESM/package exports, NodeNext declarations/negative type check, SSR import
  with no usable WebSocket global, and plain TypeScript/esbuild browser bundle
  using the package name, without source aliases or Node polyfills.
- An independent Go module outside the checkout, copied public SDK module, no
  consumer `replace`, and a dependency graph without Gin/NATS/server internals.
  A temporary Go workspace supplies the provisional `socketfabric/sdk/go` path;
  the consumer does not require an unresolvable public `v0.0.0` version. This is
  local development proof, **not public Go installation**. Canonical VCS/module
  URL, tag, package license and npm scope ownership remain release blockers.
- Actual Go HTTP business API -> Go SDK -> gateway/NATS -> installed TypeScript
  SDK -> two browser UIs, in single mode and across separate distributed nodes.
- Authenticated application-owned token endpoint; three server-owned user
  project/room policies; unauthorized application/publisher/token/subscription
  rejection, project/room isolation, exact envelopes and no routing-loop echoes.
- Explicit subscribe/unsubscribe, idempotent join, fresh-token manual refresh,
  token expiry, terminal policy close, expired-token authentication rejection,
  fresh-token automatic gateway recovery and acknowledged subscription restore.
- Intentional disconnect stops recovery; broker outage leaves sockets intact
  while health stays 200, readiness fails and publishing reports 503. Unavailable
  gateway errors, offline subscription behavior and graceful shutdown are covered.
- Only fixed built public assets are served. No publisher/signing/broker secrets
  reach browser requests, WebSocket frames, DOM, bundle or logs. No browser
  storage is used; arbitrary resource data cannot inject HTML.

With `--cli`, the runner builds the standalone CLI, verifies its dependency graph
and operation from an external working directory without signing/broker secrets,
and checks version/config/status/ping, stdin publication to real subscribers,
local queue counts, 401/403/503, unavailable gateway exit codes and redaction.

Every run uses random loopback ports, isolated single/distributed Compose
projects, generated secrets and temporary consumer directories. The Go example
processes, Chromium, containers, networks and temporary credentials are cleaned
up in `finally`, including after assertions fail. Cleanup errors fail the runner.
Do not interrupt it with SIGKILL if you need automatic cleanup. No output should
be interpreted as load testing, public registry installation, complete cluster
health or browser delivery guarantees.

Unit checks remain separate; root Go checks exclude nested modules:

```sh
(cd sdk/go && go vet ./... && go test -race ./...)
(cd examples/sdk && go vet ./... && go test -race ./...)
npm --prefix sdk/typescript run typecheck
npm --prefix sdk/typescript test
```

See [developer onboarding](../../docs/QUICKSTART.md) for the manual setup and
[example reference](../../examples/sdk/README.md) for its application API/policy.
