# Обзор и глоссарий

## 1. Назначение

LidRadar — серверная часть сервиса, который помогает малому бизнесу услуг
(автосервис, детейлинг, салон и т. п.) не терять деньги в переписках с
клиентами. Система не продаёт и не отвечает клиенту сама: она наблюдает за
переписками, находит коммерческие возможности и риски их потери, сообщает об
этом людям и фиксирует, что было сделано и сколько денег вернулось. Границы
зафиксированы в `docs/architecture/NON_GOALS.md`: никаких автоответов,
автономных AI-продаж, полноценной CRM или BI.

## 2. Глоссарий

| Термин | Значение в коде | Где живёт |
|---|---|---|
| **Organization / tenant** | Организация-арендатор, граница изоляции всех данных. `tenant_id` есть в каждой бизнес-таблице, PostgreSQL применяет к ним политики RLS. | `tenant`, все таблицы |
| **Membership, роли OWNER / MANAGER** | Членство пользователя в организации с ролью. OWNER управляет организацией, точками, каталогом, каналами и участниками; MANAGER имеет только рабочие разрешения. Разрешения выдаются по роли (`risks.read`, `risks.manage`, `revenue.confirm`, …). | `tenant` |
| **Location** | Точка обслуживания организации со своим часовым поясом и **часами работы** (семь строк недели). Рабочее время нужно правилам риска. | `tenant` |
| **Service (каталог)** | Услуга организации; упоминание названия услуги во входящем сообщении делает переписку коммерческим кандидатом. | `catalog` |
| **Channel connection** | Подключение канала (провайдеры `CONNECTED_BUSINESS_BOT`, `GENERIC_WEBHOOK`, `TEST`, `IMPORT`) с набором способностей (`CAN_RECEIVE_MESSAGES`, `CAN_RECEIVE_EDITS`, …), состоянием `ACTIVE`/`DEGRADED`/`ERROR`/`DISCONNECTED`, хешем секрета проверки вебхука и зашифрованными реквизитами. | `connector` |
| **RawEvent** | Сырое событие провайдера, сохранённое как есть до любой интерпретации (persist-first); статусы `RECEIVED` → `PROCESSING` → `PROCESSED`/`FAILED`. | `connector` |
| **CanonicalEvent** | Канало-независимое событие: `message.received.v1`, `message.edited.v1`, `message.deleted.v1`, `connection.updated.v1`. | `connector` → `conversation` |
| **Contact, Conversation, Message, Attachment** | Каноническая модель переписки: контакт клиента, диалог (`ACTIVE`/`ARCHIVED`), сообщения с направлением `INCOMING`/`OUTGOING`/`SYSTEM` и типом `TEXT`/`IMAGE`/`VIDEO`/`AUDIO`/`VOICE`/`DOCUMENT`/`OTHER`, метаданные вложений (двоичные файлы — вне PostgreSQL). У переписки есть `revision`, растущий только при фактическом изменении. | `conversation` |
| **Opportunity** | Коммерческая возможность (сделка) внутри переписки со стадией из набора `NEW`, `QUALIFYING`, `ENGAGED`, `PRICE_SENT`, `WAITING_CUSTOMER`, `WAITING_BUSINESS`, `BOOKING_INTENT`, `BOOKED`, `WON`, `LOST`, `ARCHIVED`, оценочной суммой в валюте организации и историей переходов с источником `RULE`/`USER`/`AI`/`IMPORT`. | `opportunity` |
| **Risk (RiskSignal)** | Сигнал риска потери сделки определённого типа и важности (`LOW`, `MEDIUM`, `HIGH`, `CRITICAL`). Активные статусы `OPEN` → `ACKNOWLEDGED` → `ACTED`; закрывающие — `RESOLVED`, `EXPIRED`, `IGNORED`, `FALSE_POSITIVE`. На одну возможность и тип — не больше одного активного риска (частичный уникальный индекс). | `risk` |
| **Типы риска** | `NO_RESPONSE` (менеджер не ответил в рабочее время), `BOOKING_NOT_CONFIRMED` (намерение записи без подтверждения), `PROMISE_NOT_FULFILLED` (обещание не выполнено к сроку), `CUSTOMER_SILENT_AFTER_PRICE` (клиент замолчал после цены), `FOLLOW_UP_CANDIDATE` (пора напомнить). Все правила детерминированы. | `risk` |
| **Scheduled check** | Отложенная проверка (`*_DUE`) со сроком, вычисленным по часовому поясу и часам работы; в срок планировщик превращает её в задание. Хранит только идентификаторы, не снимок состояния. | `jobs`, `risk`, `notification` |
| **Radar** | Сводка активных рисков организации с серверным порядком: важность, намерение записи, потенциальная выручка, длительность ожидания, время обнаружения. | `risk` |
| **Recommendation** | Детерминированная шаблонная рекомендация по типу риска; создаётся сервером, работает без AI. | `corrective` |
| **Action** | Неизменяемая запись корректирующего действия менеджера по риску (`CALL`, `SEND_MESSAGE`, `COPY_REPLY`, `MARK_CONTACTED`, `OPEN_CONVERSATION`, `OTHER`); первое действие переводит активный риск в `ACTED`. | `corrective` |
| **Outcome** | Неизменяемая запись исхода возможности (`RESPONDED`, `THINKING`, `BOOKED`, `PAID`, `LOST`, `NOT_A_LEAD`); исправление — новой записью. | `corrective` |
| **RevenueEvent / RevenueAttribution** | Подтверждённая пользователем выручка по возможности (источник `USER_CONFIRMED`, статус `CONFIRMED`) и её единственная атрибуция: `RECOVERED` (с обязательной цепочкой риск → действие → исход не старше 30 дней), `ORGANIC`, `UNKNOWN`. | `revenue` |
| **Notification / NotificationDelivery** | Логическое уведомление вида `RISK_OPENED`, `RISK_DIGEST`, `RISK_ESCALATED` (ключ вроде `risk:{risk_id}:opened`) и отдельные попытки доставки по каналу `TELEGRAM` или `IN_APP` со статусами `PENDING`/`PROCESSING`/`RETRY`/`SUCCEEDED`/`DEAD`. | `notification` |
| **Notification preference** | Личная политика получателя по типу риска: `IMMEDIATE`, `DIGEST`, `DISABLED`, порог важности, тихие часы (в том числе через полночь). | `notification` |
| **Digest** | Сводка накопленных уведомлений одним сообщением по расписанию (`NOTIFICATION_DIGEST_DUE`). | `notification` |
| **AI node** | Домашний GPU-узел с llama.cpp и агентом `cmd/ai-agent`; забирает задания по pull-модели, авторизуется секретом узла, допущен к явному списку организаций. | `ai` |
| **AI job / AI run** | Задание анализа переписки (статусы `PENDING`, `LEASED`, `RUNNING`, `SUCCEEDED`, `RETRY`, `DEAD`; не больше одного ожидающего на переписку) и его долговечный прогон (`RUNNING`/`SUCCEEDED`/`FAILED`) со статусом применения результата `PENDING`, `APPLIED`, `STALE`, `REJECTED`. Узел бывает `READY`, `OFFLINE`, `REVOKED`. | `ai` |
| **Semantic facts / conversation summary** | Проверенные факты анализа типов `BOOKING_INTENT`, `BUSINESS_COMMITMENT`, `PRICE_MENTIONED`, `FOLLOW_UP_CANDIDATE` с уверенностью и доказательствами, привязанные к ревизии переписки и последнему проанализированному сообщению. Их читают только детерминированные правила. | `ai` → `risk` |
| **Feedback (вердикт)** | Оценка риска человеком: `TRUE_POSITIVE` или `FALSE_POSITIVE` с причиной (`CUSTOMER_ALREADY_ANSWERED`, `CUSTOMER_ALREADY_BOOKED`, `CUSTOMER_REJECTED`, `NOT_A_LEAD`, `WRONG_INTERPRETATION`, `OTHER`); append-only, с неизменяемым снимком риска; питает метрику точности по типам. | `risk` |
| **ML consent** | Согласие организации на использование её данных в наборах (`scope DATASETS`); без него факты не помечаются пригодными для датасета. | `tenant` |
| **Outbox event** | Версионированное событие, записанное в той же транзакции, что и изменение; диспетчер доставляет его обработчикам как минимум один раз (статусы `PENDING` → `PROCESSING` → `PUBLISHED`, `RETRY`, `DEAD`). | `events` |
| **Job** | Единица фоновой работы в PostgreSQL с арендой, попытками и статусами `PENDING`/`PROCESSING`/`RETRY`/`SUCCEEDED`/`DEAD`; отложенная проверка живёт как `SCHEDULED` → `ENQUEUED` или `CANCELLED`. | `jobs` |
| **Idempotency-Key** | Заголовок критических команд; точный повтор возвращает прежний результат, тот же ключ с другим телом — `409`. | `corrective`, `revenue` |
| **PLATFORM_ADMIN** | Право платформенного администратора (строка в `platform_admins`, не членство), даёт доступ к `/api/v1/admin/*`. | `admin` |
| **Correlation ID** | Идентификатор запроса (`X-Request-ID`), протягиваемый в логи и ответы. | `platform/http`, `observability` |

## 3. Сквозные потоки

### 3.1. Входящее сообщение

```text
провайдер → POST /api/v1/webhooks/{provider}/{tenantId}/{connectionId}
  проверка секрета заголовка, размера тела, состояния подключения
  ┌ одна короткая транзакция ─────────────────────────────────────┐
  │ RawEvent (дедупликация по внешнему идентификатору события)    │
  │ + outbox_events: connector.raw-event.received.v1              │
  └───────────────────────────────────────────────────────────────┘
  ответ 202 Accepted (обработка ещё не началась)
worker: диспетчер outbox → job connector.normalize-raw-event.v1
  нормализация в CanonicalEvent → contact / conversation / message
  ревизия переписки растёт → outbox: conversation.changed.v1
```

Повтор одного и того же события провайдера безопасен на каждом шаге: сырое
событие дедуплицируется, каноническое применение идемпотентно, ревизия не
растёт без фактического изменения.

### 3.2. От сообщения к возможности и риску

```text
conversation.changed.v1
  → opportunity.evaluate-commercial-candidate.v1
      входящее сообщение упоминает услугу каталога → Opportunity (NEW)
      → outbox: opportunity.created.v1
  → risk.refresh-no-response-plan.v1
      срок ответа по часовому поясу и часам работы точки
      → scheduled_checks: NO_RESPONSE_DUE (только id возможности)
scheduler: срок наступил → job risk.evaluate-no-response.v1
  worker перечитывает PostgreSQL: был ли OUTGOING ответ?
  нет → одна активная строка risk_signals NO_RESPONSE
      → outbox: risk.opened.v1, сигнал risk.changed для SSE
  да  → активный риск (если был) → RESOLVED
```

Аналогично работают остальные правила: `BOOKING_NOT_CONFIRMED` и
`PROMISE_NOT_FULFILLED` опираются на семантические факты AI, а
`CUSTOMER_SILENT_AFTER_PRICE` и `FOLLOW_UP_CANDIDATE` — на этапы возможности
и время без ответа клиента. Каждое правило хранит в проверке лишь
идентификаторы и перечитывает состояние в момент выполнения, поэтому
опоздавшее задание не применяет устаревший снимок.

### 3.3. Уведомление и реакция человека

```text
risk.opened.v1
  → политика получателей: роль, личные настройки по типу риска,
    порог важности, тихие часы → IMMEDIATE / в очередь сводки / DISABLED
  → Notification (ключ risk:{id}:opened) + первая NotificationDelivery
  → Telegram Bot API (кнопки OPEN_RISK / ACKNOWLEDGE / SNOOZE)
     отказ → повторы 5 с, 30 с, 2 мин, 10 мин → DEAD; риск не меняется
  → IN_APP-доставка завершается локально
GET /api/v1/radar, GET /api/v1/risks — чтение
POST /risks/{id}/acknowledge, /resolve — команды, повтор безопасен
POST /risks/{id}/recommendation — шаблон по типу риска
POST /risks/{id}/actions (Idempotency-Key) — риск → ACTED
POST /opportunities/{id}/outcomes (Idempotency-Key) — BOOKED / PAID / LOST
POST /opportunities/{id}/revenue (Idempotency-Key) — RECOVERED с цепочкой
GET /api/v1/events — SSE-сигнал «перечитай», без бизнес-данных
```

### 3.4. Асинхронный AI

```text
conversation.changed.v1 → AI job (одно ожидающее на переписку, свежий снимок
                          заменяет прежний; дебаунс)
домашний узел: heartbeat → claim (аренда, допуск организации, версия модели)
  → started (ai_run) → llama.cpp → strict JSON analyze-conversation.v1
  → complete: валидация схемы и свежести (revision, analysisThroughMessageId)
      свежий → conversation_summaries.semantic_facts, ai.analysis.applied.v1
      устаревший → STALE, факты не применяются
  → правила риска читают факты (BOOKING_INTENT → BOOKING_NOT_CONFIRMED и т. д.)
```

AI никогда не пишет в `opportunities`, `risk_signals` или переписки напрямую.

### 3.5. Обратная связь и аналитика

Вердикт по риску (`POST /risks/{id}/feedback`) фиксируется append-only со
снимком; `FALSE_POSITIVE` закрывает активный риск, причина `NOT_A_LEAD`
переводит возможность в `LOST`. Точность по типам считается за окно
обнаружения с признаком надёжности (покрытие вердиктами ≥ 0,5). Сводка
аналитики (`GET /analytics/summary`) считает показатели за календарные даты в
часовом поясе организации в одной read-only транзакции.

## 4. Что система гарантирует и чего не обещает

- **Гарантирует:** сохранение внешнего события до любой обработки; доставку
  событий и заданий как минимум один раз с идемпотентными обработчиками;
  изоляцию организаций на уровне строк PostgreSQL; детерминированность
  правил риска и независимость бизнес-состояния от AI и от Telegram;
  неизменяемость журналов действий, исходов, выручки, вердиктов и аудита;
  точные десятичные деньги.
- **Не обещает:** ровно одну обработку (повторы возможны и безопасны),
  доставку уведомления при длительном отказе Telegram (попытки заканчиваются
  `DEAD`, видны администратору), синхронного результата AI (очередь и узел
  могут отставать), хранения двоичных вложений (только метаданные до
  подключения S3).
