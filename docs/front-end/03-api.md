# HTTP API для браузера

Каталог описывает все HTTP-операции, доступные текущему web-клиенту, и
отделяет их от webhook/internal API. Формы моделей раскрыты в
[02-entities.md](02-entities.md), machine-readable контракт — в
[OpenAPI](../../contracts/openapi/openapi.yaml), серверная семантика — в
[backend API](../backend/04-api.md).

<a id="transport-contract"></a>
## 1. Общий transport-контракт

- Base path: `/api/v1`; один origin с приложением в production.
- Все запросы кроме `auth/register` и `auth/login` используют opaque cookie
  `lidradar_session` и `credentials: 'include'`.
- Все tenant-scoped операции отправляют `X-Tenant-ID: <membership.tenantId>`;
  Risk/Radar/SSE пути объявляют его в OpenAPI (GAP-CONTRACT-001 закрыт),
  единственная сеансовая операция без header — `POST /invitations/accept`.
- `Content-Type: application/json`; обычное тело не более 64 KiB, корректный
  UTF-8, unknown fields отклоняются.
- `Idempotency-Key` обязателен для Action, Outcome и Revenue; 1..255 символов.
- Optional `X-Request-ID` соответствует `^[A-Za-z0-9._:-]{1,128}$`;
  `Traceparent` передаётся только через существующую observability integration.
- Cursor, returned IDs и timestamps непрозрачны; URL path/query кодируются
  стандартным encoder, а не строковой конкатенацией.
- Frontend не отправляет секреты в query string и не логирует body auth/connect.
- `204` не парсится как JSON. `200`/`201` у идемпотентных команд оба являются
  успехом.
- Серверные ответы имеют `Cache-Control: no-store`; parsed data живёт только в
  ограниченном in-memory query cache и очищается при logout/switch.

Сессия защищена `SameSite=Strict`, а state-changing запросы дополнительно
проверяют допустимый Origin. `403 ORIGIN_NOT_ALLOWED` не является недостатком
роли и показывается как ошибка безопасной конфигурации.

<a id="error-model"></a>
## 2. Ошибки и реакция UI

Единый envelope:

```json
{
  "error": {
    "code": "INVALID_ARGUMENT",
    "message": "safe server message",
    "details": {},
    "traceId": "request trace id"
  }
}
```

| HTTP | Основные code | Поведение frontend |
|---:|---|---|
| 400 | `INVALID_ARGUMENT`, `TENANT_REQUIRED`, `INVALID_TENANT` | сохранить введённые данные; field mapping только по известным details/code |
| 401 | `UNAUTHENTICATED`, `INVALID_CREDENTIALS` | login: общая ошибка учётных данных; protected app: очистить session context и перейти на login |
| 403 | `FORBIDDEN`, `ORIGIN_NOT_ALLOWED` | нейтральный access/config state, без сведений о чужом объекте |
| 404 | `NOT_FOUND`, `ROUTE_NOT_FOUND` | resource-not-found; для tenant object одинаково с чужим ID |
| 405 | `METHOD_NOT_ALLOWED` | техническая ошибка клиента, не retry |
| 409 | `CONFLICT`, `EMAIL_ALREADY_REGISTERED`, `INVALID_STAGE_TRANSITION`, `IDEMPOTENCY_CONFLICT`, `RECOVERED_ALREADY_ATTRIBUTED`, `LAST_OWNER`, `MEMBER_DISABLED`, `INVITATION_EXPIRED`, `INVITATION_REVOKED`, `INVITATION_USED`, `ALREADY_MEMBER` | понятный conflict state; refetch; не повторять автоматически |
| 413 | `PAYLOAD_TOO_LARGE` | не повторять; предложить уменьшить ввод |
| 429 | `RATE_LIMITED` | уважать `Retry-After`, заблокировать submit на заданное число секунд |
| 503 | `SERVICE_NOT_READY`, `CONNECTOR_UNAVAILABLE`, `UNAVAILABLE` (SSE) | сохранить безопасный draft; retry только по явному действию; без SSE — периодический REST refetch |
| 5xx | `INTERNAL_ERROR` | error state с retry и `traceId`; не заменять пустыми данными |

Серверный `message` не вставляется напрямую в business copy. Неизвестный
`code` должен дать общий безопасный текст и сохранить `traceId`.

<a id="auth-api"></a>
## 3. Auth и сессия

| Метод и путь | Доступ | Request | Успех | Клиентский эффект |
|---|---|---|---|---|
| `POST /auth/register` | guest | `{email,password,displayName}` | `201 AuthResponse` + cookie | refetch `/auth/me`; перейти к tenant boot |
| `POST /auth/login` | guest | `{email,password}` | `200 AuthResponse` + cookie | refetch `/auth/me`; восстановить только валидный tenant |
| `POST /auth/logout` | session optional | — | `204` + expired cookie | очистить весь защищённый cache и persisted UI context |
| `POST /auth/refresh` | active session | — | `200 AuthResponse` + rotated cookie | заменить auth snapshot; это не recovery после 401 |
| `GET /auth/me` | session | — | `200 {user,memberships[]}` | источник auth/tenant/role guards |

Register: `400/403/409/429`; login: `400/401/403/429`; logout: `403`;
refresh: `401/403/429`; me: `401`. Ошибка login не должна раскрывать, существует
ли email. На logout локальная очистка выполняется и при сетевой неопределённости,
но UI сообщает, что серверное завершение сессии не подтверждено.

<a id="tenant-api"></a>
## 4. Организация, privacy, точки и график

В таблице `T` означает обязательный `X-Tenant-ID`.

| Метод и путь | Доступ | Request/query | Успех | Invalidation |
|---|---|---|---|---|
| `POST /organizations` | session | `CreateOrganizationRequest` | `201 Organization` | `auth/me`; выбрать новый tenant |
| `GET /organization` | T, member | — | `200 Organization` | — |
| `PATCH /organization` | T, OWNER | непустой `UpdateOrganizationRequest` | `200 Organization` | organization, auth/me, analytics при timezone/currency |
| `GET /organization/ml-consent` | T, member | — | `200 MLConsentStatus` | — |
| `POST /organization/ml-consent` | T, OWNER | — | `201` новое / `200` уже active | ml-consent |
| `DELETE /organization/ml-consent` | T, OWNER | — | `204` | ml-consent |
| `GET /organization/onboarding` | T, member | — | `200 OnboardingStatus` | — (перечитывать после каждого шага настройки) |
| `GET /organization/members` | T, OWNER (`member.manage`) | — | `200 {items: Member[]}` | — |
| `PATCH /organization/members/{userId}` | T, OWNER | `{role}` | `200 Membership` | members |
| `DELETE /organization/members/{userId}` | T, OWNER | — | `204`, идемпотентно | members |
| `GET /organization/invitations` | T, OWNER | — | `200 {items: Invitation[]}` | — |
| `POST /organization/invitations` | T, OWNER | `{role, note?}` | `201 {invitation, code}` — код показать один раз | invitations |
| `DELETE /organization/invitations/{invitationId}` | T, OWNER | — | `204`, идемпотентно | invitations |
| `POST /invitations/accept` | session, без T | `{code}` | `200 {membership: MembershipSummary}` | auth/me; предложить переключить tenant |
| `GET /locations` | T, member | — | `200 {items: Location[]}` | — |
| `POST /locations` | T, OWNER | `CreateLocationRequest` (без `active`: поле отклоняется `400`) | `201 Location` | locations, onboarding |
| `PATCH /locations/{locationId}` | T, OWNER | непустой `UpdateLocationRequest` | `200 Location` | locations |
| `PUT /locations/{locationId}/business-hours` | T, OWNER | `BusinessHoursRequest` с 7 днями | `200 Location` | locations |

Все tenant operations могут вернуть `400/401/403`; операции с конкретным
объектом — также `404`. `POST /organizations` tenant header не требует, потому
что создаёт новую границу. После create нельзя строить membership локально:
нужно повторить `/auth/me`.

Команда (ADR 0045): роль/отзыв — `409 LAST_OWNER` для последнего активного
владельца (включая себя), `409 MEMBER_DISABLED` при смене роли отозванного;
приём кода — `404` неизвестный код, `409 INVITATION_EXPIRED` /
`INVITATION_REVOKED` / `INVITATION_USED` / `ALREADY_MEMBER`. Код приглашения
(43 символа) UI показывает один раз с кнопкой копирования и нигде не хранит;
список приглашений кода не содержит. Onboarding: `complete`, `nextStep`
(`ORGANIZATION|LOCATION|SERVICES|CHANNEL|TELEGRAM_LINK|null`) и `steps[]` с
`required/done` — авторитетный resume без localStorage; `TELEGRAM_LINK`
необязателен и считается по текущему пользователю.

<a id="services-api"></a>
## 5. Каталог услуг

| Метод и путь | Доступ | Request | Успех | Invalidation |
|---|---|---|---|---|
| `GET /services` | T, OWNER (`service.manage`) | — | `200 {items[]}`; active прежде inactive | — |
| `POST /services` | T, OWNER | `CreateServiceCatalogItemRequest` | `201 ServiceCatalogItem` | services |
| `PATCH /services/{serviceId}` | T, OWNER | непустой `UpdateServiceCatalogItemRequest` | `200 ServiceCatalogItem` | services |
| `DELETE /services/{serviceId}` | T, OWNER | — | `204`, идемпотентный soft delete | services |

Ошибки: `400/401/403`, а create с чужим location и object operations — `404`.
Список включает inactive элементы; вкладка/фильтр формируется frontend-ом.
Manager не может читать этот endpoint; имя услуги для карточки риска приходит
в `RiskDetail.service` (GAP-API-012 закрыт), lookup по каталогу не нужен.

<a id="integrations-api"></a>
## 6. Интеграции каналов

| Метод и путь | Доступ | Request | Успех | Invalidation |
|---|---|---|---|---|
| `GET /integrations` | T, OWNER | — | `200 {items: ChannelConnection[]}` | — |
| `POST /integrations/{provider}/connect` | T, OWNER | `ConnectChannelRequest` (`webhookSecret?` — без него секрет выпускает сервер) | `201 ConnectedChannel` = `ChannelConnection` + `webhookSecret: string \| null` (показать один раз) | integrations, connection health, onboarding |
| `DELETE /integrations/{connectionId}` | T, OWNER | — | `204` soft disconnect | integrations, health, onboarding |
| `GET /integrations/{connectionId}/health` | T, OWNER | — | `200 ConnectionHealth` (persisted snapshot) | — |
| `POST /integrations/{connectionId}/health/check` | T, OWNER | — | `200 HealthCheck` = `{health, verification: REMOTE \| LOCAL}` | integrations, health |

Connect: `400/401/403/404/503`; disconnect:
`400/401/403/404/503`; list/health: `400/401/403`, health и check также
`404`, check — `503`. `503` после disconnect означает: local state уже
`DISCONNECTED`, но удаление удалённого webhook не подтверждено; поэтому нужно
refetch списка и показать warning, а не возвращать connection визуально в
active. `GET …/health` — persisted snapshot («состояние прочитано»); кнопка
«Проверить связь» вызывает `POST …/health/check`: `REMOTE` — Telegram
опрошен (`getWebhookInfo`), результат сохранён; `LOCAL` — у провайдера нет
удалённой регистрации или подключение отключено, показан сохранённый статус.
Bot token отправляется один раз в write-only поле и очищается из формы;
выпущенный сервером `webhookSecret` показывается один раз с кнопкой
копирования (для Telegram он `null`, потому что регистрируется сервером).

<a id="conversations-api"></a>
## 7. Переписки и сообщения

| Метод и путь | Доступ | Query | Успех | Порядок |
|---|---|---|---|---|
| `GET /conversations` | T, `conversation.read` | `search?` (≤100), `withRisk?=true`, `locationId?`, `connectionId?`, `status?`, `limit? 1..100=50`, `cursor?` | `ConversationPage` (`items: ConversationListItem[]`) | `updatedAt` descending |
| `GET /conversations/{conversationId}` | T, `conversation.read` | — | `ConversationDetail` | — |
| `GET /conversations/{conversationId}/messages` | T, `conversation.read` | `limit? 1..100=50`, `cursor?` | `MessagePage` | `sentAt` descending |

Все: `400/401/403`; object calls также `404`. `nextCursor=null` означает конец.
При смене фильтра/tenant старый cursor отбрасывается: cursor привязан к набору
фильтров и с другими фильтрами отвечает `400`. Строка списка уже содержит
контакт, канал, превью, `activeRisks` и `externalLink` (GAP-API-005 закрыт);
browser не выполняет запрос detail для каждой строки. `search` ищет по имени,
телефону (по цифрам) и почте контакта без учёта регистра на всём tenant
dataset; `withRisk=true` — только переписки с активным риском.

Endpoint отправки сообщений отсутствует намеренно: frontend read-only. Переход
во внешний канал — только по `externalLink.url` из ответа (сейчас
`tg://user?id=…` для Telegram); при `url=null` CTA disabled с текстом по
`unavailableReason` (`PROVIDER_UNSUPPORTED`, `IDENTITY_UNKNOWN`). Browser
никогда не строит URL из `externalId` (GAP-API-006 закрыт).

<a id="opportunity-api"></a>
## 8. Opportunity

| Метод и путь | Доступ | Request | Успех | Invalidation |
|---|---|---|---|---|
| `GET /opportunities/{opportunityId}` | T, `opportunity.manage` | — | `200 OpportunityDetail` | — |
| `PATCH /opportunities/{opportunityId}` | T, `opportunity.manage` | `{stage}` | `200 Opportunity` | opportunity, risk, risks, radar, analytics |

Ошибки: `400/401/403/404`; PATCH также `409 INVALID_STAGE_TRANSITION`.
Повтор текущего stage идемпотентен без Idempotency-Key. UI не выполняет
optimistic transition; после success запрашивает полную историю.

<a id="radar-api"></a>
## 9. Radar и Risk

| Метод и путь | Доступ | Query/body | Успех | Invalidation |
|---|---|---|---|---|
| `GET /radar` | T, `risks.read` | `status?[]` или `active?`, `locationId?`, `severity?`, `riskType?` | `200 RadarSummary` | — |
| `GET /risks` | T, `risks.read` | `status?[]` (повторяемый) или `active?=true\|false`, `locationId?`, `severity?`, `riskType?`, `limit? 1..100=50`, `cursor?` | `200 {items: RiskDetail[], nextCursor: string \| null}` | — |
| `GET /risks/{riskId}` | T, `risks.read` | — | `200 RiskDetail` | — |
| `POST /risks/{riskId}/acknowledge` | T, `risks.manage` | — | `200 Risk`, идемпотентно | risk, risks, radar |
| `POST /risks/{riskId}/resolve` | T, `risks.manage` | — | `200 Risk`, идемпотентно | risk, risks, radar |

Все операции: `400/401/403`; detail/commands также `404`. Риски отсортированы
серверным приоритетом — frontend не пересортировывает страницы. Summary и list
получают одинаковые business filters: активная лента — `active=true`
(`OPEN|ACKNOWLEDGED|ACTED`), отдельные вкладки статусов — повторяемый
`status`; `status` и `active` вместе дают `400`; cursor привязан к набору
фильтров, включая статусы (GAP-API-003 закрыт). `RiskDetail` содержит
`contact`, `service`, `channel`, `conversation.lastMessage`, `externalLink`;
все ключи присутствуют всегда, `null` — связи нет или запись ещё не создана
(GAP-API-004/012, GAP-CONTRACT-002 закрыты). Денежные поля карточки и сводки
видит и MANAGER (GAP-API-016: политика ADR 0044); OWNER-only остаются только
`/revenue/confirmed-recovered` и `/analytics/*`.

Команды над terminal risk показываются disabled по последнему snapshot, но
race окончательно разрешается backend.

<a id="feedback-api"></a>
## 10. Feedback и точность

| Метод и путь | Доступ | Request/query | Успех | Invalidation |
|---|---|---|---|---|
| `POST /risks/{riskId}/feedback` | T, `risks.manage` | `RiskFeedbackRequest` | `201 RiskFeedback` | risk, risks, radar, precision, analytics |
| `GET /risks/precision` | T, `analytics.read` (OWNER) | `from?` RFC3339 inclusive, `to?` RFC3339 exclusive | `200 RiskPrecisionReport` | — |

Обе операции: `400/401/403`; feedback также `404`. False positive закрывает
активный Risk как `FALSE_POSITIVE`; причина `NOT_A_LEAD` дополнительно переводит
Opportunity в `LOST`. Последствия не моделируются оптимистично.

<a id="notification-api"></a>
## 11. Личный Telegram и уведомления

| Метод и путь | Доступ | Request | Успех | Invalidation |
|---|---|---|---|---|
| `POST /notifications/telegram-link-token` | T, active member | — | `201 {startUrl,expiresAt}` | token draft |
| `GET /notifications/telegram-link` | T, active member | — | `200 TelegramLinkStatus` | — |
| `DELETE /notifications/telegram-link` | T, active member | — | `204`, идемпотентно | telegram-link |
| `GET /notifications/preferences` | T, active member | — | `200`, ровно 5 effective preferences | — |
| `PUT /notifications/preferences/{riskType}` | T, active member | полная `NotificationPreferenceRequest` | `200 NotificationPreference` | preferences |
| `DELETE /notifications/preferences/{riskType}` | T, active member | — | `204`, reset к default | preferences |

Ошибки: `400/401/403`. Link token живёт 15 минут, одноразовый и не
персистится frontend-ом. После возврата из Telegram UI опрашивает status только
пока вкладка видима, с ограниченным числом попыток, либо по кнопке «Проверить».
`telegramEnabled=true` при отсутствии link требует предупреждения и перехода к
привязке; backend остаётся источником окончательной валидации.

<a id="corrective-api"></a>
## 12. Recommendation, Action и Outcome

| Метод и путь | Доступ | Request | Успех | Invalidation |
|---|---|---|---|---|
| `POST /risks/{riskId}/recommendation` | T, `risks.manage` | — | `200 Recommendation` (create-or-get) | risk detail |
| `POST /risks/{riskId}/actions` | T, `action.manage`, Idempotency-Key | `{type,note?}` | `201 Action` / replay `200` | risk, risks, radar |
| `POST /opportunities/{opportunityId}/outcomes` | T, `outcome.manage`, Idempotency-Key | `{status,note?}` | `201 Outcome` / replay `200` | opportunity, risk, risks, radar, analytics |

Ошибки: `400/401/403/404`; Action/Outcome также
`409 IDEMPOTENCY_CONFLICT`. Recommendation детерминирована и не обращается к
AI. `OPEN_CONVERSATION` — записываемый Action, но сначала должен быть выполнен
реальный переход во внешний канал по `externalLink.url`; не записывать успех,
если переход не состоялся. У OWNER и MANAGER есть `action.manage` и
`outcome.manage`; gates разделены на уровне runtime (GAP-CONTRACT-021 закрыт).

<a id="revenue-api"></a>
## 13. Выручка

| Метод и путь | Доступ | Request/query | Успех | Invalidation |
|---|---|---|---|---|
| `POST /opportunities/{opportunityId}/revenue` | T, `revenue.confirm`, Idempotency-Key | `ConfirmRevenueRequest` | `201 RevenueConfirmation` / replay `200` | risk, radar, analytics, confirmed-recovered |
| `GET /revenue/confirmed-recovered` | T, `revenue.read` (OWNER) | required `currency` | `200 Money` | — |

POST: `400/401/403/404/409`; GET: `400/401/403`. `409` различает
`IDEMPOTENCY_CONFLICT` (повторить с новым ключом) и
`RECOVERED_ALREADY_ATTRIBUTED` (предложить `ORGANIC` по явному решению
пользователя); оба кода описаны в OpenAPI (GAP-CONTRACT-017 закрыт).
После неизвестного сетевого результата повторяется то же body с тем же key.
`PAID` Outcome и Revenue подтверждаются отдельными действиями; UI не соединяет
их в один неделимый запрос.

<a id="analytics-api"></a>
## 14. Analytics

`GET /analytics/summary` — T, `analytics.read` (OWNER). Query `from?` и `to?`
в формате `YYYY-MM-DD`, обе границы календарно включительны в timezone
организации; default — последние 30 дней с сегодня, maximum — 366 дней.
Успех `200 AnalyticsSummary`; ошибки `400/401/403/404`. `404` означает, в
частности, невозможность определить организацию/валюту, а не нулевые данные.

Query key включает фактически отправленные даты. Ответный `period` является
авторитетным и используется в подписи. `AnalyticsSummary.series` — по одной
точке на каждую дату окна (дни в timezone организации, нули заполнены),
`attribution` — всегда `RECOVERED`, `ORGANIC`, `UNKNOWN` (GAP-API-007 закрыт).

`GET /analytics/payments` — T, `analytics.read`. Query `from?`, `to?` как у
summary, `limit? 1..100=50`, `cursor?`; успех `200 PaymentPage`
(`period`, `items: Payment[]`, `nextCursor`); ошибки `400/401/403/404`. Строки
идут от новых к старым, каждая несёт свою `currency` — суммировать строки
разных валют нельзя; cursor действует только внутри своего окна дат.

<a id="sse-api"></a>
## 15. SSE invalidation stream

`GET /events` — session + T, `Accept: text/event-stream`. Из-за tenant header
используется streaming `fetch`, а не native `EventSource`. Возможные события:

```text
risk.changed
risk.acknowledged
risk.resolved
risk.false_positive
resync.required
```

Payload событий `risk.*` содержит только `resourceId`; на любом из них
инвалидируются `risk(resourceId)`, видимые `risks` и `radar`; затем REST
возвращает состояние. `resync.required` (payload `{"reason":"BUFFER_OVERFLOW"}`)
означает, что сервер отбросил сигналы из-за переполнения буфера подписчика:
выполнить полный refetch summary и всех открытых списков. Unknown event
игнорируется с telemetry. Сервер посылает comment heartbeat примерно каждые
20 секунд, replay/`Last-Event-ID` нет.

Reconnect выполняется с exponential backoff + jitter только пока session и
tenant неизменны, document online/visible. Смена tenant abort-ит старый stream
до очистки cache. После reconnect обязательна общая invalidation, потому что
события за разрыв потеряны. `401` завершает auth context, `403` завершает stream
до смены контекста; parser ограничивает размер буфера.

NOTIFY между процессами остаётся best-effort, поэтому focus/online/manual REST
refetch сохраняется как страховка; переполнение буфера подписчика больше не
теряется молча — приходит `resync.required` (GAP-RELIABILITY-020 закрыт).
`503 UNAVAILABLE` при неинициализированном hub описан в OpenAPI: клиент
работает по REST с периодическим refetch и повторяет подключение с backoff.

<a id="admin-api"></a>
## 16. Platform admin API

Все пути требуют session и активный `PLATFORM_ADMIN`, не используют
`X-Tenant-ID` (tenant в filter/path — объект администрирования). `401` означает
нет сессии, `403` — нет platform permission.

### 16.1. Доступ и каталоги

| Метод и путь | Params/body | Успех |
|---|---|---|
| `GET /admin/me` | — | `200 {userId,platformAdmin}`; доступен любому session user |
| `GET /admin/admins` | — | `200 {items: PlatformAdmin[]}` включая revoked |
| `POST /admin/admins` | `{email,note? ≤500}` | `201 PlatformAdmin`; уже active — `200`; также `400/404` |
| `DELETE /admin/admins/{userId}` | path UUID | `204`, идемпотентный revoke |
| `GET /admin/organizations` | — | `200 {items: AdminOrganization[]}` |
| `GET /admin/connections` | — | `200 {items: AdminConnection[]}` |

Grant/revoke invalidируют admins; revoke своего права может немедленно закрыть
admin shell после refetch `/admin/me`.

### 16.2. Очереди и восстановление

| Метод и путь | Params | Успех |
|---|---|---|
| `GET /admin/queue` | — | `200 AdminQueueStats` |
| `GET /admin/jobs` | `tenantId?`, `status?`, `type?`, `limit? 1..200=50` | `200 {items: AdminJob[]}`; новые первыми |
| `GET /admin/dead-letters` | `limit? 1..200=50` | `200 AdminDeadLetters` |
| `POST /admin/jobs/{jobId}/retry` | — | `200 AdminJob` |
| `POST /admin/jobs/{jobId}/discard` | — | `200 AdminJob` |
| `POST /admin/outbox/{eventId}/replay` | — | `200 AdminOutboxEvent` |
| `POST /admin/outbox/{eventId}/discard` | — | `200 AdminOutboxEvent` |
| `POST /admin/ai/jobs/{jobId}/retry` | — | `200 AdminAIJob` |
| `POST /admin/ai/jobs/{jobId}/discard` | — | `200 AdminAIJob` |
| `POST /admin/notifications/deliveries/{deliveryId}/discard` | — | `200 AdminDelivery` |

List filter validation может вернуть `400`. Каждая recovery command также
может вернуть `404` или `409`: объект уже не `DEAD`, discarded либо конфликтует
с более новым AI job. После команды инвалидируются queue, dead-letters и
соответствующий list; optimistic removal запрещён. Перед командой UI показывает
точный object id/type/tenant и требует явного подтверждения.

### 16.3. AI, usage и trace

| Метод и путь | Params | Успех |
|---|---|---|
| `GET /admin/ai/nodes` | — | `200 {items: AdminAINode[]}` |
| `GET /admin/ai/runs` | `tenantId?`, `status?`, `applicationStatus?`, `limit? 1..200=50` | `200 {items: AdminAIRun[]}` |
| `GET /admin/ai/tenants/{tenantId}/conversations/{conversationId}/summary` | path IDs | `200 AdminConversationSummary`; `404` |
| `GET /admin/usage` | optional RFC3339 `from/to`, default 30 days, max 366 | `200 AdminUsageReport`; `400` |
| `GET /admin/trace/tenants/{tenantId}/messages/{messageId}` | path IDs | `200 AdminTrace`; `404` |

Admin UI не показывает prompt/raw output/message text: контракт намеренно
возвращает только metadata, безопасный fact value и evidence IDs. Все admin
views имеют явный `checkedAt`/load time и manual refresh; polling не должен
создавать операционную нагрузку в скрытой вкладке.

<a id="health-api"></a>
## 17. Служебные health probes

| Метод и путь | Auth | Успех / ошибка | Использование frontend |
|---|---|---|---|
| `GET /health/live` | нет | `200 {status:"ok",service:"lidradar-api"}` | только infrastructure/liveness; SPA не опрашивает постоянно |
| `GET /health/ready` | нет | `200 {status:"ready",build,migrations}` или `503 SERVICE_NOT_READY` | deployment/readiness и diagnostic page, не замена feature retry |

Probes находятся вне `/api/v1` и не требуют cookie/tenant. Обычная ошибка
feature-запроса не должна запускать polling health: это создаёт ложную
корреляцию и лишнюю нагрузку. Build/migration details считаются операционными и
не показываются обычному пользователю без утверждённого support UI.

<a id="excluded-api"></a>
## 18. Endpoint, которые browser не вызывает

| Путь | Причина исключения |
|---|---|
| `POST /api/v1/webhooks/{provider}/{tenantId}/{connectionId}` | machine-to-machine ingress с provider secret, body до 1 MiB |
| `POST /internal/v1/ai/nodes/heartbeat` | AI-node bearer + HMAC headers |
| `POST /internal/v1/ai/jobs/claim` | выдаёт worker prompt |
| `POST /internal/v1/ai/jobs/{aiJobId}/started` | управление lease/run worker-а |
| `POST /internal/v1/ai/jobs/{aiJobId}/complete` | принимает raw output до 1 MiB |
| `POST /internal/v1/ai/jobs/{aiJobId}/failed` | технический worker lifecycle |

Эти операции могут присутствовать в generated package, но не экспортируются
из browser API facade и не попадают в route/features.

<a id="request-checklist"></a>
## 19. Checklist любой query/mutation

1. Проверить session, выбранный membership и named permission.
2. Сформировать tenant-scoped query key до начала запроса.
3. Передать AbortSignal, credentials и нужные headers централизованно.
4. Не раскрывать предыдущие данные при смене tenant/filter/resource id.
5. Различить loading, empty, forbidden, not-found, stale и error.
6. Для mutation заблокировать повторный submit; для idempotent draft сохранить
   key и неизменное тело до однозначного результата.
7. Применить invalidation из этого каталога, не вручную править связанные
   агрегаты.
8. Проверить `traceId`, a11y-announcement результата и отсутствие PII/secrets в
   telemetry.
