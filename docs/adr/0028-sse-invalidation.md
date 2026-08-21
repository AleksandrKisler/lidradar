# 0028: Use SSE only as an invalidation signal

- Status: Accepted
- Date: 2026-08-21
- Baseline: Final System Architecture v1.1

## Context

The UI benefits from realtime awareness but durable state already belongs to PostgreSQL-backed APIs.

## Decision

Expose server-sent events for supported updates and require clients to refetch authoritative resources. Event loss or disconnect must not lose business state.

## Alternatives considered

WebSocket was rejected for MVP complexity. Treating the stream as authoritative was rejected because reconnect gaps are unavoidable.

## Consequences

Realtime delivery stays simple and recoverable; clients perform extra reads after signals.

## Migration and rollback

SSE can be disabled without data migration. Rollback uses polling and normal resource reads.
