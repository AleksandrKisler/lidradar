# 0001: Use a modular monolith

- Status: Accepted
- Date: 2026-08-21
- Baseline: Final System Architecture v1.1

## Context

LidRadar needs explicit business boundaries without the operational cost of distributed services during MVP delivery.

## Decision

Implement the backend as a modular monolith. Keep capabilities in explicit modules with small public contracts; sharing a process does not permit importing another module's internals.

## Alternatives considered

Microservices were rejected because no measured scaling or isolation need justifies distributed transactions and operations. An unstructured monolith was rejected because it erases ownership boundaries.

## Consequences

Deployment and local operation remain simple while module boundaries stay enforceable. Some failures and releases remain process-wide.

## Migration and rollback

Moving a measured hotspot to a service requires a superseding accepted ADR and an explicit data/API migration. Rollback keeps the capability in-process.
