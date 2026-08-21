# 0019: Enforce tenant integrity in PostgreSQL

- Status: Accepted
- Date: 2026-08-21
- Baseline: Final System Architecture v1.1

## Context

Application checks alone do not prevent cross-tenant references or reads caused by programming errors.

## Decision

Use composite tenant-aware foreign keys for critical relationships and RLS as defense in depth on critical tenant tables. Keep application tenant checks mandatory.

## Alternatives considered

Application-only isolation was rejected as a single point of failure. RLS-only isolation was rejected because domain authorization still belongs in the application.

## Consequences

Cross-tenant corruption is blocked at multiple layers, with added schema and connection-context complexity.

## Migration and rollback

Policies and constraints are introduced through forward migrations. Rollback must retain application checks and may remove enforcement only after integrity validation.
