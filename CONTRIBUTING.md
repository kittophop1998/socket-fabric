# Contributing to SocketFabric

Start with [project context](docs/PROJECT_CONTEXT.md), the [quick start](docs/QUICKSTART.md)
and the [release readiness report](docs/RELEASE_READINESS.md). Repository ownership,
license and public package identities are still unresolved. No contribution
license, CLA or DCO policy is selected by this document; the owner must establish
those terms before public distribution.

## Development boundaries

- Preserve `Adapters -> Application -> Domain`. Domain/application must not
  import Gin, WebSocket, NATS or other infrastructure packages.
- Keep project/room authorization, publisher/client credential separation,
  bounded queues, heartbeat and graceful shutdown intact.
- Preserve HTTP/WebSocket and SDK compatibility. Document changed behavior and
  test the actual protocol before expanding a public API.
- Delivery is best-effort. Do not claim browser delivery from HTTP acceptance,
  add hidden publish retries, or introduce persistence/replay without a separate
  approved design.
- Keep unrelated changes separate. Add focused tests for concurrency, lifecycle,
  authorization, external consumption and meaningful failures.
- Never commit filled ENV files, tokens, keys, account config or private payloads.
  See [security guidance](SECURITY.md).

## Checks

Use Go 1.27.2 or newer, Node 24.3 or newer for all checks (SDK-only work needs
Node 22), npm, Docker Compose and the pinned NATS server described in README.
Root `go test ./...` excludes the nested Go SDK and example modules.

```sh
go fmt ./...
go vet ./...
go test -race ./...
(cd sdk/go && go fmt ./... && go vet ./... && go test -race ./...)
(cd examples/sdk && go fmt ./... && go vet ./... && go test -race ./...)
npm --prefix sdk/typescript ci
npm --prefix sdk/typescript run typecheck
npm --prefix sdk/typescript test
node --test --test-isolation=none scripts/release.test.mjs
```

Provision pinned local tools (or set explicit paths to matching reviewed tools)
and install the browser prerequisite:

```sh
npm --prefix tests/sdk ci
(cd tests/sdk && npx playwright install chromium)
make test-tools
make pr-check
make integration-check
make security-check
# Includes local distribution reproducibility/packaging as well:
node scripts/check.mjs
```

Phase 5A also requires the PostgreSQL 18 Docker image. Its separate gate uses a
disposable Compose project and schemas, including actual DB stop/restart:

```sh
make management-test
```

Never point management integration tests at a production database. The complete
gate now includes this fixture and the existing real-NATS/browser regressions.

The gate checks formatting and module tidiness without rewriting source, runs
root/nested tests and race checks, builds/inspects local artifacts and exercises
them with the existing external Go/npm and Docker/NATS/Chromium runner. It also
audits npm dependencies and Go vulnerabilities in the security/full suites. A
missing scanner blocks those suites. Fast checks omit Docker/browser and advisory
network calls; integration adds real dependencies and browsers. Missing prerequisites or failed
technical checks fail the command. Ownership/license blockers remain separate
even when all local checks pass. No remote CI provider or publication is assumed.

`make benchmark` captures a bounded Linux baseline/profile, with no arbitrary
capacity threshold. Use a fresh output directory for a repeat; see
[operations](docs/PRODUCTION_OPERATIONS.md) and [performance methodology](docs/PERFORMANCE_BASELINE.md).

`bin/release` must be new for a build; the gate can verify/reuse matching artifacts
there. To rebuild changed inputs, choose a new directory with
`node scripts/release.mjs build --out bin/release-new`, then run
`node scripts/check.mjs --artifacts bin/release-new`.

## Changes and review

Explain the concrete before/after behavior, affected contracts, relevant tests
and remaining limitations. Update the [Unreleased changelog](CHANGELOG.md) for
user-visible changes and the applicable SDK/CLI docs. Keep credentials out of
examples, screenshots, test output and review messages. Do not create tags,
publish packages, upload binaries or change account/repository settings as part
of routine contribution checks.
