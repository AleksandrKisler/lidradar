# 0015: Use UUIDv7-compatible identifiers and UTC timestamps

- Status: Accepted
- Date: 2026-08-21
- Baseline: Final System Architecture v1.1

## Context

Public identifiers must not expose sequences, and time must compare consistently across tenant timezones.

## Decision

Use UUIDv7-compatible IDs for business entities and `TIMESTAMPTZ` stored in UTC. Apply tenant timezone only in business calculations and presentation.

## Alternatives considered

Sequential public IDs were rejected due to enumeration and coordination concerns. Local-time persistence was rejected due to ambiguity.

## Consequences

IDs are globally usable and roughly time ordered; timezone-aware rules must convert explicitly.

## Migration and rollback

Identifier or timestamp changes require dual-read migration. Rollback retains existing UUIDs and UTC values.
