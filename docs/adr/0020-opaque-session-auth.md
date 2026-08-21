# 0020: Use opaque server-side sessions

- Status: Accepted
- Date: 2026-08-21
- Baseline: Final System Architecture v1.1

## Context

Browser authentication requires revocation and must not expose reusable credentials in storage or logs.

## Decision

Hash passwords with Argon2id. Issue opaque session cookies with HttpOnly, Secure, and SameSite attributes; store only token hashes. Protect state-changing requests with CSRF or origin validation.

## Alternatives considered

Stateless bearer sessions were rejected because immediate revocation and server-side control are required. Plain session storage was rejected as credential exposure.

## Consequences

Sessions are controllable and auditable, while authentication depends on PostgreSQL availability.

## Migration and rollback

Authentication changes require staged session compatibility. Rollback invalidates incompatible sessions and returns to hashed opaque tokens.
