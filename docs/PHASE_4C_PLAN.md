# Phase 4C — External integration, packaging and onboarding

## Decisions

- Extend the existing independent `examples/sdk` Go module, preserving its
  `token` and `publish` commands. Add a small standard-library HTTP application.
- Authenticate example users with HTTP Basic and server-owned ENV project/room
  policy. Publisher keys and signing secrets remain server-only.
- Browser code uses TypeScript and esbuild, imports `@socketfabric/client`
  through its package exports, and installs the actual npm tarball.
- Fetch a fresh short-lived token on every connection/reconnection. Provide an
  explicit refresh/reconnect button; preserve terminal SDK policy close 1008.
- Keep Core, SDK public behavior, CLI and both deployment modes unchanged.
- The canonical Go repository/module URL is not yet supplied. Preserve the
  provisional module identity. Verify independent consumption using a temporary
  Go workspace with a copied SDK and no consumer `replace`; label this as local
  development proof, not registry/public VCS installation. Report the public
  module path, release tag, license and npm scope ownership as release blockers.

## Implementation

1. Add `serve` to the existing backend example. Serve only public HTML/built
   browser assets; never expose the working directory, source, ENV or modules.
2. Add application-owned `/api/config`, `POST /api/realtime-token` and
   `POST /api/resources/{id}/update`. Authenticate protected routes and derive
   project/rooms from server policy; validate room permission and typed payload
   before publishing fixed `resource.updated` through the existing Go SDK.
3. Add explicit server timeouts, context propagation, bounded request bodies,
   no-store responses, origin/host checks and controlled shutdown. Bind loopback
   by default. Use fixed sanitized errors and log no credentials/payloads.
4. Replace manual token entry with application login, configured gateway
   selection, subscribe/unsubscribe, refresh and disconnect. Keep credentials
   and tokens only in memory; cancel fetches/listeners on cleanup. Bound UI logs
   and render events as text. Preserve the existing built-in `/demo` and trusted
   terminal helpers; keep a small separate protocol fixture in the runner.
5. Inspect npm pack contents, install the tarball into an independent consumer,
   typecheck/build the actual example and verify SSR import and browser exports.
   Keep `private: true` and do not publish or create release automation.
6. Extend the existing isolated integration runner with actual HTTP backend and
   browser sessions, retaining existing SDK/CLI coverage and reliable cleanup.

## Validation and documentation

- Unit tests: configuration/auth/policy, JSON validation, token scopes, SDK HTTP
  contract, safe static serving, origin checks, errors, timeout/cancellation and
  graceful shutdown. Run appropriate root and independent-module Go checks.
- Real single/distributed tests: business API -> Go SDK -> gateway/NATS -> two
  authorized browser sessions; project/room isolation; unauthorized publisher,
  token and subscription; expiry; fresh-token reconnect; explicit disconnect;
  broker/server outage; no routing-loop duplicates; unavailable gateway; no
  server secrets in browser, logs or artifacts. Verify cleanup after failures.
- Verify npm artifacts, exports/types, browser build, SSR import and standalone
  Go workspace consumption. Keep public installation evidence separate.
- Update existing README, SDK/CLI/example docs and actual
  `docs/PROJECT_CONTEXT.md`. Add one reproducible `docs/QUICKSTART.md` and only
  necessary example ENV defaults. Reuse integration docs for troubleshooting.
- Execute the documented setup in both Compose modes. Ten minutes is an
  onboarding target on a prepared machine, not a measured claim without timing.
- Report actual checks, changed files, security/compatibility findings and
  remaining release blockers. Recommend Phase 5 work without implementing it.

## Implementation and evidence — 2026-10-16

The external application, packed npm consumer, local Go workspace consumer,
integration runner and onboarding documentation are implemented. Core source,
public HTTP/WebSocket contracts, SDK APIs and CLI behavior remain unchanged.
Public Go installation remains blocked by the missing canonical module origin
and tag; the current local workspace result is not a public release.

### Changed files

- `examples/sdk/main.go`: add `serve`; retain trusted `token`/`publish` commands.
- `examples/sdk/server.go`, `server_test.go`: authenticated application HTTP API,
  scoped token issuance, validated business publication through the Go SDK,
  fixed static files, bounded configuration/requests, timeouts and shutdown.
- `examples/sdk/go.mod`: use the existing JWT dependency directly in tests to
  verify issued claims/signatures; no additional production Go dependency.
- `examples/sdk/app.ts`, `index.html`: real application login, gateway selection,
  token provider, safe typed resource rendering, membership controls, explicit
  refresh and cancellation. Refresh reuses the SDK instance so socket closure
  remains part of its controlled connection lifecycle.
- `examples/sdk/app.js`: replace the old source with TypeScript and a generated
  ignored `public/app.js` bundle.
- `examples/sdk/package.json`, `package-lock.json`, `tsconfig.json`, `build.mjs`:
  strict TypeScript and pinned esbuild development tooling, actual package-name
  import and browser bundle; no runtime framework or Node polyfills.
- `examples/sdk/.env.example`, `README.md`: application configuration and usage.
- `.gitignore`: exclude generated example public assets.
- `tests/sdk/run.mjs`, `application.mjs`, `README.md`: temporary copied consumers,
  tarball/file/export/type/SSR checks, real Go HTTP application and browser flows
  in both modes, optional CLI conformance and verified process/container/network
  cleanup. The consumer go.mod has no SDK replacement or unresolvable public
  version requirement; its temporary workspace supplies the copied SDK.
- `README.md`, `docs/PROJECT_CONTEXT.md`, `docs/CLI.md`, both SDK READMEs:
  external workflow, authentication boundaries, diagnostics, release blockers
  and delivery limits.
- `docs/QUICKSTART.md` and this plan: reproducible onboarding and actual evidence.

### Checks executed

| Check | Actual result |
| --- | --- |
| Root `go mod tidy`, `go fmt ./...`, `go vet ./...` | Passed |
| Root `go test ./...`, `go test -race ./...`, `go build ./...` | Passed, including existing CLI/Core tests |
| `NATS_SERVER_BIN=/tmp/socketfabric-nats-server-2.15.1 go test -race -tags integration ./...` | Passed with a real authenticated NATS server |
| `NATS_SERVER_BIN=/tmp/socketfabric-nats-server-2.15.1 go test -count=1 -race -tags integration ./internal/adapters/outbound/nats ./internal/bootstrap` | Passed uncached; fresh real-broker adapter/bootstrap execution |
| Independent Go SDK tidy/format/vet/unit/race/build | Passed; includes typed errors, timeout/cancellation, concurrent publishing and one-attempt behavior |
| Independent Go example tidy/format/vet/unit/race/build | Passed; eight new top-level HTTP/config/policy/lifecycle tests with subtests |
| TypeScript SDK strict typecheck and deterministic tests | Passed, 25 tests |
| Existing demo generated-JS check and deterministic tests | Passed, 9 tests |
| Local example `npm run build` | Passed, strict typecheck and browser bundle |
| Actual npm pack, allowlisted contents and separate installation | Passed; 10 files: package metadata, README, ESM and declarations; no runtime dependencies |
| Independent NodeNext consumer and SSR import | Passed; bare package export, typings and no WebSocket global required to import |
| Copied independent Go consumer build/dependency graph | Passed with local temporary workspace; no Gin/NATS/Core internals or consumer replacement |
| `node tests/sdk/run.mjs` | Passed single/distributed external application flows |
| Final `node tests/sdk/run.mjs --cli` | Passed after the refresh lifecycle change; includes both Compose image builds, real NATS and Chromium |
| Documented quick-start Bash setup | Passed exact script execution with generated input, private 0600 ENV file, sourcing and user/project/room configuration; temporary fixture removed |
| Documentation relative links | Passed for the updated onboarding/SDK/CLI/context documents |

The final real-browser run verified two authorized UI sessions on separate
gateways receiving identical events, strict project/room isolation, unauthorized
publisher/application/subscription rejection, forbidden scope requests, explicit
unsubscribe/re-subscribe, policy expiry with no automatic retry, expired-token
authentication rejection, explicit fresh-token refresh and gateway restart with
a fresh token request and restored subscription. No overlapping browser sockets
or duplicate event IDs were observed in the exercised application sessions.

Broker outage preserved browser sockets while liveness stayed 200, readiness
returned 503, and SDK/business/CLI publication failed explicitly. Recovery used
the existing subscriptions without replay. Disconnected clients did not resume
retries. Gateway loss produced useful SDK/CLI failures; peer gateway operation,
normal gateway exits and application SIGTERM shutdown passed. Final cleanup
asserted that the isolated projects had no remaining containers or networks and
removed their temporary consumers/credentials.

### Security and compatibility findings

- Publisher keys, signing secret and broker token stayed out of browser requests,
  WebSocket frames, DOM, bundled assets and application/gateway logs in these
  checks. User login credentials/client JWTs remain in page memory; no browser
  storage writes or public source/ENV/module serving were observed.
- User/project/room policy belongs to the backend. The signer remains globally
  privileged; its constructor does not replace application authorization.
- The local Basic/ENV policy is an example, not a production account service.
  Production integration must use its existing identity system, trusted TLS
  termination (HTTPS/WSS), credential rotation and application abuse controls.
- Core connection limits, heartbeat, bounded queues, slow-client behavior and
  single/distributed semantics remain intact. No new Core rate limiter, refresh
  API, persistence, replay, client ACK or delivery guarantee was introduced.

### Remaining release limitations and next task

Canonical Go repository/module URL and release tag, package licensing policy and
npm scope ownership are unresolved. No clean public Go install, npm registry
publish or binary release was executed or claimed. `private: true` remains.
Validation ran on Linux with Chromium; this is functional verification, not a
load benchmark, native macOS/Windows/browser-matrix check or production TLS audit.
The ten-minute onboarding target has not been timed with a new developer.

Resolve package ownership/origin/version/license and verify clean external
installation before release. Then scope Phase 5 administration/authorization and
compatible APIs separately; no dashboard/admin endpoints have been started.
Continue measuring reconnect/backpressure with a real application's identity and
load profile before deciding production limits or stronger delivery semantics.
