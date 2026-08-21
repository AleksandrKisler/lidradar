# 0027: Publish versioned events through a transactional outbox

- Status: Accepted
- Date: 2026-08-21
- Baseline: Final System Architecture v1.1

## Context

A crash between domain commit and side-effect publication otherwise loses notifications or repeats unsafe work.

## Decision

Write domain mutation and outbox event in one PostgreSQL transaction. Dispatch asynchronously. Give every internal event a versioned immutable envelope.

## Alternatives considered

Direct post-commit publication and mutable event schemas were rejected because they create consistency gaps and consumer ambiguity.

## Consequences

Committed state always has durable publication intent; dispatch is at-least-once and consumers must be idempotent.

## Migration and rollback

Consumers can be rolled back by event version. A replacement transport must drain the outbox without dropping event identities.
