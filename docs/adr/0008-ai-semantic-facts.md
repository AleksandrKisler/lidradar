# 0008: Limit AI to semantic facts

- Status: Accepted
- Date: 2026-08-21
- Baseline: Final System Architecture v1.1

## Context

Model output is probabilistic and cannot be the final authority for revenue-impacting business state.

## Decision

AI returns versioned semantic facts. Deterministic domain policy in the Risk Engine makes final create and update decisions.

## Alternatives considered

Direct AI mutation and autonomous sales decisions were rejected due to nondeterminism, explainability, and safety concerns.

## Consequences

Business decisions remain testable and explainable; semantic coverage depends on explicit deterministic policies.

## Migration and rollback

Any expansion of AI authority requires a superseding ADR and safety evidence. Rollback disables AI application while retaining runs.
