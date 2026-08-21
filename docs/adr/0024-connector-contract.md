# 0024: Keep connectors channel-independent

- Status: Accepted
- Date: 2026-08-21
- Baseline: Final System Architecture v1.1

## Context

Provider-specific verification and payloads must not leak into the canonical conversation domain.

## Decision

Define connectors through Provider, VerifyEvent, NormalizeEvent, and Health operations. Treat outgoing transport as an optional separate capability.

## Alternatives considered

Provider logic in domain handlers and a universal provider JSON model were rejected because both couple business rules to channels.

## Consequences

New channels can be added behind adapters and contract tests; connectors must map provider details explicitly.

## Migration and rollback

Connector changes retain raw events for replay. Rollback routes a provider back to its prior adapter.
