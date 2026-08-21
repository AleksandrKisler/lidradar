# 0007: Separate opportunity stage from risk

- Status: Accepted
- Date: 2026-08-21
- Baseline: Final System Architecture v1.1

## Context

Sales progress describes current commercial state, while risk describes a condition requiring attention. They change independently.

## Decision

Keep stages such as `PRICE_SENT` and `BOOKED` on Opportunity, and risks such as `NO_RESPONSE` in the Risk aggregate.

## Alternatives considered

Encoding risks as stages was rejected because simultaneous risks, resolution, severity, and evidence cannot be represented safely.

## Consequences

Risk policy can evolve without corrupting the sales lifecycle, at the cost of explicit coordination.

## Migration and rollback

A replacement model requires a superseding ADR. Rollback maps no risk state into stages and retains risk history.
