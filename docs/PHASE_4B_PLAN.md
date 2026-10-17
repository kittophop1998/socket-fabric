# SocketFabric Phase 4B — Developer CLI

## Goal and boundaries

Build an independent, cross-platform `socketfabric` binary using the existing Go
SDK and public HTTP contracts. Preserve Phase 0–4A, project authorization,
single/distributed operation and best-effort delivery. No new server endpoints,
token signing, dashboard, persistence, replay, retries or releases.

## Implementation

- Add `cmd/socketfabric` with standard-library flags, explicit injected IO,
  environment lookup, HTTP client and context. Keep parsing, validation,
  execution and output small and testable. No server-internal/Gin/NATS imports.
- Reference the independent Go SDK with a local module replacement. Adjust the
  Docker build context/module metadata without changing the gateway entrypoint.
- Commands: help, version, status, ping, publish and config validate.
- Common flags before the command or after the final verb: `--url`, `--timeout`,
  `--project`, `--json` where relevant. Publish adds `--room`, `--event`, `--data`.
- Precedence: flags > environment > defaults. Environment: `SOCKETFABRIC_URL`
  (default `http://localhost:8080`), `SOCKETFABRIC_TIMEOUT` (default `5s`),
  `SOCKETFABRIC_PROJECT_ID`, `SOCKETFABRIC_API_KEY`. No API-key flag or automatic
  configuration persistence. Help/version bypass network configuration.
- Validate HTTP(S) URLs without userinfo/query/fragment, positive timeout,
  existing identifier rules and command-specific required credentials.
- Config validation makes no network requests; optional project/key values are
  checked when supplied. Output reveals only whether a key is configured.
- Status checks `/healthz` and `/readyz` concurrently under one timeout budget,
  reporting reachability, HTTP status, latency and validated endpoint state.
  Success requires both contracts. Ping checks `/healthz` for HTTP connectivity.
  Both describe one configured gateway, never whole-cluster or WebSocket health.
- Bound diagnostic response bodies, validate JSON/content type, disable
  redirects and omit publisher credentials from diagnostic requests.
- Publish through the SDK exactly once; accept any single JSON value including
  null. `--data -` reads bounded stdin. Enforce the existing 64 KiB request limit.
  Success reports acceptance and local queue count, never browser delivery.
- Respect context deadlines, SIGINT and SIGTERM, including pending stdin.
- Human output by default; `--json` for all result commands. Publish output omits
  payload data. Results go to stdout; sanitized errors go to stderr.
- Exit codes: 0 success, 1 remote/network/protocol/timeout/output failure,
  2 arguments/config/payload failure, 130 cancellation.
- Centralize fixed actionable errors for authentication, authorization,
  rate limiting and gateway failures; do not echo raw upstream/transport errors,
  credentials or payloads. Timeouts/503 can follow remote delivery: no retries.
- Add local/cross-platform build targets and ldflags metadata (dev/unknown
  defaults). Verify Linux/macOS/Windows amd64 and arm64 builds.

## Validation

- Focused httptest coverage: parsing/help/version; precedence; validation;
  no-network config checks; JSON/null/stdin/size; publish wire contract;
  HTTP 400/401/403/429/500/503; malformed responses/redirects; timeout/cancel;
  one attempt; diagnostics; output/exit codes; credential redaction.
- Run root and independent-module tidy/format/vet/test/race/build checks,
  real-NATS integration tests, TypeScript SDK checks and demo regressions.
- Build existing Docker Compose single/distributed configurations.
- Extend the existing integration runner with optional CLI checks. Use isolated
  temporary credentials/ports/projects, actual authorized browser subscribers,
  and guaranteed cleanup. Exercise both modes, either distributed gateway,
  isolation, bad auth, unavailable gateway and broker readiness failure.
- Run the built CLI from an external directory and verify its dependency graph.
  Cross-compilation proves builds, not native execution on macOS/Windows.

## Documentation and completion

Add a CLI usage guide and update README, ENV examples and project context with
local install/build, configuration, commands, stdin/CI/JSON examples, exit codes,
security and delivery limitations. Report actual checks and changed files;
separate skipped checks from successful ones. Do not publish binaries or start
Phase 4C.

## Completion and evidence — 2026-10-16

Phase 4B is implemented. Core HTTP/WebSocket contracts and engine source remain
unchanged. The native CLI is available at `bin/socketfabric`; six cross-platform
artifacts are under `bin/` (ignored build outputs).

### Created source/documentation files

- `cmd/socketfabric/main.go`
- `cmd/socketfabric/config.go`
- `cmd/socketfabric/diagnostics.go`
- `cmd/socketfabric/publish.go`
- `cmd/socketfabric/main_test.go`
- `cmd/socketfabric/http_test.go`
- `Makefile`
- `docs/PHASE_4B_PLAN.md`
- `docs/CLI.md`

### Modified files

- `go.mod`: local dependency on the independent Go SDK.
- `Dockerfile`, `.dockerignore`: include SDK module metadata for module resolution;
  the final image still contains only the existing gateway binary.
- `.env.example`, `README.md`, `docs/PROJECT_CONTEXT.md`: CLI configuration,
  commands, build/run instructions and phase boundaries.
- `tests/sdk/run.mjs`, `tests/sdk/README.md`: optional standalone CLI conformance
  checks alongside the existing SDK/browser scenarios.

### Checks executed

| Check | Result |
| --- | --- |
| Root `go mod tidy`, `go fmt ./...`, `go vet ./...` | Passed |
| Root `go test ./...`, `go test -race ./...`, `go build ./...` | Passed |
| `NATS_SERVER_BIN=/tmp/socketfabric-nats-server-2.15.1 go test -race -tags integration ./...` | Passed with a real authenticated NATS server |
| Go SDK tidy/format/vet/unit/race/build | Passed |
| Separate Go SDK example tidy/format/vet/test/build | Passed; example has no unit tests |
| TypeScript SDK typecheck and tests | Passed, 25 tests |
| Demo generated-JS freshness and deterministic tests | Passed, 9 tests |
| `make cli cli-cross` | Passed; Linux/macOS/Windows amd64 + arm64 |
| `go run ./cmd/socketfabric version --json` | Passed |
| Standalone Linux binary, blocked stdin + SIGINT/SIGTERM | Passed; both exit 130 with a sanitized cancellation error |
| `node tests/sdk/run.mjs --cli` | Passed after the final stdin fix; includes both Compose builds and actual Chromium subscribers |

The conformance runner verified external Go/npm consumers, SSR/types, standalone
CLI dependencies and operation from an external directory without server/broker
credentials, single/distributed status and ping, stdin publishing to authorized
browsers on either gateway, local queue counts, room/project isolation, HTTP
401/403, broker outage with liveness 200/readiness 503/publish 503, unavailable
gateway failures, recovery, token expiry, normal shutdown and secret-safe output.
Both isolated Compose projects and temporary credentials were removed in cleanup.

Runtime checks used Linux. macOS/Windows binaries were cross-compiled and their
formats verified; native execution on those systems was not performed. The final
host CLI reports Go 1.27.2; the gateway Dockerfile remains pinned to Go 1.27.1.
No performance benchmark, binary publication or release pipeline was performed.
Publish acceptance remains best-effort, not browser receipt or a cluster-wide
subscriber count. Next: integrate the CLI into a real external project's trusted
developer/CI workflow and exercise its native operating-system environments.
