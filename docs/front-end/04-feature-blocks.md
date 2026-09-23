# Функциональные блоки frontend

Документ даёт контекст разработки по независимым продуктовым блокам. Каждый
блок определяет цель, роли, данные, запросы, UI-состояния, правила завершения и
известные зависимости. Детали transport не дублируются — ссылки ведут в
[каталог API](03-api.md).

<a id="block-shell"></a>
## 1. App shell, session и tenant

**Цель.** Не показать защищённые данные до определения пользователя и явной
организации; безопасно переключать workspace.

**Роли.** Guest, authenticated without membership, OWNER, MANAGER,
PLATFORM_ADMIN (отдельный контекст).

**Данные и запросы.** `User`, `MembershipSummary`; `GET /auth/me`, logout,
optional proactive refresh, `GET /admin/me` только при входе в `/admin`.

**Сценарий.** Boot показывает нейтральный loader → `/auth/me` → ноль
memberships ведёт к созданию организации, один выбирается автоматически,
несколько ведут к `/workspaces` либо безопасно восстанавливают выбор. После
смены tenant старые запросы abort-ятся, tenant cache очищается с экрана, затем
загружается новый route.

**Обязательные состояния.** Boot loading; unauthenticated; memberships empty;
workspace selection; tenant switching; network error с retry; disabled user;
session expired во время работы; forbidden route; no-platform-admin.

**Инварианты.** Никогда не брать tenant из произвольного route param; не
выполнять 401→refresh loop; не показывать stale tenant content под новым
названием; logout чистит cache и persisted context.

**Зависимости.** Макеты registration/workspace отсутствуют —
[GAP-DESIGN-014](08-readiness-gaps.md#gap-design-014).

<a id="block-auth"></a>
## 2. Вход и регистрация

**Цель.** Создать user/session либо безопасно войти, не раскрывая наличие
учётной записи.

**Запросы.** `POST /auth/login`, `POST /auth/register`, затем обязательный
`GET /auth/me`.

**Форма входа.** Email, password, submit; password reveal с accessible label;
общая ошибка credentials; `Retry-After` countdown; Enter submit; защита от
double submit. Password никогда не восстанавливается после reload.

**Форма регистрации.** Email, display name, password ≥12; локальные проверки
повторяют только очевидные ограничения, backend остаётся арбитром. При
`EMAIL_ALREADY_REGISTERED` не переходить в app и не сохранять password.

**Успех.** Нельзя сразу направлять на `/radar` по одному `AuthResponse`:
следующий шаг определяется memberships из `/auth/me`.

**Макет.** [Вход](mockups/svg/01-vhod.svg); регистрация отсутствует.

<a id="block-onboarding"></a>
## 3. Onboarding организации

**Цель.** Получить минимальный рабочий tenant: организация → точка → полная
неделя → услуги → источник/личная Telegram-привязка → Radar.

**Шаг 1 — компания и точка.** Если membership ещё нет, создать Organization,
refetch `/auth/me`, выбрать tenant, затем создать Location. Если tenant уже
есть, загрузить Organization/Locations и не создавать дубликат после reload.

**Шаг 2 — график.** Выбрать точку, подготовить семь уникальных weekday,
отправить атомарный PUT. Timezone шага явно показан; закрытый день не несёт
скрытых opens/closes.

**Шаг 3 — услуги.** Создавать элементы отдельно, поддержать неизвестную цену и
organization-wide `locationId=null`; успешные строки остаются после ошибки
следующей. Минимум одна услуга — продуктовая цель, но API не даёт server-side
флаг «шаг завершён».

**Шаг 4 — Telegram.** Разделить подключение business source (OWNER) и личную
привязку уведомлений (любой member). Это разные сущности, secrets и запросы.

**Resume.** На каждом входе шаг берётся из `GET /organization/onboarding`:
`nextStep` и `steps[].done` — авторитетный статус, выведенный сервером из
данных; localStorage не участвует ([GAP-API-009](08-readiness-gaps.md#gap-api-009)
закрыт 2026-09-18).

**Skip.** Пропуск необязательной личной Telegram-привязки допустим; нельзя
объявлять готовым при обязательном источнике, пока source contract не
согласован.

**Макеты.** [Компания](mockups/svg/02-nastroika-kompanii.svg),
[график](mockups/svg/03-rabochee-vremia.svg),
[услуги](mockups/svg/04-uslugi-i-ceny.svg),
[Telegram](mockups/svg/05-podkliuchenie-telegram.svg).

<a id="block-radar"></a>
## 4. Radar: сводка и лента

**Цель.** За один snapshot ответить: сколько активных/критичных рисков, сколько
потенциальной и подтверждённо возвращённой суммы, какое действие делать первым.

**Роли.** OWNER и MANAGER с `risks.read`; команды требуют `risks.manage`.

**Данные.** `RadarSummary` и paginated `RiskDetail[]`. Общие filters:
location, severity, riskType; целевой фильтр status — набор active statuses.

**Запросы.** Параллельно `GET /radar` и первая страница `GET /risks`, затем
cursor pages; SSE только инвалидирует оба query.

**Композиция.** Summary cards отдельно различают potential и confirmed
recovered. Лента сохраняет server priority. Карточка содержит severity/type,
причину, возраст/due, contact/service context, opportunity stage, potential
amount, recommendation/primary action — только если эти значения пришли в
read model.

**Состояния.** Initial loading skeleton; частичная ошибка summary/list;
настоящий zero-state; filtered zero-state; stale snapshot с временем последней
успешной загрузки; pagination error с retry страницы; SSE offline badge; access
denied.

**Запреты.** Не отфильтровывать terminal risks после каждой страницы: это
ломает полноту/курсор. Не показывать нули при ошибке. Не складывать currencies.

**Блокеры.** API-блокеры сняты 2026-09-18: active pagination —
`GET /risks?active=true` с курсором, привязанным к фильтрам
([GAP-API-003](08-readiness-gaps.md#gap-api-003) закрыт); поля карточки —
`contact`, `service`, `channel`, `lastMessage`, `externalLink` в `RiskDetail`
([GAP-API-004](08-readiness-gaps.md#gap-api-004) закрыт); имя услуги приходит в
карточке и MANAGER не обращается к каталогу
([GAP-API-012](08-readiness-gaps.md#gap-api-012) закрыт). Остаётся дизайн:
доступен только empty Radar [макет](mockups/svg/14-radar-bez-riskov.svg);
основной лист v0.1 отсутствует (GAP-DESIGN-014).

<a id="block-risk-workspace"></a>
## 5. Risk Workspace

**Цель.** На одном маршруте понять причину риска, проверить переписку и
Opportunity, выполнить действие, записать outcome и при необходимости
подтвердить деньги.

**Начальная загрузка.** `GET /risks/{riskId}`. Если composite не содержит
достаточной истории, загрузить явно известные `conversation.id` и
`opportunity.id`; N+1 допустим только на detail route, не в ленте.

**Области экрана.** Заголовок risk/status/severity; объяснение/trigger;
conversation viewer read-only; opportunity/history; recommendation;
неизменяемая история actions/outcome; revenue projection/facts; audit times.

**Команды.** Acknowledge, resolve, feedback, ensure recommendation, Action,
Outcome, stage transition, Revenue. Каждая команда имеет независимый pending и
error state; одна ошибка не скрывает уже загруженный контекст.

**Порядок Money Loop.** Открыть внешний диалог → только после фактического
действия записать Action → после ответа записать Outcome → `PAID` не равен
Revenue → отдельно подтвердить сумму и attribution. `RECOVERED` доступен, если
выбрана доказуемая цепочка Risk+Action+Outcome.

**Гонки.** SSE или другой user может изменить status между render и submit.
После ответа/refetch кнопки пересчитываются. `404` — нейтральный not-found;
terminal state сохраняет read-only history.

**Блокеры.** API-блокеры сняты 2026-09-18: detail read model обогащён и все
связи явно nullable (GAP-API-004, GAP-CONTRACT-002 закрыты); внешний переход
— по `externalLink.url` из ответа с честным `unavailableReason`
(GAP-API-006 закрыт). Остаётся дизайн: утверждённый макет недоступен —
GAP-DESIGN-014.

<a id="block-conversations"></a>
## 6. Диалоги

**Цель.** Найти переписку, прочитать каноническую историю и перейти к
связанному Risk Workspace/внешнему Telegram, не превращая LidRadar в messenger.

**Список.** Infinite/cursor query; строка целевого UI: contact label, preview,
channel, location, last time/direction, active-risk badge. Server должен
поддержать search и «С риском». До расширения API нельзя делать N detail calls
или локально фильтровать только загруженные страницы.

**Detail.** Параллельно ConversationDetail и первая страница messages.
Загружать более старые сообщения вверх, сохранять scroll anchor; входящие и
исходящие визуально/семантически различимы. `providerDeletedAt` показывает
placeholder, attachment без download URL — unavailable state.

**Read-only.** Нет composer/send request. CTA «Ответить в Telegram» возможен
только по allowlisted server deeplink; `externalId` сам по себе URL не является.

**Состояния.** List loading/empty/filter-empty/error/page-error; detail loading,
conversation 404, messages empty/error, deleted message, unsupported type,
missing attachment, no external link.

**Блокеры.** Сняты 2026-09-18: `ConversationListItem` с контактом, каналом,
превью и `activeRisks`, фильтры `search`/`withRisk`
([GAP-API-005](08-readiness-gaps.md#gap-api-005) закрыт); `externalLink` в
списке и деталях ([GAP-API-006](08-readiness-gaps.md#gap-api-006) закрыт).
Макет: [Диалоги](mockups/svg/06-dialogi.svg).

<a id="block-integrations"></a>
## 7. Интеграции источников

**Цель.** OWNER видит фактическое persisted health и безопасно подключает или
отключает источник сообщений.

**Данные/запросы.** List connections; health выбранной connection; connect по
provider; disconnect. Карточка отображает provider/name/location/status,
capabilities, last event/success/error и safe error code.

**Connect.** Secret fields не сохраняются и очищаются после submit/unmount.
Результат `ACTIVE` означает успешную provision-проверку, `ERROR` — connection
создана, но remote provisioning не завершён. На `503` draft без secrets можно
сохранить только в памяти до ухода со страницы.

**Disconnect.** Confirmation показывает имя/provider. После любого ответа,
включая `503`, list refetch обязателен: local disconnect мог состояться.

**Health.** `GET …/health` подписывается «состояние прочитано»; кнопка
«Проверить связь» вызывает `POST …/health/check` и различает `REMOTE`
(Telegram опрошен) и `LOCAL` (сохранённый статус).

**Блокер.** Снят 2026-09-18 решением ADR 0045: секрет webhook выпускает сервер
и показывает один раз; bot token передаётся один раз в write-only поле по TLS
(шифруется, не возвращается); live probe — отдельный endpoint
([GAP-API-010](08-readiness-gaps.md#gap-api-010) закрыт). Макет:
[Интеграции](mockups/svg/08-integracii.svg).

<a id="block-notifications"></a>
## 8. Личная Telegram-привязка и preferences

**Цель.** Каждый active member связывает собственный Telegram и задаёт
доставку отдельно для пяти типов риска.

**Привязка.** GET status → если не linked, POST одноразового token → открыть
`startUrl` → пользователь завершает flow в Telegram → ограниченная проверка
status/manual retry. Token истекает через 15 минут и не хранится.

**Отключение.** Явное confirmation → DELETE → status refetch. Это не отключает
business source integration.

**Preferences.** GET всегда даёт пять effective строк. Редактирование одного
типа отправляет полный PUT; reset — DELETE. UI поясняет timezone, overnight
quiet interval, delivery mode и зависимость Telegram channel от link status.
`isDefault=true` визуально отличает backend default от сохранённой настройки.

**Роли/дизайн.** Runtime разрешает OWNER и MANAGER, хотя экран расположен в
owner settings макета — [GAP-UX-011](08-readiness-gaps.md#gap-ux-011). Макеты:
[Telegram](mockups/svg/05-podkliuchenie-telegram.svg),
[уведомления](mockups/svg/11-nastroiki-uvedomlenii.svg).

<a id="block-settings-company"></a>
## 9. Настройки компании, точек и графика

**Цель.** OWNER редактирует Organization и Locations, не нарушая временную и
денежную семантику.

**Организация.** Name/timezone/currency PATCH. Перед timezone/currency change
показать последствия: analytics period и notification clocks пересчитаются,
исторические деньги не конвертируются.

**Точки.** List, create, edit, deactivate/reactivate. Удаление endpoint
отсутствует. Response threshold 1..1440 minutes; timezone отдельна от org.

**График.** Редактор полной недели, локальная валидация и атомарный PUT.
Несохранённые изменения защищаются route-leave prompt. После смены выбранной
точки draft сбрасывается только с подтверждением.

**Состояния.** Loading/error отдельно для organization и locations; no
locations; form conflict; partial success; permission lost; stale form after
remote update (refetch before overwrite).

**Макет.** [Настройки компании](mockups/svg/09-nastroiki-kompanii.svg) и
[график](mockups/svg/03-rabochee-vremia.svg).

<a id="block-settings-services"></a>
## 10. Настройки услуг

**Цель.** OWNER ведёт active/inactive каталог и честные price ranges.

**Сценарий.** Load all items → фильтр active/inactive/location → create/edit →
PATCH; deactivate через DELETE с confirmation; reactivate через PATCH
`active=true`.

**Правила.** Пустая цена отправляется `null`, не `"0"`; decimal normalizer не
использует float; обе границы одной currency; location выбирается только из
текущего tenant; duplicate/validation error остаётся в форме.

**Макеты.** [Настройки услуг](mockups/svg/10-nastroiki-uslug.svg) и
[onboarding-список](mockups/svg/04-uslugi-i-ceny.svg); add/edit dialog не
утверждён — GAP-DESIGN-014.

<a id="block-team"></a>
## 11. Команда

**Цель.** OWNER видит участников, приглашает/добавляет MANAGER, меняет роль и
отзывает доступ, с защитой от потери последнего OWNER.

**Требуемые данные.** User/member id, email/displayName, role, status, joined/
invited timestamps, кто изменил роль; команды list/invite-or-add/change-role/
revoke и определённая invitation lifecycle.

**Текущее состояние.** В application layer существует AddMember, но публичных
HTTP API: `GET /organization/members`, `PATCH`/`DELETE …/members/{userId}`,
`GET`/`POST /organization/invitations`, `DELETE …/invitations/{id}`,
`POST /invitations/accept` (ADR 0045). Приглашение — одноразовый код, который
UI показывает один раз с кнопкой копирования; принимает его сотрудник после
входа без выбора tenant. Не подменять список данными `/auth/me`: там только
memberships текущего пользователя.

**Блокер.** Снят 2026-09-18 ([GAP-API-008](08-readiness-gaps.md#gap-api-008)
закрыт); confirmations и last-owner protection ждут макетов (GAP-DESIGN-014).
Макет: [Команда](mockups/svg/12-komanda.svg).

<a id="block-privacy"></a>
## 12. Privacy / ML consent

**Цель.** Прозрачно показать, используются ли реальные переписки/feedback для
datasets, и дать OWNER отзываемое явное согласие.

**Доступ.** Любой member читает status; только OWNER видит enabled mutation.
POST даёт или подтверждает consent, DELETE отзывает. В тексте нельзя обещать
удаление audit history или уже сформированных законных артефактов сверх
backend-политики.

**Состояния.** Active/inactive, mutation pending, permission lost, error;
`consent=null` при inactive нормален. Макета нет — GAP-DESIGN-014.

<a id="block-analytics"></a>
## 13. Аналитика и precision

**Цель.** OWNER оценивает activity, conversion, качество рисков, potential и
confirmed money за явное календарное окно.

**Запросы.** `GET /analytics/summary?from&to` и независимый
`GET /risks/precision?from&to` с преобразованием календарных границ в точные
instants только по согласованному adapter/backend правилу.

**Показатели.** Messages; created/booked/won/lost Opportunities; detected/
acted/resolved/false-positive Risks и byType; booked/paid/lost Outcomes;
potential/confirmed/confirmedRecovered/confirmedPayments; precision/coverage.

**Правила.** Всегда показывать response period/timezone; не вычислять revenue
из Outcomes; не делить на ноль; nullable precision — «недостаточно данных»;
`reliable=false` — явно низкое покрытие; не сравнивать разные currency.

**Состояния.** Initial load, invalid date range, partial error summary/precision,
valid zero dataset, stale snapshot, unsupported chart/list placeholders не
рисуются как реальные данные.

**Блокер.** Снят 2026-09-18: `series` и `attribution` в сводке,
`GET /analytics/payments` для списка оплат
([GAP-API-007](08-readiness-gaps.md#gap-api-007) закрыт). Макет:
[Аналитика](mockups/svg/07-analitika.svg).

<a id="block-revenue-dialog"></a>
## 14. Подтверждение оплаты

**Цель.** Создать один immutable RevenueEvent и одну формальную Attribution
без ложного заявления «выручка возвращена».

**Форма.** Opportunity context, positive amount decimal, currency, attribution
type. Для `RECOVERED` пользователь выбирает/подтверждает risk, action, outcome;
frontend разрешает только записи этой opportunity из detail snapshot.

**Отправка.** Key генерируется до первого POST. Pending блокирует duplicate;
timeout переводит draft в unknown и предлагает безопасный retry тем же key/body.
`RECOVERED_ALREADY_ATTRIBUTED` предлагает `ORGANIC`, но не меняет выбор без
подтверждения пользователя.

**Успех.** Показать amount+currency+attribution, закрыть dialog только после
однозначного ответа и refetch. Макет:
[Подтверждение оплаты](mockups/svg/13-podtverzhdenie-oplaty.svg).

<a id="block-sse"></a>
## 15. Realtime invalidation

**Цель.** Быстро обнаруживать изменение Risk без второго источника истины.

**Lifecycle.** Открывается после session+tenant; один stream на app context;
закрывается до switch/logout. Parsed signal инвалидирует REST queries.
Heartbeat не считается business activity. После offline/reconnect делается
полный refetch видимых Risk/Radar queries.

**UI.** Потеря SSE не блокирует работу: показывается ненавязчивый stale badge и
manual refresh. Никогда не выводить «данные актуальны» только по живому TCP.

**Security.** Streaming fetch передаёт tenant header/cookie, поддерживает abort,
ограничивает buffer и не логирует raw chunks.

<a id="block-admin"></a>
## 16. Platform admin

**Цель.** Диагностировать платформу и выполнять только предусмотренные
recovery-команды без раскрытия содержимого переписок/AI.

**Навигация.** `/admin` guard через `/admin/me`; разделы: overview/orgs,
connections, queues/jobs/dead letters, AI nodes/runs/summary, usage, trace,
platform admins.

**Read views.** Явное load time, filters из URL, safe empty/error, object IDs с
copy, metadata-only rendering. Long lists пока limit-based, без выдуманной
cursor pagination.

**Commands.** Grant/revoke admin; retry/discard jobs/outbox/AI/delivery. Перед
каждой destructive/operational командой confirmation с object type/id/tenant.
На `409` refetch и объяснение, что state уже изменился. Никакого bulk action,
если API его не предоставляет.

**Trace.** Ввод tenantId+messageId; показать последовательность metadata,
semantic trust/evidence и business artifacts. Не показывать message text,
prompt/raw model output — их нет в контракте намеренно.

**Зависимость.** API готов, но все admin designs отсутствуют —
[GAP-DESIGN-014](08-readiness-gaps.md#gap-design-014).

<a id="block-cross-cutting"></a>
## 17. Сквозные продуктовые правила

- Error не равен empty, stale не равен current, `null` не равен zero.
- Любая денежная подпись содержит currency и provenance: potential,
  confirmed или confirmed recovered.
- Любая mutation имеет pending/success/error announcement и защиту double submit.
- OWNER-only пункт можно скрыть в nav для MANAGER, но прямой URL остаётся
  безопасно обработанным.
- Server state не копируется в Pinia; URL хранит shareable filters, но не PII/
  secrets.
- HTML-like API content выводится как text. Внешний URL допускается только из
  allowlisted server contract и открывается с `noopener noreferrer`.
- Desktop mockup не освобождает от keyboard, 200% zoom, narrow viewport и
  reduced-motion поведения; требования — в [07-quality.md](07-quality.md).
