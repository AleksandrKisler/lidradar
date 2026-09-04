# Фоновая обработка

Вся асинхронная работа — в PostgreSQL: транзакционный outbox, очередь
заданий с арендой, отложенные проверки, попытки доставки уведомлений и
очередь AI (ADR 0026, 0027, 0029, 0036). Ни один процесс не держит очередь
в памяти; брокеров нет (`NON_GOALS.md`).

```text
 транзакция бизнес-изменения ──► outbox_events ──► worker: Dispatcher.RunOne
                                                     │ (обработчик события)
                                                     ▼
                                     jobs ◄── scheduled_checks ◄── scheduler: PromoteDue
                                       │
                                worker: Worker.RunOne (обработчик задания)
                                       │
                          notifications + notification_deliveries
                                       │
                          worker: DispatchOne → Telegram / IN_APP
```

## 1. Outbox (`events`)

- **Запись** — `AppendTx` в той же транзакции, что и изменение (сырое
  событие, переписка, сделка, риск, прогон AI). Идентичность события — его
  `id`: повтор с тем же содержимым не ошибка, с другим — конфликт. Ключ
  доставки — `type.vN` (`conversation.changed.v1`).
- **Захват** (`Claim`): истёкшие аренды с исчерпанными попытками помечаются
  `DEAD` (`LEASE_EXPIRED_MAX_ATTEMPTS`); затем `FOR UPDATE SKIP LOCKED` по
  `available_at, occurred_at, id` среди `PENDING`/`RETRY` с наступившим
  `available_at` или `PROCESSING` с истёкшей арендой; `attempt_count + 1`,
  `leased_by`, `lease_until = now + 30 с`.
- **Доставка** (`Dispatcher.RunOne`): одно событие за вызов; обработчик по
  ключу (нет обработчика → сразу `DEAD` с `UNSUPPORTED_EVENT_TYPE`);
  выполняется в `tenantctx.WithTenant(event.TenantID)`; успех → `PUBLISHED`;
  ошибка → `RETRY` через 5 с / 30 с / 2 мин / 10 мин или `DEAD` после пятой
  попытки (постоянная ошибка — сразу `DEAD`); отмена контекста — выход без
  подтверждения, событие вернётся по истечении аренды.
- **Порядок**: несколько диспетчеров берут разные строки; глобальный порядок
  не гарантируется. Несколько подписчиков одного типа — `ChainHandlers`
  строго по очереди.
- **Гарантия** — как минимум один раз. Каждый потребитель опирается на
  собственный устойчивый ключ дедупликации (§ 5).

## 2. Очередь заданий (`jobs`)

- **Постановка** (`Enqueue`): `INSERT … ON CONFLICT (tenant_id, job_type,
  dedup_key) DO NOTHING`; повтор с тем же payload возвращает существующее
  задание, с другим — конфликт. Ключ дедупликации не истекает: успешно
  выполненное задание навсегда занимает свой ключ.
- **Захват** (`Claim`): та же схема, что у outbox; порядок `priority DESC,
  available_at, created_at, id`; аренда 30 с; `attempt_count` растёт при
  захвате.
- **Выполнение** (`Worker.RunOne`): одно задание за вызов; обработчик по типу
  (нет → `DEAD`, `UNSUPPORTED_JOB_TYPE`); контекст организации задания;
  `Succeed`/`Fail` с проверкой владения (`leased_by`, `lease_until`) —
  потерявший аренду владелец получает `ErrLeaseLost` и не может подтвердить
  результат.
- **Классификация ошибок**: `Permanent(code)` → `DEAD` сразу;
  `Retryable(code)` → `RETRY` по расписанию; неизвестная ошибка считается
  временной (`UNCLASSIFIED_FAILURE`). В базу пишется только код, текст
  ошибки — нет (может содержать текст сообщения, §64).
- **Паника обработчика** не перехватывается: процесс завершается, строка
  остаётся `PROCESSING` и становится доступной после истечения аренды.
- Идемпотентность побочных эффектов — на обработчике: `job.id` или доменный
  ключ в `UNIQUE`-ограничении; нельзя считать, что побочный эффект и
  подтверждение задания образуют одну транзакцию.

### Реестр заданий

| Тип | Кто ставит (ключ дедупликации) | Обработчик |
|---|---|---|
| `connector.normalize-raw-event.v1` | обработчик `connector.raw-event.received.v1` (`raw-event:{id}`) | нормализация сырого события в переписку |
| `opportunity.evaluate-commercial-candidate.v1` | обработчик `conversation.changed.v1` (`conversation:{id}:revision:{n}`) | поиск коммерческого кандидата по каталогу |
| `risk.refresh-no-response-plan.v1` | обработчики `conversation.changed.v1` (`conversation:{id}:revision:{n}`), `opportunity.created/stage_changed.v1` (`event:{id}`), `ai.analysis.applied.v1` (`ai-run:{id}`) | пересчёт планов **всех пяти** правил риска |
| `risk.evaluate-no-response.v1` | проверка `NO_RESPONSE_DUE` | правило R1 |
| `risk.evaluate-booking-not-confirmed.v1` | `BOOKING_NOT_CONFIRMED_DUE` | правило R3 |
| `risk.evaluate-promise-not-fulfilled.v1` | `PROMISE_NOT_FULFILLED_DUE` | правило R4 |
| `risk.evaluate-customer-silent-after-price.v1` | `CUSTOMER_SILENT_AFTER_PRICE_DUE` | правило R2 (две проверки: порог и эскалация) |
| `risk.evaluate-follow-up-candidate.v1` | `FOLLOW_UP_CANDIDATE_DUE` | правило R5 |
| `notification.digest.v1` | `NOTIFICATION_DIGEST_DUE` (`digest:user:{user}:{slot}`) | сводка уведомлений получателя |
| `notification.escalate.v1` | `NOTIFICATION_ESCALATION_DUE` (`risk:{id}:escalation`) | эскалация владельцу |

Payload заданий риска содержит только идентификаторы (`conversationId` либо
`opportunityId`), состояние перечитывается при выполнении.

## 3. Отложенные проверки и планировщик

- Проверка (`scheduled_checks`): тип, субъект, тип и payload будущего
  задания, срок, ключ дедупликации `UNIQUE (tenant_id, check_type,
  dedup_key)`. Повторное планирование того же основания с другим сроком —
  конфликт (`ErrConflict`); модуль уведомлений его поглощает (слот уже
  запланирован).
- **Планировщик** (`cmd/scheduler`, роль `lidradar_platform`): `PromoteDue`
  одной транзакцией берёт до 100 просроченных проверок (`FOR UPDATE SKIP
  LOCKED`, порядок `due_at, created_at, id`), создаёт задания с **тем же
  `id`**, `available_at = due_at`, приоритетом 0 и `max_attempts 5` (`ON
  CONFLICT (tenant, type, dedup_key)` при совпадающем payload — повторно
  использует существующее), переводит проверки в `ENQUEUED`. Цикл: пачка
  заполнена → сразу следующая, иначе пауза 500 мс; ошибка → пауза 1 с.

| Тип проверки | Субъект | Задание | Ключ дедупликации |
|---|---|---|---|
| `NO_RESPONSE_DUE` | сделка | `risk.evaluate-no-response.v1` | `opportunity:{id}:message:{msg}:policy:no-response/v1` |
| `BOOKING_NOT_CONFIRMED_DUE` | сделка | `risk.evaluate-booking-not-confirmed.v1` | `…:policy:booking-not-confirmed/v1` |
| `PROMISE_NOT_FULFILLED_DUE` | сделка | `risk.evaluate-promise-not-fulfilled.v1` | `…:policy:promise-not-fulfilled/v1` |
| `CUSTOMER_SILENT_AFTER_PRICE_DUE` | сделка | `risk.evaluate-customer-silent-after-price.v1` | `…:policy:customer-silent-after-price/v1` и `…:escalation` |
| `FOLLOW_UP_CANDIDATE_DUE` | сделка | `risk.evaluate-follow-up-candidate.v1` | `…:policy:follow-up-candidate/v1` |
| `NOTIFICATION_DIGEST_DUE` | пользователь | `notification.digest.v1` | `digest:user:{user}:{slot}` |
| `NOTIFICATION_ESCALATION_DUE` | риск | `notification.escalate.v1` | `risk:{id}:escalation` |

Срок проверок риска вычисляется в рабочем времени точки (часовой пояс и
семь строк расписания); проверка хранит только идентификаторы, поэтому
опоздавшее задание не применяет устаревший снимок.

## 4. Процесс `worker`

Один процесс — одна горутина, три очереди по кругу
(`backend/cmd/worker/main.go`):

| Параметр | Значение |
|---|---|
| пулы | `lidradar_worker` для обработчиков (RLS по организации задания), `lidradar_platform` для захвата заданий, событий и доставок |
| владелец аренд | новый UUIDv7 на запуск с суффиксами `:outbox`, `:jobs`, `:notifications` (после перезапуска старые аренды подхватываются по сроку) |
| размер пачки | 1 событие, 1 задание, 1 доставка за итерацию |
| аренда | 30 с везде |
| пауза | 500 мс без работы, 1 с после ошибки, иначе без паузы |
| диагностика | раз в минуту события `background.queue.status` (`jobs_pending/processing/retry/dead`, `jobs_expired_leases`, `scheduled_checks_overdue`) и `notification.queue.status` (`deliveries_*`) — без содержимого заданий |
| Telegram | транспорт включается только при `LIDRADAR_TELEGRAM_TOKEN` (прежнее имя `LIDAR_TELEGRAM_TOKEN` принимается); иначе предупреждение `notification.telegram.disabled`, доставки `TELEGRAM` не захватываются |

Зарегистрированные обработчики событий:

| Событие | Цепочка обработчиков (по порядку) |
|---|---|
| `connector.raw-event.received.v1` | постановка задания нормализации |
| `conversation.changed.v1` | кандидат сделки → постановка AI-анализа → пересчёт планов риска |
| `ai.analysis.applied.v1` | переходы этапов сделки по фактам → пересчёт планов риска |
| `opportunity.created.v1`, `opportunity.stage_changed.v1` | пересчёт планов риска |
| `risk.opened.v1` | политика уведомлений и постановка доставок |

Масштабирование — числом процессов; безопасность параллелизма даёт `FOR UPDATE
SKIP LOCKED` и уникальные владельцы аренд. Измерено: один worker — 171
задание/с, четыре — 480 (см. [10-operations.md](10-operations.md)).

## 5. Ключи идемпотентности по цепочке

| Шаг | Что защищает |
|---|---|
| сырое событие | `UNIQUE (connection_id, external_event_id)` + сравнение хеша тела |
| событие outbox | `id` события; для заданий обработчик использует `dedup_key` |
| каноническое сообщение | `UNIQUE (connection_id, external_id)` у переписки и сообщения; повтор идентичного изменения не меняет ревизию |
| кандидат сделки | `conversation:{id}:revision:{n}` + частичный индекс одной активной сделки |
| риск | частичный индекс одного активного риска на сделку и тип; `UpsertActive` с повтором до трёх раз |
| уведомление | `UNIQUE (tenant_id, dedup_key)` с персональным ключом; сводка — один элемент на риск и получателя |
| команды пользователя | `idempotency_keys` (`corrective`, `revenue`) |
| AI | одно ожидающее задание на переписку; уникальный снимок; один активный прогон |
| кнопки Telegram | `callback_query.id` в `telegram_callback_commands` |

## 6. Доставка уведомлений

- `DispatchOne` (worker) захватывает одну доставку в `PENDING` с наступившим
  `available_at` или `PROCESSING` с истёкшей арендой (`attempt < 5`), аренда
  30 с; `IN_APP` завершается локально (`provider_message_id = in-app:{id}`);
  `TELEGRAM` — `sendMessage` (таймаут клиента 10 с) с кнопками для
  `RISK_OPENED`/`RISK_ESCALATED`.
- Повтор — **новая строка** попытки с `attempt + 1` и `available_at = now +
  5 с / 30 с / 2 мин / 10 мин`, текущая помечается `RETRY`
  (`TELEGRAM_PROVIDER_ERROR`); после пятой — `DEAD`. Транспортная ошибка,
  `429` и `5xx` — повторяемые; прочие `4xx` и `ok:false` — нет.
- Истёкшие аренды с пятой попыткой помечаются `DEAD`
  (`LEASE_EXPIRED_MAX_ATTEMPTS`).
- Ни один отказ доставки не меняет риск и не создаёт второе логическое
  уведомление.

## 7. Сквозные цепочки

### 7.1. Вебхук → переписка

```text
POST /api/v1/webhooks/{provider}/{tenant}/{connection}
  RawEvent + outbox(connector.raw-event.received.v1)  — одна транзакция, 202
  → worker: job connector.normalize-raw-event.v1 (raw-event:{id})
  → NormalizationService.Process
      служебное обновление Telegram → ControlSink (/start, кнопки) → PROCESSED
      иначе NormalizeEvent → IngestCanonical (одна транзакция на событие)
        → при фактическом изменении outbox(conversation.changed.v1)
  → raw_events.status = PROCESSED
```

### 7.2. Переписка → сделка → риск → уведомление

```text
conversation.changed.v1
  ├─ job opportunity.evaluate-commercial-candidate.v1 → Opportunity(NEW) → outbox(opportunity.created.v1)
  ├─ ai_jobs (одно ожидающее, дебаунс 60 с) → узел → outbox(ai.analysis.applied.v1)
  └─ job risk.refresh-no-response-plan.v1 → scheduled_checks *_DUE (срок в рабочем времени)
opportunity.created.v1 / stage_changed.v1 → job risk.refresh-no-response-plan.v1
ai.analysis.applied.v1 → этапы сделки по фактам → job risk.refresh-no-response-plan.v1
scheduler: *_DUE → job risk.evaluate-*.v1 → правило перечитывает состояние
  → risk_signals (один активный на сделку и тип) + outbox(risk.opened.v1) + pg_notify(risk.changed)
risk.opened.v1 → для каждого активного участника: политика → Notification + Delivery
  либо элемент сводки + NOTIFICATION_DIGEST_DUE; при флаге — NOTIFICATION_ESCALATION_DUE
worker: DispatchOne → Telegram / IN_APP
```

### 7.3. Реакция человека

`POST …/actions` переводит риск в `ACTED` и публикует `risk.changed`;
`POST …/outcomes` фиксирует исход; `POST …/revenue` с `RECOVERED` замыкает
денежную петлю и публикует `risk.changed`; ответ бизнеса в переписке через
цепочку 7.2 закрывает `NO_RESPONSE` (`risk.resolved`).

## 8. Сигналы инвалидации (SSE)

- Публикация: `PostgresInvalidator.Publish` → `pg_notify('lidradar_risk_invalidations',
  {tenantId, type, resourceId})` с таймаутом 2 с, вне транзакции предметных
  данных (потеря сигнала допустима — источник истины остаётся в базе).
- Приём: `cmd/api` держит отдельное соединение `LISTEN`, при разрыве
  переподключается через 1 с; валидные сигналы (`risk.changed`,
  `risk.acknowledged`, `risk.resolved`, `risk.false_positive`, UUID
  организации и ресурса) уходят в
  `Hub` и далее подписчикам `GET /api/v1/events` (буфер 16, heartbeat 20 с).
- После вердикта о ложном срабатывании модуль обратной связи публикует
  `risk.false_positive` (ADR 0038): клиент перечитывает Radar так же, как
  после закрытия риска.

## 9. Мёртвые элементы и вмешательство администратора

`GET /api/v1/admin/dead-letters` показывает `DEAD` без `discarded_at` по
четырём очередям (`jobs`, `outbox_events`, `ai_jobs`,
`notification_deliveries`). `retry`/`replay` возвращают элемент в `PENDING`
с нулём попыток и снятой арендой (для AI-заданий — с учётом единственности
ожидающего задания), `discard` лишь скрывает элемент, статус остаётся `DEAD`.
Каждая команда пишет `admin_audit_log` в той же транзакции. Повтора доставки
уведомления в API нет — только откладывание.

Типичные постоянные ошибки в панели: `RISK_PLAN_INVALID` /
`RISK_EVALUATION_INVALID` (у точки нет семи строк расписания или у переписки
нет значимого сообщения), `NORMALIZATION_INVALID` (payload не разобран),
`COMMERCIAL_CANDIDATE_INVALID`, `INVALID_OUTBOX_PAYLOAD`,
`UNSUPPORTED_JOB_TYPE` / `UNSUPPORTED_EVENT_TYPE` (обработчик не
зарегистрирован в этой сборке worker).

## 10. Сводка временных параметров

| Параметр | Значение |
|---|---|
| аренда события / задания / доставки | 30 с |
| попыток | 5 (события, задания, доставки, AI-задания) |
| расписание повторов | 5 с, 30 с, 2 мин, 10 мин |
| пауза worker без работы / после ошибки | 500 мс / 1 с |
| пачка планировщика | 100 (максимум домена) |
| дебаунс AI-анализа | 60 с |
| аренда AI-задания / потолок | 120 с / 15 мин |
| heartbeat SSE | 20 с |
| переподключение LISTEN | 1 с |
| таймаут Telegram Bot API | 10 с |
