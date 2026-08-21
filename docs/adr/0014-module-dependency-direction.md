# 0014: Enforce inward module dependency direction

- Status: Accepted
- Date: 2026-08-21
- Baseline: Final System Architecture v1.1

## Context

Business rules must remain usable and testable without transport, database, provider, or UI concerns.

## Decision

Dependencies flow from transport and infrastructure toward application and domain contracts. Domain packages do not import pgx, HTTP routers, Telegram, AI SDKs, or transport DTOs.

## Alternatives considered

Layer mixing was rejected because it couples policy to delivery and persistence. Shared internal implementations were rejected because they erase ownership.

## Consequences

Boundaries improve testability and replaceability but require adapters and explicit mapping.

## Migration and rollback

Boundary changes require an accepted ADR. Rollback restores adapters and removes outward domain imports.
