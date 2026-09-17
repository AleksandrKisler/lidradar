# Архитектура фронтенда

<a id="architecture-baseline"></a>
## 1. Зафиксированный baseline

Web-клиент — SPA на **Vue 3 + TypeScript + Vite**. Навигация — Vue Router,
локальное клиентское состояние — Pinia, серверное состояние — TanStack Query
для Vue, HTTP-типы и клиент — из OpenAPI, realtime — SSE.

Базовый проект с пакетами, развёртыванием и общими тестовыми командами уже
существует. Эта спецификация не требует пересоздавать scaffold и не фиксирует
другие версии пакетов поверх существующего lock-файла.

Неподвижные архитектурные правила:

- PostgreSQL через backend REST остаётся единственным источником истины;
- Pinia не хранит Radar, Risk, Conversation, Opportunity, Analytics и другие
  ответы API;
- TypeScript DTO не пишутся вручную и сгенерированный код не редактируется;
- frontend adapters отделяют transport DTO от view model и форматирования;
- SSE только инвалидирует запросы, но не изменяет cache вручную;
- Conversation, Opportunity, Risk, Outcome и Revenue остаются разными
  сущностями, даже если показаны в одном workspace;
- выбранная организация входит в ключ любого tenant-scoped cache;
- переход между организациями отменяет активные запросы и очищает данные
  предыдущей организации с экрана;
- пароль, session cookie, Telegram bot token и webhook secret не попадают в
  Pinia persistence, `localStorage`, логи, analytics и error reporting.

<a id="folder-layout"></a>
## 2. Структура исходников

Целевая структура сохраняет направления зависимостей из архитектурного
baseline:

```text
src/
  app/
    router/          # таблица маршрутов и guards
    providers/       # QueryClient, error boundary, i18n/theme при наличии
    config/          # публичная runtime-конфигурация
    styles/          # reset, tokens, global layout
  pages/
    login/
    register/
    workspace-select/
    onboarding/
    radar/
    risk/
    conversations/
    analytics/
    integrations/
    settings/
    admin/
  widgets/
    app-shell/
    radar-summary/
    risk-feed/
    conversation-viewer/
    opportunity-summary/
    revenue-summary/
  features/
    auth-session/
    select-workspace/
    acknowledge-risk/
    resolve-risk/
    risk-feedback/
    ensure-recommendation/
    record-action/
    record-outcome/
    confirm-revenue/
    connect-source/
    telegram-user-link/
    update-notification-preference/
    update-organization/
    update-location/
    update-service/
  entities/
    user/
    membership/
    organization/
    location/
    service/
    integration/
    contact/
    conversation/
    message/
    opportunity/
    risk/
    recommendation/
    action/
    outcome/
    revenue/
    notification-preference/
  shared/
    api/
      generated/     # результат OpenAPI generation; не редактировать
      client/        # credentials, tenant header, errors, request id
      adapters/      # DTO -> view model
    ui/
    lib/
      money/
      date-time/
      idempotency/
    config/
    types/
```

Допустимое направление импорта:

```text
app/pages → widgets/features → entities → shared
```

Нижний слой не импортирует верхний. Страница композирует сценарий, feature
выполняет одно пользовательское действие, entity знает представление одной
сущности, `shared` не содержит бизнес-правил LidRadar.

<a id="state-ownership"></a>
## 3. Владение состоянием

### 3.1. Pinia

Разрешено хранить:

- текущего `User` и массив `MembershipSummary`, полученные из `/auth/me`;
- подтверждённый `selectedTenantId` и соответствующую роль;
- transient UI state: открытая панель, выбранная вкладка, несохранённый фильтр;
- безопасные пользовательские предпочтения представления;
- результат `/admin/me` в рамках текущей сессии.

Нельзя создавать `riskStore`, `conversationStore`, `analyticsStore`, копировать
туда query results или вручную синхронизировать две версии server state.

`selectedTenantId` можно сохранять как несекретную настройку, но только с
привязкой к `user.id`. При каждом `/auth/me` значение проверяется по текущему
`memberships`; неизвестное значение удаляется. При logout очищаются выбранный
tenant, admin context, query cache и все незавершённые idempotency drafts.

### 3.2. TanStack Query

Минимальные канонические ключи:

```ts
['auth', 'me']
['tenant', tenantId, 'organization']
['tenant', tenantId, 'locations']
['tenant', tenantId, 'services']
['tenant', tenantId, 'integrations']
['tenant', tenantId, 'integration-health', connectionId]
['tenant', tenantId, 'conversations', filters]
['tenant', tenantId, 'conversation', conversationId]
['tenant', tenantId, 'messages', conversationId]
['tenant', tenantId, 'opportunity', opportunityId]
['tenant', tenantId, 'radar', filters]
['tenant', tenantId, 'risks', filters]
['tenant', tenantId, 'risk', riskId]
['tenant', tenantId, 'risk-precision', period]
['tenant', tenantId, 'telegram-link']
['tenant', tenantId, 'notification-preferences']
['tenant', tenantId, 'analytics', period]
['tenant', tenantId, 'recovered-revenue', currency]
['tenant', tenantId, 'ml-consent']
['admin', 'me']
['admin', 'organizations']
['admin', 'connections']
['admin', 'queue']
['admin', 'jobs', filters]
['admin', 'dead-letters', limit]
['admin', 'ai-nodes']
['admin', 'ai-runs', filters]
['admin', 'usage', period]
['admin', 'trace', tenantId, messageId]
```

Фильтры сериализуются детерминированно: одинаковый набор не должен создавать
разные cache keys из-за порядка полей. `undefined`, пустая строка и отсутствие
фильтра нормализуются до одного представления.

GET можно ограниченно повторять после сетевой ошибки и `5xx`. Не повторяются
автоматически `400`, `401`, `403`, `404`, `409`, `413`, `429`. Мутация без
серверной идемпотентности не повторяется автоматически. Для Action, Outcome и
Revenue повтор разрешён только с тем же `Idempotency-Key` и тем же телом.

<a id="api-client"></a>
## 4. HTTP-клиент

### 4.1. Базовая конфигурация

- В браузере используется относительный base URL `/api/v1`.
- Локально Vite проксирует `/api` на `http://127.0.0.1:8081` по
  [runbook](../runbooks/frontend-development.md).
- Каждый запрос с сессией отправляет `credentials: 'include'`.
- Каждый tenant-scoped запрос получает `X-Tenant-ID` из проверенного
  membership, а не из URL или произвольного пользовательского ввода.
- Клиент может отправлять валидный `X-Request-ID`; полученный `traceId`
  показывается в подробностях ошибки и доступен поддержке.
- JSON отправляется только с `Content-Type: application/json`.
- На смене tenant и уничтожении контекста запросы отменяются через
  `AbortSignal`.

В текущем OpenAPI у части Risk/SSE операций не описан обязательный
`X-Tenant-ID`, хотя runtime требует его. До исправления контракта единый
transport interceptor обязан добавлять заголовок ко **всем** tenant-scoped
операциям; бизнес-feature не добавляет его вручную.

### 4.2. Generated client

Pipeline:

```text
contracts/openapi/openapi.yaml
  → generated TypeScript client + types
  → transport wrapper
  → frontend adapters
  → query/mutation functions
  → view models/components
```

Generation воспроизводима одной существующей проектной командой и проверяется
в CI на отсутствие diff. Генерируемая папка read-only для разработчика.
Разрывы схемы перечислены в [реестре](08-readiness-gaps.md#contract-gaps): до
их исправления нельзя расширять union через `as any` по месту использования.
Временная совместимость оформляется одним типизированным adapter с тестом и
ссылкой на конкретный gap.

### 4.3. Ошибка как значение

Нормализованный клиентский тип:

```ts
type ApiError = {
  httpStatus: number
  code: string
  message: string
  details: Record<string, unknown>
  traceId: string
  retryAfterSeconds?: number
}
```

Пользователь получает безопасный локализованный текст по `code`. Серверное
`message` не используется как готовая UI-строка. `traceId` не заменяет
понятное объяснение, но доступен в блоке «Технические детали».

### 4.4. Идемпотентная отправка

Для Action, Outcome и Revenue логическая отправка имеет объект draft:

```text
body + idempotencyKey + state(draft/submitting/unknown/succeeded/failed)
```

Ключ создаётся через `crypto.randomUUID()` перед первым POST. При timeout или
разрыве сети состояние `unknown`, кнопка не создаёт новый ключ: повтор идёт с
исходным телом и ключом. Новый ключ допустим только после подтверждённого
ответа, явной отмены draft или фактического изменения тела пользователем.
Ответ `200` означает replay прежнего результата, `201` — первую запись.
`409 IDEMPOTENCY_CONFLICT` не повторяется и требует устранить ошибку клиента.

<a id="session-tenant-boot"></a>
## 5. Boot, сессия и организация

Последовательность старта:

1. Показать нейтральный app-loading, не login и не данные прошлого tenant.
2. Выполнить `GET /api/v1/auth/me`.
3. При `401` очистить auth/tenant/query state и открыть `/login`.
4. При сетевой/`5xx` ошибке показать retry; не считать пользователя вышедшим.
5. При нуле memberships отправить в создание организации.
6. При одном membership выбрать его автоматически.
7. При нескольких — восстановить только валидный сохранённый выбор либо
   показать `/workspaces`.
8. После выбора tenant загрузить данные текущего маршрута.

`POST /auth/refresh` вращает ещё действующую opaque session; это не отдельный
refresh token. После `401` он не способен восстановить уже отсутствующую,
просроченную или отозванную сессию, поэтому цикл `401 → refresh → retry`
запрещён.

На `429` формы входа используется `Retry-After`. Абсолютное время разблокировки
можно хранить в `sessionStorage`, чтобы reload не включал кнопку раньше срока;
email и пароль туда не пишутся. После истечения UI-разрешения сервер всё равно
остаётся окончательным арбитром.

<a id="routes"></a>
## 6. Маршруты и guards

| Маршрут | Контекст | Guard | Готовность макета/API |
|---|---|---|---|
| `/login` | вход | только guest | макет есть, API готов |
| `/register` | регистрация | только guest | макета нет, API готов |
| `/workspaces` | выбор tenant | auth, ≥ 2 memberships | макета нет, API готов |
| `/onboarding/company` | организация и первая точка | auth | макет есть; resume-state не завершён контрактом |
| `/onboarding/business-hours` | график точки | auth + tenant + OWNER | макет/API есть |
| `/onboarding/services` | первые услуги | auth + tenant + OWNER | макет/API есть |
| `/onboarding/telegram` | источник и личная привязка | auth + tenant | макет есть; source-connect flow требует решения |
| `/radar` | summary и активная лента | auth + tenant + `risks.read` | пустой макет есть; read model неполон |
| `/risks/:riskId` | единый Risk Workspace | auth + tenant + `risks.read` | исходный макет v0.1 недоступен; API неполон |
| `/conversations` | список диалогов | auth + tenant + `conversation.read` | макет есть; list API неполон |
| `/conversations/:conversationId` | выбранный диалог | тот же | макет есть; внешний deep link отсутствует |
| `/analytics` | агрегаты | auth + tenant + `analytics.read` | макет шире API |
| `/integrations` | источники | auth + tenant + OWNER | макет есть; live probe отсутствует |
| `/settings/company` | организация, точки, график | auth + tenant + OWNER | макет/API есть |
| `/settings/services` | каталог | auth + tenant + OWNER | list API готов; add/edit dialog не нарисован |
| `/settings/notifications` | личные предпочтения | auth + tenant + active member | макет есть, но визуально назван owner-only |
| `/settings/team` | участники | auth + tenant + OWNER | макет есть, HTTP API отсутствует |
| `/settings/privacy` | ML consent | auth + tenant; mutation OWNER | макета нет, API готов |
| `/admin/*` | platform operations | auth + `platformAdmin=true` | API готов, макетов нет |

Guard улучшает UX, но не является контролем доступа. При прямом переходе
запрос всё равно выполняется только после проверки контекста, а `403` даёт
нейтральное состояние «Раздел недоступен» без сведений о чужих данных.

<a id="permissions"></a>
## 7. Матрица ролей

Фактические runtime permissions:

| Возможность | OWNER | MANAGER |
|---|:---:|:---:|
| читать/обрабатывать риски | ✓ | ✓ |
| читать диалоги | ✓ | ✓ |
| читать/менять Opportunity stage | ✓ | ✓ |
| создавать Action и Outcome | ✓ | ✓ |
| подтверждать RevenueEvent | ✓ | ✓ |
| читать organization/locations | ✓ | ✓ |
| читать прямую revenue summary | ✓ | — |
| читать analytics и risk precision | ✓ | — |
| управлять organization/location | ✓ | — |
| читать/управлять services | ✓ | — |
| управлять integrations | ✓ | — |
| управлять командой во внутреннем service | ✓ | — |
| личная Telegram-привязка/preferences | ✓ | ✓ |

`PLATFORM_ADMIN` не является tenant-role и проверяется отдельно через
`GET /api/v1/admin/me`.

В composite read models есть эффективные исключения: Radar возвращает суммы
пользователю с `risks.read`, а RiskDetail может содержать revenue projection.
Frontend не должен самостоятельно расширять или сужать это поведение —
решение о видимости данных должно быть закреплено backend-контрактом.

<a id="query-invalidation"></a>
## 8. Invalidation после команд

| Успешная команда | Что инвалидировать |
|---|---|
| login/register/refresh/logout | `auth/me`; при logout — весь защищённый cache |
| create organization | `auth/me`, затем выбрать созданный tenant |
| update organization | organization, auth/me (имя membership), analytics при смене timezone/currency |
| create/update location, replace hours | locations; Radar/Risks не переписывать локально |
| create/update/delete service | services; новые результаты анализа придут отдельно |
| connect/disconnect integration | integrations и health этого connection |
| acknowledge/resolve risk | risk, risks, radar; дождаться ответа, SSE может повторить invalidation |
| ensure recommendation | risk и recommendation в локальном workspace query |
| create action | risk, risks, radar |
| create outcome / change stage | opportunity, risk, risks, radar, analytics |
| confirm revenue | risk, radar, analytics, recovered-revenue |
| record feedback | risk, risks, radar, risk-precision, analytics |
| Telegram link issue/disable | telegram-link; выпуск токена сам по себе не означает linked |
| put/reset preference | notification-preferences |
| grant/revoke ML consent | ml-consent |
| admin retry/discard/replay | соответствующий объект, queue и dead-letters |

Оптимистическое обновление допустимо только для чистого UI-state. Для
бизнес-состояния LidRadar применяется ответ сервера и последующее
invalidating/refetch: команды могут запускать транзакционные эффекты в
нескольких модулях.

<a id="data-format"></a>
## 9. Деньги, время и nullable

- Суммы приходят строками. Для отображения они разбираются decimal-библиотекой
  из базового проекта либо форматируются без двоичной арифметики; `Number()` и
  арифметика IEEE-754 для денег запрещены.
- В API ввода принимается строка до двух дробных знаков; UI нормализует
  пробелы и десятичную запятую в точку перед отправкой, но не округляет скрыто.
- Валюты не складываются. Контекст валюты берётся из ответа сущности или
  `Organization.defaultCurrency`, а не жёстко из `RUB`.
- RFC 3339 хранится как абсолютный момент. Бизнес-график точки отображается в
  `Location.timezone`, analytics date range — в `Organization.defaultTimezone`.
- `null` цены/оценки означает «неизвестно», не `0`.
- Отсутствующая optional relation в RiskDetail означает «ещё не создана или
  недоступна в read model», не ошибку всего экрана.
- Пустой список — успешный ответ с `items: []`; конец cursor pagination —
  `nextCursor: null` в фактическом runtime.

<a id="realtime"></a>
## 10. SSE

Нативный `EventSource` нельзя использовать: он не умеет передать обязательный
`X-Tenant-ID`. Поток открывается через streaming `fetch` с cookie и tenant
header.

Клиент обрабатывает только `risk.changed`, `risk.acknowledged`,
`risk.resolved`, `risk.false_positive`. `data.resourceId` служит подсказкой
для точечной invalidation, но после reconnect обязательно перечитываются
Radar и текущая лента. У событий нет replay ID; потеря сигнала ожидаема.

Состояния соединения: `connecting`, `open`, `backoff`, `offline`, `stopped`.
Backoff ограниченный, с jitter; offline browser приостанавливает повторы.
На смене tenant/logout старый stream закрывается до открытия нового.

SSE — ускоритель, а не гарантия доставки: normal query staleness,
`refetchOnWindowFocus`, refetch после возврата online и ручное обновление
остаются включены. Частота возможного safety refetch для долго открытого Radar
зависит от решения [GAP-RELIABILITY-020](08-readiness-gaps.md#gap-reliability-020);
до него UI всегда показывает время последнего успешного REST snapshot.

Подробные диаграммы — в [карте последовательностей](05-sequence-map.md).
