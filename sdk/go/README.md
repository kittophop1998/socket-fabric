# SocketFabric Go SDK

This separate module publishes trusted backend events and issues short-lived
browser tokens without importing the SocketFabric engine, Gin or NATS.
Use Go 1.27.2 or newer, including the security patch used by the release builds.
See [release readiness and compatibility](../../docs/RELEASE_READINESS.md) for
local artifacts, module/tag conventions and unresolved public-install decisions.
The module identity `socketfabric/sdk/go` is provisional. It has no canonical
public repository host or release tag, so public `go get ...@version` installation
is a release blocker. Do not substitute an invented repository URL.

For development in an existing independent Go module, create a local workspace:

```sh
# Run from your application's directory containing go.mod.
go work init . /absolute/path/to/socket-fabric/sdk/go
# Add imports/usage below, then build your application.
go build ./...
```

The workspace supplies the SDK module directly; it requires no `replace` or
provisional `v0.0.0` requirement in the consumer's go.mod. `go mod tidy` is not a
public-install check: it can still attempt to resolve the unpublished module.
The external integration runner copies both projects into a temporary workspace
outside the checkout and verifies imports/build and the dependency graph. That
proves local development consumption, not registry/VCS installation.

The existing checkout examples and CLI retain their local replacements for
compatibility. This alternative also works in a separate application:

```sh
go mod edit -require=socketfabric/sdk/go@v0.0.0
go mod edit -replace=socketfabric/sdk/go=/absolute/path/to/socket-fabric/sdk/go
go mod tidy
```

Before public release, select the owning VCS/module path, migration/versioning
policy and license, update public imports together, then verify a clean consumer
without workspace or replacements. No package or binary is published by this
phase. See the [external application quick start](../../docs/QUICKSTART.md) and
[example HTTP backend](../../examples/sdk/README.md) for a working authenticated
token endpoint and business publish flow.

```go
import (
    "context"
    "time"
    socketfabric "socketfabric/sdk/go"
)

// Load credentials from your trusted backend configuration.
client, err := socketfabric.NewClient(socketfabric.ClientOptions{
    BaseURL: "http://localhost:8080", ProjectID: "my-project",
    PublisherKey: publisherKey,
})
if err != nil { /* handle configuration error */ }
result, err := client.Publish(context.Background(), socketfabric.PublishInput{
    RoomID: "notifications", Type: "notification.created",
    Data: map[string]any{"message": "Hello"}, RequestID: "request-123",
})
// Handle err before inspecting result.Event or result.QueuedConnections.

signer, err := socketfabric.NewTokenSigner([]byte(signingSecret))
if err != nil { /* handle configuration error */ }
// Your app must first authenticate this user and authorize every room.
token, err := signer.IssueToken(socketfabric.ClientTokenInput{
    ProjectID: "my-project", UserID: "user-123",
    Rooms: []string{"notifications"}, TTL: 5 * time.Minute,
})
// Return only the short-lived token to the authorized browser.
```

`NewClient` binds one project. `Data: nil` publishes JSON `null`; raw JSON may
use `json.RawMessage`. Requests are limited to 64 KiB, including their envelope.
The SDK accepts response bodies up to 1 MiB to allow JSON escaping expansion.
Identifiers follow the engine's 1–128 byte ASCII letters/digits/`._:-` rules;
optional request IDs use the same rules without `:`.

The default request timeout is 10 seconds and caller context deadlines take
precedence. Supply `HTTPClient` for custom transport/TLS configuration. The
client is copied and redirects are disabled to protect publisher credentials.
Use HTTPS outside a trusted local network. Callers must not mutate supplied
payload values while a publish is running, or mutate a supplied HTTP client's
shared transport concurrently with use.

There is one publish attempt and no SDK retries. A 202 means server acceptance
and local queueing, not browser delivery. `QueuedConnections` counts only local
connections on the accepting gateway. A timeout or 503 may occur after remote
delivery; blindly retrying can duplicate events. There is no persistence,
replay, delivery acknowledgment or deduplication.

Use `errors.As(err, &apiError)` with `var apiError *socketfabric.APIError` to
inspect `StatusCode`, `Code`, `Message` and `RequestID`. Malformed non-202 error
bodies still yield an `APIError` with status and a valid header request ID when
present. Error strings omit upstream body contents and credentials; explicitly
inspecting API error fields remains your application's responsibility.
`errors.Is` recognizes `context.Canceled`, `context.DeadlineExceeded` and the
SDK sentinels `ErrInvalidInput`, `ErrInvalidResponse` and `ErrRequestFailed`.

The signing secret is installation-wide authority to mint tokens for every
configured project. Keep it only in a trusted token issuer; publisher keys
cannot substitute for it. The SDK does not know which projects the server has
configured. Tokens allow 1–64 unique explicit rooms, default to five minutes
and allow TTLs from one second through fifteen minutes. Times and fractional
TTL seconds are truncated to whole seconds for compatibility. Existing server
JWT issuer, audience, HS256 and expiry rules are preserved.

```sh
go mod tidy
go fmt ./...
go vet ./...
go test ./...
go test -race ./...
go build ./...
```
