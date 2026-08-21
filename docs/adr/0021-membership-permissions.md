# 0021: Authorize through membership permissions

- Status: Accepted
- Date: 2026-08-21
- Baseline: Final System Architecture v1.1

## Context

A user's authority varies by tenant, and role checks scattered through business code are difficult to evolve safely.

## Decision

Attach OWNER or MANAGER roles to Membership and resolve them centrally into named permissions. Application operations check permissions rather than role literals.

## Alternatives considered

Global tenant roles and inline `role == OWNER` checks were rejected because they couple identity to policy and risk privilege drift.

## Consequences

Authorization policy is consistent and testable, with a central resolver that must remain reliable.

## Migration and rollback

Permission changes are additive or explicitly migrated. Rollback restores the prior role-to-permission mapping without changing memberships.
