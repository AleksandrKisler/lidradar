# 0025: Use a short atomic webhook transaction

- Status: Accepted
- Date: 2026-08-21
- Baseline: Final System Architecture v1.1

## Context

Acknowledging before durability loses events, while downstream work inside the request increases timeout and duplicate risk.

## Decision

After verification, atomically insert the unique RawEvent and normalization work, commit, then return HTTP success. Do no AI, notification, lengthy normalization, or analytics in the request.

## Alternatives considered

Acknowledge-first and process-inline designs were rejected because they fail persist-first durability and latency requirements.

## Consequences

Receipt is fast and durable; canonical state appears asynchronously and requires observable jobs.

## Migration and rollback

Rollback replays committed raw events and restores the prior worker. The receipt path must never bypass the transaction.
