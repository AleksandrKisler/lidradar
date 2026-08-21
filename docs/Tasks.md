# LidRadar — Backend Technical Specification & Sequential Delivery Plan v1.0

**Статус:** Ready for Backend Development
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

Единственное намеренное исключение:

```text
INT-TELEGRAM-001
```

Он выполняется параллельно Foundation/Identity, потому что Telegram является наиболее рискованной внешней зависимостью.

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
