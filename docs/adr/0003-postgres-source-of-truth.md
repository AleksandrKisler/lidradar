# 0003: Use PostgreSQL as the source of truth

- Status: Accepted
- Date: 2026-08-21
- Baseline: Final System Architecture v1.1

## Context

Durable business state, sessions, asynchronous coordination, and notification state require one authoritative store.

## Decision

PostgreSQL is authoritative for business state, sessions, jobs, scheduled checks, idempotency, outbox records, AI metadata, and notification state. Caches, SSE, frontend state, and AI nodes are derived.

## Alternatives considered

In-memory state and external queues were rejected as authorities because they weaken recovery and consistency. Additional authoritative datastores were rejected for MVP complexity.

## Consequences

Recovery and correctness center on PostgreSQL, increasing dependence on its availability and operational quality.

## Migration and rollback

Changing authority requires a data migration and superseding ADR. Rollback restores PostgreSQL-backed reads and writes before removing the alternative.
