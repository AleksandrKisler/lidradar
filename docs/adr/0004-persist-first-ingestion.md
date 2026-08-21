# 0004: Persist external events before processing

- Status: Accepted
- Date: 2026-08-21
- Baseline: Final System Architecture v1.1

## Context

External deliveries may be duplicated, delayed, or impossible to replay. Slow downstream work must not determine webhook durability.

## Decision

Verify an event, then atomically persist its raw form and normalization work before acknowledging it. Normalize, analyze, notify, and run AI only after commit.

## Alternatives considered

Inline processing was rejected because timeouts and crashes can lose events or repeat side effects. A queue-first flow without durable raw input was rejected.

## Consequences

Webhook latency is bounded by a short database transaction, while downstream results are eventually consistent.

## Migration and rollback

A different ingestion sequence requires a superseding ADR and replay proof. Rollback routes receipt through raw-event persistence.
