# API Contracts

SocketFabric's checked-in HTTP contract is [OpenAPI 3.1](../api/openapi.yaml), the shared JSON schemas are in [schemas.json](../api/schemas.json), and the public WebSocket frame contract is [AsyncAPI 3](../api/asyncapi.yaml). These specifications describe the Gateway implementation; they do not include separate example business services such as `examples/sdk`.

## HTTP API and authentication

`GET /healthz` reports process liveness. `GET /readyz` reports whether the local gateway can accept traffic (including NATS readiness in distributed mode). `GET /ws` performs the WebSocket upgrade; a client token is sent as the first JSON text frame after upgrade, not as a query parameter or handshake Authorization header. `POST /v1/events` uses `Authorization: Bearer <publisher-key>` and requires `projectId`, `roomId`, `type`, and `data`. The request's project must match the authenticated publisher scope.

Management routes under `/admin/v1` exist only when Management is enabled. They require HTTPS unless the explicit development-only insecure HTTP option is set. Login returns an opaque operator session, which is sent as a Bearer token. Operator sessions are distinct from project publisher keys and client JWTs. Admin can mutate projects and credentials and read audit logs; Viewer can read projects and diagnostics. All other access is denied. Credential creation returns the plaintext key once; list and later detail surfaces contain metadata only. Every new managed-key authentication checks the PostgreSQL primary for hash, expiry, revocation and project status; requests already authorized may finish after revocation. WebSocket project policy uses the separately documented bounded snapshot refresh. A successful publish still does not guarantee browser delivery.

Management list endpoints accept `limit` (default 25, maximum 100) and `cursor`; each endpoint documents its supported filters. Cursors are stable ascending identifiers. Unknown or duplicate query parameters are rejected. Responses use the implementation's JSON field casing and the shared `ErrorResponse` envelope, including `requestId`.

## WebSocket protocol

The first text frame after upgrade is `{"op":"auth","token":"…"}`. After an `authenticated` response, send `join` and `leave` frames with a scoped `roomId`; joins are idempotent and authorization is checked for each join. Events arrive as `{"op":"event","event":{…}}`. The contract documents server errors, close codes, control-frame heartbeats, queue behavior, reconnect, and re-subscription. A fresh connection needs a fresh valid token, and rooms must be joined again. Messages emitted while disconnected can be permanently missed. There is no application-level ACK, persistence, replay, or exactly-once delivery.

## Example flows

Operator flow (use a trusted terminal and keep the returned token private):

```sh
curl -sS -X POST https://gateway.example/admin/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"admin@example.com","password":"<operator-password>"}'
curl -sS https://gateway.example/admin/v1/projects \
  -H 'Authorization: Bearer <operator-session>'
curl -sS -X POST https://gateway.example/admin/v1/projects/project-a/keys \
  -H 'Authorization: Bearer <operator-session>' \
  -H 'Content-Type: application/json' -d '{}'
curl -sS https://gateway.example/admin/v1/diagnostics/overview \
  -H 'Authorization: Bearer <operator-session>'
```

Publisher flow (run only on a trusted backend; never put the publisher key in browser code):

```sh
curl -i -X POST https://gateway.example/v1/events \
  -H 'Authorization: Bearer <publisher-key>' \
  -H 'Content-Type: application/json' \
  -d '{"projectId":"project-a","roomId":"room-123","type":"resource.updated","data":{"status":"completed"}}'
```

To connect a browser, its trusted backend issues a short-lived client token scoped to a project, user, and permitted rooms using the Go SDK token signer. The browser sends that token as the first text frame, waits for `authenticated`, then joins the permitted room. The Core Gateway does not expose a token-issuance HTTP route.

## Validation

Install the pinned contract-tool dependencies and run the route/schema/runtime contract checks:

```sh
npm ci --prefix tests/contracts --ignore-scripts --no-audit --no-fund
make contracts-test
```

The checks parse OpenAPI and AsyncAPI, generate and typecheck a temporary OpenAPI TypeScript consumer, compile local schemas without remote references, validate representative HTTP and WebSocket payloads, compare all registered Gin routes against OpenAPI in each feature mode, and validate live Management response bodies in an isolated PostgreSQL integration fixture. The final step needs Docker and a pullable PostgreSQL image, but not a standalone NATS binary. The full SDK and browser integration gates remain separate and are run by `node scripts/check.mjs` when their documented Docker/NATS prerequisites are available.

When changing a route or frame, update the implementation, contract, representative runtime test, and SDK contract types together. Keep operation IDs stable. Additive compatible fields may be introduced deliberately; removing or changing existing fields, auth, status semantics, or frame meanings requires a documented compatibility decision before implementation. OpenAPI and AsyncAPI are maintained specifications, not a claim of guaranteed client delivery.

## Compatibility baseline and change policy

Phase 5D freezes the current checked-in `api/openapi.yaml`, `api/asyncapi.yaml`
and `api/schemas.json` as the review baseline for frontend integration. The
baseline is identified by the source/spec contents in this checkout, not by a
Git commit or tag; there is no usable Git history here. `v1` in HTTP paths is
the HTTP compatibility family. OpenAPI/AsyncAPI `info.version` describes the
document revision and is not a negotiated runtime version. WebSocket frames
have no protocol negotiation today.

Treat any change below as a contract change requiring API-owner review,
specification updates, generated-type checks, representative runtime tests,
Go/TypeScript SDK checks, and a compatibility note:

- operation IDs, paths/methods, auth scheme, role/scope rules, status/error
  meanings, required fields, field types/nullability, or pagination semantics;
- WebSocket frame names/fields, ordering/confirmation behavior, close codes,
  token validation, reconnect rules, or delivery meaning;
- credential disclosure/revocation semantics or Management availability.

Additive response fields and new optional response properties may be compatible
for tolerant clients. Requests use strict JSON decoding, so adding a field to a
request can break older gateways that reject unknown properties. Do not send a
new request property until the minimum deployed Gateway supports it, or use an
explicitly versioned migration. New response statuses/error codes must be
handled as unknown by clients and must not change existing outcomes. New
WebSocket messages require proof that existing clients safely tolerate them;
there is no protocol version switch to rely on.

Keep the existing HTTP path families (`/v1` and `/admin/v1`) and do not add a
second endpoint family solely for Dashboard convenience. Security fixes remain
permitted under the freeze, but must preserve compatibility where possible and
document any unavoidable behavior change before implementation. For future
published components, use SemVer: Core and CLI share the root Go module line;
the nested Go SDK and TypeScript SDK can version independently, each with a
tested compatibility tuple against a Gateway contract revision. Before 1.0,
incompatible changes require a documented migration and owner review; public
tags/versions remain unselected until repository identity and release ownership
are confirmed. The current specs' `0.1.0` values do not constitute a release.
