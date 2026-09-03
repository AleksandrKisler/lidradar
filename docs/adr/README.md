# Architecture decision records

Architecture decision records (ADRs) explain significant decisions in LidRadar's
architecture. The baseline records below codify decisions 001–031 from Final
System Architecture v1.1; later changes must follow the workflow in this file.

## Baseline index

| ADR | Status |
| --- | --- |
| [0001: Use a modular monolith](0001-modular-monolith.md) | Accepted |
| [0002: Build separate runtime processes from one repository](0002-runtime-processes.md) | Accepted |
| [0003: Use PostgreSQL as the source of truth](0003-postgres-source-of-truth.md) | Accepted |
| [0004: Persist external events before processing](0004-persist-first-ingestion.md) | Accepted |
| [0005: Separate raw provider data from interpretation](0005-raw-data-separation.md) | Accepted |
| [0006: Separate conversations from opportunities](0006-conversation-opportunity-separation.md) | Accepted |
| [0007: Separate opportunity stage from risk](0007-stage-risk-separation.md) | Accepted |
| [0008: Limit AI to semantic facts](0008-ai-semantic-facts.md) | Accepted |
| [0009: Keep AI inference asynchronous](0009-asynchronous-ai.md) | Accepted |
| [0010: Keep AI nodes disposable and non-authoritative](0010-disposable-ai-node.md) | Accepted |
| [0011: Standardize the backend platform](0011-go-platform-baseline.md) | Accepted |
| [0012: Use REST over net/http and chi](0012-http-rest-stack.md) | Accepted |
| [0013: Use pgx and SQL without an ORM](0013-pgx-sqlc-no-orm.md) | Accepted |
| [0014: Enforce inward module dependency direction](0014-module-dependency-direction.md) | Accepted |
| [0015: Use UUIDv7-compatible identifiers and UTC timestamps](0015-uuidv7-and-time.md) | Accepted |
| [0016: Represent money with exact decimals](0016-exact-money.md) | Accepted |
| [0017: Restrict JSONB to extensible data](0017-jsonb-boundary.md) | Accepted |
| [0018: Make organization the tenant boundary](0018-tenant-owned-data.md) | Accepted |
| [0019: Enforce tenant integrity in PostgreSQL](0019-tenant-integrity-rls.md) | Accepted |
| [0020: Use opaque server-side sessions](0020-opaque-session-auth.md) | Accepted |
| [0021: Authorize through membership permissions](0021-membership-permissions.md) | Accepted |
| [0022: Use OpenAPI as the REST contract](0022-openapi-rest-contract.md) | Accepted |
| [0023: Persist idempotency records for critical commands](0023-idempotency-records.md) | Accepted |
| [0024: Keep connectors channel-independent](0024-connector-contract.md) | Accepted |
| [0025: Use a short atomic webhook transaction](0025-webhook-transaction.md) | Accepted |
| [0026: Use a leased PostgreSQL job queue](0026-postgres-job-queue.md) | Accepted |
| [0027: Publish versioned events through a transactional outbox](0027-transactional-outbox-events.md) | Accepted |
| [0028: Use SSE only as an invalidation signal](0028-sse-invalidation.md) | Accepted |
| [0029: Separate notifications from delivery attempts](0029-notification-delivery-separation.md) | Accepted |
| [0030: Use an outbound pull model for local AI](0030-local-pull-ai.md) | Accepted |
| [0031: Validate AI output and enforce freshness](0031-ai-validation-freshness.md) | Accepted |
| [0032: Ограничивать попытки аутентификации через PostgreSQL](0032-persistent-auth-throttling.md) | Accepted |
| [0033: Ограничивать AI-узлы явным списком организаций](0033-ai-node-tenant-allowlist.md) | Accepted |
| [0034: Роли PostgreSQL и fail-closed контекст для RLS](0034-rls-roles-fail-closed.md) | Accepted |
| [0035: Явная единица порога риска](0035-risk-threshold-unit.md) | Proposed |
| [0036: Одно ожидающее AI-задание на переписку и дебаунс анализа](0036-ai-queue-single-queued-job.md) | Accepted |
| [0037: Политика уведомлений: получатели, тихие часы и сводки](0037-notification-policy-delivery.md) | Accepted |
| [0038: Обратная связь по рискам, окно точности и граница ML-согласия](0038-risk-feedback-precision-consent.md) | Accepted |
| [0039: Базовая аналитика читает необработанные факты модулей](0039-basic-analytics-raw-facts.md) | Accepted |
| [0040: Платформенное администрирование читает все модули и правит очереди](0040-platform-admin-observability.md) | Accepted |

## Workflow

1. Create `NNNN-short-title.md` in this directory using the next available
   four-digit number.
2. Describe the context, decision, alternatives, consequences, and migration or
   rollback considerations.
3. Mark the record `Proposed` while it is under discussion.
4. Obtain project approval and mark it `Accepted` before implementation.
5. Supersede old decisions with a new ADR rather than rewriting their history.

An accepted ADR is required before changing the modular-monolith shape, module
boundaries, dependency direction, data ownership, PostgreSQL source-of-truth
policy, or cross-module communication model.

## Minimal template

```md
# NNNN: Decision title

- Status: Proposed
- Date: YYYY-MM-DD

## Context

## Decision

## Alternatives considered

## Consequences

## Migration and rollback
```

The current architecture guardrails are summarized in
[`../architecture/ARCHITECTURE.md`](../architecture/ARCHITECTURE.md).
