# 0017: Restrict JSONB to extensible data

- Status: Accepted
- Date: 2026-08-21
- Baseline: Final System Architecture v1.1

## Context

Provider payloads and versioned metadata vary, while core domain state needs constraints, joins, and migrations.

## Decision

Use JSONB for provider payloads, metadata, capabilities, extensible events, and AI output. Store core domain state in typed relational columns.

## Alternatives considered

Putting whole aggregates in JSONB was rejected because it weakens integrity and queryability. Fully normalizing unstable provider payloads was rejected.

## Consequences

Flexible evidence can be retained while core invariants remain enforceable; mapping boundaries must be deliberate.

## Migration and rollback

Promoting a JSON field uses an additive column migration and backfill. Rollback can continue reading the retained JSON source.
