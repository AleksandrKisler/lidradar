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
- `GET /health/ready` подтверждает не только доступность PostgreSQL, но и точное
  совпадение встроенных миграций с журналом `schema_migrations`. Успешный ответ
  сообщает безопасные версию/ревизию сборки и последнюю миграцию.
- Хранилища `NewTestMemory...` являются только испытательными адаптерами.
  Рабочим командам в `backend/cmd` запрещено использовать их вместо PostgreSQL.

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
connection, inserts the `RawEvent`, inserts exactly one versioned outbox event
for a valid new event, and updates connection health. The unique
key is `(connection_id, external_event_id)`. A duplicate with the same payload
returns the original receipt; reuse of that external identifier with different
bytes is a conflict.

The HTTP handler returns `202` after this transaction and never calls
normalization, downstream AI, or another external service. An authenticated
malformed payload is retained once as `FAILED` with `INVALID_PAYLOAD` and
creates no normalization intent. Non-JSON bytes are represented losslessly by a
base64 JSON wrapper because PostgreSQL owns the raw payload as `JSONB`.
Миграция этапа 6 переносит ожидающие записи временной таблицы
`raw_event_normalization_work` в исходящий журнал и удаляет эту таблицу.

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
на уникальные внешние идентификаторы, идемпотентные операции и общую очередь
этапа 6 с арендой, параллельным захватом и ограниченной политикой повторов.

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

### Коммерческие возможности

Модуль `opportunity` владеет отдельным от переписки агрегатом Opportunity.
Conversation остаётся полным журналом общения, а Opportunity описывает только
коммерческий жизненный цикл. Одна переписка со временем может иметь несколько
последовательных возможностей, но частичный уникальный индекс PostgreSQL
разрешает не более одной активной записи одновременно.

Поддерживаются этапы:

```text
NEW → ENGAGED → QUALIFYING → PRICE_SENT → WAITING_CUSTOMER
    → WAITING_BUSINESS → BOOKING_INTENT → BOOKED → WON
                                           └────→ LOST
WON / LOST → ARCHIVED
```

Ручная корректировка может пропустить промежуточные активные этапы только
вперёд. В `LOST` можно перейти из любого активного этапа, в `WON` — только из
`BOOKED`; `WON` и `LOST` затем архивируются. Переход назад и повторное открытие
закрытой записи запрещены. Повтор текущего этапа идемпотентен и не раздувает
историю.

Каждое фактическое изменение атомарно добавляет строку в
`opportunity_stage_history`. Источник равен `RULE`, `AI`, `USER` или `IMPORT`;
ручная запись обязана содержать пользователя, а запись AI — уверенность.
Табличный триггер запрещает `UPDATE` и `DELETE`: исправление представляется новой
записью. Поэтому состояние полностью воспроизводится от первой записи
`NULL → NEW` до текущего этапа.

Денежный потенциал хранится точным `NUMERIC(14,2)` и отдаётся строкой JSON.
Автоматическое правило переносит цену только тогда, когда у найденной услуги
совпадают обе границы цены. Диапазон, единственная граница или отсутствие цены
оставляют `estimated_amount` и уверенность равными `NULL`; система не придумывает
среднее или нулевое значение.

Коммерческий кандидат обрабатывается после фиксации сообщения. Транзакция
переписки вместе с изменением создаёт `conversation.changed.v1`; диспетчер
ставит `opportunity.evaluate-commercial-candidate.v1` с ключом, включающим
переписку и её revision. Обработчик перечитывает актуальное состояние, поэтому
не принимает устаревший снимок из очереди за источник истины. Консервативное
правило создаёт Opportunity только по входящему неудалённому текстовому
сообщению с одним недвусмысленным совпадением активной услуги нужной точки.
Сервисное обращение, шум и несколько совпадений не создают лид без подтверждения.

OWNER и MANAGER используют разрешение `opportunity.manage`:

```text
GET   /api/v1/opportunities/{opportunityId}
PATCH /api/v1/opportunities/{opportunityId}
```

`GET` возвращает агрегат и всю историю. `PATCH` принимает только поле `stage` и
создаёт запись с источником `USER`. Каждый запрос требует сеанс и корректный
UUID в `X-Tenant-ID`; чужой идентификатор выглядит как отсутствующий.

### Риск NO_RESPONSE

Модуль Risk владеет агрегатом риска и устранением повторов активного состояния.
Проверка `NO_RESPONSE` использует детерминированное версионированное правило
`no-response/v1` и не обращается к AI. Риск создаётся или обновляется, только
когда в момент выполнения одновременно истинны условия:

- последнее значимое каноническое сообщение является входящим;
- после сообщения-основания нет исходящего ответа бизнеса;
- связанная Opportunity активна;
- в timezone IANA и недельном расписании Location прошло не меньше заданного
  для точки порога ответа.

Значимым для этого правила считается последнее неудалённое сообщение с
направлением `INCOMING` или `OUTGOING`. Системные и удалённые сообщения не
запускают и не закрывают правило. Первые 45–89 прошедших рабочих минут дают
важность `HIGH`, 90 и более — `CRITICAL`. Закрытое время не учитывается, а
остаток порога переносится в следующий рабочий период.

`conversation.changed.v1` ставит отдельное задание обновления плана. Создание и
фактическая смена этапа Opportunity атомарно добавляют в исходящий журнал
`opportunity.created.v1` или `opportunity.stage_changed.v1`. Поэтому первая
проверка не теряется при гонке между разбором сообщения и созданием Opportunity,
а закрытие сделки запускает автоматическое разрешение риска.

Проверка по расписанию содержит tenant и идентификатор Opportunity, срок и ключ
дедупликации, но не содержит авторитетный снимок переписки. При наступлении срока
worker заново читает из PostgreSQL Opportunity, Conversation, последнее
неудалённое значимое Message, Location и все семь строк рабочих часов. Ответ,
неактивная Opportunity или более новое входящее сообщение до собственного срока
не дают устаревшему заданию создать ложный риск. Ответ и неактивная Opportunity
также идемпотентно закрывают уже активный `NO_RESPONSE`.

PostgreSQL является рабочим источником истины. Частичный уникальный индекс
разрешает не более одного активного риска на сочетание tenant, Opportunity и
типа; повторные и конкурентные положительные проверки обновляют ту же строку.
Все операции хранилища требуют tenant и Opportunity. Чужое состояние не
раскрывается и не изменяется.

Отсутствующая Opportunity для события переписки означает отсутствие
коммерческого сценария и не является ошибкой. Отсутствующие Location, сообщение
или полный набор рабочих часов являются постоянной ошибкой конфигурации:
задание получает безопасный код `RISK_PLAN_INVALID` либо
`RISK_EVALUATION_INVALID` и переходит в `DEAD`. Ошибки PostgreSQL считаются
временными и проходят обычную ограниченную сетку повторов.

### Фоновая обработка

Общая очередь, проверки по расписанию и исходящий журнал хранятся в PostgreSQL
в таблицах `jobs`, `scheduled_checks` и `outbox_events`. Жизненный цикл задания:
`PENDING → PROCESSING → SUCCEEDED`; временная ошибка переводит его в `RETRY`, а
постоянная ошибка либо пятая неудачная попытка — в `DEAD`.

Захват выполняется атомарно через `FOR UPDATE SKIP LOCKED`. Он увеличивает номер
попытки, записывает уникального владельца процесса и устанавливает аренду на 30
секунд. Подтвердить результат может только текущий владелец до истечения аренды.
После истечения другой worker вправе повторно захватить то же задание; прежний
владелец получает ошибку потери аренды. Базовые задержки после неудачных попыток:
5 секунд, 30 секунд, 2 минуты и 10 минут. Неизвестная ошибка считается временной,
чтобы работа не потерялась; некорректные данные и неподдерживаемые типы явно
помечаются постоянными.

Scheduler захватывает наступившие `scheduled_checks` с пропуском заблокированных
строк, создаёт дедуплицированное задание и отмечает проверку поставленной в
очередь в одной транзакции. Повторный запуск не создаёт второе задание.

До появления административной панели worker раз в минуту журналирует только
сводные количества `PENDING`, `PROCESSING`, `RETRY`, `DEAD`, истёкших аренд и
просроченных `scheduled_checks`. Полезные нагрузки, сообщения и другие данные
организаций в диагностическую запись не попадают.

Изменение состояния и `outbox_events` записываются одной транзакцией владельца
бизнес-операции. Событие имеет неизменяемые ID, type, version, occurredAt,
tenantId, aggregate, traceId и data. Диспетчер доставляет его как минимум один
раз и применяет ту же аренду и ограниченную политику повторов. Обработчики
обязаны использовать ID задания или устойчивый ключ дедупликации при записи
побочного эффекта: падение после эффекта, но до подтверждения неизбежно приводит
к повторному вызову, который не должен создавать второй Message, Risk,
RevenueEvent, Notification или критическое действие.

Путь канонизации использует этот механизм полностью: webhook атомарно сохраняет
`RawEvent` и `connector.raw-event.received` версии 1; диспетчер создаёт уникальное
`connector.normalize-raw-event.v1`; worker при выполнении повторно читает
актуальные RawEvent и ChannelConnection из PostgreSQL. Успешное и ошибочное
завершение RawEvent идемпотентно.

### Radar API и сигналы об изменениях Risk

Чтение Radar и команды Risk всегда ограничены организацией и проверяют
именованные разрешения `risks.read` и `risks.manage`. Поддерживаются фильтры по
точке, важности и типу риска, а список использует непрозрачную курсорную
пагинацию. Канонический порядок целиком принадлежит серверу: важность по
убыванию, признак `BOOKING_INTENT`, потенциальная выручка по убыванию с
отсутствующими суммами в конце, длительность ожидания по убыванию (`due_at` по
возрастанию), время обнаружения и ID риска. Курсор содержит все поля порядка и
отпечаток фильтров, поэтому его нельзя применить к другой выборке.

Сводка считает только активные состояния `OPEN`, `ACKNOWLEDGED` и `ACTED`.
Потенциальная выручка суммируется по уникальным Opportunity только в основной
валюте организации, чтобы не складывать разные валюты. После этапа 12
`confirmedRecoveredRevenue` содержит только подтверждённые события с формальной
атрибуцией `RECOVERED`; до первого такого события значение равно `0.00`.
Денежные значения API передаются точными десятичными строками.

Детали Risk включают связанные Opportunity и Conversation из PostgreSQL.
Recommendation, Action, Outcome и Revenue появляются только после того, как их
создаст владеющий модуль; этап 9 не подменяет их вымышленными записями. Команды
подтверждения просмотра и закрытия идемпотентны. Идентификатор другой
организации неотличим от отсутствующего и не позволяет изменить состояние.

SSE передаёт только ограниченные организацией сигналы после долговечного
изменения. Между отдельными процессами `worker` и `api` сигнал переносится через
PostgreSQL `LISTEN/NOTIFY`, а внутри API раздаётся подключённым клиентам.
Клиент всегда перечитывает авторитетную REST-модель; потеря сигнала или разрыв
подключения не приводит к потере бизнес-данных. Версионированный HTTP-контракт
поддерживается в
[`../../contracts/openapi/openapi.yaml`](../../contracts/openapi/openapi.yaml).
Целевые операции, ещё не подключённые в `cmd/api`, имеют расширение
`x-lidradar-runtime-status: planned`; пути без этого расширения входят в
действующую поверхность API.

### Telegram risk notifications

Модуль Notification владеет логическим уведомлением, попытками доставки,
одноразовыми кодами Telegram и установленными связями пользователя с Telegram.
Оповещение об открытии риска использует детерминированный ключ
`risk:{risk_id}:opened`: повтор события или запроса Telegram не создаёт второе
видимое пользователю уведомление. Намерение и первая доставка сохраняются
атомарно до внешнего запроса. Каждый повтор остаётся отдельной попыткой по
общей временной сетке, а отказ Telegram никогда не меняет состояние Risk.

Открытый код привязки не сохраняется: в PostgreSQL находятся только SHA-256,
срок действия и время единственного использования. Команды Telegram требуют
ограниченной организацией связи пользователя и ключа идемпотентности; разрешены
только `OPEN_RISK`, `ACKNOWLEDGE` и `SNOOZE`. Денежные изменения через эту
границу запрещены.

### Рекомендации, действия и исходы

Каждый поддерживаемый тип Risk имеет детерминированную шаблонную рекомендацию,
поэтому полезная инструкция не зависит от доступности AI. Тип сервер читает из
авторитетной записи Risk и не принимает от клиента. Одна шаблонная рекомендация
на Risk создаётся или возвращается повторно без дублирования.

Action — ограниченный организацией неизменяемый факт, связанный с Risk. Первая
запись Action атомарно переводит активный Risk из `OPEN` или `ACKNOWLEDGED` в
`ACTED`, заполняя недостающие отметки ознакомления и действия. Повторный Action
остаётся отдельным фактом; для уже закрытого Risk история дополняется без
повторного открытия.

Outcome — ограниченный организацией неизменяемый факт, связанный с Opportunity.
Исправление создаёт новый Outcome вместо переписывания истории. В карточке Risk
показывается последний Outcome этой Opportunity, а полный журнал остаётся в
PostgreSQL.

Команды Action и Outcome требуют разрешение `risks.manage` и заголовок
`Idempotency-Key` длиной до 255 знаков. Ключ ограничен организацией и операцией:
точный повтор возвращает сохранённый ответ, а повторное использование с другим
содержимым даёт конфликт. Action либо Outcome, запись `idempotency_keys` и
`audit_log` фиксируются одной транзакцией. Эти таблицы, как и сами факты,
защищены от `UPDATE` и `DELETE` триггером PostgreSQL. Чужие идентификаторы
неотличимы от отсутствующих и не позволяют создать запись.

Architecture changes require an ADR; see [`../adr/README.md`](../adr/README.md).

### Home AI node infrastructure

AI inference uses the outbound pull model from ADR 0030. Registered nodes are
authenticated by a rotatable secret whose SHA-256 digest is the only persisted
form. A ready heartbeat renews only leases currently owned by that node. Jobs
carry the tenant, conversation, base conversation revision, and last analyzed
message identifier.

Каждый машинный POST-запрос содержит `Authorization: Bearer`,
`X-LidRadar-Node-ID`, UTC `X-LidRadar-Timestamp`, уникальный
`X-LidRadar-Request-ID` и `X-LidRadar-Signature`. HMAC-SHA256 связывает method,
path, точный timestamp, request ID и SHA-256 точного тела. Cloud Core отклоняет
неверную подпись, время вне 60-секундного окна и повтор request ID. Реквизиты,
подписи и тела запросов не журналируются.

Локальные административные команды создают реквизиты в новом файле `0600` и
никогда не выводят открытый секрет. Вращение секрета переводит узел в `OFFLINE`,
сразу запрещает старый секрет и оставляет текущей аренде только естественное
истечение. Отзыв переводит узел в `REVOKED`, сохраняя историю Job и Run.

The default lease is 120 seconds. Claim is atomic, requires an exact match
between the job model requirement and node model version, expired work may be
reclaimed after a node disconnect, and a former owner cannot complete a reclaimed job.
Each attempt has a durable AI run with snapshot freshness fields. Successful
inference is recorded independently from application status: invalid output is
`REJECTED`, and a changed revision or analyzed-message identifier is `STALE`.
Neither state mutates domain data. The Go AI agent retains no customer text on
disk, uses outbound calls, and resumes polling after restart. A deterministic
fake provider supports development and disconnect testing without a GPU.

Проверка свежести перед применением результата выполняется повторно внутри
транзакции завершения под блокировкой строки Conversation. Изменение между
предварительным чтением и фиксацией переводит Run в `STALE` и создаёт новое
задание из заново прочитанного контекста.

### Выручка и атрибуция

Подтверждение выручки требует разрешение `revenue.confirm` и заголовок
`Idempotency-Key`. Сервер сохраняет точную положительную десятичную сумму как
подтверждённый RevenueEvent с источником `USER_CONFIRMED`. RevenueEvent, его
единственная атрибуция, сохранённый ответ идемпотентности и запись аудита
создаются атомарно. Повтор ключа с изменённым содержимым является конфликтом.

Атрибуция `RECOVERED` требует Risk, Action и Outcome из той же организации и
Opportunity. Каждый факт должен существовать не позже подтверждения и попадать
в единое 30-дневное окно. Точный повтор сначала разрешается по сохранённому
ключу и потому остаётся допустимым после истечения окна. `ORGANIC` и `UNKNOWN`
не содержат корректирующую цепочку.

Показатель подтверждённой возвращённой выручки суммирует только события
`CONFIRMED` с формальной атрибуцией `RECOVERED` и возвращается отдельно для
каждой трёхбуквенной валюты. Эвристическое совпадение в показатель не входит.
Ссылки на объекты другой организации неотличимы от отсутствующих.

### AI conversation analysis

Анализ переписки использует версионированный контракт
`analyze-conversation.v1` из
`contracts/ai/analyze_conversation_v1.schema.json`. Контекст ограничен 20
последними анализируемыми сообщениями и консервативной целью примерно 3000
токенов. Он содержит контекст компании, прежнее производное резюме, задачу и
версию результата. Идентификатор организации модели не передаётся.

Исходный ответ поставщика сохраняется для аудита и строго разбирается до
использования. Лишние и отсутствующие поля, дополнительные данные после JSON,
неподдерживаемые типы фактов, уверенность вне диапазона 0–1, отсутствующие
доказательства и несогласованные поля цены отклоняются. Одинаковые факты
объединяются с минимальной уверенностью и общей доказательной базой;
противоречивые повторы отклоняются. Уверенность от 0,85 считается сильной, от
0,65 до 0,849 — слабой, ниже 0,65 — недоверенной. Недоверенные факты не
передаются предметным правилам. Версии модели, инструкции, схемы, revision и
последнего анализируемого сообщения сохраняются в заданиях, попытках и
производных резюме.

Свежесть проверяется одновременно по revision переписки и последнему
анализируемому текстовому сообщению. Материал без текста учитывается revision,
но не создаёт ложное несовпадение message ID. Устаревшая попытка сохраняется со
статусом применения `STALE` и ставит повторный анализ актуального снимка.
Отклонённый и устаревший ответ не может изменять Opportunity или Risk. Свежий
допустимый результат может обновить только производное ConversationSummary;
будущие Risk-функции обязаны получать доверенные смысловые факты через
детерминированные правила.

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
