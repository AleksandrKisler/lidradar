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

Все экземпляры API используют общие атомарные окна ограничения частоты в
PostgreSQL. Регистрация допускает не более 5 запросов с одного сетевого адреса
за час, вход — не более 20 запросов с адреса за минуту и не более 5 попыток для
одной нормализованной учётной записи за 15 минут, обновление сеанса — не более
60 запросов с адреса за минуту. Успешный вход сбрасывает счётчик учётной записи;
счётчик адреса не сбрасывается. В таблице находятся только SHA-256-отпечатки.
Отказ всегда имеет единый код `RATE_LIMITED`, статус 429 и `Retry-After`, не
сообщая о существовании пользователя. Адрес берётся только из доверенного
сетевого соединения `RemoteAddr`; присланные клиентом `X-Forwarded-For` и
подобные заголовки не принимаются без отдельной настройки доверенного
пограничного прокси.

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

Членство не удаляется физически: на него ссылаются неизменяемые факты
(выручка, действия, исходы, аудит). Отзыв доступа переводит запись в статус
`DISABLED` и заполняет `revoked_at`; триггер базы запрещает `DELETE`.
Разрешения выдаются только активному членству, поэтому доступ теряется сразу,
а история подтверждений остаётся на месте.

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

Сильный проверенный факт `BOOKING_INTENT` (уверенность не ниже `0,85`,
`trusted = true`) переводит активную сделку переписки в этап `BOOKING_INTENT`
с источником истории `AI`, уверенностью и ссылкой на AI-run. Переход
выполняется обработчиком события `ai.analysis.applied.v1`, только вперёд по
машине этапов и идемпотентно: повторная доставка находит сделку уже на нужном
этапе, а сделка на `BOOKED` и позже не трогается. Слабый и ненадёжный факт
этап не меняют.

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

### Риск BOOKING_NOT_CONFIRMED

Правило `booking-not-confirmed/v1` отделяет смысловой вывод модели от
предметного решения. После каждого изменения переписки обработчик строит
актуальный ограниченный контекст и идемпотентно ставит анализ в очередь
домашнего узла. Только свежий и прошедший строгую проверку результат обновляет
`ConversationSummary`, сохраняет проверенные смысловые факты и атомарно создаёт
событие `ai.analysis.applied.v1`.

Правило R3 считает намерение записаться установленным, когда выполняется одно
из условий:

- модель вернула положительный `BOOKING_INTENT` с уверенностью не ниже `0,85`;
- Opportunity уже находится на этапе `BOOKING_INTENT`.

Правило работает только на этапах `QUALIFYING`, `PRICE_SENT`,
`WAITING_CUSTOMER`, `WAITING_BUSINESS` и `BOOKING_INTENT` (канон
LR-BE-RM-013). На `NEW` и `ENGAGED` риск не открывается: сильный факт сначала
переводит сделку в `BOOKING_INTENT` (см. «Коммерческие возможности»), после
чего правило работает по этапу. `BOOKED`, `WON`, `LOST` и `ARCHIVED` закрывают
риск.

Для создания проверки последнее каноническое направление должно быть
`INCOMING`, то есть ответ ожидается от бизнеса. Срок равен 30 рабочим минутам
по часовому поясу и недельному расписанию Location. При наступлении срока
создаётся `CRITICAL`-риск `BOOKING_NOT_CONFIRMED`. Путь через модель имеет
источник `HYBRID` и сохраняет `confidence`, `ai_run_id`, сообщение-доказательство
и версию правила. Путь через этап имеет источник `RULE` и не притворяется
выводом модели.

Недостаточная уверенность, отклонённый результат и устаревший анализ не создают
проверку, не открывают и не закрывают Risk и не меняют этап Opportunity.
Подтверждённый этап `BOOKED`, а также закрытая Opportunity, идемпотентно
переводят активный риск в `RESOLVED`. Ответ бизнеса без подтверждения записи не
считается достаточным основанием для закрытия уже открытого риска.

Частичный уникальный индекс базы оставляет не более одного активного риска R3
на Opportunity. Ключ проверки включает Opportunity, сообщение-доказательство и
версию правила. Для типа настроены немедленное Telegram-уведомление и
шаблонная рекомендация «Предложить клиенту конкретный свободный слот.»; ради
этого текста модель повторно не вызывается.

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

Изменения архитектуры требуют ADR; перечень решений находится в
[`../adr/README.md`](../adr/README.md).

### Домашний AI-узел

Выполнение AI-задач использует исходящую модель опроса из ADR 0030.
Зарегистрированные узлы удостоверяются заменяемым секретом; в постоянном
хранилище находится только его SHA-256. Сигнал готовности продлевает только те
аренды, которыми уже владеет отправивший его узел. Задание содержит организацию,
переписку, исходную ревизию переписки и идентификатор последнего
проанализированного сообщения.

Каждый узел имеет явный разрешительный список организаций в PostgreSQL.
Регистрация требует исходную организацию, а дополнительное назначение возможно
только локальной административной командой. Захват фильтрует задания по этому
списку до чтения JSON с инструкцией. Составные внешние ключи
`(leased_by, tenant_id)` и `(node_id, tenant_id)` запрещают межорганизационную
аренду и запуск даже при ошибке прикладного SQL. Узел без назначений не получает
ни одного задания.

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

Стандартная аренда длится 120 секунд. Захват атомарен и требует точного
совпадения требуемой заданием версии модели с версией узла. Просроченное задание
можно перехватить после отключения узла, а прежний владелец уже не может его
завершить. Каждая попытка создаёт долговечную запись запуска AI с полями
свежести снимка. Успешное обращение к модели учитывается отдельно от результата
применения: недопустимый ответ получает `REJECTED`, а изменившаяся ревизия или
идентификатор последнего сообщения — `STALE`. Ни одно из этих состояний не
изменяет предметные данные. Агент на Go не сохраняет переписки на диске,
использует только исходящие соединения и возобновляет опрос после перезапуска.
Детерминированная заглушка позволяет разрабатывать и проверять потерю связи без
графического ускорителя.

Проверка свежести перед применением результата выполняется повторно внутри
транзакции завершения под блокировкой строки Conversation. Изменение между
предварительным чтением и фиксацией переводит Run в `STALE` и создаёт новое
задание из заново прочитанного контекста.

Поверх скользящей аренды действует абсолютный потолок: `leased_at` ставится
при захвате и не продлевается heartbeat, а задание старше 15 минут с момента
захвата перехватывается другим узлом даже при живом heartbeat. Прежний запуск
получает `LEASE_CAP_EXCEEDED`, число попыток растёт, поздний ответ прежнего
узла отбрасывается идемпотентно.

На одну переписку существует не более одного ожидающего задания. Более
свежий снимок заменяет инструкцию, ревизию и последнее сообщение ожидающего
задания, сохраняя его идентификатор; захваченное задание не меняется, потому
что узел уже получил его инструкцию, а новое сообщение во время выполнения
ставит отдельное ожидающее задание. Задание доступно для захвата не раньше
чем через минуту после постановки, поэтому всплеск сообщений даёт один
inference с последней ревизией (ADR 0036).

Каждый сохранённый факт несёт признак `trusted`, вычисленный Cloud Core по
уровням уверенности: сильный факт (не ниже `0,85`) доверенный и может
открывать Risk; слабый (`0,65–0,849`) сохраняется с `trusted = false` для
наблюдения и метрик; ненадёжный (ниже `0,65`) в проекцию не попадает. Модель
прислать это поле не может — оно отклоняется строгим разбором ответа, а
ограничение базы не принимает факт без признака.

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

Каждое звено цепочки — RevenueEvent, Risk, Action, Outcome — связано с
атрибуцией составным внешним ключом, включающим Opportunity, поэтому чужая
Opportunity внутри своей организации отклоняется базой без участия триггера.
Действие хранит `opportunity_id` своего риска. Одна Opportunity даёт не более
одной атрибуции `RECOVERED`; попытка второй отклоняется PostgreSQL и
возвращается как `409 RECOVERED_ALREADY_ATTRIBUTED`. При оплате частями
`RECOVERED` получает первое событие, остальные подтверждаются как `ORGANIC`.

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

### Проверка и фиксация AI-модели

Модели анализа переписки сравниваются вне рабочего контура на версионированных
случаях JSONL `lidradar-ai-benchmark.v1`. Идентификаторы случаев уникальны, а
каждый случай явно относится к `GOLDEN` или `DEV`; обучающей выборки нет,
потому что дообучение не предусмотрено. Отсутствие ожидаемых фактов задаётся
пустым `expectedFacts`, а не пропуском разметки. Репозиторий содержит только
синтетические переписки. Контрольный файл защищён SHA-256; несовпадение суммы
прекращает запуск с кодом `GOLDEN_DIGEST_MISMATCH`.

Исполнитель отправляет тот же версионированный запрос, который используется в
рабочем контуре, применяет рабочую проверку ответа и сравнивает с разметкой
только доверенные факты (уверенность не ниже `0,85`): слабый факт не открывает
Risk и потому не считается обнаруженным. Отчёт
содержит точность, полноту, F1, долю полностью верных случаев, долю допустимого
JSON, точное совпадение доказательств, p50/p95/p99 задержки и пропускную
способность. Отдельно рассчитывается точность каждого типа смыслового факта,
чтобы общий результат не скрывал небезопасную категорию.

До открытия контрольной выборки для `lidradar-main-v1` установлены следующие
границы: общая точность не ниже 0,90; точность каждого поддерживаемого типа
факта не ниже 0,85; полнота и F1 не ниже 0,90; полностью верные случаи не ниже
0,85; допустимый JSON не ниже 0,99; точное совпадение доказательств не ниже
0,90; p95 не выше 8000 мс. На RTX 4060 дополнительно требуются отсутствие OOM,
пиковое потребление видеопамяти не выше 7500 МиБ и скорость генерации не ниже
20 токенов/с.

Манифест можно перевести в `FROZEN` только после прохождения всех границ на
размеченном наборе из 500 случаев (400 `GOLDEN` + 100 `DEV`) и целевой
RTX 4060 с 8 GB видеопамяти, с записанными SHA-256 модели и контрольной
выборки. Настройка инструкции и параметров генерации допускается по `DEV`;
`GOLDEN` запускается только перед сменой статуса манифеста и не используется
для подгонки. Любой незаполненный обязательный результат оставляет модель
кандидатом. Результаты в манифесте, измеренные на прежней контрольной выборке,
помечены как требующие повторного прогона.

Текущая версия `analyze-conversation.v1` измеряет четыре поддерживаемых факта:
намерение записаться, деловое обязательство, необходимость следующего контакта
и упоминание цены. Метрики лида, стадии и вида услуги нельзя объявлять
проверенными, пока эти поля не появятся в контракте и размеченном наборе.

### Отложенные правила схемы (этап R)

Часть задач этапа R описывает таблицы, которые ещё не созданы. Их
канонический вид зафиксирован здесь, чтобы создающие их этапы не разошлись с
ТЗ:

- `risk_policy_configs` (этап 20): `threshold_value` + `threshold_unit`
  (`BUSINESS_MINUTES` | `CALENDAR_MINUTES`), `escalation_value`, уникальность
  `UNIQUE NULLS NOT DISTINCT (tenant_id, risk_type)`, чтобы строка платформы
  с `tenant_id IS NULL` не вставлялась дважды (ADR 0035, LR-BE-RM-002/014).
- `encrypted_secrets`: `UNIQUE NULLS NOT DISTINCT (tenant_id, kind)`
  (LR-BE-RM-002).
- `feature_flags` (этап 26): суррогатный `id`, `UNIQUE NULLS NOT DISTINCT
  (key, tenant_id)`; строка организации побеждает строку платформы,
  отсутствие обеих — флаг выключен (LR-BE-RM-003).
- `platform_admins` (этап 23): суррогатный `id`, `revoked_by`, частичный
  уникальный индекс на `user_id WHERE revoked_at IS NULL`; отзыв не удаляет
  строку (LR-BE-RM-008).
- `notification_preferences` (этап 20): полный набор полей §3.7
  (`minimum_severity`, `in_app_enabled`, `telegram_enabled`,
  `quiet_hours_enabled`, `created_at`), часовой пояс организации, запрет
  вырожденных тихих часов `quiet_hours_start <> quiet_hours_end` и семантика
  интервала через полночь `[start, 24:00) ∪ [00:00, end)` (LR-BE-RM-015/020).
- `risk_feedback` (этап 21): точность считается по последнему вердикту на
  Risk через `DISTINCT ON (risk_id)`, а критерий Milestone E засчитывается
  только при `coverage_rate >= 0.5` (LR-BE-RM-019).
- RLS (этап 24): роли `lidradar_app`, `lidradar_worker`, `lidradar_platform` и
  fail-closed политика по `current_setting('lidradar.tenant_id', true)`
  (ADR 0034).
