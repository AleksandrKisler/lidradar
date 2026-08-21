# 0018: Make organization the tenant boundary

- Status: Accepted
- Date: 2026-08-21
- Baseline: Final System Architecture v1.1

## Context

Every business lookup must prevent accidental or malicious cross-organization access.

## Decision

Treat Organization as Tenant. Every tenant-owned business table carries `tenant_id`, and repository methods require both tenant and entity identifiers.

## Alternatives considered

Implicit tenant context and entity-ID-only repositories were rejected because UUID knowledge could bypass isolation.

## Consequences

Isolation is visible in every data access path, with repetitive but auditable method signatures.

## Migration and rollback

Changing tenancy requires a superseding ADR and data migration. Rollback restores explicit tenant predicates.
