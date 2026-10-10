<!-- GENERATED; source-sha256: 9b1f3085d41d4703afc410738019fc265022d54b3e801063b2eba8ef9c2a7cde -->
> Источник: [канонический документ](../adr/README.md). Правки вносятся в источник.

# Architecture decision records

Architecture decision records (ADRs) explain significant decisions in LidRadar's
architecture. The baseline records below codify decisions 001–031 from Final
System Architecture v1.1; later changes must follow the workflow in this file.

## Baseline index

| ADR | Status |
| --- | --- |
| [0001: Use a modular monolith](../adr/0001-modular-monolith.md) | Accepted |
| [0002: Build separate runtime processes from one repository](../adr/0002-runtime-processes.md) | Accepted |
| [0003: Use PostgreSQL as the source of truth](../adr/0003-postgres-source-of-truth.md) | Accepted |
| [0004: Persist external events before processing](../adr/0004-persist-first-ingestion.md) | Accepted |
| [0005: Separate raw provider data from interpretation](../adr/0005-raw-data-separation.md) | Accepted |
| [0006: Separate conversations from opportunities](../adr/0006-conversation-opportunity-separation.md) | Accepted |
| [0007: Separate opportunity stage from risk](../adr/0007-stage-risk-separation.md) | Accepted |
| [0008: Limit AI to semantic facts](../adr/0008-ai-semantic-facts.md) | Accepted |
| [0009: Keep AI inference asynchronous](../adr/0009-asynchronous-ai.md) | Accepted |
| [0010: Keep AI nodes disposable and non-authoritative](../adr/0010-disposable-ai-node.md) | Accepted |
| [0011: Standardize the backend platform](../adr/0011-go-platform-baseline.md) | Accepted |
| [0012: Use REST over net/http and chi](../adr/0012-http-rest-stack.md) | Accepted |
| [0013: Use pgx and SQL without an ORM](../adr/0013-pgx-sqlc-no-orm.md) | Accepted |
| [0014: Enforce inward module dependency direction](../adr/0014-module-dependency-direction.md) | Accepted |
| [0015: Use UUIDv7-compatible identifiers and UTC timestamps](../adr/0015-uuidv7-and-time.md) | Accepted |
| [0016: Represent money with exact decimals](../adr/0016-exact-money.md) | Accepted |
| [0017: Restrict JSONB to extensible data](../adr/0017-jsonb-boundary.md) | Accepted |
| [0018: Make organization the tenant boundary](../adr/0018-tenant-owned-data.md) | Accepted |
| [0019: Enforce tenant integrity in PostgreSQL](../adr/0019-tenant-integrity-rls.md) | Accepted |
| [0020: Use opaque server-side sessions](../adr/0020-opaque-session-auth.md) | Accepted |
| [0021: Authorize through membership permissions](../adr/0021-membership-permissions.md) | Accepted |
| [0022: Use OpenAPI as the REST contract](../adr/0022-openapi-rest-contract.md) | Accepted |
| [0023: Persist idempotency records for critical commands](../adr/0023-idempotency-records.md) | Accepted |
| [0024: Keep connectors channel-independent](../adr/0024-connector-contract.md) | Accepted |
| [0025: Use a short atomic webhook transaction](../adr/0025-webhook-transaction.md) | Accepted |
| [0026: Use a leased PostgreSQL job queue](../adr/0026-postgres-job-queue.md) | Accepted |
| [0027: Publish versioned events through a transactional outbox](../adr/0027-transactional-outbox-events.md) | Accepted |
| [0028: Use SSE only as an invalidation signal](../adr/0028-sse-invalidation.md) | Accepted |
| [0029: Separate notifications from delivery attempts](../adr/0029-notification-delivery-separation.md) | Accepted |
| [0030: Use an outbound pull model for local AI](../adr/0030-local-pull-ai.md) | Accepted |
| [0031: Validate AI output and enforce freshness](../adr/0031-ai-validation-freshness.md) | Accepted |
| [0032: Ограничивать попытки аутентификации через PostgreSQL](../adr/0032-persistent-auth-throttling.md) | Accepted |
| [0033: Ограничивать AI-узлы явным списком организаций](../adr/0033-ai-node-tenant-allowlist.md) | Accepted |
| [0034: Роли PostgreSQL и fail-closed контекст для RLS](../adr/0034-rls-roles-fail-closed.md) | Accepted |
| [0035: Явная единица порога риска](../adr/0035-risk-threshold-unit.md) | Superseded by 0043 |
| [0036: Одно ожидающее AI-задание на переписку и дебаунс анализа](../adr/0036-ai-queue-single-queued-job.md) | Accepted |
| [0037: Политика уведомлений: получатели, тихие часы и сводки](../adr/0037-notification-policy-delivery.md) | Accepted |
| [0038: Обратная связь по рискам, окно точности и граница ML-согласия](../adr/0038-risk-feedback-precision-consent.md) | Accepted |
| [0039: Базовая аналитика читает необработанные факты модулей](../adr/0039-basic-analytics-raw-facts.md) | Accepted |
| [0040: Платформенное администрирование читает все модули и правит очереди](../adr/0040-platform-admin-observability.md) | Accepted |
| [0041: Реализация RLS через роли пула и усиление периметра](../adr/0041-rls-enforcement-and-hardening.md) | Accepted |
| [0042: Нагрузочное испытание в процессе на синтетическом наборе](../adr/0042-capacity-test-method.md) | Accepted |
| [0043: Пороги правил риска в бизнес-времени без таблицы конфигурации](../adr/0043-risk-thresholds-in-code.md) | Accepted |
| [0044: Обогащённые модели чтения Radar и переписок для интерфейса](../adr/0044-frontend-read-models.md) | Accepted |
| [0045: Команда по одноразовым кодам, статус онбординга и безопасное подключение каналов](../adr/0045-team-onboarding-and-secure-connect.md) | Accepted |

- [0047: Сделки с неизвестной услугой и незавершённые договорённости](../adr/0047-unfinished-agreements.md) — Accepted, 2026-10-07.
- [0048: Сохранение независимых незавершённых ожиданий](../adr/0048-retain-independent-agreements.md) — Accepted, 2026-10-09.
- [0049: Доверенные proxy и адрес клиента](../adr/0049-trusted-proxies-client-address.md) — Accepted, 2026-10-10.
- [0050: Production-топология пилота](../adr/0050-production-topology.md) — Accepted, 2026-10-10.
- [0051: Резервные копии по расписанию и копия вне хоста](../adr/0051-offhost-backups.md) — Accepted, 2026-10-10.
- [0052: Роли PostgreSQL production-стека: владелец без суперправ и рабочий логин без владения](../adr/0052-database-roles-owner-and-runtime.md) — Accepted, 2026-10-10.

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

## Предложения на рассмотрении

- [0046 — атомарный аудит критических команд](../adr/0046-atomic-critical-command-audit.md) — Proposed, H-04.

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
