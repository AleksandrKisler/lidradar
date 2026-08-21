# 0002: Build separate runtime processes from one repository

- Status: Accepted
- Date: 2026-08-21
- Baseline: Final System Architecture v1.1

## Context

HTTP serving, background work, scheduling, AI execution, and migrations have different lifecycles even though they share one codebase.

## Decision

Build `lidradar-api`, `lidradar-worker`, `lidradar-scheduler`, `lidradar-ai-agent`, and `lidradar-migrate` from the same repository.

## Alternatives considered

A single all-in-one process was rejected because it couples scaling and failure modes. Separate repositories were rejected because they fragment the modular monolith.

## Consequences

Each runtime can be deployed and stopped independently while contracts and releases remain coordinated.

## Migration and rollback

A runtime may be consolidated only through a superseding ADR. Existing binaries provide the rollback boundary.
