# Backend specification

## Purpose

The LidRadar backend is implemented in Go as a modular monolith. This document
is the starting point for backend requirements and contracts. Feature-specific
behavior should be added here, or linked from here, before it is implemented.

## Baseline contract

- PostgreSQL is the source of truth for persistent application state.
- Business capabilities must respect the module and dependency rules in
  [`../architecture/ARCHITECTURE.md`](../architecture/ARCHITECTURE.md).
- Inputs are validated at the system boundary; domain invariants are enforced
  inside the owning module.
- Failures must be represented deliberately and must not expose credentials,
  internal implementation details, or sensitive data.
- State changes that must succeed or fail together use an explicit PostgreSQL
  transaction with a clearly identified owner.
- External or asynchronous side effects must account for retries and
  idempotency; they must not override PostgreSQL as the authoritative state.

## Feature specifications

### Identity and tenant setup

Identity owns platform users and opaque server-side sessions. Registration
normalizes email with trim plus lowercase, requires a 12-1024 byte password,
hashes it with Argon2id, and creates the User and initial Session in one
PostgreSQL transaction. Session tokens contain 256 random bits; only their
SHA-256 digests are persisted. Login failures use one public error regardless
of whether the email or password was wrong. Refresh atomically revokes the old
session and issues a replacement, while logout is idempotent. Session cookies
are HttpOnly and SameSite=Strict. Secure is mandatory in staging and production
and may be disabled only for local development and tests over HTTP.

Organization is the tenant boundary. Creating an Organization and its active
OWNER Membership is one PostgreSQL transaction. Clients obtain active
membership summaries from `GET /api/v1/auth/me` and explicitly select a tenant
with `X-Tenant-ID`; repositories never infer a tenant from an entity ID.
OWNER receives the approved tenant permission set. MANAGER receives only risk,
conversation, opportunity, action, outcome, and revenue-confirm permissions and
cannot change Organization, Location, member, integration, notification, or
analytics settings.

Locations are always queried with tenant and Location identifiers, carry an
IANA timezone and a 1-1440 minute response threshold, and default to 45
minutes. Replacing business hours requires exactly one validated entry for each
weekday 1-7 and atomically replaces the schedule together with the Location
timezone. Cross-tenant Location identifiers are returned as missing when used
inside another tenant; selecting a tenant without an active Membership is
forbidden. Browser mutation requests with an Origin header are accepted only
from the API origin or an explicitly configured trusted origin.

### Service Catalog

The Catalog module owns tenant-scoped `ServiceCatalogItem` records. Every
repository lookup and mutation requires both the selected tenant and, for a
single item, its identifier. An optional Location must belong to that same
tenant; a foreign Location or item is indistinguishable from a missing
resource. Catalog management requires the centralized `service.manage`
permission, which is granted to OWNER and not to MANAGER.

Names are stored in cleaned display form and with a deterministic lowercase,
whitespace-collapsed `normalized_name` for later matching. Price boundaries are
optional exact decimals: PostgreSQL stores `NUMERIC(14,2)`, Go uses a decimal
value, and REST sends strings with exactly two fractional digits. JSON numbers,
negative prices, more than two fractional digits, and a lower boundary above
the upper boundary are rejected. Currency defaults to `RUB` and is normalized
to three uppercase ASCII letters.

`DELETE /api/v1/services/{serviceId}` is an idempotent soft deactivation so
historical references remain valid; `PATCH` may explicitly reactivate an item.
The list includes active and inactive items for OWNER settings management.
Nullable price fields may be cleared with JSON `null`. When no trustworthy
price is configured, both stored boundaries remain SQL `NULL`; later
Opportunity logic must therefore leave potential revenue `NULL` rather than
inventing or defaulting an amount.

### Connector Core

Connector Core owns tenant-scoped `ChannelConnection`, its persisted health,
immutable provider `RawEvent` receipts, and the durable handoff to later
normalization. Supported connection states are `ACTIVE`, `DEGRADED`, `ERROR`,
and `DISCONNECTED`. Every repository operation carries both tenant and entity
identifiers; an optional Location must belong to the same tenant. Disconnect is
an idempotent soft operation so receipt history remains available.

Every provider adapter implements `Provider`, `VerifyEvent`, `NormalizeEvent`,
and `Health`; an event-identifier capability extracts the provider dedup key
before normalization. The webhook path verifies the stored SHA-256 secret
digest first. An authentication failure returns `401` and persists nothing.
For an authenticated request, one short PostgreSQL transaction locks the
connection, inserts the `RawEvent`, inserts exactly one pending normalization
work record for a valid new event, and updates connection health. The unique
key is `(connection_id, external_event_id)`. A duplicate with the same payload
returns the original receipt; reuse of that external identifier with different
bytes is a conflict.

The HTTP handler returns `202` after this transaction and never calls
normalization, downstream AI, or another external service. An authenticated
malformed payload is retained once as `FAILED` with `INVALID_PAYLOAD` and
creates no normalization work. Non-JSON bytes are represented losslessly by a
base64 JSON wrapper because PostgreSQL owns the raw payload as `JSONB`. The
stage-four `raw_event_normalization_work` table is only the durable handoff;
the generic leased job runtime remains owned by stage six.

OWNER manages list/connect/disconnect/health under `/api/v1/integrations`.
TEST, IMPORT, and GENERIC_WEBHOOK are deterministic local adapters sharing the
versioned fixture envelope. The Connected Business Bot adapter registers a
tenant- and connection-specific HTTPS webhook through `setWebhook`, restricts
updates to the four Business event families, and verifies the resulting address
through `getWebhookInfo`. Disconnect first records authoritative local state and
then calls `deleteWebhook`, so a failed remote cleanup can be retried safely.

The bot token is accepted only for `CONNECTED_BUSINESS_BOT`, encrypted with
AES-256-GCM using a deployment key, bound to tenant/provider/connection
identifiers, and never returned through JSON. The webhook secret is stored only
as a SHA-256 verifier. Missing public HTTPS or encryption configuration makes
Telegram connection unavailable without disabling the other adapters. Remote
setup failure leaves a durable connection in `ERROR` with the safe code
`TELEGRAM_WEBHOOK_SETUP_FAILED`; provider text and credentials are not exposed.
No Bot API call occurs on the event receipt path. Automated adapter tests do not
claim the real-Telegram exit gate: the non-Premium account spike and its evidence
report remain mandatory.

### Ядро переписок

Модуль `conversation` владеет контактами, внешними личностями, переписками,
сообщениями и метаданными вложений. Он принимает только канонические события
`message.received.v1`, `message.edited.v1` и `message.deleted.v1` и ничего не
знает о формате Telegram, импорта либо HTTP-уведомления. Преобразование формата
конкретного поставщика остаётся в адаптере модуля `connector`.

Рабочий процесс читает сохранённые `RawEvent` только после фиксации HTTP-
транзакции. Успешно преобразованное событие передаётся модулю переписок, после
чего исходное событие отмечается как обработанное. Некорректные канонические
данные получают состояние `FAILED` и код `NORMALIZATION_INVALID_PAYLOAD`.
Временная ошибка базы не удаляет ожидающую работу: безопасный повтор опирается
на уникальные внешние идентификаторы и идемпотентные операции. Общая очередь с
арендой, параллельным захватом и политикой повторов относится к этапу 6.

Первая встреча внешней личности атомарно создаёт `Contact`,
`ExternalIdentity`, `Conversation` и `Message`. Пространство внешнего
идентификатора включает организацию, поставщика, подключение и значение
поставщика; одинаковые значения из разных подключений не склеиваются. Контакт
обновляется только сведениями не старше сохранённых. Автоматического объединения
по телефону или почте нет, поскольку ТЗ не задаёт безопасного правила такого
объединения.

Переписка уникальна внутри подключения по внешнему идентификатору. Сообщение
уникально внутри подключения по собственному внешнему идентификатору. Точный
повтор события возвращает уже сохранённое состояние, а повтор с противоречащими
каноническими данными считается конфликтом. Ссылка на сообщение-ответ должна
указывать на сообщение той же переписки.

Изменение обновляет каноническое содержимое существующего сообщения и полностью
заменяет набор метаданных вложений. Удаление поставщиком не стирает сообщение, а
записывает `provider_deleted_at`. Каждое фактическое создание, изменение или
удаление сообщения атомарно увеличивает `conversations.revision`; точный повтор
не увеличивает её. Первая и последняя отметки времени, а также направление
последнего сообщения пересчитываются в той же транзакции.

Таблица `attachments` хранит только ключ объекта, MIME-тип, размер, SHA-256 и
внешний идентификатор файла. Двоичное содержимое запрещено сохранять в
PostgreSQL и должно находиться в S3-совместимом объектном хранилище. Пока внешние
сервисы отключены, локальные примеры и макет Telegram используют явно
обозначенные ключи-заглушки; перенос самих файлов ещё не считается выполненным.

Чтение доступно OWNER и MANAGER через единое разрешение `conversation.read`:

```text
GET /api/v1/conversations
GET /api/v1/conversations/{conversationId}
GET /api/v1/conversations/{conversationId}/messages
```

Каждый запрос требует сеанс и явный `X-Tenant-ID`. Чужой идентификатор выглядит
как отсутствующий. Списки упорядочены стабильно, используют непрозрачный курсор,
размер страницы 50 по умолчанию и не более 100. Поставщик канала не выводится в
представлении переписки: сведения о канале доступны через отдельный модуль
подключений.

### NO_RESPONSE risk

The Risk module owns the `Risk` aggregate and active-risk deduplication. A
`NO_RESPONSE` check uses a versioned deterministic policy; it does not invoke
AI. The policy creates or refreshes a risk only when all of these conditions
hold at execution time:

- the last meaningful canonical message is incoming;
- no outgoing message exists after that triggering message;
- the related Opportunity is active; and
- at least the Location response threshold has elapsed in the Location's IANA
  timezone and weekly business-hours schedule.

The first 45–89 elapsed business minutes have `HIGH` severity and 90 or more
have `CRITICAL` severity. Closed periods do not contribute to elapsed time. A
threshold crossing outside the current working period is carried into the next
working period.

Scheduled work contains tenant and Opportunity identifiers plus its due time,
not authoritative conversation state. The worker must reload current canonical
state before evaluation. A reply or an inactive Opportunity prevents creation
and resolves any active `NO_RESPONSE` risk. Replayed or concurrent checks
atomically create at most one active risk per tenant, Opportunity, and risk
type; later positive evaluations refresh its evidence instead.

All repository operations require both tenant and Opportunity identifiers.
PostgreSQL is the production source of truth and must enforce active-risk
uniqueness. Cancellation and persistence errors are returned to the worker for
its normal retry classification; invalid or cross-tenant state is rejected and
must not mutate a risk.

Feature-level backend contracts added later must likewise define observable
behavior, data ownership, error behavior, and operational requirements before
production code is added.

### Radar and Risk realtime API

Radar reads and commands are tenant-scoped and authorized through the named
`risks.read` and `risks.manage` membership permissions. The list is ordered by
active state, severity (`CRITICAL` before `HIGH`), oldest detection time, and a
stable ID tie-breaker, and uses cursor pagination. Risk detail includes related
Opportunity, Conversation, Recommendation, Action, Outcome, and Revenue data
only when those owning modules have produced it. Exact money totals are exposed
as decimal strings.

Acknowledge and resolve commands are idempotent. Cross-tenant identifiers are
indistinguishable from missing resources and cannot mutate state. SSE carries
only tenant-scoped invalidation signals after durable changes; clients always
refetch the authoritative REST read model, and losing an SSE signal never loses
business data. The versioned HTTP contract is maintained in
[`../../contracts/openapi/openapi.yaml`](../../contracts/openapi/openapi.yaml).

### Telegram risk notifications

The Notification module owns the logical Notification, transport delivery
attempts, one-time Telegram link tokens, and installed Telegram user links. A
risk-opened alert uses the deterministic key `risk:{risk_id}:opened`; replaying
the event or retrying Telegram cannot create another user-visible notification.
Notification intent and its initial delivery are persisted atomically before
the external request. Each retry is retained as a separate delivery attempt,
using the standard retry schedule, and Telegram failure never changes Risk
state.

Link token plaintext is never persisted: only its SHA-256 digest, expiry, and
single-use timestamp are stored. Telegram callbacks require a tenant-scoped
user link and idempotency key and are restricted to `OPEN_RISK`, `ACKNOWLEDGE`,
and `SNOOZE`; financial mutations are not accepted through this boundary.

### Recommendations, actions, and outcomes

Every supported Risk type has a deterministic template recommendation, so a
useful corrective instruction does not depend on AI availability. Actions are
tenant-scoped append-only facts attached to a Risk. Outcomes are tenant-scoped
append-only facts attached to an Opportunity; a correction creates another
Outcome rather than rewriting history.

Action and Outcome commands require the `risks.manage` permission and an
`Idempotency-Key`. The key is scoped by tenant and operation: replaying the
same request returns the stored response, while reusing it for different
content returns a conflict. Each successful new Action or Outcome is recorded
in the audit trail atomically with the fact. Cross-tenant identifiers are
indistinguishable from missing resources and cannot produce records.

Architecture changes require an ADR; see [`../adr/README.md`](../adr/README.md).

### Home AI node infrastructure

AI inference uses the outbound pull model from ADR 0030. Registered nodes are
authenticated by a rotatable secret whose SHA-256 digest is the only persisted
form. A ready heartbeat renews only leases currently owned by that node. Jobs
carry the tenant, conversation, base conversation revision, and last analyzed
message identifier.

The default lease is 120 seconds. Claim is atomic, expired work may be reclaimed
after a node disconnect, and a former owner cannot complete a reclaimed job.
Each attempt has a durable AI run with snapshot freshness fields. Successful
inference is recorded independently from application status: invalid output is
`REJECTED`, and a changed revision or analyzed-message identifier is `STALE`.
Neither state mutates domain data. The Go AI agent retains no customer text on
disk, uses outbound calls, and resumes polling after restart. A deterministic
fake provider supports development and disconnect testing without a GPU.

### Revenue and attribution

Revenue confirmation requires `revenue.confirm` and an `Idempotency-Key` and
stores an exact positive decimal amount as a confirmed RevenueEvent. The
RevenueEvent, its single attribution, idempotency response, and audit record
are one atomic write. Key reuse with changed content is a conflict.

Recovered attribution requires a Risk, Action, and Outcome from the same
tenant and Opportunity, each no later than confirmation and within the central
30-day attribution window. Organic and unknown revenue carry no corrective
chain. Confirmed Recovered Revenue sums only confirmed events with a formal
`RECOVERED` attribution and is returned separately per ISO currency; heuristic
association never contributes to this KPI. Cross-tenant references are
indistinguishable from missing resources.

### AI conversation analysis

Conversation analysis uses the versioned `analyze-conversation.v1` contract in
`contracts/ai/analyze_conversation_v1.schema.json`. Context is limited to the
latest 20 messages and a conservative 3,000-token target, and contains the
company context, prior derived summary, task, and output contract. Tenant IDs
are not sent to the model.

Provider output is retained for audit and is strictly decoded before use.
Unknown fields, missing fields, unsupported fact enums, confidence outside
0–1, missing evidence, and inconsistent price facts are rejected. Confidence
of 0.85 or above is strong, 0.65–0.849 is weak, and below 0.65 is untrusted;
untrusted facts are not supplied to domain policies. Model, prompt, schema,
conversation revision, and last-message versions are retained on jobs, runs,
and derived summaries.

Freshness is checked against both the conversation revision and last analyzed
message. A stale run is preserved with `STALE` application status and schedules
a replacement analysis for the current snapshot. Rejected and stale output
cannot mutate Opportunity or Risk state. A fresh valid result may update only
the derived ConversationSummary; later risk features must consume trusted
semantic facts through deterministic policies.

### AI benchmark and model freeze

Conversation-analysis models are compared offline with the versioned JSONL
format `lidradar-ai-benchmark.v1`. Dataset case IDs are unique and cases are
assigned explicitly to `TRAIN`, `VALIDATION`, or `GOLDEN`; a case with no facts
uses an empty `expectedFacts` array rather than omitting its labels. Repository
fixtures contain synthetic content only. The reviewed golden file is protected
by SHA-256 and the runner fails closed when its digest changes.

The runner sends the same versioned request consumed in production, applies the
production output validator and confidence policy, and reports precision,
recall, F1, exact-case rate, invalid output count, p50/p95/p99 latency, and
throughput. Quality and performance thresholds must be supplied explicitly
until product owners approve fixed values. A model manifest may be marked
frozen only after a 300–500 case labelled dataset passes those approved gates on
the target RTX 4060; a missing artifact digest or hardware run leaves it a
candidate.
