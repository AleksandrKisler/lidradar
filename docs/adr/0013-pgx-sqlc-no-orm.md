# 0013: Use pgx and SQL without an ORM

- Status: Accepted
- Date: 2026-08-21
- Baseline: Final System Architecture v1.1

## Context

The persistence model relies on explicit transactions, partial indexes, composite tenant keys, RLS, JSONB, cursor queries, and lock-aware queues.

## Decision

Use `pgx/v5` for PostgreSQL access and permit `sqlc` for typed queries. Do not use an ORM.

## Alternatives considered

ORMs were rejected because their abstractions obscure required PostgreSQL behavior and transaction ownership.

## Consequences

SQL behavior stays visible and testable, with more deliberate mapping between records and domain entities.

## Migration and rollback

Library upgrades must retain SQL semantics. Adopting an ORM requires a superseding ADR; rollback returns repositories to pgx queries.
