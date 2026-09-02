# LidRadar — Backend Technical Specification & Sequential Delivery Plan v1.1

**Статус:** Ready for Backend Development
**Изменение v1.0 → v1.1:** добавлен внеочередной ЭТАП R — CONSISTENCY REMEDIATION (между этапами 16 и 17), задачи `LR-BE-RM-001 … LR-BE-RM-026`; уточнён §3.5 (абсолютный потолок аренды); §77 дополнен вторым намеренным исключением и этапом A2; §78 дополнен PR #17.5. Основание — Errata v1.2.2 (сквозная сверка Tasks.md, Плана разработки MVP v1.2, GLOSSARY v1.0, README v1.0).
**Architecture baseline:** Final System Architecture v1.1
**Implementation baseline:** Development Specification v1.1
**Execution baseline:** Detailed MVP Development Plan v1.0
**Backend:** Go 1.26.x
**Database:** PostgreSQL 18.x
**API:** REST + OpenAPI
**Architecture:** Modular Monolith
**AI:** Local-first / asynchronous / pull-based AI Node
**Primary AI Node:** Ubuntu Server 24.04 LTS / RTX 4060 8 GB / i5-13400F / 16 GB RAM / llama.cpp
**Primary production source:** Telegram Connected Business Bot
**Primary KPI:** Confirmed Recovered Revenue

---

# 1. Назначение документа

Этот документ является рабочим техническим заданием для backend-команды LidRadar.

Он определяет:

* архитектурные ограничения;
* обязательный backend stack;
* структуру Go-кода;
* доменные границы;
* правила PostgreSQL;
* API-контракты;
* правила multi-tenancy;
* правила асинхронной обработки;
* Connector Architecture;
* Opportunity и Risk Engine;
* Money Loop;
* Notification Engine;
* локальный AI-контур;
* требования безопасности;
* observability;
* testing strategy;
* Definition of Ready;
* Definition of Done;
* последовательный backend backlog;
* Exit Gate каждого этапа.

Backend-разработчик **не должен переизобретать архитектуру внутри задачи**.

Если реализация требует изменения одного из зафиксированных архитектурных решений, создаётся ADR и изменение отдельно утверждается техническим директором.

---

# 2. Иерархия документации

При противоречиях использовать следующий порядок приоритета:

1. **Final System Architecture v1.1**
2. **Development Specification v1.1**
3. **Detailed MVP Development Plan v1.0**
4. **MVP Implementation Plan v1.0**
5. **Functional Scope MVP v0.2**

Detailed Development Plan является основным документом для последовательности разработки.

Development Specification является основным документом для:

* DB schema;
* domain contracts;
* API;
* Risk rules;
* AI contracts;
* events.

Final System Architecture является источником истины для архитектурных ограничений.

---

# 3. CTO normalization decisions

Ниже зафиксированы решения по неоднозначностям исходной документации.

## 3.1. Risk naming

Физическая таблица PostgreSQL:

`risk_signals`

Domain aggregate:

`Risk`

REST resource:

`/api/v1/risks`

Никакую дополнительную таблицу `risks` в MVP не создавать.

---

## 3.2. Response threshold

Бизнес-логика `NO_RESPONSE` требует `response_threshold`, но поле отсутствует в исходном DDL.

Добавить в `locations`:

```sql
response_threshold_minutes SMALLINT NOT NULL DEFAULT 45
CHECK (
    response_threshold_minutes >= 1
    AND response_threshold_minutes <= 1440
)
```

Порог является настройкой **Location**, поскольку расчёт выполняется относительно business hours и timezone конкретной Location.

---

## 3.3. Auth API

Канонический набор:

```text
POST /api/v1/auth/register
POST /api/v1/auth/login
POST /api/v1/auth/logout
POST /api/v1/auth/refresh
GET  /api/v1/auth/me
```

Все варианты `/auth/...` без `/api/v1` в execution plan считать сокращённой записью.

---

## 3.4. AI stale result

`ai_jobs` обязательно содержит:

```text
base_conversation_revision
analysis_through_message_id
```

`ai_runs` также должен сохранять snapshot этих значений.

Чтобы не смешивать технический результат inference с применением результата к domain, вводится:

```text
application_status:
PENDING
APPLIED
STALE
REJECTED
```

Таким образом:

```text
run.status = SUCCEEDED
application_status = STALE
```

является корректным состоянием.

Stale inference не считается технической ошибкой модели.

---

## 3.5. AI lease renewal

Базовый lease AI job = 120 секунд.

Чтобы inference длительностью >120 секунд не создавал параллельное выполнение одной job, heartbeat AI Agent должен продлевать lease принадлежащих ему RUNNING jobs.

Если heartbeat исчез:

* lease перестаёт продлеваться;
* после `lease_until` job разрешается забрать другому node;
* повторный result должен безопасно отбрасываться/обрабатываться idempotently.

Скользящая аренда не покрывает случай, когда inference завис, а heartbeat-горутина жива. Поэтому поверх неё действует абсолютный потолок, который не продлевается никогда:

```text
lease_until   = now() + 120s     -- продлевается heartbeat
max_lease_age = leased_at + 15m  -- не продлевается
```

Reclaim забирает job при `lease_until < now()` ИЛИ `leased_at + 15m < now()`.

См. LR-BE-RM-016.

---

## 3.6. External Identity namespace

Для connection-scoped providers:

```text
tenant
+
provider
+
connection
+
external_id
```

Для providers с глобальным namespace:

```text
tenant
+
provider
+
external_id
```

В PostgreSQL реализовать двумя partial unique indexes, чтобы `NULL connection_id` не ломал уникальность.

---

## 3.7. Notification Preferences

`notification_preferences` является полноценной persisted entity, а не JSON в Organization.

Минимальные поля:

```text
id
tenant_id
user_id
risk_type
minimum_severity
delivery_mode
in_app_enabled
telegram_enabled
quiet_hours_enabled
quiet_hours_start
quiet_hours_end
digest_time
created_at
updated_at
```

Unique:

```text
tenant_id + user_id + risk_type
```

MVP timezone notification policy берётся из Organization default timezone.

---

## 3.8. Telegram linking

Одноразовый `/start` token никогда не хранится открытым текстом.

Необходимо хранить hash token + TTL + used_at.

`telegram_user_links` содержит уже установленную связь LidRadar user ↔ Telegram user/chat.

---

# 4. Что должен доказать MVP

Главный backend E2E:

```text
OWNER registers
        ↓
Organization
        ↓
Location
        ↓
Business Hours + Response Threshold
        ↓
Service Catalog
        ↓
Source Connector
        ↓
Real inbound event
        ↓
RawEvent
        ↓
Canonical Message
        ↓
Contact
        ↓
Conversation
        ↓
Opportunity
        ↓
Risk Engine
        ↓
Risk
        ↓
Radar
        ↓
Telegram notification
        ↓
Recommendation
        ↓
Action
        ↓
Outcome
        ↓
Confirmed Revenue
        ↓
Revenue Attribution
        ↓
Confirmed Recovered Revenue
```

Ни один шаг этого сценария не должен требовать ручного изменения PostgreSQL разработчиком.

---

# 5. Архитектурные non-negotiables

Следующие решения запрещено упрощать.

### 5.1. Modular Monolith

Backend остаётся modular monolith.

Микросервисы до появления измеренной необходимости не вводятся.

### 5.2. Несколько runtime процессов

Из одного repository собираются отдельные процессы:

```text
lidradar-api
lidradar-worker
lidradar-scheduler
lidradar-ai-agent
lidradar-migrate
```

### 5.3. PostgreSQL — Source of Truth

PostgreSQL является источником истины для:

* business state;
* sessions;
* jobs;
* scheduled checks;
* idempotency;
* outbox;
* AI job metadata;
* AI run metadata;
* notification state.

SSE, AI Node и frontend cache источниками истины не являются.

### 5.4. Persist first

External event сначала сохраняется.

Только после этого выполняются:

* normalization;
* conversation processing;
* Opportunity calculation;
* Risk processing;
* AI;
* notifications.

### 5.5. Raw Data != Interpretation

Raw provider payload сохраняется отдельно от:

* Message;
* Opportunity;
* semantic interpretation;
* Risk.

### 5.6. Conversation != Opportunity

Conversation — долгоживущая переписка.

Opportunity — конкретная коммерческая возможность внутри неё.

В MVP на Conversation допускается максимум одна активная Opportunity.

### 5.7. Stage != Risk

`PRICE_SENT`, `BOOKED`, `WAITING_CUSTOMER` — состояния продажи.

`NO_RESPONSE`, `BOOKING_NOT_CONFIRMED` — риски.

Эти понятия запрещено объединять.

### 5.8. AI не принимает финальное бизнес-решение

AI возвращает semantic facts.

Финальное создание/обновление Risk выполняет Risk Engine.

### 5.9. AI полностью asynchronous

Ни один пользовательский API request и Telegram webhook не ждёт LLM inference.

### 5.10. AI Node не является Source of Truth

AI-NODE-01 допускается полностью уничтожить и развернуть заново без потери business data.

---

# 6. Что запрещено добавлять в MVP

Без отдельного ADR запрещено добавлять:

* GraphQL;
* WebSocket;
* Redis;
* Kafka;
* NATS;
* Kubernetes;
* Elasticsearch;
* ClickHouse;
* microservices;
* userbot;
* MTProto production sessions;
* autonomous AI sales;
* automatic customer reply;
* полноценную CRM;
* полноценный BI;
* complex billing;
* филиальную permission hierarchy;
* отдельное мобильное приложение.

---

# 7. Backend technology baseline

Обязательный baseline:

```text
Go: PostgreSQL:
1.26.x 18.x
```

Рекомендуемый implementation stack фиксируется следующим образом.

### HTTP

```text
net/http
+
chi/v5
```

Business/domain слой не зависит от router.

### PostgreSQL

```text
pgx/v5
```

ORM не использовать.

Причина: проект интенсивно использует:

* explicit transactions;
* partial indexes;
* composite tenant FK;
* RLS;
* JSONB;
* `FOR UPDATE SKIP LOCKED`;
* cursor pagination;
* PostgreSQL-specific queue patterns.

Для типизированных SQL queries допускается/рекомендуется `sqlc`.

### Logging

Go `log/slog` с JSON handler.

### Money

Decimal library.

Никакого `float32/float64` для денежных значений.

### API

OpenAPI является контрактом backend.

Файл:

```text
contracts/openapi/openapi.yaml
```

Frontend client генерируется из него.

### Metrics

Prometheus-compatible metrics.

### Tracing

OpenTelemetry-compatible tracing.

### Local environment

Docker Compose.

Минимально:

```text
PostgreSQL 18
Go API
Go Worker
Go Scheduler
Fake AI Provider
S3-compatible local object storage
```

RTX 4060 не должна требоваться обычному backend/frontend разработчику.

---

# 8. Структура repository

```text
lidradar/
├── backend/
│   ├── cmd/
│   │   ├── api/
│   │   ├── worker/
│   │   ├── scheduler/
│   │   ├── ai-agent/
│   │   └── migrate/
│   │
│   ├── internal/
│   │   ├── auth/
│   │   ├── tenant/
│   │   ├── organization/
│   │   ├── location/
│   │   ├── integration/
│   │   ├── contact/
│   │   ├── conversation/
│   │   ├── opportunity/
│   │   ├── risk/
│   │   ├── recommendation/
│   │   ├── action/
│   │   ├── outcome/
│   │   ├── revenue/
│   │   ├── ai/
│   │   ├── notification/
│   │   ├── analytics/
│   │   ├── admin/
│   │   ├── audit/
│   │   ├── jobs/
│   │   └── events/
│   │
│   └── platform/
│       ├── postgres/
│       ├── http/
│       ├── objectstorage/
│       ├── crypto/
│       └── observability/
│
├── frontend/
├── contracts/
│   ├── openapi/
│   ├── events/
│   └── ai/
├── infra/
│   ├── local/
│   ├── staging/
│   ├── production/
│   └── ai-node/
├── models/
│   └── manifests/
└── docs/
    ├── architecture/
    └── adr/
```

---

# 9. Структура каждого domain module

Пример:

```text
internal/risk/

domain/
    entity.go
    value_objects.go
    repository.go
    events.go
    policy.go

application/
    evaluate.go
    acknowledge.go
    resolve.go
    feedback.go

infrastructure/
    postgres_repository.go

transport/
    http_handler.go
```

Dependency direction:

```text
transport
    ↓
application
    ↓
domain
```

Infrastructure реализует ports/interfaces application/domain.

Domain запрещено импортировать:

```text
PostgreSQL
pgx
HTTP
chi
Telegram
llama.cpp
OpenAI/provider SDK
transport DTO
Vue
```

---

# 10. Общие правила Go

Каждый public application operation принимает `context.Context`.

Handler не содержит business logic.

Repository не содержит domain policy.

SQL DTO/records не должны автоматически становиться domain entities.

Transport DTO не должны использоваться внутри domain.

Ошибки domain/application должны типизироваться и переводиться в HTTP errors только transport layer.

Network calls запрещены внутри открытой PostgreSQL transaction.

Все критические side effects должны быть:

* transactional;
* idempotent;
* retry-safe.

---

# 11. PostgreSQL conventions

## 11.1. IDs

Использовать UUIDv7-compatible IDs.

Не использовать sequential integer IDs как public business identifiers.

## 11.2. Время

Все timestamp:

```text
TIMESTAMPTZ
```

В PostgreSQL хранится UTC.

Timezone применяется только при вычислении business logic и отображении.

## 11.3. Деньги

PostgreSQL:

```text
NUMERIC(14,2)
```

Go:

```text
decimal.Decimal
```

REST JSON:

```json
{
  "amount": "47000.00",
  "currency": "RUB"
}
```

Деньги никогда не передаются JSON number.

## 11.4. JSONB

JSONB разрешён для:

* provider payload;
* metadata;
* capabilities;
* extensible event payload;
* AI output.

Core domain state целиком в JSONB не помещать.

---

# 12. Multi-tenancy

Organization = Tenant.

Каждая tenant-owned business table содержит:

```text
tenant_id UUID NOT NULL
```

Каждый repository method получает:

```text
tenantID + entityID
```

Запрещён интерфейс:

```go
GetConversation(ctx, conversationID)
```

Обязателен:

```go
GetConversation(ctx, tenantID, conversationID)
```

Это правило распространяется на:

* contacts;
* conversations;
* messages;
* opportunities;
* risks;
* recommendations;
* actions;
* outcomes;
* revenue;
* notifications;
* analytics;
* integrations.

До первого production pilot обязательны RLS policies минимум на:

```text
contacts
conversations
messages
opportunities
risk_signals
outcomes
revenue_events
revenue_attributions
```

RLS является вторым уровнем защиты.

Application tenant checks остаются обязательными.

---

# 13. Tenant-aware referential integrity

Критические связи должны предотвращать cross-tenant FK даже при programming error.

Пример:

```sql
UNIQUE (tenant_id, id)
```

и:

```sql
FOREIGN KEY (tenant_id, conversation_id)
REFERENCES conversations(tenant_id, id)
```

Обязательные tenant-aware chains:

```text
Opportunity → Conversation
Risk → Opportunity
Recommendation → Risk
Action → Risk
Outcome → Opportunity
Revenue → Opportunity
RevenueAttribution → Risk/Action/Outcome/Revenue
Message → Conversation
```

---

# 14. Authentication

Использовать server-side opaque sessions.

Browser получает cookie:

```text
HttpOnly
Secure
SameSite
```

В БД хранится только hash session token.

Plain session token в БД не сохраняется.

Password:

```text
plaintext
   ↓
Argon2id
   ↓
password_hash
```

Plaintext password никогда:

* не сохраняется;
* не логируется;
* не попадает в audit;
* не попадает в tracing.

State-changing browser requests должны иметь CSRF protection/origin validation.

---

# 15. Roles и permissions

MVP roles:

```text
OWNER
MANAGER
```

System role:

```text
PLATFORM_ADMIN
```

Role принадлежит Membership, а не User.

Application layer работает с permissions, а не с проверками `role == "OWNER"` по всему коду.

Минимальные permissions:

```text
risk.read
risk.manage
conversation.read
opportunity.manage
action.manage
outcome.manage
revenue.confirm
revenue.read
analytics.read
integration.manage
organization.manage
location.manage
service.manage
notification.manage
member.manage
```

OWNER получает все tenant permissions.

MANAGER:

```text
risk.read
risk.manage
conversation.read
opportunity.manage
action.manage
outcome.manage
revenue.confirm
```

MANAGER не получает:

```text
organization.manage
integration.manage
member.manage
notification.manage
analytics.read
```

если это отдельно не разрешено позднейшим ADR.

---

# 16. API conventions

Base URL:

```text
/api/v1
```

Internal machine API:

```text
/internal/v1
```

Единый error envelope:

```json
{
  "error": {
    "code": "RISK_NOT_FOUND",
    "message": "Risk not found",
    "details": {},
    "traceId": "..."
  }
}
```

Каждый external request получает:

```text
request_id
trace_id
```

Идентификатор возвращается в response header.

---

# 17. Pagination

Для:

* messages;
* conversations;
* risks;
* audit logs;

используется cursor pagination.

Формат:

```text
?limit=50&cursor=...
```

Default:

```text
50
```

Maximum:

```text
100
```

Offset pagination для этих потоков запрещена.

Cursor должен быть opaque для клиента.

---

# 18. Public API baseline

## Auth

```text
POST /api/v1/auth/register
POST /api/v1/auth/login
POST /api/v1/auth/logout
POST /api/v1/auth/refresh
GET  /api/v1/auth/me
```

## Organization

```text
POST  /api/v1/organizations
GET   /api/v1/organization
PATCH /api/v1/organization
```

## Locations

```text
GET   /api/v1/locations
POST  /api/v1/locations
PATCH /api/v1/locations/{id}

PUT   /api/v1/locations/{id}/business-hours
```

## Service Catalog

```text
GET    /api/v1/services
POST   /api/v1/services
PATCH  /api/v1/services/{id}
DELETE /api/v1/services/{id}
```

DELETE в MVP реализует deactivate, если hard delete нарушает audit/history.

## Integrations

```text
GET    /api/v1/integrations
POST   /api/v1/integrations/{provider}/connect
DELETE /api/v1/integrations/{id}
GET    /api/v1/integrations/{id}/health
```

## Radar

```text
GET /api/v1/radar
```

Filters:

```text
locationId
severity
riskType
```

## Risks

```text
GET  /api/v1/risks
GET  /api/v1/risks/{id}

POST /api/v1/risks/{id}/acknowledge
POST /api/v1/risks/{id}/resolve
POST /api/v1/risks/{id}/feedback
POST /api/v1/risks/{id}/actions
```

## Conversations

```text
GET /api/v1/conversations
GET /api/v1/conversations/{id}
GET /api/v1/conversations/{id}/messages
```

## Opportunities

```text
GET   /api/v1/opportunities/{id}
PATCH /api/v1/opportunities/{id}

POST /api/v1/opportunities/{id}/outcomes
POST /api/v1/opportunities/{id}/revenue
```

## Analytics

```text
GET /api/v1/analytics/summary
```

## Realtime

```text
GET /api/v1/events
```

SSE.

---

# 19. Idempotency

`Idempotency-Key` обязателен минимум для:

```text
POST revenue
POST Action с critical side effect
POST Outcome
Telegram callback commands
```

Хранилище:

`idempotency_keys`

Unique:

```text
tenant_id + key + operation
```

Один и тот же key с другим request hash должен вернуть conflict.

Повтор с тем же request hash возвращает сохранённый результат.

---

# 20. Connector architecture

Domain является channel-independent.

Connector contract:

```go
type Connector interface {
    Provider() string

    VerifyEvent(
        ctx context.Context,
        connection ChannelConnection,
        payload []byte,
        headers http.Header,
    ) error

    NormalizeEvent(
        ctx context.Context,
        connection ChannelConnection,
        event RawEvent,
    ) ([]CanonicalEvent, error)

    Health(
        ctx context.Context,
        connection ChannelConnection,
    ) ConnectionHealth
}
```

Outgoing transport является отдельной optional capability.

---

# 21. Production connector priority

P0:

```text
CONNECTED_BUSINESS_BOT
```

Fallback:

```text
STANDARD_BOT
```

Development:

```text
TEST
IMPORT
GENERIC_WEBHOOK
```

Запрещено production MVP:

```text
MTProto
userbot
```

Telegram source connector и Telegram notification transport — **два разных модуля**, даже если используют одного физического Bot.

---

# 22. Webhook processing contract

Критический flow:

```text
Webhook
   ↓
signature / event verification
   ↓
short PostgreSQL transaction
   ↓
INSERT RawEvent
   ↓
INSERT normalize job/outbox
   ↓
COMMIT
   ↓
HTTP success
```

После COMMIT:

```text
Worker
   ↓
normalize
   ↓
canonical events
   ↓
Conversation processing
```

В webhook request запрещено:

* AI inference;
* отправлять Telegram notification;
* выполнять длительный normalization;
* ждать downstream worker;
* делать сложную business аналитику.

---

# 23. RawEvent

Основная гарантия:

```text
UNIQUE(connection_id, external_event_id)
```

Повтор provider event не создаёт новый RawEvent.

Lifecycle:

```text
RECEIVED
PROCESSING
PROCESSED
FAILED
```

Permanent malformed event:

```text
FAILED
```

с error_code.

Raw payload сохраняется для replay/debugging согласно retention policy.

---

# 24. Canonical Conversation Model

Основные entities:

```text
Contact
ExternalIdentity
Conversation
Message
Attachment
```

`ExternalIdentity` не является Contact.

Один Contact может иметь несколько provider identities.

Message direction:

```text
INCOMING
OUTGOING
SYSTEM
```

Типы:

```text
TEXT
IMAGE
VOICE
AUDIO
VIDEO
DOCUMENT
OTHER
```

---

# 25. Conversation revision

`conversations.revision`:

```text
BIGINT NOT NULL DEFAULT 0
```

Revision атомарно увеличивается при canonical изменении, способном изменить interpretation:

* новое meaningful message;
* edit message;
* delete message;
* изменение canonical content.

Revision является freshness token для asynchronous AI.

---

# 26. Opportunity

Opportunity stages:

```text
NEW
ENGAGED
QUALIFYING
PRICE_SENT
WAITING_CUSTOMER
WAITING_BUSINESS
BOOKING_INTENT
BOOKED
WON
LOST
ARCHIVED
```

MVP invariant:

```text
не более одной active Opportunity
на одну Conversation
```

DB enforcement:

```sql
UNIQUE (tenant_id, conversation_id)
WHERE stage NOT IN ('WON','LOST','ARCHIVED')
```

Каждое изменение stage создаёт append-only `opportunity_stage_history`.

Source:

```text
RULE
AI
USER
IMPORT
```

---

# 27. Risk model

Risk types MVP:

```text
NO_RESPONSE
CUSTOMER_SILENT_AFTER_PRICE
BOOKING_NOT_CONFIRMED
PROMISE_NOT_FULFILLED
FOLLOW_UP_CANDIDATE
```

Severity:

```text
LOW
MEDIUM
HIGH
CRITICAL
```

Status:

```text
OPEN
ACKNOWLEDGED
ACTED
RESOLVED
FALSE_POSITIVE
IGNORED
EXPIRED
```

Source:

```text
RULE
HYBRID
MANUAL
```

AI source отсутствует намеренно.

AI не создаёт Risk напрямую.

---

# 28. Risk deduplication

Одновременно может существовать только один active Risk:

```text
tenant
+
opportunity
+
risk type
```

Active:

```text
OPEN
ACKNOWLEDGED
ACTED
```

При повторном обнаружении существующий Risk обновляет:

* severity;
* reason;
* detected metadata;
* updated_at.

Новый Risk не создаётся.

---

# 29. Risk R1 — NO_RESPONSE

AI не используется.

Условия:

```text
last meaningful message = INCOMING
AND no OUTGOING after triggering message
AND active Opportunity exists
AND elapsed business time >= response threshold
```

Default:

```text
45 business minutes
```

Severity:

```text
45–89 min → HIGH
>=90 min  → CRITICAL
```

Business time считается относительно:

```text
Location.timezone
+
location_business_hours
```

Если incoming пришёл в 20:50, Location работает до 21:00 и осталось 35 минут threshold:

```text
10 минут считаются сегодня
25 минут переносятся на следующий рабочий период
```

При появлении OUTGOING после triggering incoming Risk автоматически закрывается.

---

# 30. R2 — CUSTOMER_SILENT_AFTER_PRICE

Условие:

```text
stage = PRICE_SENT
OR AI semantic flag = PRICE_MENTIONED
```

После outgoing бизнеса отсутствует incoming клиента.

Default:

```text
24 business hours → MEDIUM
48 business hours → HIGH
```

Не создавать при:

```text
BOOKED
WON
LOST
ARCHIVED
CUSTOMER_REJECTED
```

Закрывается после нового meaningful incoming клиента.

---

# 31. R3 — BOOKING_NOT_CONFIRMED

Условие:

```text
bookingIntent >= 0.85
OR stage = BOOKING_INTENT
```

и:

```text
stage != BOOKED
waitingFor = BUSINESS
```

Default:

```text
30 business minutes
```

Severity:

```text
CRITICAL
```

Закрывается после подтверждённого booking.

---

# 32. R4 — PROMISE_NOT_FULFILLED

Semantic fact:

```text
businessCommitment.detected = true
```

Если AI уверенно определил срок:

```text
due_at = promised time
```

Если срок не определён:

```text
60 business minutes
```

Если после commitment отсутствует соответствующий follow-up:

```text
Risk = PROMISE_NOT_FULFILLED
severity = HIGH
```

---

# 33. R5 — FOLLOW_UP_CANDIDATE

Semantic signal:

```text
CUSTOMER_HESITATES
OR FOLLOW_UP_POSSIBLE
```

и:

```text
stage = WAITING_CUSTOMER
```

при отсутствии явного отказа.

Default delay:

```text
24 business hours
```

Severity:

```text
MEDIUM
```

По умолчанию отправляется через digest, а не immediate notification.

---

# 34. Radar ordering

Ordering принадлежит backend.

Frontend запрещено самостоятельно пересчитывать priority.

Canonical lexicographic ordering MVP:

```text
1. severity DESC
2. booking_intent DESC NULLS LAST
3. estimated_revenue DESC NULLS LAST
4. waiting_duration DESC
5. detected_at ASC
6. risk_id ASC
```

Последний ID нужен для stable pagination/order.

---

# 35. Recommendations

Для каждого Risk должен существовать useful recommendation даже при полном падении AI.

Первый слой:

```text
Template Recommendation
```

Второй слой:

```text
AI-generated reply draft
```

LLM не вызывается автоматически только ради красивого текста.

Пример:

```text
NO_RESPONSE
→ "Ответить клиенту сейчас."

BOOKING_NOT_CONFIRMED
→ "Предложить клиенту конкретный свободный слот."

FOLLOW_UP_CANDIDATE
→ "Уточнить, остаётся ли услуга актуальной."
```

---

# 36. Actions

Action является append-only business fact.

MVP types:

```text
OPEN_CONVERSATION
COPY_REPLY
MARK_CONTACTED
CALL
SEND_MESSAGE
OTHER
```

Для Telegram callback допускаются безопасные действия:

```text
OPEN_RISK
ACKNOWLEDGE
SNOOZE
```

Финансовые операции из Telegram callback запрещены.

---

# 37. Outcomes

Outcome append-only.

Statuses:

```text
RESPONDED
BOOKED
PAID
LOST
THINKING
NOT_A_LEAD
```

Ошибочный Outcome не изменяется задним числом.

Создаётся новый Outcome.

---

# 38. Revenue

Revenue Event:

```text
POTENTIAL
CONFIRMED
```

Sources:

```text
SERVICE_CATALOG
AI_EXTRACTION
USER_CONFIRMED
IMPORT
INTEGRATION
```

Potential revenue никогда не считается фактической выручкой.

---

# 39. Recovered Revenue Attribution

Confirmed Revenue и Confirmed Recovered Revenue являются разными показателями.

Recovered Revenue существует только при формальной цепочке:

```text
Risk
  ↓
Action
  ↓
Outcome
  ↓
Confirmed RevenueEvent
  ↓
RevenueAttribution
```

Attribution:

```text
RECOVERED
ORGANIC
UNKNOWN
```

Один `RevenueEvent` может иметь максимум одну attribution.

Для `RECOVERED` все entities обязаны:

* принадлежать одному tenant;
* принадлежать одной Opportunity;
* укладываться в attribution window.

Baseline attribution window:

```text
30 days
```

Настраивается centrally.

Confirmed Recovered Revenue:

```text
SUM(CONFIRMED RevenueEvent)
WHERE attribution_type = RECOVERED
```

Никакие heuristic assumptions не увеличивают этот KPI.

---

# 40. Background job infrastructure

На MVP используется PostgreSQL queue.

Generic lifecycle:

```text
PENDING
PROCESSING
RETRY
SUCCEEDED
DEAD
```

Claim:

```sql
SELECT ...
FOR UPDATE SKIP LOCKED
```

с lease.

Worker должен выдерживать:

```text
job выполняет side effect
        ↓
process падает до ACK
        ↓
job выполняется повторно
```

без duplicate:

* Message;
* Risk;
* RevenueEvent;
* Notification;
* Action critical side effect.

---

# 41. Retry policy

Retryable:

```text
network timeout
temporary provider error
provider 5xx
AI node unavailable
temporary DB conflict
```

Permanent:

```text
invalid payload
invalid schema
unsupported event
entity missing after validation
```

Permanent error не должен выполнять пять бессмысленных retries.

Baseline backoff:

```text
attempt 1 → immediately
attempt 2 → ~5 sec
attempt 3 → ~30 sec
attempt 4 → ~2 min
attempt 5 → ~10 min
then DEAD
```

Допускается jitter.

---

# 42. Transactional Outbox

Domain state mutation и Outbox Event записываются в **одной PostgreSQL transaction**.

Запрещено:

```text
COMMIT domain
↓
attempt external notification
↓
hope process does not crash
```

Обязательный вариант:

```text
BEGIN
    mutate domain
    insert outbox event
COMMIT

background worker
    ↓
dispatch event
```

---

# 43. Event contract

Все internal events имеют envelope:

```json
{
  "id": "uuid",
  "type": "risk.opened",
  "version": 1,
  "occurredAt": "...",
  "tenantId": "uuid",
  "aggregate": {
    "type": "risk",
    "id": "uuid"
  },
  "traceId": "uuid",
  "data": {}
}
```

MVP events:

```text
message.received.v1

conversation.created.v1
conversation.updated.v1

opportunity.created.v1
opportunity.stage_changed.v1

ai.analysis_completed.v1

risk.opened.v1
risk.updated.v1
risk.resolved.v1

recommendation.created.v1

action.recorded.v1
outcome.recorded.v1

revenue.confirmed.v1

notification.created.v1
```

Event schema после публикации считается immutable.

Изменение payload требует новой версии.

---

# 44. SSE

Endpoint:

```text
GET /api/v1/events
```

Минимальные events:

```text
risk.created
risk.updated
risk.resolved
conversation.updated
revenue.updated
notification.created
```

SSE является сигналом invalidate/refetch.

SSE **не является Source of Truth**.

Потеря SSE connection не должна приводить к потере business state.

---

# 45. Notification Engine

`Notification` — логический факт.

`NotificationDelivery` — конкретная попытка доставки transport.

Transports MVP:

```text
IN_APP
TELEGRAM
```

Один logical notification может иметь несколько deliveries.

Telegram failure не закрывает и не удаляет Risk.

---

# 46. Notification defaults

Default policy:

```text
NO_RESPONSE
→ IMMEDIATE Telegram

BOOKING_NOT_CONFIRMED
→ IMMEDIATE Telegram

PROMISE_NOT_FULFILLED
→ IMMEDIATE Telegram

CUSTOMER_SILENT_AFTER_PRICE
→ DIGEST/configurable

FOLLOW_UP_CANDIDATE
→ DIGEST/configurable
```

Modes:

```text
IMMEDIATE
DIGEST
DISABLED
```

Поддержать quiet hours.

---

# 47. Notification deduplication

Risk notification получает deterministic dedup key.

Например:

```text
risk:{risk_id}:opened
```

Повторные recalculations одного Risk не создают повторный user-visible alert.

Retry Telegram API также не создаёт новый logical Notification.

---

# 48. Local AI architecture

Cloud Core не соединяется напрямую с домашним компьютером.

AI-NODE-01 самостоятельно выполняет outbound HTTPS requests.

```text
AI Agent
    ↓ HTTPS
Cloud Core
    ↓
claim job
    ↓
AI Agent
    ↓ localhost
llama.cpp
    ↓
result
    ↓ HTTPS
Cloud Core
```

AI Node не требует:

* public IP;
* port forwarding;
* public llama.cpp;
* public SSH;
* public VPN endpoint.

---

# 49. AI Node baseline

Hardware:

```text
Intel i5-13400F
16 GB DDR5
RTX 4060 8 GB
```

OS:

```text
Ubuntu Server 24.04 LTS
```

Runtime:

```text
NVIDIA Driver
Docker Engine
NVIDIA Container Toolkit
Go AI Agent
llama.cpp
```

llama.cpp:

```text
localhost/internal Docker network only
```

Никогда:

```text
0.0.0.0:<llama-port> → Internet
```

---

# 50. AI Node retention

Business data retention на AI Node:

```text
0
```

После выполнения job:

```text
prompt → RAM
result → Cloud
prompt → released
```

Постоянно могут храниться только:

* model files;
* model manifests;
* AI agent config;
* benchmarks;
* system logs без customer text.

---

# 51. AI Node protocol

Heartbeat:

```text
POST /internal/v1/ai/nodes/heartbeat
```

Claim:

```text
POST /internal/v1/ai/jobs/claim
```

Started:

```text
POST /internal/v1/ai/jobs/{id}/started
```

Complete:

```text
POST /internal/v1/ai/jobs/{id}/complete
```

Failed:

```text
POST /internal/v1/ai/jobs/{id}/failed
```

Heartbeat baseline:

```text
~10 sec
```

Node unavailable:

```text
30–60 sec without heartbeat
```

---

# 52. AI Node authentication

Каждый node имеет:

```text
node_id
node_secret
```

Request содержит:

```text
X-LidRadar-Node-ID
Authorization/signature
timestamp
```

Cloud проверяет:

* node status;
* timestamp replay window;
* signature;
* body hash.

Node secret:

* encrypted в cloud;
* root-readable config на AI Node;
* никогда не логируется.

---

# 53. AI job

AI lifecycle:

```text
PENDING
LEASED
RUNNING
SUCCEEDED
RETRY
DEAD
```

Основные fields:

```text
tenant_id
job_type
entity_type
entity_id
priority
payload
model_requirement
schema_version
prompt_version
base_conversation_revision
analysis_through_message_id
attempts
available_at
leased_by
lease_until
```

Initial:

```text
max_inflight = 1
```

---

# 54. AI Provider abstraction

Cloud application зависит от:

```go
type AIProvider interface {
    AnalyzeConversation(...)
    GenerateRecommendation(...)
    SummarizeConversation(...)
}
```

Implementations:

```text
LocalAIProvider
FakeAIProvider
CloudAIProvider
```

Domain не знает, какой provider выполнял inference.

---

# 55. AI policy

Организация в будущем поддерживает:

```text
LOCAL_ONLY
LOCAL_PREFERRED
CLOUD_ALLOWED
```

MVP default:

```text
LOCAL_ONLY
```

Fake Provider обязателен для tests/development.

Cloud Provider допускается для:

* benchmark;
* quality comparison;
* emergency experiments.

---

# 56. AI context

Maximum:

```text
4096 tokens
```

Target:

```text
1500–3000 tokens
```

Context:

```text
SYSTEM INSTRUCTION
COMPANY CONTEXT
CONVERSATION SUMMARY
LAST 10–20 MESSAGES
TASK
OUTPUT SCHEMA
```

Полная многолетняя история клиента модели не отправляется.

---

# 57. AI output

Analysis:

```text
<=400 tokens
```

Recommendation:

```text
<=200 tokens
```

Summary:

```text
<=500 tokens
```

AI возвращает только versioned structured semantic result.

---

# 58. AI validation pipeline

Перед применением результата:

```text
JSON parse
    ↓
JSON schema validation
    ↓
enum validation
    ↓
numeric range validation
    ↓
semantic consistency validation
    ↓
freshness check
    ↓
domain application
```

Пример invalid:

```text
price.mentioned = false
price.amount = 55000
```

Результат сохраняется для observability, но domain не изменяется.

---

# 59. AI confidence

Baseline:

```text
>=0.85       strong
0.65–0.849   weak
<0.65        untrusted
```

Untrusted interpretation не может автоматически изменить Opportunity.

Low confidence должен вести к:

* no domain mutation;
* optional manual review;
* metrics.

---

# 60. AI freshness

Перед inference:

```text
job.base_revision =
conversation.revision
```

После inference:

```text
current revision == base revision?
```

Если да:

```text
validate
→ apply semantic facts
```

Если нет:

```text
AIRun preserved
application_status = STALE
domain unchanged
optional new AI job
```

Это concurrency invariant.

---

# 61. AI model baseline

Initial model class:

```text
4B–8B
GGUF
Q4
context 4096
parallel 1
max_inflight 1
```

Конкретная модель до benchmark не фиксируется.

---

# 62. Benchmark gate

Dataset v1:

```text
ориентир 300–500
качественно размеченных случаев
```

Train/prompt tuning data отделяется от golden test.

Минимально измерять:

* JSON validity;
* lead precision;
* lead recall;
* stage accuracy;
* intent accuracy;
* service accuracy;
* booking intent accuracy;
* risk-related semantic precision;
* p50 latency;
* p95 latency;
* VRAM;
* OOM;
* tokens/sec.

JSON validity target:

```text
>=99%
```

Risk precision важнее красивого общего benchmark score.

---

# 63. Security baseline

Cloud:

* HTTPS only;
* firewall;
* SSH keys;
* PostgreSQL private;
* object storage private;
* encrypted integration credentials;
* secure cookies;
* CSRF protection;
* tenant checks;
* RLS before pilot;
* rate limiting;
* audit.

AI Node:

* inbound deny by default;
* no public llama.cpp;
* no public PostgreSQL;
* no permanent conversation storage;
* no raw prompts in logs.

---

# 64. Никогда не логировать

Запрещено логировать:

* passwords;
* session tokens;
* integration secrets;
* API secrets;
* Authorization headers;
* Telegram bot token;
* full message body;
* raw customer conversation;
* raw AI prompt;
* AI request containing customer payload.

---

# 65. Audit

Audit обязателен для critical actions:

```text
login/logout
integration connect/disconnect
member changes
organization settings
manual opportunity stage change
risk acknowledge
risk resolve
risk feedback
action record
outcome record
revenue confirmation
notification policy change
admin retry/replay/discard
```

Audit log append-only.

---

# 66. Observability

Каждый runtime пишет structured JSON logs.

Обязательные correlation fields по контексту:

```text
timestamp
level
service
request_id
trace_id
tenant_id
connection_id
raw_event_id
conversation_id
message_id
opportunity_id
risk_id
job_id
ai_run_id
duration_ms
```

Не каждый log обязан иметь все поля.

---

# 67. Metrics

API:

```text
http_requests_total
http_request_duration
http_4xx
http_5xx
db_query_duration
```

Workers:

```text
jobs_pending
jobs_processing
jobs_dead
job_wait_time
job_execution_time
```

Integration:

```text
raw_events_received
raw_events_duplicate
raw_events_failed
connection_health
```

AI:

```text
ai_node_online
ai_jobs_pending
ai_jobs_completed
ai_jobs_failed
ai_invalid_output
ai_stale_output
ai_queue_wait
ai_latency
ai_input_tokens
ai_output_tokens
ai_gpu_utilization
ai_gpu_memory_used
ai_gpu_temperature
```

Product:

```text
opportunities_created
risks_created
risks_false_positive
risks_acted
bookings
confirmed_payments
potential_revenue
confirmed_recovered_revenue
```

---

# 68. Business/technical cost metrics

С самого начала считать:

```text
AI Cost per Tenant
```

и:

```text
AI Cost per Recovered Revenue
```

Даже если local GPU имеет условную стоимость, инфраструктура должна позволять учитывать compute usage.

---

# 69. Data retention

Retention не hardcode'ится.

Central config:

```text
raw_event_days
message_days
ai_run_days
attachment_days
```

AI Node:

```text
business_data_retention = 0
```

---

# 70. Telegram data / ML boundary

Telegram business messages разрешено обрабатывать для непосредственного предоставления LidRadar.

Автоматически добавлять реальные customer conversations в:

* training dataset;
* prompt tuning dataset;
* evaluation dataset за пределами оказания сервиса;

запрещено.

Для этого требуется отдельное:

```text
explicit
active
revocable consent
```

и отдельная retention policy.

---

# 71. Reliability targets

Первый production:

```text
RPO <= 15 min
RTO <= 4 h
```

После появления существенного MRR:

```text
RPO <= 5 min
RTO <= 1 h
```

Backup считается работающим только после успешного restore test.

---

# 72. Performance baseline

На этапе capacity test:

```text
100 organizations
×
500 conversations/month
×
10 messages
≈
500 000 messages/month
+
bursts
```

Targets:

```text
API p95 without AI < 300 ms
Webhook persist p95 < 200 ms
Rule Risk creation < 10 sec after due/check
```

AI latency измеряется отдельно.

---

# 73. Scale triggers

До 100 organizations:

```text
1–2 API instances
1 Worker
1 Scheduler
1 PostgreSQL
RTX 4060 AI Node
```

AI capacity не масштабируется по количеству customers.

Наблюдаем:

```text
AI queue lag
p95 inference latency
GPU utilization
VRAM
jobs/day
```

Trigger:

```text
AI queue p95 wait >60 sec стабильно
```

или:

```text
GPU ≈100% при backlog
```

Только после этого увеличивать AI capacity.

---

# 74. Testing pyramid

Обязательны:

1. Domain unit tests.
2. PostgreSQL integration tests.
3. Connector contract tests.
4. AI contract tests.
5. E2E golden-path tests.
6. Tenant isolation tests.
7. Failure/retry tests.
8. Load tests перед pilot.

Unit tests должны прежде всего покрывать:

* Risk policies;
* business-time calculations;
* Opportunity transitions;
* Revenue Attribution;
* permissions;
* idempotency decisions.

---

# 75. Definition of Ready

Backend task не начинается, пока не определены:

* business goal;
* input;
* output;
* domain rules;
* API contract;
* DB impact;
* async/event effects;
* permissions;
* failure behavior;
* idempotency;
* acceptance criteria.

---

# 76. Definition of Done

Каждый backend feature считается Done только если:

* implementation complete;
* migration/schema готовы;
* unit tests;
* integration tests;
* errors mapped;
* logging добавлен;
* metrics добавлены;
* OpenAPI обновлён;
* tenant isolation проверена;
* permissions проверены;
* audit добавлен для critical action;
* idempotency определена;
* retry semantics определены;
* no sensitive logging;
* CI green;
* Exit Gate этапа проходит.

---

# 77. Правила выполнения backend backlog

Основной critical path выполняется **строго по порядку этапов**.

Следующий этап нельзя считать начатым в production branch, пока Exit Gate предыдущего не зелёный.

Намеренных исключений два.

**Первое — параллельное:**

```text
INT-TELEGRAM-001
```

Выполняется параллельно Foundation/Identity, потому что Telegram является наиболее рискованной внешней зависимостью.

Из него вырастает **INT-SHADOW-001** (этап A2) — теневой сбор реальных диалогов для golden dataset. Стартует сразу после Exit Gate этапа A и идёт параллельно этапам 13–16, потому что сбор 500 размеченных диалогов занимает 2–3 недели календарного времени и иначе оказывается на критическом пути к Milestone D. См. LR-BE-RM-025.

**Второе — вне очереди:**

```text
ЭТАП R — CONSISTENCY REMEDIATION
```

Выполняется между этапами 16 и 17. Устраняет расхождения между документами и дефекты схемы, которые не проявляются на этапах 0–16, но делают невозможной корректную реализацию этапов 17–26 либо искажают Confirmed Recovered Revenue. Этап 17 не начинается, пока Exit Gate этапа R не зелёный.

---

# ЭТАП 0 — ARCHITECTURE / REPOSITORY FREEZE

## Цель

Зафиксировать backend skeleton и не позволить разработчикам создавать несовместимые architectural patterns.

## Зависимости

Нет.

## Задачи

### LR-BE-0001 — Repository skeleton

Создать:

```text
cmd/api
cmd/worker
cmd/scheduler
cmd/ai-agent
cmd/migrate
internal/*
platform/*
contracts/*
```

**Acceptance:**

Все binaries собираются.

---

### LR-BE-0002 — Domain module template

Создать canonical module structure:

```text
domain
application
infrastructure
transport
```

**Acceptance:**

На одном reference module показана правильная dependency direction.

---

### LR-BE-0003 — Architecture dependency check

CI должен обнаруживать запрещённые imports.

Например domain → pgx должен приводить к failed build/lint.

---

### LR-BE-0004 — ADR baseline

Создать ADR для решений 001–031 из Architecture v1.1.

---

### LR-BE-0005 — Non-goals guard

Документировать список запрещённых MVP technologies.

## Exit Gate

Новый backend-разработчик однозначно понимает:

* куда добавлять domain logic;
* куда добавлять SQL;
* куда добавлять handler;
* как создавать runtime;
* какие решения нельзя менять внутри feature PR.

---

# ЭТАП 1 — FOUNDATION

## Цель

Получить пустую, но полностью deployable систему.

## Зависимость

Этап 0.

## Задачи

### LR-BE-0101 — Go bootstrap

Создать Go module и запуск всех runtime binaries.

### LR-BE-0102 — Typed configuration

ENV → typed configuration → startup validation.

Application не должен стартовать с invalid/missing critical config.

### LR-BE-0103 — Structured logging

JSON logging + service + request/trace correlation.

### LR-BE-0104 — PostgreSQL platform

pgx pool, connection health, graceful shutdown.

### LR-BE-0105 — Migration framework

Forward-compatible SQL migrations.

### LR-BE-0106 — HTTP platform

HTTP server:

```text
/health/live
/health/ready
```

Graceful shutdown.

### LR-BE-0107 — Error envelope

Единая error model.

### LR-BE-0108 — Docker Compose

One command local startup.

### LR-BE-0109 — CI baseline

Минимально:

```text
gofmt
go vet
static analysis
unit tests
integration smoke
migration smoke
OpenAPI validation
docker build
```

## Exit Gate

```text
git clone
→ docker compose up
→ PostgreSQL ready
→ migrations
→ API ready
→ /health succeeds
```

без ручных изменений.

---

# ПАРАЛЛЕЛЬНЫЙ ЭТАП A — INT-TELEGRAM-001

## Цель

До строительства product pipeline доказать реальный Telegram integration path.

## Зависимость

Foundation bootstrap.

## Задачи

### LR-BE-TG-001 — Development bot

Создать `@LidRadarDevBot`.

Включить Business Mode.

### LR-BE-TG-002 — Non-Premium account test

Подключить реальный российский Telegram account без Premium.

### LR-BE-TG-003 — business_connection

Получить и стабильно идентифицировать business connection.

### LR-BE-TG-004 — Incoming

Проверить реальный inbound message.

### LR-BE-TG-005 — Manual outgoing

Убедиться, что LidRadar получает manual outgoing сообщения менеджера.

### LR-BE-TG-006 — Event variants

Проверить:

```text
edit
delete
photo
voice
media
```

### LR-BE-TG-007 — Reconnect

Проверить disconnect/reconnect.

### LR-BE-TG-008 — Duplicate delivery

Один update ×10 не должен создавать 10 logical events.

### LR-BE-TG-009 — Stable identifiers

Зафиксировать stable IDs:

```text
connection
conversation
contact
message
```

### LR-BE-TG-010 — Telegram user linking spike

Одноразовый `/start` token.

### LR-BE-TG-011 — Test notification

Отправить реальный notification владельцу.

### LR-BE-TG-012 — Spike report

Зафиксировать:

```text
P0 = Connected Business Bot
fallback = Standard Bot
```

или documented blocking reason.

## Exit Gate

Подтверждено:

```text
CLIENT
→ BUSINESS
→ CLIENT
```

с правильным direction, dedup, reconnect и test alert.

Если Gate не пройден, semantic AI разработка не начинается.

---

# ЭТАП 2 — IDENTITY / TENANT / BUSINESS SETUP

## Цель

Создать SaaS-контур и tenant isolation с первого дня.

## Зависимость

Этап 1.

## Задачи

### LR-BE-0201 — User

Entity + repository + migration.

### LR-BE-0202 — Password authentication

Argon2id hashing.

### LR-BE-0203 — Session management

Opaque server-side sessions.

### LR-BE-0204 — Organization

Organization = Tenant.

### LR-BE-0205 — Membership

OWNER/MANAGER.

### LR-BE-0206 — Permission service

Central permission resolver.

### LR-BE-0207 — Location

Location + timezone + `response_threshold_minutes`.

### LR-BE-0208 — Business Hours

CRUD/upsert weekly schedule.

### LR-BE-0209 — Auth API

Register/login/logout/refresh/me.

### LR-BE-0210 — Organization API

Create/get/update.

### LR-BE-0211 — Location API

List/create/update/business hours.

### LR-BE-0212 — Tenant test harness

Reusable integration helper:

```text
Tenant A
Tenant B
```

для security tests.

## Exit Gate

OWNER:

```text
register
→ create organization
→ create location
→ configure hours
→ logout
→ login
→ data preserved
```

MANAGER не может менять Organization settings.

Tenant A с известным UUID entity Tenant B получает 404/403, но никогда данные B.

---

# ЭТАП 3 — SERVICE CATALOG

## Цель

Получить достоверный источник диапазона стоимости услуги.

## Задачи

### LR-BE-0301 — ServiceCatalogItem domain

### LR-BE-0302 — Service migration

Поля:

```text
name
normalized_name
price_from
price_to
currency
active
location_id
```

### LR-BE-0303 — Money validation

```text
price >= 0
price_from <= price_to
```

### LR-BE-0304 — Service CRUD API

### LR-BE-0305 — Tenant tests

## Exit Gate

OWNER может создать/изменить/деактивировать service.

При отсутствии данных potential revenue остаётся NULL — система не выдумывает цену.

---

# ЭТАП 4 — CONNECTOR CORE

## Цель

Создать channel-independent persist-first ingestion.

## Задачи

### LR-BE-0401 — ChannelConnection domain

### LR-BE-0402 — Connection Health

States:

```text
ACTIVE
DEGRADED
ERROR
DISCONNECTED
```

### LR-BE-0403 — Connector interface

Verify / Normalize / Health.

### LR-BE-0404 — RawEvent storage

Unique external event.

### LR-BE-0405 — Persist-first transaction

RawEvent + normalize work atomically persisted.

### LR-BE-0406 — TestConnector

### LR-BE-0407 — ImportConnector

### LR-BE-0408 — GenericWebhookConnector

### LR-BE-0409 — Telegram Connected Business Connector

На основе успешно завершённого spike.

### LR-BE-0410 — Integration API

Connect/list/delete/health.

### LR-BE-0411 — Duplicate tests

### LR-BE-0412 — Invalid payload tests

## Exit Gate

Настоящий Telegram update:

```text
HTTP receive
→ persisted once
→ fast response
```

и не зависит от downstream AI.

---

# ЭТАП 5 — CONVERSATION CORE

## Цель

Преобразовать provider events в стабильный canonical domain.

## Задачи

### LR-BE-0501 — Canonical Event contracts

### LR-BE-0502 — Contact resolution

### LR-BE-0503 — ExternalIdentity connection-aware namespace

### LR-BE-0504 — Conversation create/reuse

### LR-BE-0505 — Message ingestion

### LR-BE-0506 — Direction detection

### LR-BE-0507 — Edit semantics

### LR-BE-0508 — Delete semantics

### LR-BE-0509 — Attachments metadata

Binary → Object Storage.

### LR-BE-0510 — Conversation revision

Atomic revision increment.

### LR-BE-0511 — Conversation/Messages read API

### LR-BE-0512 — Connector fixtures tests

## Exit Gate

Первое сообщение:

```text
Contact
+
ExternalIdentity
+
Conversation
+
Message
```

Второе сообщение того же диалога создаёт только новый Message.

Provider-specific logic не попадает в Conversation domain.

---

# ЭТАП 6 — BACKGROUND INFRASTRUCTURE

## Цель

Создать crash-safe asynchronous foundation.

## Задачи

### LR-BE-0601 — Generic jobs table

### LR-BE-0602 — Job claim

`FOR UPDATE SKIP LOCKED`.

### LR-BE-0603 — Lease recovery

### LR-BE-0604 — Retry classification

### LR-BE-0605 — DEAD jobs

### LR-BE-0606 — Scheduler runtime

### LR-BE-0607 — Scheduled checks

### LR-BE-0608 — Transactional Outbox

### LR-BE-0609 — Outbox dispatcher

### LR-BE-0610 — Idempotent worker framework

### LR-BE-0611 — Crash test

`kill -9` после claim.

### LR-BE-0612 — Duplicate-side-effect tests

## Exit Gate

Worker можно убить после claim.

После lease expiration другой worker заканчивает работу.

Committed business event не теряется.

---

# ЭТАП 7 — OPPORTUNITY DOMAIN

## Цель

Создать отдельный commercial lifecycle.

## Задачи

### LR-BE-0701 — Opportunity aggregate

### LR-BE-0702 — Stage state machine

### LR-BE-0703 — One-active-opportunity invariant

Partial unique index.

### LR-BE-0704 — StageHistory

Append-only.

### LR-BE-0705 — PotentialRevenue

### LR-BE-0706 — Commercial candidate processor

### LR-BE-0707 — Manual stage API

### LR-BE-0708 — State transition tests

## Exit Gate

Conversation и Opportunity физически и логически разделены.

История stage полностью воспроизводима.

---

# ЭТАП 8 — FIRST RISK: NO_RESPONSE

## Цель

Доказать product value без LLM.

## Задачи

### LR-BE-0801 — Risk aggregate

### LR-BE-0802 — RiskRepository

### LR-BE-0803 — Risk versioned policy interface

### LR-BE-0804 — Business-time calculator

### LR-BE-0805 — NO_RESPONSE policy

### LR-BE-0806 — Scheduled due calculation

### LR-BE-0807 — Re-read current state at due time

Нельзя создавать Risk на основании stale scheduled payload.

### LR-BE-0808 — Risk dedup

### LR-BE-0809 — Auto-resolution

### LR-BE-0810 — Boundary tests

Обязательные:

```text
12:00 +45m → 12:45 Risk
20:50 +45 business min → next working period
response before threshold → no Risk
replayed check → one Risk
```

## Exit Gate — Milestone B backend core

Настоящий message flow автоматически создаёт корректный `NO_RESPONSE`.

AI полностью выключен.

---

# ЭТАП 9 — RADAR API / RISK DETAIL / SSE

## Цель

Предоставить frontend полный backend read-model первой product value.

## Задачи

### LR-BE-0901 — Radar query

### LR-BE-0902 — Backend priority ordering

### LR-BE-0903 — Radar summary

```text
openRisks
criticalRisks
potentialRevenue
confirmedRecoveredRevenue
```

### LR-BE-0904 — Risk list API

### LR-BE-0905 — Risk detail API

Response содержит связанные:

```text
Opportunity
Conversation
Recommendation
Actions
Outcome
Revenue
```

по мере их появления.

### LR-BE-0906 — Acknowledge API

### LR-BE-0907 — Resolve API

### LR-BE-0908 — SSE infrastructure

### LR-BE-0909 — SSE domain notifications

### LR-BE-0910 — Permission tests

## Exit Gate

После создания Risk frontend может получить его без reload через:

```text
SSE signal
→ REST refetch
```

---

# ЭТАП 10 — TELEGRAM NOTIFICATIONS

## Цель

Доставить critical Risk владельцу на телефон.

## Задачи

### LR-BE-1001 — Notification domain

### LR-BE-1002 — NotificationDelivery

### LR-BE-1003 — Telegram user link storage

### LR-BE-1004 — One-time link tokens

### LR-BE-1005 — TelegramNotificationTransport

### LR-BE-1006 — Notification Outbox handler

### LR-BE-1007 — Retry

### LR-BE-1008 — Dedup key

### LR-BE-1009 — Safe callbacks

```text
OPEN_RISK
ACKNOWLEDGE
SNOOZE
```

### LR-BE-1010 — Telegram-down failure tests

## Exit Gate — Milestone B

```text
real incoming
→ NO_RESPONSE
→ Risk
→ Radar
→ real Telegram alert
```

---

# ЭТАП 11 — RECOMMENDATION / ACTION / OUTCOME

## Цель

Превратить обнаруженный Risk в управляемое corrective action.

## Задачи

### LR-BE-1101 — Recommendation domain

### LR-BE-1102 — Template engine

### LR-BE-1103 — Action append-only entity

### LR-BE-1104 — Action API

### LR-BE-1105 — Outcome append-only entity

### LR-BE-1106 — Outcome API

### LR-BE-1107 — Idempotency

### LR-BE-1108 — Audit

## Exit Gate

Полный flow:

```text
Risk
→ Recommendation
→ Action
→ Outcome
```

проходит только через API.

---

# ЭТАП 12 — REVENUE / ATTRIBUTION

## Цель

Замкнуть Money Loop.

## Задачи

### LR-BE-1201 — RevenueEvent

### LR-BE-1202 — Revenue confirmation API

`Idempotency-Key` required.

### LR-BE-1203 — RevenueAttribution

### LR-BE-1204 — Cross-entity validation

Tenant + Opportunity consistency.

### LR-BE-1205 — Attribution window

Baseline 30 days.

### LR-BE-1206 — UNIQUE revenue attribution

### LR-BE-1207 — Confirmed Recovered Revenue query

### LR-BE-1208 — Revenue audit

### LR-BE-1209 — Money-loop integration test

Обязательный сценарий:

```text
NO_RESPONSE
→ MARK_CONTACTED
→ BOOKED
→ PAID 47 000
→ RECOVERED 47 000
```

## Exit Gate — Milestone C

LidRadar способен доказать возвращённую выручку без AI.

**До этого момента запрещено отвлекать основной backend team на сложный AI.**

---

# ЭТАП 13 — HOME AI NODE INFRASTRUCTURE

## Цель

Подключить домашний GPU как заменяемый compute resource.

## Задачи

### LR-BE-1301 — AI Node registry

### LR-BE-1302 — AI jobs

### LR-BE-1303 — AI runs

### LR-BE-1304 — Freshness fields

### LR-BE-1305 — Node authentication

### LR-BE-1306 — Heartbeat

### LR-BE-1307 — Claim API

### LR-BE-1308 — Started/complete/failed

### LR-BE-1309 — Lease renewal

### LR-BE-1310 — Go AI Agent

### LR-BE-1311 — llama.cpp local integration

### LR-BE-1312 — Automatic reboot recovery

### LR-BE-1313 — Fake AI Provider

### LR-BE-1314 — AI Node disconnect test

## Exit Gate

```text
Ubuntu reboot
→ Docker
→ llama.cpp
→ model
→ AI Agent
→ READY heartbeat
→ claim
→ inference
→ validated result
```

AI Node выключается — core API/NO_RESPONSE/Money Loop продолжают работать.

---

# ЭТАП 14 — AI CONVERSATION ANALYSIS

## Цель

Получать semantic facts, не передавая модели управление business state.

## Задачи

### LR-BE-1401 — ContextBuilder

### LR-BE-1402 — AnalyzeConversation request v1

### LR-BE-1403 — Output JSON schema v1

### LR-BE-1404 — JSON validation

### LR-BE-1405 — Enum/range validation

### LR-BE-1406 — Semantic consistency

### LR-BE-1407 — Confidence policy

### LR-BE-1408 — Model/prompt/schema versioning

### LR-BE-1409 — AIRun audit

### LR-BE-1410 — Freshness guard

### LR-BE-1411 — Stale reschedule

### LR-BE-1412 — ConversationSummary

### LR-BE-1413 — Contract tests

Cases:

```text
invalid JSON
missing field
unknown enum
timeout
low confidence
stale revision
```

## Exit Gate

Ни один invalid/stale result не изменяет Opportunity и не создаёт Risk.

---

# ЭТАП 15 — AI BENCHMARK / MODEL FREEZE

## Цель

Выбрать модель измерениями.

## Задачи

### LR-BE-1501 — Dataset format

### LR-BE-1502 — 300–500 labelled cases target

### LR-BE-1503 — Dataset split

### LR-BE-1504 — Golden set protection

### LR-BE-1505 — Model manifest

### LR-BE-1506 — Benchmark runner

### LR-BE-1507 — Quality metrics

### LR-BE-1508 — Performance metrics

### LR-BE-1509 — lidradar-main-v1 freeze

## Exit Gate

Модель проходит установленный quality/performance gate на RTX 4060.

До Gate конкретная model family архитектурой не фиксируется.

---

# ЭТАП 16 — SEMANTIC RISK: BOOKING_NOT_CONFIRMED

## Цель

Первый Risk, где AI реально добавляет value.

## Задачи

### LR-BE-1601 — Booking intent semantic mapping

### LR-BE-1602 — Deterministic R3 policy

### LR-BE-1603 — Scheduled 30 business min check

### LR-BE-1604 — Dedup

### LR-BE-1605 — Auto-resolution BOOKED

### LR-BE-1606 — Explainable reason

### LR-BE-1607 — Recommendation template

### LR-BE-1608 — Full E2E test

## Exit Gate

```text
"А завтра вечером можно?"
→ AI booking intent
→ deterministic policy
→ Risk
→ Radar
→ Telegram
```

Low confidence не изменяет domain.

---

# ЭТАП R — CONSISTENCY REMEDIATION

**Порядок:** вне очереди, между Этапом 16 и Этапом 17.
**Статус:** обязательный. Этап 17 не начинается в production branch, пока Exit Gate Этапа R не зелёный.

## Цель

Устранить расхождения между Tasks.md, Планом разработки MVP v1.2, GLOSSARY и README, а также технические дефекты схемы, обнаруженные при сквозной сверке документов (Errata v1.2.2).

Все дефекты этого этапа обладают одним общим свойством: они не проявляются на этапах 0–16, но делают невозможной корректную реализацию этапов 17–26 либо искажают Confirmed Recovered Revenue.

## Зависимости

Этап 16 закрыт.

## Правило нумерации миграций

Все schema-задачи этапа собираются в **одну** forward-only миграцию:

```text
000018_consistency_remediation.sql
```

Номер = следующий свободный после последней применённой миграции. Если на момент старта этапа применено больше миграций — занять следующий свободный номер и сдвинуть карту миграций Плана v1.2 на ту же величину. Миграция не переписывает существующие файлы: только `ALTER`, `CREATE INDEX`, `CREATE POLICY`.

## Задачи

---

### БЛОК R1 — Целостность схемы

---

### LR-BE-RM-001 — Attribution chain integrity

`revenue_attributions` не содержит `opportunity_id`, поэтому §39 («все entities обязаны принадлежать одной Opportunity») не проверяется на уровне БД, а одна Opportunity может получить несколько `RECOVERED`-атрибуций через разные `revenue_events`.

Добавить денормализованный `opportunity_id`, composite FK на все звенья цепочки и partial unique index.

```sql
ALTER TABLE revenue_events
    ADD CONSTRAINT revenue_events_tenant_opportunity_unique
    UNIQUE (tenant_id, id, opportunity_id);

ALTER TABLE risk_signals ADD CONSTRAINT risk_signals_tenant_opportunity_unique
    UNIQUE (tenant_id, id, opportunity_id);
ALTER TABLE actions ADD CONSTRAINT actions_tenant_opportunity_unique
    UNIQUE (tenant_id, id, opportunity_id);
ALTER TABLE outcomes ADD CONSTRAINT outcomes_tenant_opportunity_unique
    UNIQUE (tenant_id, id, opportunity_id);

ALTER TABLE revenue_attributions
    ADD COLUMN opportunity_id UUID,
    ADD COLUMN outcome_at TIMESTAMPTZ;

UPDATE revenue_attributions a
SET opportunity_id = e.opportunity_id
FROM revenue_events e
WHERE e.tenant_id = a.tenant_id AND e.id = a.revenue_event_id;

ALTER TABLE revenue_attributions
    ALTER COLUMN opportunity_id SET NOT NULL,
    ADD CONSTRAINT revenue_attributions_event_fk2
        FOREIGN KEY (tenant_id, revenue_event_id, opportunity_id)
        REFERENCES revenue_events(tenant_id, id, opportunity_id),
    ADD CONSTRAINT revenue_attributions_risk_fk2
        FOREIGN KEY (tenant_id, risk_id, opportunity_id)
        REFERENCES risk_signals(tenant_id, id, opportunity_id),
    ADD CONSTRAINT revenue_attributions_action_fk2
        FOREIGN KEY (tenant_id, action_id, opportunity_id)
        REFERENCES actions(tenant_id, id, opportunity_id),
    ADD CONSTRAINT revenue_attributions_outcome_fk2
        FOREIGN KEY (tenant_id, outcome_id, opportunity_id)
        REFERENCES outcomes(tenant_id, id, opportunity_id),
    ADD CONSTRAINT revenue_attributions_window_respected CHECK (
        kind <> 'RECOVERED'
        OR (outcome_at IS NOT NULL
            AND attributed_at <= outcome_at + make_interval(days => window_days))
    );

CREATE UNIQUE INDEX revenue_attributions_one_recovered_per_opportunity_idx
    ON revenue_attributions(tenant_id, opportunity_id)
    WHERE kind = 'RECOVERED';
```

Старые FK `revenue_attributions_risk_fk` / `_action_fk` / `_outcome_fk` удалить после проверки, что новые применились.

Продуктовое следствие: одна Opportunity даёт максимум одну `RECOVERED`-атрибуцию. При оплате частями RECOVERED получает первое событие, остальные — `ORGANIC`.

**Acceptance:**

Попытка создать вторую `RECOVERED`-атрибуцию для той же Opportunity отклоняется PostgreSQL; application возвращает `409 RECOVERED_ALREADY_ATTRIBUTED`. Попытка собрать цепочку из Risk/Action/Outcome чужой Opportunity внутри своего tenant отклоняется FK.

---

### LR-BE-RM-002 — Platform-default uniqueness

`tenant_id IS NULL` используется как «platform default», но `NULL` в обычном UNIQUE не конфликтует сам с собой — platform-строку можно вставить многократно.

```sql
ALTER TABLE risk_policy_configs
    DROP CONSTRAINT risk_policy_configs_one_per_type_and_tenant,
    ADD CONSTRAINT risk_policy_configs_one_per_type_and_tenant
        UNIQUE NULLS NOT DISTINCT (tenant_id, risk_type);

ALTER TABLE encrypted_secrets
    ADD CONSTRAINT encrypted_secrets_one_active_per_kind
        UNIQUE NULLS NOT DISTINCT (tenant_id, kind);
```

**Acceptance:**

Повторный `INSERT` platform-строки с тем же `risk_type` отклоняется. Дубликаты, если они успели появиться, устраняются в той же миграции до наложения constraint.

---

### LR-BE-RM-003 — feature_flags scope key

`key TEXT PRIMARY KEY` делает невозможным одновременное существование platform-default и tenant-override, то есть feature flag нельзя включить одному tenant'у. Это ломает LR-BE-2604 и Milestone D.

Таблица создаётся на этапе 26 — задача фиксирует **финальный** DDL, а не правит существующее:

```sql
CREATE TABLE feature_flags (
    id                 UUID PRIMARY KEY,
    key                TEXT NOT NULL,
    tenant_id          UUID REFERENCES organizations(id) ON DELETE CASCADE,
    enabled            BOOLEAN NOT NULL DEFAULT FALSE,
    rollout_percentage SMALLINT NOT NULL DEFAULT 0 CHECK (rollout_percentage BETWEEN 0 AND 100),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by         UUID,
    CONSTRAINT feature_flags_key_valid CHECK (key = btrim(key) AND char_length(key) BETWEEN 1 AND 100),
    CONSTRAINT feature_flags_unique_per_scope UNIQUE NULLS NOT DISTINCT (key, tenant_id)
);
```

Правило разрешения: строка с конкретным `tenant_id` побеждает строку с `tenant_id IS NULL`; отсутствие обеих = флаг выключен.

**Acceptance:**

`ai_enabled = FALSE` globally и `TRUE` для T1 сосуществуют; `IsEnabled` возвращает `true` для T1 и `false` для T2.

---

### LR-BE-RM-004 — ai_runs tenant_id

`ai_runs` не содержит `tenant_id`, из-за чего таблица не может участвовать в RLS, а FK `conversation_summaries.latest_valid_run_id` не является tenant-aware — нарушение §13.

```sql
ALTER TABLE ai_runs ADD COLUMN tenant_id UUID;

UPDATE ai_runs r SET tenant_id = j.tenant_id
FROM ai_jobs j WHERE j.id = r.job_id;

ALTER TABLE ai_runs
    ALTER COLUMN tenant_id SET NOT NULL,
    ADD CONSTRAINT ai_runs_tenant_fk FOREIGN KEY (tenant_id)
        REFERENCES organizations(id) ON DELETE CASCADE,
    ADD CONSTRAINT ai_runs_tenant_id_unique UNIQUE (tenant_id, id),
    ADD CONSTRAINT ai_runs_job_fk2 FOREIGN KEY (tenant_id, job_id)
        REFERENCES ai_jobs(tenant_id, id);
```

`ai_nodes` остаётся platform-level без `tenant_id` — узел обслуживает все tenant'ы.

**Acceptance:**

`ai_runs` включён в RLS-список; `AIRun` в Go содержит `TenantID`; все repository-методы принимают `tenantID` (§12).

---

### LR-BE-RM-005 — conversation_summaries optimistic lock

Гонка двух AI runs с разными revision сейчас разрешается тем, кто закоммитил вторым, — возможна перезапись свежего результата устаревшим. §60 требует обратного.

Привести таблицу к конвенции `id` + `UNIQUE (tenant_id, …)` и зафиксировать upsert:

```sql
INSERT INTO conversation_summaries (...)
VALUES (...)
ON CONFLICT (tenant_id, conversation_id) DO UPDATE
SET latest_valid_run_id      = excluded.latest_valid_run_id,
    derived_facts            = excluded.derived_facts,
    confidence               = excluded.confidence,
    conversation_revision_at = excluded.conversation_revision_at,
    last_message_id_at       = excluded.last_message_id_at,
    updated_at               = now()
WHERE excluded.conversation_revision_at > conversation_summaries.conversation_revision_at;
```

**Acceptance:**

Два параллельных run с revision 5 и 7 в любом порядке коммита оставляют в таблице результат revision 7. Отброшенный upsert логируется как `SUMMARY_SUPERSEDED`.

---

### LR-BE-RM-006 — AI queue deduplication + debounce

Freshness guard делает результат STALE на каждое новое сообщение и планирует новый job. Без дедупликации активный диалог порождает по заданию на сообщение, при `max_inflight = 1` и одной RTX 4060 это прямая потеря пропускной способности.

```sql
CREATE UNIQUE INDEX ai_jobs_one_active_per_entity_idx
    ON ai_jobs(tenant_id, entity_type, entity_id)
    WHERE status IN ('PENDING', 'LEASED', 'RUNNING');
```

Постановка задания — идемпотентная: `INSERT … ON CONFLICT DO UPDATE SET base_conversation_revision = excluded.base_conversation_revision`.

Дебаунс: повторный анализ той же Conversation планируется не раньше чем через `AI_ANALYSIS_DEBOUNCE = 60s` через существующее поле `available_at` (§53). Stale-переплан использует ту же задержку.

**Acceptance:**

Диалог, в который пришло 5 сообщений за 20 секунд, порождает одно задание с последней revision, а не пять.

---

### LR-BE-RM-007 — ai_jobs lease consistency

CHECK допускает половинчатое состояние (владелец есть, срок аренды пуст).

```sql
ALTER TABLE ai_jobs
    DROP CONSTRAINT ai_jobs_lease_consistency,
    ADD CONSTRAINT ai_jobs_lease_consistency CHECK (
        (status IN ('LEASED','RUNNING') AND leased_by IS NOT NULL AND lease_until IS NOT NULL)
     OR (status NOT IN ('LEASED','RUNNING') AND leased_by IS NULL AND lease_until IS NULL)
    );
```

**Acceptance:**

`INSERT` строки с `leased_by IS NOT NULL, lease_until IS NULL` отклоняется.

---

### LR-BE-RM-008 — platform_admins re-grant

`user_id UUID PRIMARY KEY` + `revoked_at` делает невозможной повторную выдачу прав после отзыва.

```sql
ALTER TABLE platform_admins DROP CONSTRAINT platform_admins_pkey;
ALTER TABLE platform_admins ADD COLUMN id UUID;
UPDATE platform_admins SET id = gen_random_uuid() WHERE id IS NULL;
ALTER TABLE platform_admins
    ALTER COLUMN id SET NOT NULL,
    ADD PRIMARY KEY (id),
    ADD COLUMN revoked_by UUID;

CREATE UNIQUE INDEX platform_admins_one_active_idx
    ON platform_admins(user_id) WHERE revoked_at IS NULL;
```

`Revoke` проставляет `revoked_at`/`revoked_by`, строку не удаляет.

**Acceptance:**

Grant → Revoke → Grant для одного пользователя проходит; в таблице две строки, активная одна; история выдач сохранена.

---

### LR-BE-RM-009 — memberships soft revoke

`revenue_events` и `risk_feedback` — append-only с триггером против UPDATE/DELETE — ссылаются на `memberships(tenant_id, user_id)`. Физическое удаление membership либо блокируется, либо каскадом сносит фактические записи.

```sql
ALTER TABLE memberships ADD COLUMN IF NOT EXISTS revoked_at TIMESTAMPTZ;

CREATE UNIQUE INDEX memberships_one_active_idx
    ON memberships(tenant_id, user_id) WHERE revoked_at IS NULL;
```

Правило в §11: членство не удаляется физически; отзыв доступа — `UPDATE … SET revoked_at = now()`. Permission service читает только `revoked_at IS NULL`.

**Acceptance:**

Отзыв доступа у сотрудника, подтвердившего выручку, проходит; `revenue_events` не затронуты; сотрудник теряет доступ немедленно.

---

### БЛОК R2 — Согласование имён и enum

---

### LR-BE-RM-010 — AI job lifecycle canonical naming

План v1.2 использует `status IN ('PENDING','CLAIMED','SUCCEEDED','FAILED','DEAD')`, `lease_owner`, `lease_expires_at`, `conversation_id`, `last_analyzed_message_id`. Каноническим является §53 настоящего документа.

Канон:

```text
status:      PENDING | LEASED | RUNNING | SUCCEEDED | RETRY | DEAD
владение:    leased_by, lease_until
готовность:  available_at
сущность:    entity_type, entity_id
свежесть:    base_conversation_revision, analysis_through_message_id
```

**Acceptance:**

В коде, миграциях, OpenAPI и Плане v1.2 не остаётся идентификаторов `CLAIMED`, `lease_owner`, `lease_expires_at`, `last_analyzed_message_id`. `grep` по репозиторию пуст.

---

### LR-BE-RM-011 — application_status canonical value

§3.4 фиксирует `PENDING | APPLIED | STALE | REJECTED`. План v1.2 использует `VALID` вместо `APPLIED`.

Канон — `APPLIED`.

**Acceptance:**

`ai_runs.application_status` CHECK содержит `APPLIED`; `VALID` не встречается в коде и контрактах.

---

### LR-BE-RM-012 — Risk severity baseline

План v1.2 задаёт severity, противоречащие §30, §31 и GLOSSARY:

| Risk | Канон (§30–33, GLOSSARY) | Ошибочно в Плане v1.2 |
|---|---|---|
| `CUSTOMER_SILENT_AFTER_PRICE` | 24 ч → MEDIUM, 48 ч → HIGH | 24 ч → HIGH, 48 ч → CRITICAL |
| `BOOKING_NOT_CONFIRMED` | CRITICAL | HIGH |

**Acceptance:**

Policy-код и фикстуры используют канон; Radar ordering (§34) пересчитан на исправленные severity.

---

### LR-BE-RM-013 — R3 eligible stages

Политика `BOOKING_NOT_CONFIRMED` в Плане v1.2 разрешает стадии `QUALIFYING / PRICE_SENT / WAITING_BUSINESS`. Но §31 задаёт условие `bookingIntent >= 0.85 OR stage = BOOKING_INTENT`: при обнаружении факта Opportunity закономерно переходит в стадию `BOOKING_INTENT` и выпадает из собственной политики.

Канонический список:

```text
QUALIFYING
PRICE_SENT
WAITING_CUSTOMER
WAITING_BUSINESS
BOOKING_INTENT
```

Исключающие: `NEW`, `ENGAGED`, `BOOKED`, `WON`, `LOST`, `ARCHIVED`.

**Acceptance:**

Тест: Opportunity в стадии `BOOKING_INTENT`, факт с confidence 0.92, прошло 35 бизнес-минут без подтверждения → Risk создан.

---

### LR-BE-RM-014 — Risk threshold unit

`risk_policy_configs.threshold_minutes` не указывает, бизнес-минуты это или календарные. Для R1/R3/R4 канон — бизнес-время; для R2/R5 §30 и §33 задают бизнес-часы, но при 9-часовом рабочем дне «24 бизнес-часа» — это около 2,7 календарных суток, что для риска «клиент молчит» требует явного подтверждения продуктового решения.

Задача — сделать единицу явной, поведение по умолчанию не менять:

```sql
ALTER TABLE risk_policy_configs
    RENAME COLUMN threshold_minutes TO threshold_value;

ALTER TABLE risk_policy_configs
    ADD COLUMN threshold_unit TEXT
        CHECK (threshold_unit IN ('BUSINESS_MINUTES','CALENDAR_MINUTES')),
    ADD COLUMN escalation_value INTEGER
        CHECK (escalation_value IS NULL OR escalation_value > threshold_value);

UPDATE risk_policy_configs SET threshold_unit = 'BUSINESS_MINUTES';

UPDATE risk_policy_configs
SET escalation_value = 2880
WHERE risk_type = 'CUSTOMER_SILENT_AFTER_PRICE';

ALTER TABLE risk_policy_configs
    ALTER COLUMN threshold_unit SET NOT NULL;
```

Открытый вопрос для владельца продукта, решается внутри этапа: перевести R2 и R5 на `CALENDAR_MINUTES` (1440 = ровно сутки) или оставить бизнес-время. Решение фиксируется ADR-033.

**Acceptance:**

Ни один расчёт порога не выводит единицу из типа риска; единица читается из строки конфига.

---

### LR-BE-RM-015 — notification_preferences full field set

План v1.2 задаёт таблицу с четырьмя полями и теряет поля, обязательные по §3.7: `minimum_severity`, `in_app_enabled`, `telegram_enabled`, `quiet_hours_enabled`, `created_at`. Потеря `minimum_severity` вырезает возможность, объявленную в GLOSSARY («Notification Policy — тип риска, минимальная severity, режим, quiet hours») и в §45.

Второе расхождение: §3.7 берёт timezone из Organization default, План v1.2 — из Location. Канон — **Organization default** (§3.7); Location-timezone используется только для расчёта business hours.

**Acceptance:**

Таблица содержит полный набор полей §3.7; настройка «уведомлять только о HIGH и выше» работает; quiet hours и digest считаются в timezone организации.

---

### БЛОК R3 — Корректность runtime

---

### LR-BE-RM-016 — Absolute lease cap

§3.5 намеренно разрешает heartbeat AI Agent продлевать lease своих RUNNING jobs. Отказ, который это не покрывает: inference завис, но heartbeat-горутина жива — задание удерживается бесконечно и в очередь не возвращается.

Добавить абсолютный потолок поверх скользящей аренды:

```sql
ALTER TABLE ai_jobs ADD COLUMN leased_at TIMESTAMPTZ;
```

```text
lease_until   = now() + 120s        -- продлевается heartbeat (§3.5)
max_lease_age = leased_at + 15m     -- не продлевается никогда
```

Reclaim-обработчик забирает задание при `lease_until < now()` **или** `leased_at + interval '15 minutes' < now()`. Во втором случае `attempts += 1` и `error_code = LEASE_CAP_EXCEEDED`.

Потолок выбран как 60× p95 latency (15 с) — заведомо выше любого честного inference на 8 GB VRAM.

**Acceptance:**

AI Agent с искусственно зависшим Provider и живым heartbeat теряет задание через 15 минут; задание успешно выполняется другим узлом; повторный `complete` от зависшего узла отбрасывается идемпотентно.

---

### LR-BE-RM-017 — Confidence tiers codified

§59 задаёт три уровня (`>=0.85` strong, `0.65–0.849` weak, `<0.65` untrusted), но План v1.2 упоминает только 0.65 и 0.85 в разных местах и не связывает их. Факты в диапазоне 0.65–0.849 сейчас хранятся как обычные и молча не используются.

Зафиксировать:

| Уровень | Диапазон | Поведение |
|---|---|---|
| strong | `>= confidence_min` (0.85) | открывает Risk |
| weak | `0.65 … confidence_min` | сохраняется, помечен `trusted = false`, доступен в admin summary и метриках, Risk не открывает |
| untrusted | `< 0.65` | не применяется к domain, только метрика |

Каждый факт в `derived_facts` получает вычисляемое поле `trusted`. Downstream-модули читают только `trusted = true` и сверяются с `confidence_min` политики.

**Acceptance:**

Факт с confidence 0.70 виден в `/admin/ai/conversations/{id}/summary`, не создаёт Risk, попадает в метрику `ai_facts_weak_total`.

---

### LR-BE-RM-018 — RLS roles and fail-closed (ADR-032)

§12 требует RLS до первого pilot, но policy строится на `current_setting('lidradar.tenant_id')`, а три сценария работают вне tenant-контекста: claim заданий (охватывает все tenant'ы), outbox dispatcher, admin read-models. При включении RLS на этапе 24 сломаются этапы 6, 13 и 23 — в документах это не оговорено.

Ввести три роли:

| Роль | Кто использует | RLS | Контекст |
|---|---|---|---|
| `lidradar_app` | `cmd/api` | FORCE | `SET LOCAL lidradar.tenant_id` из `X-Tenant-ID` |
| `lidradar_worker` | `cmd/worker`, `cmd/scheduler` | FORCE | `SET LOCAL` из `jobs.tenant_id` после claim |
| `lidradar_platform` | claim/dispatch/admin | `BYPASSRLS` | не устанавливается |

```sql
CREATE POLICY tenant_isolation ON conversations
    USING (tenant_id = current_setting('lidradar.tenant_id', true)::uuid);
ALTER TABLE conversations FORCE ROW LEVEL SECURITY;
```

Аргумент `true` возвращает NULL вместо ошибки при незаданной переменной — сравнение даёт «строк нет», то есть fail-closed.

Круг запросов под `lidradar_platform` держать явным списком: `jobs.claim`, `ai_jobs.claim`, `reclaim-expired-*`, `outbox.dispatch`, `admin.*`.

AI Node в этот список не входит: узел не имеет доступа к PostgreSQL вообще, только к HTTP API (§48). Записать это явно.

**Acceptance:**

`SELECT` без `SET LOCAL` под ролью `lidradar_app` возвращает 0 строк. Worker после claim видит только свой tenant. Этапы 6, 13, 23 проходят регресс с включённым RLS.

---

### LR-BE-RM-019 — Precision metric per risk

`risk_feedback` append-only, и §3 этапа 21 разрешает несколько записей на один Risk. Метрика precision, считающая строки, даёт лишний вес рискам с несколькими правками — а именно на неё завязан критерий перехода Milestone E.

Считать последний вердикт на Risk:

```sql
WITH latest AS (
    SELECT DISTINCT ON (risk_id) risk_id, verdict
    FROM risk_feedback
    WHERE tenant_id = $1 AND created_at >= $2 AND created_at < $3
    ORDER BY risk_id, created_at DESC, id DESC
)
SELECT r.type,
       count(*)                                             AS total_with_feedback,
       count(*) FILTER (WHERE l.verdict = 'TRUE_POSITIVE')  AS tp,
       count(*) FILTER (WHERE l.verdict = 'FALSE_POSITIVE') AS fp
FROM latest l
JOIN risk_signals r ON r.tenant_id = $1 AND r.id = l.risk_id
GROUP BY r.type;
```

Добавить `coverage_rate = total_with_feedback / total_risks`. Критерий Milestone E засчитывается только при `coverage_rate >= 0.5` — иначе precision смещена выборкой.

**Acceptance:**

Risk с тремя feedback учитывается один раз; метрика возвращает `coverage_rate`.

---

### LR-BE-RM-020 — Quiet hours midnight wrap

Значение по умолчанию 22:00–08:00 переходит через полночь, но семантика нигде не задана, а наивное `BETWEEN` не даст ни одного попадания. Совпадение границ схемой допускается и неоднозначно.

```sql
ALTER TABLE notification_preferences
    ADD CONSTRAINT notification_preferences_quiet_not_degenerate CHECK (
        quiet_hours_start IS NULL OR quiet_hours_start <> quiet_hours_end
    );
```

Семантика: при `start > end` интервал трактуется как `[start, 24:00) ∪ [00:00, end)` в timezone организации. Уведомление, попавшее в тихие часы, доставляется в `end` одним сообщением с актуальным на момент отправки состоянием риска.

**Acceptance:**

Risk, открытый в 23:00 при quiet 22:00–08:00, доставляется в 08:00 одним сообщением. Risk, открытый в 12:00, доставляется немедленно.

---

### LR-BE-RM-021 — Deterministic clock in tests

План v1.2 предлагает управлять временем через `SET LOCAL clock.time` — такой конструкции в PostgreSQL нет: `SET LOCAL` работает только с параметрами конфигурации, `now()` им не подменяется.

Правило: бизнес-время управляется на стороне Go.

* все application-сервисы принимают `clock.Clock`; в тестах — `clock.NewFake(t0)`;
* семантически значимые метки (`confirmed_at`, `attributed_at`, `detected_at`, `due_at`, `lease_until`, `available_at`) передаются параметрами запроса, а не берутся из `now()`;
* `created_at DEFAULT now()` остаётся — это техническая метка вставки;
* истечение аренды в тесте проверяется прямым сдвигом `lease_until` в фикстуре, без ожидания.

**Acceptance:**

Money-loop integration test (LR-BE-1209) и все risk-тесты проходят без реальных задержек; в репозитории нет упоминаний `clock.time`.

---

### БЛОК R4 — Документы и данные

---

### LR-BE-RM-022 — Migration map canonicalization

Часть I Плана v1.2 обещает `000012_ai_node.sql` и `000013_ai_analysis.sql`, Часть II занимает те же номера под revenue. Health-check LR-BE-2605 ожидает конкретный `latest`.

Свести к одной карте, зафиксировать в шапке Плана, обновить ожидаемое значение `applied` в health-check с учётом миграции этого этапа.

**Acceptance:**

`/health/ready` возвращает `applied` и `latest`, совпадающие с картой; расхождение ломает CI.

---

### LR-BE-RM-023 — AI Node hardware baseline

Шапка настоящего документа и GLOSSARY фиксируют RTX 4060 **8 GB**. План v1.2 в двух местах указывает 12 GB (`GPULayers: 33 for RTX 4060 12GB`, `"vram_gb": 12`). Видеокарты GeForce RTX 4060 с 12 GB не существует.

Канон — 8 GB. Проверка помещаемости: `llama-3.1-8b-instruct-q4_k_m` ≈ 4,9 GB весов + KV-кэш ≈ 0,5 GB при контексте 4096 → все 33 слоя выгружаются на GPU, `GPULayers: 33` остаётся верным. Запас ~2 GB, поэтому контекст 4096 (§56) — потолок, `parallel 1` / `max_inflight 1` (§61) не повышаются.

**Acceptance:**

Манифест модели содержит `"vram_gb": 8`; бенчмарк фиксирует фактический пик VRAM и отсутствие OOM (§62).

---

### LR-BE-RM-024 — Golden dataset split

LR-BE-1503 и План v1.2 задают «60% TRAIN, 20% VALIDATION, 20% GOLDEN» от 500 кейсов — это 100 golden, тогда как гейт требует прогона на 300. Сплит TRAIN бессмыслен: дообучения в проекте нет (§70, LR-BE-2107), варьируется только промпт.

Канон: **400 GOLDEN + 100 DEV**. `split` принимает значения `GOLDEN | DEV`. SHA-256 считается только по GOLDEN. Промпт итерируется на DEV; прогон по GOLDEN — только перед сменой статуса манифеста.

**Acceptance:**

`golden_v1.jsonl` содержит 400 GOLDEN-кейсов; runner падает с `GOLDEN_DIGEST_MISMATCH` при подмене; DEV-файл не влияет на freeze.

---

### LR-BE-RM-025 — Shadow collection scheduling

Этап 15 требует 500 реальных диалогов «из pilot-детейлинга», но пилот стартует на этапе 26 — то есть Milestone D заблокирован данными, появляющимися внутри Milestone E.

Ввести параллельный этап **A2 — INT-SHADOW-001**, стартующий сразу после Exit Gate этапа A:

* дружественная студия подключается через Connected Business Bot;
* активны только этапы 4–5 (RawEvent → нормализация); `ai_enabled = FALSE`;
* риски не создаются, уведомления не отправляются, владелец продуктом не пользуется;
* обезличивание выполняется при экспорте для разметки;
* обязательны письменное согласие владельца и ДПА (§70).

Выход: 500 обезличенных диалогов → вход LR-BE-1502. Ожидаемая длительность при 200 сообщениях в день — 2–3 недели.

**Acceptance:**

Этап A2 заведён в план как параллельный трек с собственным Exit Gate; зависимость этапа 15 переписана с «pilot-детейлинг» на «этап A2».

---

### LR-BE-RM-026 — Documentation sync

Свести документы к одному состоянию:

* GLOSSARY: переписать статью `Revenue Event` (потенциальная выручка Revenue Event'ом не является — это `estimatedAmount` Opportunity); добавить `Attribution window`, `Trust threshold / untrusted fact`, `Lease renewal`, `Lease cap`, `Shadow-сбор`, `Debounce`, `Threshold unit`;
* README: ссылки «План разработки MVP v1.0» → v1.2; «План внедрения MVP v1.0» → «Пилотный план (планируется)» со строкой в таблице статусов;
* счётчик ADR: 31 → 34 (добавляются ADR-032 RLS-роли, ADR-033 единица порога риска, ADR-034 дебаунс AI-очереди); изменение архитектуры v1.1 проводится через процедуру Architecture Freeze единым пакетом;
* §78 настоящего документа и «Порядок PR» Плана v1.2 привести к одному списку (25 против 28 PR).

**Acceptance:**

Ни один термин, встречающийся в документах, не отсутствует в GLOSSARY; перекрёстные ссылки открываются; версии и даты в шапках подняты.

---

## Exit Gate

```text
000018_consistency_remediation.sql применена на staging
→ регресс этапов 0–16 зелёный
→ Money Loop e2e зелёный на исправленной attribution-схеме
→ grep по репозиторию не находит CLAIMED / lease_owner / VALID / clock.time
→ карта миграций и health-check совпадают
→ GLOSSARY, README, План v1.2 не противоречат Tasks.md
```

Проверка, закрывающая этап: попытка задвоить Confirmed Recovered Revenue по одной Opportunity отклоняется базой, а не application-слоем.

Этап 17 начинается только после этого.

---

# ЭТАП 17 — SEMANTIC RISK: PROMISE_NOT_FULFILLED

## Цель

Обнаруживать обещание бизнеса и просроченный follow-up.

## Задачи

### LR-BE-1701 — businessCommitment semantic fact

### LR-BE-1702 — Due-time extraction

### LR-BE-1703 — 60 business minute fallback

### LR-BE-1704 — Scheduler integration

### LR-BE-1705 — Relevant follow-up detection

### LR-BE-1706 — Dedup/auto-resolution

### LR-BE-1707 — Tests

## Exit Gate

Risk создаётся только при реальном просроченном commitment.

---

# ЭТАП 18 — SEMANTIC RISK: CUSTOMER_SILENT_AFTER_PRICE

## Цель

Находить исчезновение клиента после отправки цены.

## Задачи

### LR-BE-1801 — PRICE_MENTIONED mapping

### LR-BE-1802 — PRICE_SENT stage integration

### LR-BE-1803 — 24h/48h business-time policy

### LR-BE-1804 — Terminal-stage exclusions

### LR-BE-1805 — CUSTOMER_REJECTED exclusion

### LR-BE-1806 — Auto-resolution on incoming

### LR-BE-1807 — Revenue confidence guard

## Exit Gate

Неопределённая сумма никогда не выдумывается.

---

# ЭТАП 19 — SEMANTIC RISK: FOLLOW_UP_CANDIDATE

## Цель

Находить soft opportunities без генерации spam.

## Задачи

### LR-BE-1901 — Hesitation semantics

### LR-BE-1902 — Explicit rejection exclusion

### LR-BE-1903 — WAITING_CUSTOMER policy

### LR-BE-1904 — 24 business hour delay

### LR-BE-1905 — MEDIUM severity

### LR-BE-1906 — Dedup

### LR-BE-1907 — Quality tests

## Exit Gate

Hesitation создаёт candidate.

Явный отказ — нет.

---

# ЭТАП 20 — NOTIFICATION POLICY

## Цель

Управлять шумностью системы.

## Задачи

### LR-BE-2001 — NotificationPreference schema

### LR-BE-2002 — Defaults

### LR-BE-2003 — IMMEDIATE mode

### LR-BE-2004 — DIGEST mode

### LR-BE-2005 — DISABLED mode

### LR-BE-2006 — Quiet hours

### LR-BE-2007 — Digest scheduler

### LR-BE-2008 — Digest dedup

### LR-BE-2009 — Settings API

### LR-BE-2010 — Escalation feature flag foundation

Actual OWNER escalation можно оставить disabled.

## Exit Gate

Владелец способен управлять alerts по Risk type.

---

# ЭТАП 21 — FEEDBACK / QUALITY CONTROL

## Цель

Получить реальные данные о precision.

## Задачи

### LR-BE-2101 — Risk feedback

Verdict:

```text
TRUE_POSITIVE
FALSE_POSITIVE
```

### LR-BE-2102 — Feedback reasons

```text
CUSTOMER_ALREADY_BOOKED
CUSTOMER_ALREADY_ANSWERED
NOT_A_LEAD
CUSTOMER_REJECTED
WRONG_INTERPRETATION
OTHER
```

### LR-BE-2103 — Lead/not-lead correction

### LR-BE-2104 — Immutable historical context

### LR-BE-2105 — Permission checks

### LR-BE-2106 — Precision metrics

### LR-BE-2107 — Explicit ML consent boundary

## Exit Gate

Можно рассчитать Risk Precision и false-positive rate по каждому Risk type.

---

# ЭТАП 22 — BASIC ANALYTICS

## Цель

Показать доказанную business value.

## Задачи

### LR-BE-2201 — Analytics queries

### LR-BE-2202 — Period/timezone boundaries

### LR-BE-2203 — Messages metric

### LR-BE-2204 — Opportunities metric

### LR-BE-2205 — Risk metrics

```text
detected
acted
resolved
false positive
```

### LR-BE-2206 — Outcome metrics

```text
booked
paid
lost
```

### LR-BE-2207 — Potential revenue

### LR-BE-2208 — Confirmed recovered revenue

### LR-BE-2209 — Analytics API

## Exit Gate

Business видит реальные деньги, а не AI benchmark.

---

# ЭТАП 23 — ADMIN / OBSERVABILITY

## Цель

Диагностировать production pipeline без SQL/SSH.

## Задачи

### LR-BE-2301 — Admin auth

PLATFORM_ADMIN.

### LR-BE-2302 — Organizations read model

### LR-BE-2303 — Connection health read model

### LR-BE-2304 — Jobs panel API

### LR-BE-2305 — Dead jobs API

### LR-BE-2306 — AI Nodes API

### LR-BE-2307 — AI Runs API

### LR-BE-2308 — Usage API

### LR-BE-2309 — Replay/retry admin commands

Обязателен audit.

### LR-BE-2310 — Traceability

Должна восстанавливаться цепочка:

```text
Message
→ Job
→ AIRun
→ Semantic Result
→ Risk
→ Notification
→ Action
→ Outcome
→ Revenue
```

## Exit Gate

Искусственно сломанный pipeline диагностируется без прямого подключения к production DB.

---

# ЭТАП 24 — SECURITY / RELIABILITY HARDENING

## Цель

Подготовить backend к реальным business data.

## Задачи

### LR-BE-2401 — RLS

### LR-BE-2402 — Composite tenant FK

### LR-BE-2403 — Secrets encryption

### LR-BE-2404 — Rate limiting

### LR-BE-2405 — Security headers/cookie audit

### LR-BE-2406 — Critical audit coverage

### LR-BE-2407 — Backup automation

### LR-BE-2408 — Restore test

### LR-BE-2409 — Forward-compatible migration tests

### LR-BE-2410 — Worker crash test

### LR-BE-2411 — Connector failure test

### LR-BE-2412 — AI disconnect test

### LR-BE-2413 — Invalid AI test

### LR-BE-2414 — Stale AI test

### LR-BE-2415 — Notification retry test

### LR-BE-2416 — Revenue retry test

### LR-BE-2417 — Cross-tenant UUID attack tests

## Exit Gate — Gate E security portion

Все failure scenarios воспроизводимо проходят.

Backup реально восстанавливается.

---

# ЭТАП 25 — LOAD / CAPACITY TEST

## Цель

Измерить baseline до первых ~100 organizations.

## Задачи

### LR-BE-2501 — Load dataset generator

### LR-BE-2502 — API load

### LR-BE-2503 — Webhook burst

### LR-BE-2504 — Worker throughput

### LR-BE-2505 — Scheduler lag

### LR-BE-2506 — PostgreSQL query profiling

### LR-BE-2507 — AI queue metrics

### LR-BE-2508 — Capacity report

Зафиксировать:

```text
API p50/p95/p99
DB p95
webhook p95
worker throughput
scheduler lag
AI queue p95
AI inference p95
GPU utilization
```

## Exit Gate

Нет архитектурного bottleneck на целевой стартовой нагрузке.

Scale triggers документированы.

---

# ЭТАП 26 — STAGING / PRODUCTION / PILOT

## Цель

Перевести backend из development в контролируемый pilot.

## Задачи

### LR-BE-2601 — Immutable images

```text
lidradar-api:<git-sha>
lidradar-worker:<git-sha>
lidradar-scheduler:<git-sha>
lidradar-ai-agent:<git-sha>
```

### LR-BE-2602 — Staging

Никаких production customer messages в staging.

### LR-BE-2603 — Production migration pipeline

```text
build
→ test
→ image
→ staging
→ migration
→ smoke
→ production
```

### LR-BE-2604 — Feature flags

Минимально:

```text
ai_enabled
ai_model_v2
risk_engine_v2
connector_x
ai_cloud_fallback
follow_up_generation
```

### LR-BE-2605 — Production health checks

### LR-BE-2606 — Connector smoke

### LR-BE-2607 — Notification smoke

### LR-BE-2608 — Money Loop smoke

### LR-BE-2609 — Backup/restore confirmation

### LR-BE-2610 — Pilot tenant rollout

Последовательно:

```text
internal organization
→
1 дружественный детейлинг
→
3–5
→
10–20 organizations
```

## Exit Gate — Pilot Ready

Система готова к реальному ограниченному pilot.

---

# 78. Рекомендуемый merge / Pull Request order

```text
#1  bootstrap-core-foundation

#2  identity-tenant

#3  service-catalog

#4  connector-core

#5  telegram-connected-bot

#6  conversation-domain

#7  background-jobs

#8  opportunity-domain

#9  risk-no-response

#10 radar-api-realtime

#11 telegram-notifications

#12 recommendation-actions

#13 outcome-revenue

#14 revenue-attribution

#15 ai-node-agent

#16 ai-conversation-analysis

#17 risk-booking-not-confirmed

#17.5 consistency-remediation   ← ЭТАП R, вне очереди

#18 risk-promise-not-fulfilled

#19 risk-silent-after-price

#20 risk-follow-up

#21 notification-policy

#22 feedback

#23 analytics

#24 admin-observability

#25 production-hardening
```

Load tests и pilot deployment выполняются после #25.

---

# 79. Контрольные milestones

## Milestone A — Telegram Proof

```text
Real Telegram
→ RawEvent
→ Message
→ manual outgoing
→ test notification
```

Доказывает:

**Telegram действительно подходит как production source.**

---

## Milestone B — First Value

```text
Real incoming
→ NO_RESPONSE
→ Radar
→ Telegram Alert
```

Доказывает:

**LidRadar способен вовремя обнаружить реальную потерю лида.**

---

## Milestone C — Business MVP

```text
Risk
→ Action
→ Outcome
→ Confirmed Payment
→ Revenue Attribution
→ Confirmed Recovered Revenue
```

Доказывает:

**LidRadar способен измерить собственную business value без сложного AI.**

---

## Milestone D — AI Value

```text
Conversation
→ Local AI
→ semantic facts
→ deterministic Risk Engine
→ semantic Risk
```

Доказывает:

**AI расширяет покрытие ситуаций, которые невозможно достаточно надёжно определить простыми rules.**

---

## Milestone E — Pilot Ready

```text
real connector
+
4+ risk types
+
notifications
+
money loop
+
AI freshness
+
tenant isolation
+
RLS
+
backup/restore
+
admin
+
observability
+
failure tests
```

Доказывает:

**система готова к ограниченному production pilot.**

---

# 80. Backend Pilot Ready Checklist

До запуска первого внешнего pilot все пункты обязательны.

* Real Connected Business Bot connector проверен на non-Premium account.
* Incoming messages сохраняются persist-first.
* Manual outgoing messages корректно определяются.
* Duplicate Telegram update не создаёт duplicate RawEvent/Message.
* Disconnect/reconnect обрабатывается.
* Connection health виден.
* Contact/Conversation/Message canonical flow работает.
* One active Opportunity invariant включён.
* NO_RESPONSE полностью проходит E2E.
* Radar API работает.
* Telegram alert работает.
* Notification dedup работает.
* Actions append-only.
* Outcomes append-only.
* Revenue confirmation idempotent.
* Recovered Revenue считается только через Attribution.
* PostgreSQL jobs переживают worker crash.
* Transactional Outbox работает.
* AI Node полностью отключаемый.
* AI Node не хранит customer data постоянно.
* AI output проходит schema/domain validation.
* Stale AI result не изменяет domain.
* AI model/prompt/schema versioned.
* Fake AI Provider существует.
* Минимум четыре Risk scenarios проверены на пилотных fixtures.
* Risk feedback сохраняется.
* Notification Policy работает.
* Quiet hours работают.
* Analytics совпадает с raw domain data.
* Tenant A не может получить Tenant B даже зная UUID.
* Critical tenant tables защищены RLS.
* Critical relations имеют tenant-aware FK.
* Secrets encrypted.
* Password/session tokens не логируются.
* Message body и raw prompt не логируются.
* Audit critical actions работает.
* Dead jobs видны admin.
* Message → Job → AIRun → Risk → Notification → Action → Outcome → Revenue trace восстанавливается.
* Backup автоматизирован.
* Restore практически проверен.
* RPO/RTO target подтверждён.
* API p95 target подтверждён.
* Webhook persist p95 target подтверждён.
* AI queue lag измеряется.
* GPU utilization/VRAM измеряются.
* Feature flags позволяют отключить AI/risk/provider без rollback DB.
* Production images immutable.
* Production smoke test автоматизирован.

---

# 81. Финальное правило разработки

Backend-команда не должна пытаться сделать LidRadar «умным» раньше, чем он станет **надёжным**.

Приоритет реализации:

```text
Persist
→ Normalize
→ Domain
→ Deterministic Rules
→ Risk
→ Alert
→ Action
→ Outcome
→ Revenue
→ Attribution
→ AI
→ Semantic Risks
→ Optimization
```

Не:

```text
LLM
→ prompt engineering
→ красивый demo
→ потом разбираться с data consistency
```

Первый production-grade вертикальный slice должен быть:

```text
REAL TELEGRAM MESSAGE
→
RAW EVENT
→
CANONICAL CONVERSATION
→
OPPORTUNITY
→
NO_RESPONSE
→
RADAR
→
TELEGRAM ALERT
→
ACTION
→
OUTCOME
→
CONFIRMED PAYMENT
→
RECOVERED REVENUE
```

Только после работоспособности этой цепочки AI становится частью critical development path.

Это является основным техническим принципом MVP LidRadar.
