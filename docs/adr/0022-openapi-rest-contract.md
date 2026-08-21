# 0022: Use OpenAPI as the REST contract

- Status: Accepted
- Date: 2026-08-21
- Baseline: Final System Architecture v1.1

## Context

Backend and frontend need one versioned source for endpoint shapes, errors, pagination, and generated clients.

## Decision

Maintain the `/api/v1` REST contract in `contracts/openapi/openapi.yaml`, use a uniform error envelope, and use stable cursor pagination rather than offsets for mutable lists.

## Alternatives considered

Implementation-only contracts were rejected because clients drift. Offset pagination was rejected for unstable ordering under writes.

## Consequences

Contract changes become reviewable and client generation is repeatable; schema maintenance is required alongside code.

## Migration and rollback

Breaking changes require a new API or schema version. Rollback deploys handlers compatible with the prior OpenAPI contract.
