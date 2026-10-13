# 0023: Persist idempotency records for critical commands

- Status: Accepted
- Date: 2026-08-21
- Baseline: Final System Architecture v1.1

## Context

Retries of payment, action, outcome, and callback commands must not repeat critical effects.

## Decision

Persist idempotency by tenant, key, and operation with a request hash and stored result. Return conflict when a key is reused with a different request.

## Alternatives considered

Best-effort in-memory deduplication was rejected because restarts lose it. Blind retry was rejected because it duplicates business facts.

## Consequences

Clients can retry safely, while storage retention and atomic command integration must be managed.

## Migration and rollback

A replacement must preserve keys and responses. Rollback continues consulting existing PostgreSQL records.

## Note of 2026-10-12

[ADR 0056](0056-data-retention-and-erasure.md) (Accepted) sets the retention of these records: the key, operation and request hash are kept indefinitely, while the stored response body is erased 30 days after creation. A retry with a key whose response was erased receives a "key expired" refusal instead of re-executing the command. Nothing is erased yet: the cleanup process and the refusal code are not implemented.
