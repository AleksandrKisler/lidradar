# 0012: Use REST over net/http and chi

- Status: Accepted
- Date: 2026-08-21
- Baseline: Final System Architecture v1.1

## Context

The product needs a conventional, versioned HTTP API without coupling domain code to delivery libraries.

## Decision

Expose REST endpoints using `net/http` and `chi/v5`. Router and transport DTOs remain outside domain packages.

## Alternatives considered

GraphQL and WebSocket APIs were rejected for MVP scope. Framework-heavy HTTP stacks were rejected because the standard library plus chi is sufficient.

## Consequences

The HTTP surface stays small and idiomatic; realtime updates use a separate SSE decision.

## Migration and rollback

Transport replacement requires contract compatibility and a superseding ADR when it changes the API shape. Rollback preserves REST handlers.
