# Качество, безопасность и тестирование frontend

Документ дополняет существующие команды сборки/развёртывания базового
frontend-проекта. Он задаёт предметные проверки LidRadar, Definition of Done и
fixture matrix. Backend-окружение описано в
[frontend development runbook](../runbooks/frontend-development.md).

<a id="quality-gates"></a>
## 1. Обязательные quality gates

Каждая frontend-задача завершена только при успешных:

1. format/lint/typecheck существующими командами проекта;
2. unit и component tests затронутого кода;
3. contract generation/check без ручного изменения generated files;
4. integration/MSW tests всех новых query/mutation branches;
5. accessibility checks и keyboard walkthrough затронутого route;
6. visual comparison с указанным SVG/PNG для desktop и утверждённым narrow
   state;
7. relevant E2E на реальном local backend fixture;
8. production build и отсутствие secrets/source fixtures в bundle;
9. обновление frontend docs/backlog при изменении контракта или поведения.

Документация базовых команд не копируется сюда: в PR указываются реальные
команды из существующего проекта и их результат.

<a id="test-pyramid"></a>
## 2. Test pyramid

| Уровень | Что проверяет | Что не подменяет |
|---|---|---|
| Unit | enum mapping, money/date adapters, transitions, error mapping, idempotency draft, SSE parser/backoff | DOM и HTTP wiring |
| Component | формы, focus, a11y names, validation, loading/error/empty, long text, permissions | реальный OpenAPI/runtime |
| Contract | generated client compiles; required headers/schema/status; recorded safe examples validate | domain integration |
| Integration | page/query/mutation с deterministic HTTP mock, abort/invalidation/races | cookie/CSRF/DB runtime |
| E2E | browser + реальный backend/PostgreSQL fixtures, session/tenant/roles/cursors/SSE | exhaustive visual variants |
| Visual | key viewport snapshots against approved mockups | semantic/a11y correctness |

Не проверять внутреннюю реализацию TanStack Query/Pinia. Tests наблюдают
пользовательский результат, вызовы public facade и отсутствие cross-tenant
данных.

<a id="unit-matrix"></a>
## 3. Unit test matrix

### 3.1. Transport и ошибки

- tenant header добавляется всем tenant calls, включая Radar/Risk/SSE;
- public auth/admin calls не получают случайный tenant header;
- credentials и AbortSignal передаются;
- `204` не вызывает JSON parse;
- error envelope нормализуется, malformed/non-JSON 5xx получает safe fallback;
- `Retry-After` seconds разбирается с bounded fallback;
- unknown enum/error/event не падает и не раскрывает raw content;
- GET retry policy не повторяет 4xx, mutation не повторяется без разрешения.
- focus/online resync работает даже без SSE disconnect; переполнение буфера
  обрабатывается событием `resync.required` (GAP-RELIABILITY-020 закрыт).

### 3.2. Деньги

- input `31 000,5` нормализуется в `31000.50`, если это разрешено UX;
- больше двух fractional digits, negative/zero для revenue и non-digit input
  отклоняются;
- `0.00`, `null` и отсутствие поля различаются;
- большие допустимые значения не теряют precision;
- `priceFrom <= priceTo`; currency uppercased; разные currency не суммируются;
- `RECOVERED` требует все evidence ids; `ORGANIC/UNKNOWN` не получают
  синтетические ids.

### 3.3. Дата и время

- analytics date range inclusive и не более 366 дней;
- UTC interval `[from,to)` строится/отображается в timezone ответа;
- DST spring/fall transitions не добавляют/теряют календарный день;
- weekday 1..7 mapping не зависит от locale API `getDay()`;
- business hours полны, уникальны, рабочий день имеет две границы;
- quiet hours поддерживают переход через полночь, но не равные границы;
- relative/absolute timestamp корректен до и после browser timezone change.

### 3.4. State machines

- Opportunity предлагает только разрешённые forward/terminal переходы;
- Risk active/terminal classification покрывает все семь statuses;
- unknown status не активирует command;
- false-positive reason обязательна; `NOT_A_LEAD` copy предупреждает о LOST;
- notification preference PUT строит полное тело;
- idempotency draft сохраняет key/body при timeout и сбрасывает при изменении
  body или однозначном success/cancel.

<a id="component-matrix"></a>
## 4. Component и page-state matrix

Для каждого data-bearing блока тестируются минимум следующие cases:

| Case | Ожидание |
|---|---|
| initial pending | skeleton, meaningful accessible busy state, нет старых данных |
| success data | точные values/mapping, currency/timezone/provenance |
| success empty | предметный zero-state, не error |
| filtered empty | filters сохранены, reset доступен |
| partial error | успешные siblings остаются, локальный retry |
| total error | safe copy, retry, traceId details |
| stale/offline | snapshot остаётся только с явной отметкой времени |
| 401 | content удалён; redirect без retry loop |
| 403 | no-access без object/tenant leak |
| 404 | neutral missing state |
| 409 | no optimistic success; refetch/decision path |
| 429 | submit disabled до server delay; countdown announced умеренно |
| mutation pending | одна request, controls locked по области операции |
| unknown mutation result | success не заявлен; safe retry semantics |
| long/null/unknown | layout не ломается; честный fallback |

Форма дополнительно тестирует server error после успешной local validation,
double click, Enter, route leave с dirty draft, permission loss и response после
unmount.

<a id="contract-tests"></a>
## 5. Contract checks

- TypeScript client генерируется из
  [`contracts/openapi/openapi.yaml`](../../contracts/openapi/openapi.yaml) одной
  закреплённой командой; CI генерирует повторно и требует zero diff.
- Все используемые operationId доступны через один browser facade; webhook/
  internal AI operations не экспортируются application code.
- Contract fixture проходит schema validation для success и Error envelope.
- Тест отдельно удерживает временные adapters для известных gaps; после
  исправления OpenAPI тест требует удалить adapter.
- Нормативные header tests подтверждают `X-Tenant-ID` во всех Risk/SSE
  операциях сгенерированного клиента ([GAP-CONTRACT-001](08-readiness-gaps.md#gap-contract-001)
  закрыт; временный allowlist-interceptor удаляется).
- Runtime smoke сверяет `Risk.source=MANUAL`, явные `null` у relations
  `RiskDetail` и nullable `nextCursor` (GAP-CONTRACT-002 закрыт; adapter,
  принимавший отсутствующие поля, удаляется).

Contract mock не должен содержать дополнительные поля только потому, что они
нужны макету. Для них сначала меняется backend contract.

<a id="integration-tests"></a>
## 6. Integration scenarios

1. Boot: guest, zero/one/many memberships, stale persisted tenant, network
   failure, switch while queries and SSE active.
2. Login: success, invalid credentials, origin rejected, 429 countdown, double
   submit.
3. Onboarding: reload после каждого успешного шага, частичный service batch,
   duplicate create response, role loss.
4. Radar: summary/list independent results, filters reset cursor, next-page
   error, SSE duplicate event, reconnect full invalidation.
5. Risk: missing optional relation, terminal transition race, feedback cascade,
   recommendation create-or-get.
6. Money Loop: action/outcome/revenue keys, timeout+same-key replay, key/body
   mismatch, recovered already attributed.
7. Conversations: switch during fetch, older-page prepend/scroll anchor,
   deleted message, missing attachment, no deeplink.
8. Integrations: created `ACTIVE`, created `ERROR`, connect `503`, disconnect
   `503` with local `DISCONNECTED`, secret field cleanup.
9. Notifications: token expiry, link poll visibility, overnight hours, reset to
   defaults, Telegram requested while unlinked.
10. Analytics: zero data, date validation, partial precision error, unreliable/
    nullable metrics, timezone/DST.
11. Admin: false guard, permission revoked mid-page, filters, `409` recovery
    race, metadata-only trace.

<a id="backend-fixtures"></a>
## 7. Реальные backend fixtures

Запуск и credentials берутся только из
[runbook](../runbooks/frontend-development.md): `make frontend-up`, API
`127.0.0.1:8081`, PostgreSQL `127.0.0.1:5433`. Текущий runtime генерирует пароль
в `runtime/frontend/password.txt`; его нельзя коммитить или печатать в CI log.

| Fixture user | Назначение | Ожидаемый объём |
|---|---|---|
| `empty` | честные empty states | организация без business data |
| `small` | основной smoke/E2E | 1 location, 24 conversations, 144 messages, 24 opportunities, 20 risks / 10 active |
| `large` | pagination/performance | 3 locations, 240 conversations, 1440 messages, 240 opportunities, 200 risks / 100 active |

Для small/large проверяются разные statuses/severities/types/source, null price,
deleted/attachment metadata, Action/Outcome/Revenue и analytics. Fixture
attachment может ссылаться на отсутствующий object — это ожидаемый UI case.
TEST/IMPORT connection не подтверждает реальную Telegram-интеграцию, а local
AI/Telegram delivery не должны требоваться для deterministic E2E.

Нельзя утверждать точные числа из mockup как fixture expectations. Для
проверки данных E2E читает API snapshot либо использует documented fixture
invariants, а не визуальные demo values.

<a id="e2e-critical"></a>
## 8. Критические E2E paths

### P0 smoke

1. Login small OWNER → tenant выбран → Radar загружен, money помечены корректно.
2. Login MANAGER → Radar/Conversations доступны, owner settings/analytics нет,
   прямой URL безопасен.
3. Empty OWNER → successful zero Radar отличается от disconnected/error.
4. Открыть Risk → acknowledge → записать Action и Outcome → reload показывает
   persisted history.
5. Подтвердить Revenue с фиксированным key → повтор того же запроса не создаёт
   второе событие.
6. Переключить tenant во время запроса/stream → DOM и network не содержат
   смешанного snapshot.

### P1 functional

- onboarding с reload/resume;
- conversation cursor и missing attachment;
- notification token/preferences;
- integration connect/disconnect failure states;
- analytics zero/nonzero/DST;
- feedback false positive cascade;
- admin dead-letter conflict.

Large fixture E2E измеряет не абсолютную microbenchmark-скорость, а отсутствие
неограниченного N+1, зависания UI и дублирования cursor pages.

<a id="accessibility"></a>
## 9. Accessibility

Target — WCAG 2.2 AA для пользовательских workflows.

- Native semantic elements прежде ARIA; один `h1`, логичная heading structure.
- Все inputs имеют persistent label, errors связаны `aria-describedby`, invalid
  state не определяется одним цветом.
- Focus видим на каждом interactive element, порядок соответствует визуальному.
- Dialog удерживает focus и возвращает его trigger; Escape работает, кроме
  момента неделимой pending operation.
- Async result/error объявляется polite/assertive live region без повторного
  озвучивания countdown каждую секунду.
- Tables имеют caption/headers; dense region доступна keyboard и screen reader.
- Status/severity имеют текст, не только dot/color/icon.
- Message direction читается по подписи/семантике, не только выравниванию.
- Touch target и spacing соответствуют существующей design system; кнопка 44 px
  из токенов подходит как baseline.
- `prefers-reduced-motion` отключает необязательное движение; skeleton не
  мерцает агрессивно.
- 200% zoom и 320 CSS px не скрывают действие, error или form label.

Automated axe/linters обязательны, но не заменяют keyboard + VoiceOver/NVDA
smoke для login, Radar, Risk action, revenue dialog и settings form.

<a id="responsive"></a>
## 10. Responsive и устойчивость content

- Проверки viewport: минимум 320, 375, 768, 1024, 1440 CSS px.
- Conversations на narrow используют отдельные list/detail states с Back,
  вместо сжатия двух колонок.
- Cards и forms переходят в одну колонку; sticky элементы не перекрывают focus.
- Tables либо дают semantic card mode, либо собственный labelled horizontal
  scroll; page-wide scroll запрещён.
- Длинные email, organization/service/contact names, Russian/English enum copy,
  amounts и UUID не обрезают доступ к полному значению.
- Safe-area и browser text scaling не ломают primary action.
- RTL не входит в MVP без отдельного требования, но layout не строится на
  несемантических space characters.

До утверждения responsive designs действует blocker GAP-DESIGN-015 для
визуальной приёмки, но semantic reflow tests проектируются сразу.

<a id="security"></a>
## 11. Security и privacy tests

- Нет токена сессии в JS, localStorage, URL или Authorization header.
- Tenant берётся только из `/auth/me` membership; spoofed route/storage value
  удаляется до request.
- Cache/query key всегда tenant-scoped; switch/logout удаляет видимые данные.
- API strings выводятся textContent/template escaping; security test использует
  `<img onerror=...>` и `<script>` как обычный текст.
- External links проверяются по разрешённому contract/scheme/domain и получают
  `noopener noreferrer`; raw `externalId` ссылкой не становится.
- Password, bot token, webhook secret, message text, phone/email, notes и raw
  errors не отправляются в analytics/log/error reporting.
- Error details/trace показываются без stack/env/secrets.
- CSP, secure cookie и allowed origins проверяются deployment/security layer,
  frontend не ослабляет их inline script/eval.
- 403/404 cases не позволяют определить чужой tenant/resource по copy,
  duration-dependent UI или cached title.
- Admin UI не раскрывает content, отсутствующий в metadata-only contract.

Dependency audit и secret scan выполняются существующим pipeline; найденные
уязвимости triage-ятся до merge, но пакеты не обновляются скрыто внутри
feature-задачи.

<a id="performance"></a>
## 12. Performance и наблюдаемость

- Route-level lazy loading для settings/admin/heavy analytics; критический
  boot/login не ждёт admin bundle.
- Conversation/Risk lists используют cursor и при необходимости windowing;
  запрещён N+1 detail per row.
- Один SSE stream на tenant; hidden/offline lifecycle ограничивает reconnect.
- Query cache имеет bounded retention, tenant removal и не хранит бесконечные
  message pages после logout.
- Web Vitals/route timings не содержат URL с PII; API failure telemetry: route
  template, operationId, status, safe code, traceId, attempt classification.
- Mutation telemetry никогда не содержит body/idempotency key/secrets.
- Visual stability проверяется skeleton geometry; изображения/шрифты не
  блокируют управление формой.

Бюджеты bundle/latency берутся из существующей frontend-базы. Если они не
определены, отдельная задача сначала фиксирует baseline, затем допустимый
регрессионный порог — нельзя объявлять произвольное число задним числом.

<a id="visual-qa"></a>
## 13. Visual QA макетов

- Desktop snapshot сравнивается с конкретным PNG из
  [`mockups/preview`](mockups/preview), учитывая реальные dynamic values.
- SVG/PNG исходники не используются как production DOM/background screenshot.
- Проверяются Inter fallback, weights, рубль, long text и browser font rendering.
- Token values импортируются один раз; hardcoded color/spacing lint или review
  выявляет дубли.
- Success screenshot дополняется error/empty/loading/permission/narrow stories.
- Diff обновляется только вместе с design approval и ссылкой на причину.

Пакет макетов уже имеет `qa-report.json`; он проверяет исходные листы, но не
реализованный frontend.

<a id="definition-of-done"></a>
## 14. Definition of Done одной задачи

- Выполнен полный task text и acceptance criteria из [TASKS.md](TASKS.md).
- Нет незадокументированного fallback, mock endpoint или type cast `any`.
- Все новые server state идут через query keys/invalidation из архитектуры.
- Все permissions, tenant headers, errors, null/money/time cases проверены.
- Нет regressions для OWNER/MANAGER/guest/PLATFORM_ADMIN согласно scope.
- Добавлены tests пропорционально риску и приложены команды/результаты.
- Visual/a11y/manual checks приложены для UI change.
- Локальные docs ссылки валидны; изменённые API/design gaps обновлены.
- PR не содержит runtime credentials, generated fixture secrets или лишние
  unrelated изменения.
