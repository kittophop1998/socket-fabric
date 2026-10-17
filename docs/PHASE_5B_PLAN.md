# SocketFabric Phase 5B Plan

## Goal

Publish implementation-accurate OpenAPI and AsyncAPI contracts for the
SocketFabric Gateway, then make specification and runtime drift fail the local
quality gate. Preserve existing HTTP, management and WebSocket behavior.

## Contract work

- Inventory Gin routes and document the two infrastructure routes, publish API,
  WebSocket handshake, optional demo assets, and all optional management routes.
- Use OpenAPI 3.1.1 and AsyncAPI 3.0.0. Keep shared Event and error JSON Schemas
  in a local reusable schema file; reference them from both contracts.
- Derive DTO fields, nullable/omitted values, defaults, filters, pagination,
  errors, headers, security and role permissions from handlers and tests.
- Describe the WebSocket JSON frames separately from protocol ping/pong, close
  behavior and reconnect requirements. Preserve best-effort delivery language.
- Document only Gateway routes. Describe the external example token flow in
  onboarding docs without making its application API a Core contract.

## Validation work

- Add pinned private Node tooling for OpenAPI, AsyncAPI and TypeScript type
  generation; add Go test support to validate shared-schema examples and real
  HTTP/WebSocket frames without linking validation dependencies into the server.
- Compare actual Gin routes against documented operations in every Management
  and demo feature combination. Fail on extra/missing routes and duplicate
  operation IDs.
- Extend existing HTTP and PostgreSQL/NATS fixtures for representative success,
  error, security, role and WebSocket flows. Avoid copying the integration suite.
- Generate TypeScript types into a temporary directory and typecheck them with
  small HTTP and event consumer examples. Keep generated clients out of source.
- Add a contract target to the existing Makefile and release quality gate.

## Documentation and acceptance

- Add API conventions and contract readiness documentation; update README and
  Project Context with verified commands, auth flows, feature gates, delivery
  semantics, known schema-validation gaps and future frontend constraints.
- Run format, tidy, vet, unit/race/build, static contract validation, runtime
  route/frame checks, PostgreSQL/NATS integration, SDK/browser regressions and
  Docker builds. Report route and semantic coverage separately, and mark checks
  blocked or unverified when the environment prevents execution.

## Decisions

- OpenAPI version: 3.1.1. AsyncAPI version: 3.0.0.
- Specification scope: the Gateway only, including feature-gated demo assets;
  exclude the separate `examples/sdk` business API.
- Contracts describe current behavior; changing the implementation later
  requires updating its contract and corresponding drift tests in the same work.
- No endpoint, authentication mechanism, schema migration or delivery behavior
  is added or changed by this phase.
