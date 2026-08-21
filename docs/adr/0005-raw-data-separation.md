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
