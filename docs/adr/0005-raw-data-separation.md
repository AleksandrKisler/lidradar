# 0005: Separate raw provider data from interpretation

- Status: Accepted
- Date: 2026-08-21
- Baseline: Final System Architecture v1.1

## Context

Provider evidence must remain auditable while canonical and semantic interpretations evolve.

## Decision

Store immutable raw provider payloads separately from messages, opportunities, semantic facts, and risks.

## Alternatives considered

Storing only normalized data was rejected because normalization errors could not be replayed. Treating provider JSON as the domain model was rejected because it leaks channel concerns.

## Consequences

Storage usage increases, but parsing can be corrected and historical evidence remains traceable.

## Migration and rollback

Retention changes must preserve audit and replay requirements. Rollback restores canonical records by replaying retained raw events.

## Note of 2026-10-12

[ADR 0056](0056-data-retention-and-erasure.md) (Accepted) refines this decision: raw payloads stay immutable until their retention term, then the `payload` content is erased while the row, `payload_hash` and `external_event_id` remain. The term is 14 days after processing for processed events and 30 days after receipt for failed ones. Replaying raw events for rollback is therefore possible only within that horizon. Nothing is erased yet: the cleanup process is not implemented.
