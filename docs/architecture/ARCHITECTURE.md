# LidRadar architecture

## System shape

LidRadar is a modular monolith. It is deployed as one Go backend, while its
business capabilities are separated into explicit modules. Module boundaries
must remain visible in packages and APIs; sharing a process is not permission
to couple module internals.

PostgreSQL is the source of truth for durable application state. Caches,
search indexes, queues, and external systems, if introduced, are derived or
supporting infrastructure and must not silently become authoritative.

## Technology baseline

The following implementation-level choices are fixed for the backend and are
not left to individual implementation runs:

- Go 1.26.x
- `net/http`
- `chi/v5`
- `pgx/v5`
- PostgreSQL 18.x
- `log/slog`
- `sqlc`
- OpenAPI
- Argon2id
- Decimal arithmetic for values that require exact decimal representation
- Prometheus-compatible metrics
- OpenTelemetry-compatible tracing
- Docker Compose

The backend does not use an ORM, GraphQL, Redis, or microservices. Replacing or
adding alternatives to this baseline requires an accepted ADR before
implementation.

## Dependency rules

- Business rules belong in the module that owns the relevant capability.
- A module exposes a small public contract; other modules must not import its
  internal implementation.
- Dependencies point from delivery and infrastructure code toward application
  and domain contracts, not the reverse.
- Cross-module workflows use explicit application-level contracts and keep
  transaction ownership clear.
- PostgreSQL access is owned by the relevant module. Direct reads or writes to
  another module's data are architectural decisions, not shortcuts.
- Transport concerns (HTTP, jobs, or CLI), persistence details, and domain
  behavior remain separate.

## Change policy

This document records the current guardrails, not every implementation detail.
Any change to the system shape, module boundaries, dependency direction,
source-of-truth policy, or cross-module communication requires an accepted ADR
in [`../adr/`](../adr/README.md) before implementation.

For backend behavior and engineering workflow, continue with
[`../spec/BACKEND_SPEC.md`](../spec/BACKEND_SPEC.md) and
[`../engineering/CODEX_RULES.md`](../engineering/CODEX_RULES.md).
