# SocketFabric developer CLI

The standalone `socketfabric` binary uses the existing HTTP API and Go SDK. It
needs no server checkout, signing secret, NATS access or running SDK example to
execute. Network commands need a reachable SocketFabric gateway. Delivery
remains best-effort.

Both ENV and managed project publisher keys work through the same SDK/API. Use
`SOCKETFABRIC_API_KEY` for either key; this CLI has no operator/admin commands.
`status`/`ping` inspect data-plane readiness/liveness. With ENV projects, healthy
results can coexist with PostgreSQL/Management downtime. See the
[control-plane reference](MANAGEMENT.md) for authenticated diagnostics and the
separate trusted migration/operator tool.

## Build and install locally

Requirements: Go 1.27.2 or newer. From the repository root:

```sh
go run ./cmd/socketfabric --help
go build -trimpath -o bin/socketfabric ./cmd/socketfabric
./bin/socketfabric version
go install ./cmd/socketfabric
```

`go install` installs into `GOBIN`, or `$(go env GOPATH)/bin` when unset. Add that
directory to PATH. On Windows use `socketfabric.exe`; the direct Go build/install
commands work in PowerShell too. Run the resulting executable from any project.
Public module installation with `@version` is not available yet; the repository
uses a local replacement for the independent Go SDK.

Optional Make targets, on systems with Make and a POSIX shell:

```sh
make cli VERSION=0.1.0 COMMIT=your-commit BUILD_TIME=2026-10-16T00:00:00Z
make cli-cross
```

Cross builds produce Linux/macOS/Windows amd64 and arm64 binaries under `bin/`,
using `CGO_ENABLED=0`. Metadata defaults to `dev` and `unknown`; version also
includes Go version and target OS/architecture. Direct builds can use
`-ldflags '-X main.version=0.1.0 -X main.commit=your-commit -X main.buildTime=2026-10-16T00:00:00Z'`.
Cross-compilation is not native macOS/Windows runtime verification. The existing
Docker image still starts the gateway server, not this developer CLI.

Phase 4D adds `make cli-dist` (or `node scripts/release.mjs build`), producing
raw CLI binaries for all six targets, the packed npm SDK, a manifest, SHA256SUMS
and third-party notices under `bin/release`. It builds each binary twice from
separate source copies, compares hashes and verifies embedded target/version
metadata. `make release-verify` checks the complete artifact set. The output
directory must be new; select another with `--out` for a changed build.

Build metadata defaults to `dev`/`unknown`, with epoch-based build time from
`SOURCE_DATE_EPOCH` (default 0). Pin all metadata/toolchain versions for comparison;
no public version is selected. A host binary is run from outside the checkout,
and the quality gate tests it against real gateways. Foreign binaries are only
cross-built/inspected. See [distribution verification and PATH installation](RELEASE_READINESS.md)
for Linux/macOS/Windows checksums, safe local installation, support limits and
the public ownership/license blockers.

## Configuration

| Environment | Flag | Default / requirement |
| --- | --- | --- |
| `SOCKETFABRIC_URL` | `--url` | `http://localhost:8080`; absolute HTTP(S) URL |
| `SOCKETFABRIC_TIMEOUT` | `--timeout` | `5s`; positive duration |
| `SOCKETFABRIC_PROJECT_ID` | `--project` | Required for publish |
| `SOCKETFABRIC_API_KEY` | None | Project-scoped publisher key, required for publish |

Flags override environment values, including explicitly empty values. Unset URL
and timeout use defaults; explicitly empty URL/timeout values are invalid.
URLs may contain a reverse-proxy path prefix, but no embedded credentials,
query or fragment. HTTP commands do not follow redirects. The timeout bounds a
network command, including both concurrent status probes; it does not bound
the time spent waiting for piped input before publishing.

The CLI reads the environment directly. It does not load `.env`, save credentials,
prompt interactively, or read server signing/NATS configuration. Supply API keys
through your shell or CI secret environment. Diagnostics require no key and do
not send it. There is no API-key command-line flag.

```sh
export SOCKETFABRIC_URL=http://localhost:8080
export SOCKETFABRIC_PROJECT_ID=project-a
export SOCKETFABRIC_TIMEOUT=5s
# Reuse the existing publisher key for project-a from your trusted environment.
export SOCKETFABRIC_API_KEY="$PUBLISHER_KEY"
socketfabric config validate
```

PowerShell uses `$env:SOCKETFABRIC_URL = 'http://localhost:8080'` and the same
command flags. `config validate` checks the effective URL and timeout, plus any
provided project/key syntax, with no network requests. A key must contain at
least 32 bytes without whitespace/control characters. Missing project/key is
valid for diagnostics configuration; publishing requires both. Validation does
not prove that a key is authorized by the gateway. Output reports only whether
a key is configured.

## Commands

```sh
socketfabric --help
socketfabric version --json
socketfabric status
socketfabric ping --timeout 2s
socketfabric config validate --json
```

Common flags can precede the command or follow its final verb. Project applies
to publish/config validation; URL/timeout apply to network/config commands.
Version accepts JSON/help flags. Flags cannot be interleaved with `config
validate`'s two words. Repeated flags use the last value. Every command supports
`--help` without requiring valid environment configuration.

### Diagnostics

`status` checks `/healthz` and `/readyz` and displays each endpoint's
reachability, HTTP status, elapsed round-trip in milliseconds and validated
state. It succeeds only when both return their expected JSON contract. A
reachable HTTP 503 is distinct from an unreachable gateway or malformed JSON.
Health bodies are limited to 8 KiB; a proxy's HTML success page is not healthy.

`ping` checks `/healthz` for HTTP connectivity and process liveness. It does not
connect a WebSocket, publish an event or confirm browser delivery. During a NATS
outage it can succeed while `status` reports readiness failure.

Each command describes only its configured gateway. Inspect nodes separately:

Use direct node URLs for per-node checks; a load-balanced URL can route the two
status requests to different instances.

```sh
socketfabric status --url http://localhost:8080 --json
socketfabric status --url http://localhost:8081 --json
```

JSON `status` includes `gateway`, `scope: "gateway"`, `health` and `readiness`.
JSON `ping` includes `gateway`, `scope: "http"` and `http`. Each probe contains
`endpoint`, `reachable`, `httpStatus`, `responseTimeMs`, `ok` and an optional safe
`error` code. HTTP status is zero if no HTTP response arrived. These checks do
not imply cluster-wide health, presence or subscriber counts.

### Publish

```sh
socketfabric publish --project project-a --room room-123 \
  --event resource.updated --data '{"status":"completed"}'

printf '%s' '{"status":"completed"}' | socketfabric publish \
  --room room-123 --event resource.updated --data - --json

socketfabric publish --room room-123 --event resource.updated --data null
```

Project/room/type use 1–128 ASCII letters/digits or `._:-`. Routing supports
project rooms only. `--data` is required and accepts one JSON value, including
objects, arrays, scalars and `null`. `--data -` reads stdin explicitly and is
bounded to 64 KiB; the SDK also validates the complete encoded request against
the existing 64 KiB limit before sending. JSON data is not printed in output.

Publish uses the project-bound Go SDK to send one authenticated `POST
/v1/events`. Authorization remains enforced by the server. The CLI does not
issue client tokens or bypass room/project policy.

JSON publish output includes `accepted`, `eventId`, `projectId`, `roomId`,
`type`, `timestamp`, `queuedConnections`, and `deliveryConfirmed: false`.
`queuedConnections` counts queues on the accepting gateway only; zero is valid
even if another node has subscribers. Acceptance is not browser receipt or
processing. There is no client ACK, persistence, replay or delivery guarantee.

There are no automatic publish retries. A timeout, network interruption or HTTP
503 may happen after a remote gateway received the event. Blind retries may
create duplicates; this phase adds no idempotency contract.

## Shell/CI output and failures

All result commands support `--json`. Results go to stdout; errors go to stderr
and contain a safe `error` object (`code`, `message`, optional `httpStatus`). A
failed diagnostic emits the collected probe results on stdout as well as its
command error on stderr. Help remains human-readable. Put `--json` before the
command when you also need JSON for argument parsing failures.

```sh
socketfabric --json status > status.json 2> status-error.json
socketfabric --json config validate
socketfabric --json publish --room room-123 --event resource.updated \
  --data - < event.json > accepted.json 2> publish-error.json
```

| Exit code | Meaning |
| --- | --- |
| `0` | Command actually succeeded; zero local subscribers is a valid acceptance |
| `1` | Gateway/API/network/protocol/timeout/input-read/output failure |
| `2` | Invalid command, flags, configuration or payload |
| `130` | Context canceled, including handled SIGINT/SIGTERM |

HTTP 401 asks you to check the key; 403 asks you to check its project scope;
429 reports a rate limit without retrying; 5xx directs you to readiness/broker
availability. Connection failures ask you to check the URL, listener, network
and TLS. Malformed upstream responses are rejected. Raw response bodies,
transport errors, keys and payloads are not echoed. SIGINT/SIGTERM cancel
pending HTTP requests and owned stdin reads.

Use HTTPS across trust boundaries. Keep keys in trusted backend/CI secret
environments, never browser code or committed files. A client JWT or the server's
signing secret is not a publisher API key. Logs and command output omit data;
piping sensitive event data avoids putting it into command arguments.

## Checks

For the [external application example](QUICKSTART.md), run diagnostics against
each configured gateway and use a typed resource payload so its browser UI can
render it:

```sh
./bin/socketfabric status --url http://127.0.0.1:8080
./bin/socketfabric status --url http://127.0.0.1:8081 --json
SOCKETFABRIC_API_KEY="$PUBLISHER_KEY" ./bin/socketfabric publish --project project-a \
  --room room-a --event resource.updated \
  --data '{"resourceId":"cli-resource","status":"completed"}'
```

Keep `PUBLISHER_KEY` in the trusted developer terminal. Application login
credentials, browser JWTs and the signing secret do not replace the publisher
key. If the API accepts but a browser shows nothing, check its authorized
project/room and subscription ACK; status/ping do not prove end-to-end delivery.
No Core API or CLI command changes are needed for the external integration.

```sh
go test ./cmd/socketfabric
go test -race ./cmd/socketfabric
make cli-cross
# After installing the existing SDK integration runner prerequisites:
node tests/sdk/run.mjs --cli
```

The optional runner checks a standalone CLI from an external working directory,
actual single/distributed gateways and authorized Chromium subscribers, invalid
auth, broker outage and unavailable gateway failures. See
[integration instructions](../tests/sdk/README.md). No binaries are published.
