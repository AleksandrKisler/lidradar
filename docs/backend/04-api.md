# HTTP API

Нормативный контракт — `contracts/openapi/openapi.yaml` (OpenAPI 3.1, все
пути подключены к рабочему `cmd/api`; проверяется Redocly в CI). Этот документ
описывает соглашения, которых контракт не показывает, и даёт каталог
конечных точек с правами и кодами.

## 1. Соглашения

### 1.1. Аутентификация и выбор организации

- **Браузерная сессия** — cookie `lidradar_session` (`HttpOnly`,
  `SameSite=Strict`, `Path=/`, `Secure` при `LIDRADAR_COOKIE_SECURE`,
  `Max-Age` = `LIDRADAR_SESSION_TTL`, по умолчанию 30 суток). Значение —
  непрозрачный токен, сервер хранит только его SHA-256. Заголовок
  `Authorization` для пользовательского API не используется.
- **Организация** выбирается заголовком `X-Tenant-ID` (UUID из
  `GET /api/v1/auth/me`). Без него маршруты организации отвечают
  `400 TENANT_REQUIRED`; невалидный UUID → `400 INVALID_TENANT` ещё в
  middleware. Тот же заголовок задаёт контекст RLS для запроса.
- Не требуют `X-Tenant-ID`: `/health/*`, `/api/v1/auth/*`,
  `POST /api/v1/organizations`, `/api/v1/admin/*`, вебхуки, API AI-узла.
- **Права** проверяются в прикладном слое по роли членства (OWNER —
  все; MANAGER — `risks.read`, `risks.manage`, `conversation.read`,
  `opportunity.manage`, `action.manage`, `outcome.manage`, `revenue.confirm`).
  Чужой `X-Tenant-ID` → `403 FORBIDDEN`; чужой идентификатор ресурса внутри
  своей организации → `404 NOT_FOUND` без раскрытия данных.
- **API AI-узла** (`/internal/v1/ai/*`) — `Authorization: Bearer <секрет узла>`
  плюс подписанные заголовки `X-LidRadar-*` (см. [07-ai.md](07-ai.md)).
- **Вебхуки** — без сессии; секрет подключения в заголовке провайдера.

### 1.2. Тела запросов и ответов

- JSON, `Content-Type: application/json`. Разбор строгий: тело ≤ 64 КиБ
  (вебхуки ≤ 1 МиБ, API узла ≤ 1 МиБ), неизвестные поля и второе
  JSON-значение в теле отвергаются, строки с `\x00` отвергаются, тело обязано
  быть валидным UTF-8 → иначе `400 INVALID_ARGUMENT`.
- Деньги — **строки** с двумя знаками (`"1200.00"`), никогда не числа;
  числовое значение цены отвергается. Уверенность (`confidence`) — число.
- Время — RFC 3339 в UTC; даты аналитики — `YYYY-MM-DD` в часовом поясе
  организации.
- Идентификаторы — UUID (UUIDv7 у новых записей).
- Списки всегда отдают `items: []`, а не `null`; `nextCursor` присутствует
  только при наличии следующей страницы.
- Идентификатор организации в ответах не возвращается.

### 1.3. Ошибки

Единый конверт:

```json
{"error":{"code":"INVALID_ARGUMENT","message":"Invalid request","details":{},"traceId":"<32 hex>"}}
```

| HTTP | code | Когда |
|---|---|---|
| 400 | `INVALID_ARGUMENT` | неверное тело, параметр, переход |
| 400 | `TENANT_REQUIRED` | нет `X-Tenant-ID` на маршруте организации |
| 400 | `INVALID_TENANT` | `X-Tenant-ID` не UUID |
| 401 | `UNAUTHENTICATED` | нет или истекла сессия; узел не прошёл подпись |
| 401 | `INVALID_CREDENTIALS` | вход: неверные данные, нет пользователя, `DISABLED` |
| 401 | `WEBHOOK_UNAUTHENTICATED` | секрет вебхука не совпал |
| 403 | `FORBIDDEN` | нет права, нет членства, не платформенный администратор |
| 403 | `ORIGIN_NOT_ALLOWED` | мутация с недоверенного `Origin` |
| 404 | `NOT_FOUND` | ресурс не найден в организации |
| 404 | `ROUTE_NOT_FOUND` | нет такого маршрута |
| 405 | `METHOD_NOT_ALLOWED` | |
| 409 | `CONFLICT` | конфликт состояния (дубликат email-подключения, неподходящий статус очереди, чужая переписка у контакта) |
| 409 | `EMAIL_ALREADY_REGISTERED` | регистрация |
| 409 | `INVALID_STAGE_TRANSITION` | недопустимый переход этапа сделки |
| 409 | `IDEMPOTENCY_CONFLICT` | тот же `Idempotency-Key` с другим содержимым |
| 409 | `RECOVERED_ALREADY_ATTRIBUTED` | вторая атрибуция `RECOVERED` на сделку |
| 409 | `LEASE_LOST` | API узла: аренда задания потеряна |
| 413 | `PAYLOAD_TOO_LARGE` | вебхук больше 1 МиБ |
| 429 | `RATE_LIMITED` | превышен предел по адресу или по учётной записи; есть `Retry-After` |
| 503 | `SERVICE_NOT_READY` | `/health/ready`: база или миграции не совпали |
| 503 | `CONNECTOR_UNAVAILABLE` | Telegram не настроен или недоступен, подключение отключено |
| 500 | `INTERNAL_ERROR` / `INTERNAL` | необработанная ошибка; `INTERNAL` отдают модули risk, corrective, revenue, analytics и API узла, остальные — `INTERNAL_ERROR` |

`message` никогда не содержит секретов, текста сообщений и деталей исключений.

### 1.4. Идемпотентность и повторы

- Команды, создающие неизменяемые факты (`actions`, `outcomes`, `revenue`),
  требуют заголовок `Idempotency-Key` (1…255 символов). Точный повтор
  возвращает сохранённый ответ с `200`, первая запись — `201`, тот же ключ с
  другим содержимым — `409 IDEMPOTENCY_CONFLICT`. Ключ живёт в
  `idempotency_keys` бессрочно.
- Команды состояния (`acknowledge`, `resolve`, `PATCH stage`, `DELETE`
  услуги, повтор ML-согласия) идемпотентны по семантике и отвечают `200`/`204`
  при повторе.
- Вебхук повторяется провайдером сколько угодно: тот же внешний
  идентификатор с тем же телом — `202` и `duplicate: true`, с другим телом —
  `409 CONFLICT`.

### 1.5. Пагинация

Курсорная. `limit` 1…100 (по умолчанию 50), `cursor` — непрозрачная строка
`base64url`. Переписки сортируются по `updated_at DESC, id DESC`, сообщения —
по `sent_at DESC, id DESC`, риски — серверным порядком Radar; курсор рисков
привязан к набору фильтров и с другими фильтрами отвергается. Нечисловой
`limit` → `400`.

### 1.6. Корреляция, заголовки безопасности, ограничение частоты

- Клиент может передать `X-Request-ID` (`^[A-Za-z0-9._:-]{1,128}$`) и
  `Traceparent`; ответ всегда несёт `X-Request-ID`, `traceId` попадает в
  конверт ошибки и в логи.
- Каждый ответ, включая ошибки и `/health`, несёт `X-Content-Type-Options:
  nosniff`, `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`,
  `Cache-Control: no-store`, `Content-Security-Policy: default-src 'none';
  frame-ancestors 'none'`, `Permissions-Policy: camera=(), microphone=(),
  geolocation=()`; HSTS при `LIDRADAR_COOKIE_SECURE=true` или TLS.
- Мутации (все методы, кроме `GET`/`HEAD`/`OPTIONS`) с заголовком `Origin`
  принимаются только с того же origin или из `LIDRADAR_ALLOWED_ORIGINS`;
  иначе `403 ORIGIN_NOT_ALLOWED`. Это защита от CSRF в дополнение к
  `SameSite=Strict`.
- Ограничение по адресу соединения (заголовки прокси не читаются):
  `/api/v1/auth/*` — 120 запросов в минуту, `/api/v1/webhooks/*` — 1200;
  ответ `429` с `Retry-After`. Отдельно вход ограничивается по учётной записи
  (5 неудач за 15 минут) и по адресу (20 в минуту) в PostgreSQL.

### 1.7. SSE

`GET /api/v1/events` (`text/event-stream`, право `risks.read`) — только
сигнал «перечитай REST» (ADR 0028). События `risk.changed`,
`risk.acknowledged`, `risk.resolved` с телом `{"resourceId":"<uuid риска>"}`,
комментарий-heartbeat каждые 20 с, без `id:`/`retry:` и `Last-Event-ID`:
после разрыва клиент переподключается и перечитывает `GET /api/v1/radar`
и списки. Буфер подписчика 16 сигналов, переполнение сбрасывает сигнал.

## 2. Каталог конечных точек

Обозначения: 🔓 — без сессии; 🍪 — сессия; 🍪+T — сессия и `X-Tenant-ID`;
право — из набора членства; A — `PLATFORM_ADMIN`.

### 2.1. Служебные

| Метод | Путь | Доступ | Ответ |
|---|---|---|---|
| GET | `/health/live` | 🔓 | `200 {"status":"ok","service":"lidradar-api"}` |
| GET | `/health/ready` | 🔓 | `200 {"status":"ready","build":{version,revision,modified},"migrations":{applied,latest}}`; `503 SERVICE_NOT_READY`, если база недоступна или журнал миграций не совпал со сборкой |

### 2.2. Аутентификация (`/api/v1/auth`)

| Метод | Путь | Доступ | Тело → ответ | Особые коды |
|---|---|---|---|---|
| POST | `/register` | 🔓 | `{email,password,displayName}` → `201 {"user"}` + cookie | `409 EMAIL_ALREADY_REGISTERED`, `429` |
| POST | `/login` | 🔓 | `{email,password}` → `200 {"user"}` + cookie | `401 INVALID_CREDENTIALS`, `429` |
| POST | `/logout` | 🍪 (необязательно) | → `204`, cookie стирается | |
| POST | `/refresh` | 🍪 | → `200 {"user"}` + новая cookie (старая отзывается) | `401`, `429` |
| GET | `/me` | 🍪 | → `200 {"user","memberships":[{tenantId,organizationName,role}]}` только активные членства активных организаций | `401` |

### 2.3. Организация, точки, ML-согласие

| Метод | Путь | Доступ / право | Ответ |
|---|---|---|---|
| POST | `/api/v1/organizations` | 🍪 | `{name,defaultTimezone,defaultCurrency?}` → `201 Organization`; создаёт OWNER-членство |
| GET | `/api/v1/organization` | 🍪+T, членство | `200 Organization` |
| PATCH | `/api/v1/organization` | 🍪+T, `organization.manage` | частичное обновление имени, зоны, валюты → `200` |
| GET | `/api/v1/locations` | 🍪+T, членство | `200 {"items":[Location]}` |
| POST | `/api/v1/locations` | 🍪+T, `location.manage` | `{name,timezone,responseThresholdMinutes?,active?}` → `201 Location` |
| PATCH | `/api/v1/locations/{locationId}` | 🍪+T, `location.manage` | `200 Location` |
| PUT | `/api/v1/locations/{locationId}/business-hours` | 🍪+T, `location.manage` | `{timezone,days:[7×{weekday,closed,opensAt?,closesAt?}]}` → `200 Location` |
| GET | `/api/v1/organization/ml-consent` | 🍪+T, членство | `200 {"scope":"DATASETS","active":bool,"consent":obj\|null}` |
| POST | `/api/v1/organization/ml-consent` | 🍪+T, `organization.manage` | `201` при выдаче, `200` при повторе |
| DELETE | `/api/v1/organization/ml-consent` | 🍪+T, `organization.manage` | `204` (и без действующего согласия) |

### 2.4. Каталог услуг (`/api/v1/services`)

| Метод | Путь | Право | Ответ |
|---|---|---|---|
| GET | `/` | `service.manage` | `200 {"items":[ServiceCatalogItem]}` (порядок: активные, затем по нормализованному имени) |
| POST | `/` | `service.manage` | `{name,locationId?,priceFrom?,priceTo?,currency?}` → `201`; чужая точка → `404` |
| PATCH | `/{serviceId}` | `service.manage` | поля опциональны, `null` сбрасывает `locationId`/цены → `200` |
| DELETE | `/{serviceId}` | `service.manage` | деактивация → `204`, повтор тоже `204` |

### 2.5. Каналы и вебхуки

| Метод | Путь | Доступ / право | Ответ |
|---|---|---|---|
| GET | `/api/v1/integrations` | 🍪+T, `integration.manage` | `200 {"items":[ChannelConnection]}` без хеша секрета и реквизитов |
| POST | `/api/v1/integrations/{provider}/connect` | 🍪+T, `integration.manage` | `{name,locationId?,webhookSecret,botToken?}` → `201 ChannelConnection`; Telegram без `LIDRADAR_PUBLIC_BASE_URL`/ключа шифрования → `503 CONNECTOR_UNAVAILABLE`; ошибка Bot API → `201` со статусом `ERROR/TELEGRAM_WEBHOOK_SETUP_FAILED` |
| DELETE | `/api/v1/integrations/{connectionId}` | 🍪+T, `integration.manage` | `204`; удаляет webhook у Telegram после локального отключения |
| GET | `/api/v1/integrations/{connectionId}/health` | 🍪+T, `integration.manage` | `200 ConnectionHealth` |
| POST | `/api/v1/webhooks/{provider}/{tenantId}/{connectionId}` | 🔓 + секрет | `202 {"rawEventId","status":"RECEIVED"\|"FAILED","duplicate"}`; `401 WEBHOOK_UNAUTHENTICATED`, `404` (нет подключения или провайдер не совпал), `409` (тот же внешний id, другое тело), `413`, `503` (подключение `DISCONNECTED`) |

Контракт вебхука по провайдерам:

| Провайдер | Заголовок секрета | Тело | Внешний идентификатор |
|---|---|---|---|
| `TEST`, `IMPORT`, `GENERIC_WEBHOOK` | `X-LidRadar-Webhook-Secret` | конверт `{"id","type","occurredAt","data":{conversationExternalId,messageExternalId,contactExternalId?,contactDisplayName?,direction,messageType,text?,sentAt,replyToMessageExternalId?,attachments,metadata}}`, `type` ∈ `message.received.v1`/`message.edited.v1`/`message.deleted.v1` | `id` конверта (≤ 512) |
| `CONNECTED_BUSINESS_BOT` | `X-Telegram-Bot-Api-Secret-Token` | Telegram `Update` ровно с одним из `business_connection`, `business_message`, `edited_business_message`, `deleted_business_messages`, `message`, `callback_query` | `update_id` |

Проверка секрета — сравнение SHA-256 в константное время; при провале в базу
ничего не пишется. Невалидный, но аутентифицированный payload сохраняется
как `FAILED` с `INVALID_PAYLOAD` и отвечает `202`.

### 2.6. Переписки (`/api/v1/conversations`, право `conversation.read`)

| Метод | Путь | Ответ |
|---|---|---|
| GET | `/` | `{"items":[Conversation],"nextCursor"}`; `limit`, `cursor` |
| GET | `/{conversationId}` | `{"conversation","contact"}` |
| GET | `/{conversationId}/messages` | `{"items":[{"message","attachments":[]}],"nextCursor"}`; удалённые сообщения остаются с `providerDeletedAt` |

### 2.7. Сделки (`/api/v1/opportunities`)

| Метод | Путь | Право | Ответ |
|---|---|---|---|
| GET | `/{opportunityId}` | `opportunity.manage` | `{"opportunity","stageHistory":[]}` |
| PATCH | `/{opportunityId}` | `opportunity.manage` | `{"stage"}` → `200 Opportunity` (и при повторе того же этапа); недопустимый переход → `409 INVALID_STAGE_TRANSITION` |
| POST | `/{opportunityId}/outcomes` | `risks.manage`, `Idempotency-Key` | `{"status","note?"}` → `201`/`200 Outcome` |
| POST | `/{opportunityId}/revenue` | `revenue.confirm`, `Idempotency-Key` | `{"amount","currency","attributionType","riskId?","actionId?","outcomeId?"}` → `201`/`200 {"revenue","attribution"}`; `409 RECOVERED_ALREADY_ATTRIBUTED`; нарушение окна 30 дней или чужая цепочка → `400` |

### 2.8. Radar и риски

| Метод | Путь | Право | Ответ |
|---|---|---|---|
| GET | `/api/v1/radar` | `risks.read` | `{"openRisks","criticalRisks","potentialRevenue","confirmedRecoveredRevenue"}`; фильтры `locationId`, `severity`, `riskType` |
| GET | `/api/v1/risks` | `risks.read` | `{"items":[RiskDetail],"nextCursor"}`; фильтры `status`, `locationId`, `severity`, `riskType`, `limit`, `cursor` |
| GET | `/api/v1/risks/{riskId}` | `risks.read` | `RiskDetail` с рекомендацией, действиями, последним исходом |
| POST | `/api/v1/risks/{riskId}/acknowledge` | `risks.manage` | `200 Risk`, идемпотентно |
| POST | `/api/v1/risks/{riskId}/resolve` | `risks.manage` | `200 Risk`, идемпотентно |
| POST | `/api/v1/risks/{riskId}/recommendation` | `risks.manage` | `200 Recommendation` (создать или вернуть существующую) |
| POST | `/api/v1/risks/{riskId}/actions` | `risks.manage`, `Idempotency-Key` | `{"type","note?"}` → `201`/`200 Action`; риск → `ACTED` |
| POST | `/api/v1/risks/{riskId}/feedback` | `risks.manage` | `{"verdict","reason?","note?"}` → `201 Feedback` |
| GET | `/api/v1/risks/precision` | `analytics.read` | `PrecisionReport` по пяти типам; `from`, `to` (RFC 3339, `from < to`) |
| GET | `/api/v1/events` | `risks.read` | SSE (см. § 1.7) |

### 2.9. Выручка

| Метод | Путь | Право | Ответ |
|---|---|---|---|
| GET | `/api/v1/revenue/confirmed-recovered?currency=RUB` | `revenue.read` | `{"amount":"12345.00","currency":"RUB"}` — только `CONFIRMED` + `RECOVERED` |

### 2.10. Уведомления (`/api/v1/notifications`, активное членство)

| Метод | Путь | Ответ |
|---|---|---|
| POST | `/telegram-link-token` | `201 {"startUrl":"https://t.me/<bot>?start=…","expiresAt"}`; код одноразовый, 15 минут |
| GET | `/telegram-link` | `200 {"linked","linkedAt?"}` |
| DELETE | `/telegram-link` | `204` |
| GET | `/preferences` | `200 {"items":[5 × Preference]}` с `isDefault`, `timezone` |
| PUT | `/preferences/{riskType}` | полная замена `{deliveryMode,minimumSeverity,inAppEnabled,telegramEnabled,quietHoursEnabled,quietHoursStart?,quietHoursEnd?,digestTime}` → `200` |
| DELETE | `/preferences/{riskType}` | сброс к умолчанию → `204` |

### 2.11. Аналитика

| Метод | Путь | Право | Ответ |
|---|---|---|---|
| GET | `/api/v1/analytics/summary?from=YYYY-MM-DD&to=YYYY-MM-DD` | `analytics.read` | `{"period","messages","opportunities","risks","outcomes","revenue"}`; период по умолчанию 30 дней, максимум 366 |

### 2.12. Администрирование (`/api/v1/admin`, 🍪, без `X-Tenant-ID`)

| Метод | Путь | Доступ | Назначение |
|---|---|---|---|
| GET | `/me` | 🍪 | `{"userId","platformAdmin"}` |
| GET / POST | `/admins` | A | история выдач; `{"email","note?"}` → `201`/`200` |
| DELETE | `/admins/{userId}` | A | отзыв → `204` |
| GET | `/organizations`, `/connections`, `/queue`, `/jobs`, `/dead-letters` | A | read-модели; фильтры `tenantId`, `status`, `type`, `limit` (≤ 200) |
| POST | `/jobs/{jobId}/retry`, `/jobs/{jobId}/discard` | A | только `DEAD`; иначе `409 CONFLICT` |
| POST | `/outbox/{eventId}/replay`, `/outbox/{eventId}/discard` | A | то же для outbox |
| POST | `/ai/jobs/{jobId}/retry`, `/ai/jobs/{jobId}/discard` | A | то же для AI-заданий |
| POST | `/notifications/deliveries/{deliveryId}/discard` | A | откладывание мёртвой доставки |
| GET | `/ai/nodes`, `/ai/runs` | A | узлы с допусками; прогоны без сырого вывода (`tenantId`, `status`, `applicationStatus`, `limit`) |
| GET | `/ai/tenants/{tenantId}/conversations/{conversationId}/summary` | A | факты с признаком доверия, без текста резюме |
| GET | `/usage?from&to` | A | потребление по организациям за окно UTC |
| GET | `/trace/tenants/{tenantId}/messages/{messageId}` | A | цепочка от сообщения до выручки без текстов |

### 2.13. API AI-узла (`/internal/v1/ai`, только `POST`, подпись)

`/nodes/heartbeat`, `/jobs/claim`, `/jobs/{jobId}/started`,
`/jobs/{jobId}/complete`, `/jobs/{jobId}/failed` — контракт, заголовки и коды
описаны в [07-ai.md](07-ai.md) § 5.

## 3. Типичные сценарии для фронтенда

1. **Вход и выбор организации:** `POST /auth/login` → `GET /auth/me` →
   выбрать `tenantId` → слать `X-Tenant-ID` со всеми запросами.
2. **Экран Radar:** `GET /radar` + `GET /risks?limit=20`; открыть SSE
   `/events`; по любому событию перечитать оба запроса.
3. **Работа с риском:** `GET /risks/{id}` → `POST …/recommendation` → `POST
   …/actions` с `Idempotency-Key` (UUID на попытку пользователя) → `POST
   /opportunities/{id}/outcomes` → при оплате `POST /opportunities/{id}/revenue`
   с `attributionType: RECOVERED` и тремя идентификаторами цепочки.
4. **Подключение канала:** OWNER → `POST /integrations/GENERIC_WEBHOOK/connect`
   с собственным секретом; для Telegram — через безопасный помощник
   `scripts/telegram-connect-safe.sh` (токен не должен проходить через
   браузер и логи).
5. **Личные уведомления:** `POST /notifications/telegram-link-token` →
   открыть `startUrl` в Telegram → `GET /notifications/telegram-link` →
   настроить `PUT /notifications/preferences/{riskType}`.
