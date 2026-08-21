# 0011: Standardize the backend platform

- Status: Accepted
- Date: 2026-08-21
- Baseline: Final System Architecture v1.1

## Context

A fixed implementation baseline reduces incompatible patterns and makes operational behavior predictable.

## Decision

Use Go 1.26.x, PostgreSQL 18.x, `log/slog`, Prometheus-compatible metrics, OpenTelemetry-compatible tracing, and Docker Compose for local development.

## Alternatives considered

Per-module technology selection was rejected because it increases maintenance and undermines a coherent monolith.

## Consequences

Teams share tooling and conventions, while upgrades require coordinated validation.

## Migration and rollback

Version changes require compatibility testing and, when architectural, a superseding ADR. Rollback uses the prior toolchain and images.
