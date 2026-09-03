# Модули и зоны ответственности

Каждый раздел описывает один модуль `backend/internal/<module>`: что он
владеет, какие правила закреплены в домене, какие сценарии предоставляет, что
читает у соседей и что публикует. Полные таблицы конечных точек и схемы — в
[04-api.md](04-api.md) и [05-data-model.md](05-data-model.md); фоновые
цепочки — в [06-async-processing.md](06-async-processing.md); AI — в
[07-ai.md](07-ai.md).

Общие соглашения для всех модулей:

- права проверяются в `application` через `tenant.PermissionService.Allowed`
  до любой мутации; активное членство в активной организации обязательно;
- ошибки PostgreSQL переводятся в доменные: `23505` → конфликт, `23503` → не
  найдено, `23514`/`22P02`/`22003` (и в большинстве модулей `22001`) → неверный
  ввод; остальное — 500 без деталей;
- запись аудита выполняется сразу после успешного изменения, ошибка записи
  возвращается вызывающему (след обязателен, ТЗ §65);
- идентификаторы организаций (`tenantId`) наружу в JSON не отдаются
  (`json:"-"`).

## 1. `identity` — пользователи и сессии

**Владеет:** `users`, `sessions`, `auth_rate_limits`. **Не владеет**
организациями, ролями и правами: `/auth/me` получает членства через узкий
контракт `MembershipLister` модуля `tenant`.

**Правила домена.** Email нормализуется (`ToLower(TrimSpace)`), должен
разбираться `net/mail` без имени отправителя и быть не длиннее 254 символов.
Имя пользователя 1…200 символов. Пароль 12…1024 байт. Сессия хранит только
`sha256(token)` (64 hex), срок обязан быть позже создания, `User-Agent`
обрезается до 1024 символов. Статусы пользователя `ACTIVE`/`DISABLED`.

**Сервис** (`application.Service`): `Register`, `Login`, `Authenticate`,
`Logout`, `Refresh`. Троттлинг в PostgreSQL (ADR 0032), отпечаток субъекта —
`sha256(lower(trim(value)))`:

| Scope | Лимит | Окно | Где |
|---|---|---|---|
| `REGISTER_IP` | 5 | 1 ч | регистрация |
| `LOGIN_IP` | 20 | 1 мин | вход |
| `LOGIN_ACCOUNT` | 5 | 15 мин | вход, сбрасывается при успехе |
| `REFRESH_IP` | 60 | 1 мин | ротация сессии |

Вход по неизвестному email выполняет холостое хеширование пароля, чтобы время
ответа не выдавало существование учётной записи; неверный пароль и `DISABLED`
дают один и тот же `401 INVALID_CREDENTIALS`. Ошибка троттлера (недоступная
база) не пропускает запрос. Аудит: `USER_REGISTERED`, `USER_LOGGED_IN`,
`USER_LOGGED_OUT` в `auth_audit_log`; у `Refresh` аудита нет.

**Транспорт:** `/api/v1/auth/*`, cookie `lidradar_session` (`HttpOnly`,
`SameSite=Strict`, `Secure` по конфигурации, `MaxAge = LIDRADAR_SESSION_TTL`,
по умолчанию 30 суток). `Resolver.User`/`Resolver.Principal` — то, чем
остальные модули узнают актора и организацию.

**Хранение:** токен — 32 случайных байта в `base64.RawURLEncoding`; ротация
под `FOR UPDATE OF s` с проверкой `RowsAffected == 1` (гонка ротации не даёт
двух действующих токенов). Таблицы модуля без `tenant_id`, RLS на них нет.

## 2. `tenant` — организации, членства, точки, ML-согласие

**Владеет:** `organizations`, `memberships`, `locations`,
`location_business_hours`, `ml_consents` и **единственной таблицей прав**
(`PermissionService`), которой пользуются все модули.

**Правила домена.** Организация: имя 1…200, валидная IANA-зона, валюта из трёх
букв `A–Z` (по умолчанию `RUB`), статусы `ACTIVE`/`SUSPENDED`/`ARCHIVED`.
Членство: роли `OWNER`/`MANAGER`, статусы `ACTIVE`/`INVITED`/`DISABLED`;
физически не удаляется (триггер), отзыв = `DISABLED` + `revoked_at`. Точка:
имя 1…200, зона, порог ответа 1…1440 мин (по умолчанию 45), часы работы —
ровно 7 строк, `weekday` 1…7, `opens < closes` формата `HH:MM`, закрытый день
без границ. ML-согласие: единственная область `DATASETS`, одно действующее
согласие на организацию, история хранится, повторный отзыв и удаление
запрещены триггером.

**Права по ролям** (`application/service.go`):

| Право | OWNER | MANAGER |
|---|---|---|
| `risks.read`, `risks.manage`, `conversation.read`, `opportunity.manage`, `action.manage`, `outcome.manage`, `revenue.confirm` | ✓ | ✓ |
| `revenue.read`, `analytics.read`, `integration.manage`, `organization.manage`, `location.manage`, `service.manage`, `notification.manage`, `member.manage` | ✓ | — |

`Allowed` требует членство `ACTIVE` **и** организацию `ACTIVE`: приостановленная
или архивная организация теряет все права разом.

**Сервис:** `CreateOrganization` (только аутентификация; создаёт организацию и
`OWNER`-членство атомарно и подставляет новый `tenant_id` в контекст, чтобы
прошёл `WITH CHECK` RLS), `GetOrganization`, `UpdateOrganization` (аудит
`ORGANIZATION_UPDATED`), `AddMember` (аудит `MEMBER_ADDED`; HTTP-маршрута нет,
вызывается из тестов), `MembershipsForUser`, `ListLocations`, `CreateLocation`,
`UpdateLocation`, `ReplaceBusinessHours`, `MLConsent`, `GrantMLConsent`
(аудит `ML_CONSENT_GRANTED` в той же транзакции), `RevokeMLConsent`
(`ML_CONSENT_REVOKED`). Приглашений участников в модуле нет: статус `INVITED`
объявлен, но не присваивается.

**Транспорт:** `/api/v1/organizations`, `/api/v1/organization`,
`/api/v1/locations`, `/api/v1/organization/ml-consent` (монтируется на
`/api/v1` последним, поэтому более специфичные префиксы имеют приоритет).

## 3. `catalog` — каталог услуг

**Владеет:** `service_catalog_items`. Не пишет аудит, не порождает событий.

**Правила домена.** Цена — строка `^[0-9]{1,12}(\.[0-9]{1,2})?$`, хранится и
возвращается как точная десятичная строка (`"1200.00"`), `NULL` при
отсутствии; `priceFrom <= priceTo`; имя схлопывает пробелы, хранится и
нормализованная форма (`lower`) для поиска; валюта из трёх букв; точка
принадлежит той же организации (составной внешний ключ). Одноимённые услуги
допускаются.

**Сервис:** `List`, `Create`, `Update` (трёхзначные поля: не передано /
`null` = сброс / значение), `Deactivate` (мягкое отключение, повтор
идемпотентен). Единственное право — `service.manage`, оно требуется даже для
чтения списка, то есть каталог виден только владельцу.

**Кто читает:** `opportunity` (распознавание коммерческого кандидата) через
интерфейс `CatalogSource.List`.

## 4. `connector` — каналы и приём событий

**Владеет:** `channel_connections`, `raw_events`; проверкой подлинности
вебхука, шифрованными реквизитами, здоровьем подключения и нормализацией
провайдерского payload в `CanonicalEvent` (ADR 0024, 0025). Не создаёт
контакты и переписки (это делает `conversation` через `CanonicalSink`), не
запускает канонизацию внутри HTTP-запроса.

**Провайдеры и способности.** `TEST`, `IMPORT`, `GENERIC_WEBHOOK`,
`CONNECTED_BUSINESS_BOT` (Telegram; строковое значение без префикса
`TELEGRAM_`). `ParseProvider` принимает регистр и дефисы свободно
(`connected-business-bot`). Способности: `CAN_RECEIVE_MESSAGES`,
`CAN_SEND_MESSAGES`, `CAN_IMPORT_HISTORY`, `CAN_RECEIVE_EDITS`,
`CAN_RECEIVE_DELETES`, `CAN_RECEIVE_ATTACHMENTS`, `CAN_IDENTIFY_CONTACT`;
Telegram единственный с `CAN_SEND_MESSAGES`, `GENERIC_WEBHOOK` без импорта
истории.

**Правила домена.** Имя подключения 1…200; хеш секрета — 64 hex; шифротекст
реквизитов ≤ 8192 байт; статусы `ACTIVE`/`DEGRADED`/`ERROR`/`DISCONNECTED`
(`ACTIVE` без кода ошибки). Сырое событие: внешний идентификатор ≤ 512,
payload — валидный JSON с хешем, статусы `RECEIVED`/`PROCESSING`/`PROCESSED`/`FAILED`
(`FAILED` только с кодом ошибки и временем обработки). Каноническое событие
требует направление, тип сообщения, `sentAt` для всего, кроме удаления, и
внешний идентификатор контакта для `message.received.v1`.

**Сервис** (`application.Service`): `Connect` (секрет 16…256 байт; для
Telegram — токен по маске, шифрование AES-256-GCM с AAD
`lidradar:v1:{tenant}:{provider}:{connection}`, запись в базу до сетевого
вызова, затем `setWebhook`; ошибка провижининга сохраняется как
`ERROR/TELEGRAM_WEBHOOK_SETUP_FAILED`), `List`, `Health`, `Disconnect`
(сначала локальный статус и аудит, затем `deleteWebhook`; повтор допустим),
`Receive` (persist-first: проверка секрета → внешний идентификатор → хеш →
`RawEvent` + outbox в одной транзакции → `202`). Право `integration.manage`
для управления; `Receive` аутентифицируется секретом. Аудит:
`INTEGRATION_CONNECTED`, `INTEGRATION_DISCONNECTED`.

**Нормализация** (`NormalizationService.Process`, задание
`connector.normalize-raw-event.v1`): уже обработанное событие — успех без
действий; служебные обновления Telegram (`/start`, кнопки) уходят в
`ControlSink` модуля `notification`; остальное — `NormalizeEvent` →
`IngestCanonical` по каждому событию; ошибка приёмника оставляет сырое событие
в `RECEIVED` для повтора; невалидный payload → `FAILED` с
`NORMALIZATION_INVALID_PAYLOAD`.

**Telegram-специфика:** заголовок `X-Telegram-Bot-Api-Secret-Token`; ровно
одно из полей `business_connection` / `business_message` /
`edited_business_message` / `deleted_business_messages` / `message` /
`callback_query`; идентификаторы `conversation = businessConnectionId:chatId`,
`message = chatId:messageId`, `contact = chatId`; направление `OUTGOING`, если
есть `sender_business_bot` или `from.id != chat.id`; вложения — только
метаданные с `stub/telegram/{file_unique_id}`. `allowed_updates` при
регистрации включает все шесть типов.

**Прямые связи:** `conversation` читает `channel_connections` при приёме
(`FOR UPDATE`), `notification` — служебные обновления через `ControlSink`.

## 5. `conversation` — контакты, переписки, сообщения

**Владеет:** `contacts`, `external_identities`, `conversations`, `messages`,
`attachments` (ADR 0005, 0006). Не знает провайдеров (только строка), не
принимает решений о сделке или риске, не хранит двоичные вложения.

**Правила домена.** Направления `INCOMING`/`OUTGOING`/`SYSTEM`; типы `TEXT`,
`IMAGE`, `VOICE`, `AUDIO`, `VIDEO`, `DOCUMENT`, `OTHER`; статусы переписки
`ACTIVE`/`ARCHIVED`; `revision ≥ 0`; границы переписки согласованы (первое,
последнее сообщение и направление последнего заданы вместе); метаданные —
JSON-объект; текст — валидный UTF-8 без `\x00`; внешние идентификаторы ≤ 512.
Пространство имён внешней личности включает подключение
(`UNIQUE NULLS NOT DISTINCT (tenant, provider, connection, external_id)`).

**Приём** (`IngestCanonical`, одна транзакция): блокировка подключения →
контакт (`external_identities` под `FOR UPDATE`, last-write-wins по времени) →
переписка (`FOR UPDATE`, создание со статусом `ACTIVE`) → сообщение (повтор
идентичного изменения — `Changed:false`, расхождение содержимого — конфликт)
→ полная замена вложений → пересчёт границ и `revision + 1` → событие
`conversation.changed.v1` **только при фактическом изменении**. Правка
обновляет поля сообщения, удаление ставит `provider_deleted_at` и не стирает
строку.

**Чтение:** `List`, `Detail`, `Messages` — право `conversation.read`,
курсорная пагинация (`base64url({"at","id"})`, `limit` 1…100, по умолчанию
50). Страница сообщений вычитывается целиком, вложения загружаются одним
запросом `ANY($ids)` — вложенный запрос при открытом курсоре требовал второе
соединение и блокировал пул при конкуренции, равной его размеру.
`CommercialSnapshot` отдаёт `opportunity` последнее сообщение без проверки
прав (права проверяет владелец сценария).

## 6. `opportunity` — коммерческие возможности

**Владеет:** `opportunities`, `opportunity_stage_history` (ADR 0006, 0007).
Не создаёт риски, не считает фактическую выручку.

**Этапы и переходы.** Активные по порядку: `NEW` → `ENGAGED` → `QUALIFYING` →
`PRICE_SENT` → `WAITING_CUSTOMER` → `WAITING_BUSINESS` → `BOOKING_INTENT` →
`BOOKED`; терминальные `WON`, `LOST`, `ARCHIVED`. Разрешено: вперёд с пропуском
этапов, тот же этап (идемпотентно), из любого активного в `LOST`, в `WON`
только из `BOOKED`, в `ARCHIVED` только из `WON`/`LOST`; откат назад и
`ARCHIVED` из активного запрещены. Не больше одной активной сделки на
переписку (частичный уникальный индекс). Источники истории: `RULE`, `AI`
(обязательна уверенность), `USER` (обязателен актор), `IMPORT`. Оценка суммы
— `NUMERIC(14,2)` строкой, уверенность `NUMERIC(4,3)`; неизвестная сумма —
`nil`, а не ноль.

**Кандидат** (`CandidateProcessor.Evaluate`, задание
`opportunity.evaluate-commercial-candidate.v1`, дедупликация
`conversation:{id}:revision:{n}`): переписка `ACTIVE`, последнее сообщение
`INCOMING` + `TEXT` с текстом и не удалено; пословное вхождение названия
**ровно одной** активной услуги каталога (глобальной или той же точки);
сумма ставится только при `priceFrom == priceTo` с уверенностью 1.
Неоднозначность — без сделки.

**Реакция на AI** (`ai.analysis.applied.v1`): доверенные факты применяются в
порядке цена → follow-up → намерение записи (`PRICE_SENT` → `WAITING_CUSTOMER`
→ `BOOKING_INTENT`), только вперёд; оценка обновляется лишь в валюте сделки и
не понижает более уверенную.

**Команды пользователя:** `Detail`, `ChangeStage` (право
`opportunity.manage`, источник `USER`, аудит `OPPORTUNITY_STAGE_CHANGED`),
`MarkNotALead` (вызывается модулем `risk` при вердикте `NOT_A_LEAD`: активная
сделка → `LOST`). События: `opportunity.created.v1`,
`opportunity.stage_changed.v1` (идентификатор события = идентификатор записи
истории).

## 7. `risk` — сигналы риска, правила, Radar, обратная связь

**Владеет:** `risk_signals`, `risk_feedback`, read-моделью Radar и SSE-хабом.
Читает только для чтения `opportunities`, `conversations`, `locations`,
`location_business_hours`, `messages`, `conversation_summaries`, `outcomes`,
`recommendations`, `actions`, `revenue_*`, `organizations` (ADR 0007, 0028,
0035, 0038).

**Типы, важность, статусы.** Пять типов; важность по типу ограничена:
`NO_RESPONSE` → `HIGH`/`CRITICAL`; `BOOKING_NOT_CONFIRMED` → `CRITICAL`;
`PROMISE_NOT_FULFILLED` → `HIGH` и только `HYBRID`; `CUSTOMER_SILENT_AFTER_PRICE`
→ `MEDIUM`/`HIGH`; `FOLLOW_UP_CANDIDATE` → `MEDIUM`. Источник `RULE` без
уверенности и прогона, `HYBRID` с уверенностью 0…1 и `ai_run_id`. Активные
статусы `OPEN`/`ACKNOWLEDGED`/`ACTED`; закрывающие `RESOLVED`, `FALSE_POSITIVE`,
`IGNORED`, `EXPIRED`; `due_at ≤ detected_at`. Не больше одного активного риска
на сделку и тип (частичный уникальный индекс). `Acknowledge` идемпотентен и
не открывает закрытый риск; `Resolve` закрывает активный; `Refresh`
обновляет активный на месте; `ACTED` ставит модуль `corrective`.

**Правила** (все пороги — в рабочем времени точки; интервалы через полночь
в расписании запрещены, нужны два периода):

| Правило | Порог | Важность | Закрывает |
|---|---|---|---|
| `no-response/v1` | порог ответа точки (по умолчанию 45 мин), `CRITICAL` от 90 мин | HIGH/CRITICAL | исходящий ответ, закрытие сделки |
| `booking-not-confirmed/v1` | 30 мин после доверенного `BOOKING_INTENT` на этапах QUALIFYING…BOOKING_INTENT | CRITICAL | `BOOKED`/`WON`/`LOST`/`ARCHIVED` |
| `promise-not-fulfilled/v1` | срок из текста обещания (`через…`, `до HH:MM`, `завтра`, части дня; горизонт 14 дней) либо 60 мин | HIGH, HYBRID | любое исходящее после обещания |
| `customer-silent-after-price/v1` | 24 ч после цены, эскалация до HIGH через 48 ч (вторая проверка `…:escalation`) | MEDIUM/HIGH | входящее клиента, исход `LOST`/`NOT_A_LEAD` |
| `follow-up-candidate/v1` | 24 ч после колебания клиента | MEDIUM | входящее клиента |

Порог доверия к факту AI везде 0,85. Проверки хранят только идентификаторы
(`opportunityId`), состояние перечитывается в момент выполнения; несовпадение
организации или объекта — нарушение границы (`ErrInvalidCheck`), а не промах.
Ошибки настройки (нет 7 строк расписания, нет точки) — постоянные:
`RISK_PLAN_INVALID` / `RISK_EVALUATION_INVALID`.

**Radar и команды:** `Summary` (активные риски, критические, потенциал в
валюте организации, возвращённая выручка — последняя **не** сужается при
закрытии рисков), `List` с фильтрами `status`, `locationId`, `severity`,
`riskType`, курсор привязан к набору фильтров; порядок `severity DESC,
booking_intent DESC, estimated_amount DESC, due_at, detected_at, id`.
`Acknowledge`/`Resolve` — право `risks.manage`, аудит `RISK_ACKNOWLEDGED` /
`RISK_RESOLVED`, сигналы `risk.acknowledged` / `risk.resolved`.

**Обратная связь** (ADR 0038): вердикт `TRUE_POSITIVE`/`FALSE_POSITIVE`, для
ложного обязательна причина (`CUSTOMER_ALREADY_BOOKED`,
`CUSTOMER_ALREADY_ANSWERED`, `NOT_A_LEAD`, `CUSTOMER_REJECTED`,
`WRONG_INTERPRETATION`, `OTHER`), заметка ≤ 1000; append-only со снимком
риска и этапа; `dataset_eligible` фиксирует наличие ML-согласия в момент
вердикта; `FALSE_POSITIVE` закрывает активный риск, `NOT_A_LEAD` дополнительно
переводит сделку в `LOST`; аудит `RISK_FEEDBACK_RECORDED`. Точность по типам
за окно обнаружения: `precision = TP/(TP+FP)`, покрытие = доля рисков с
вердиктом, `reliable` при покрытии ≥ 0,5; право `analytics.read`. Сигнал
`risk.false_positive` публикуется, но фильтрами SSE отбрасывается (см.
[06-async-processing.md](06-async-processing.md)).

## 8. `corrective` — рекомендации, действия, исходы

**Владеет:** `recommendations`, `actions`, `outcomes`; делит с `revenue`
таблицы `idempotency_keys` и `audit_log`. Никогда не обращается к AI: тип
риска читается из базы, шаблон детерминирован.

**Шаблоны рекомендаций** (по одному на тип, покрытие закреплено тестом):
`NO_RESPONSE` — «Ответить клиенту сейчас.»; `BOOKING_NOT_CONFIRMED` —
«Предложить клиенту конкретный свободный слот.»; `PROMISE_NOT_FULFILLED` —
«Выполнить обещанное клиенту или сообщить новый точный срок.»;
`CUSTOMER_SILENT_AFTER_PRICE` — «Напомнить клиенту о предложении и уточнить,
остались ли вопросы.»; `FOLLOW_UP_CANDIDATE` — «Уточнить, остаётся ли услуга
актуальной.»

**Правила.** Действия `OPEN_CONVERSATION`, `COPY_REPLY`, `MARK_CONTACTED`,
`CALL`, `SEND_MESSAGE`, `OTHER`; исходы `RESPONDED`, `BOOKED`, `PAID`, `LOST`,
`THINKING`, `NOT_A_LEAD`; заметки ≤ 2000 рун. Право `risks.manage` на все три
команды; активное членство дополнительно проверяется в самом SQL
(`EXISTS memberships … status='ACTIVE'`). `Idempotency-Key` обязателен для
действий и исходов (≤ 255), хеш запроса — `sha256(actor, id, тип, заметка)`:
точный повтор возвращает сохранённый ответ (`200`), другое содержимое —
`409 IDEMPOTENCY_CONFLICT`. Всё пишется одной транзакцией: ключ, факт, перевод
риска в `ACTED` (идемпотентно, только из активных статусов), аудит
`ACTION_RECORDED` / `OUTCOME_RECORDED`. Журналы неизменяемы на уровне базы
(триггеры). После действия публикуется сигнал `risk.changed`.

## 9. `revenue` — подтверждённая выручка и атрибуция

**Владеет:** `revenue_events`, `revenue_attributions`; читает `risk_signals`,
`actions`, `outcomes`, `opportunities`, `memberships`.

**Правила.** Сумма — строго положительная десятичная строка до 12 цифр и 2
знаков (`NUMERIC(14,2)`), валюта из трёх букв; статус только `CONFIRMED`,
источник `USER_CONFIRMED`. Атрибуция одна на событие: `RECOVERED` требует
риск, действие и исход **той же сделки**, созданные не позже подтверждения и
не раньше чем за 30 дней до него (проверяется и в коде, и триггером базы);
`ORGANIC`/`UNKNOWN` требуют их отсутствия. Одна `RECOVERED` на сделку
(частичный уникальный индекс `revenue_attributions_one_recovered_per_opportunity_idx`,
повтор → `409 RECOVERED_ALREADY_ATTRIBUTED`; доплаты подтверждаются как
`ORGANIC`). Права `revenue.confirm` (подтверждение, `Idempotency-Key`
обязателен) и `revenue.read` (сумма `CONFIRMED + RECOVERED` в запрошенной
валюте). Аудит `REVENUE_CONFIRMED`; после `RECOVERED` — сигнал `risk.changed`.
Журналы append-only.

## 10. `notification` — уведомления, доставка, Telegram, политика

**Владеет:** `notifications`, `notification_deliveries`,
`notification_preferences`, `notification_digest_items`,
`telegram_user_links`, `telegram_link_tokens`, `telegram_callback_commands`
(ADR 0029, 0037). Читает `organizations`, `memberships`, `risk_signals`,
`opportunities`, `conversations`, `contacts`.

**Модель.** Логическое уведомление вида `RISK_OPENED`, `RISK_DIGEST`,
`RISK_ESCALATED` с персональным ключом (`risk:{id}:opened:user:{user}`,
`digest:user:{user}:{slot}`), заголовок ≤ 200, тело ≤ 2000. Доставка — строка
на попытку (`attempt` 1…5) по каналу `IN_APP` или `TELEGRAM`, статусы
`PENDING`/`PROCESSING`/`RETRY`/`SUCCEEDED`/`DEAD`. Сводка без кнопок.

**Политика получателя** (по типу риска, для каждого активного участника):
режим `IMMEDIATE`/`DIGEST`/`DISABLED`, порог важности, флаги каналов, тихие
часы (по умолчанию 22:00–08:00, выключены), время сводки 09:00. По
умолчанию `DIGEST` для `CUSTOMER_SILENT_AFTER_PRICE` и `FOLLOW_UP_CANDIDATE`,
`IMMEDIATE` для остальных. Тихие часы через полночь — `[start,24:00) ∪
[00:00,end)`, вырожденный интервал запрещён. `IMMEDIATE` в тихие часы →
элемент очереди сводки с причиной `QUIET_HOURS` и проверка на конец тихих
часов; `DIGEST` → следующее наступление времени сводки (со сдвигом за тихие
часы). Telegram-канал требует активной привязки. Эскалация владельцу
(`RISK_ESCALATED`) — под флагом `LIDRADAR_NOTIFICATIONS_OWNER_ESCALATION`,
через `…_AFTER` (30 мин) для рисков `HIGH`+, только если риск ещё `OPEN`.

**Сводка** (`notification.digest.v1`): элементы одного пользователя и слота,
только активные риски, до 15 строк «важность · тип: контакт, с DD.MM HH:MM»
в часовом поясе организации; без активных рисков элементы закрываются без
сообщения. Риск попадает в очередь получателя один раз (уникальность
`(tenant, user, risk)`).

**Доставка** (`DispatchOne`, worker): захват одной доставки, `IN_APP`
завершается локально; Telegram — `sendMessage` с кнопками `OPEN_RISK` /
`ACKNOWLEDGE` / `SNOOZE`; отказ 429/5xx/сеть → новая строка попытки через
5 с / 30 с / 2 мин / 10 мин (`TELEGRAM_PROVIDER_ERROR`), прочие 4xx и
`ok:false` → `DEAD`. Ни один отказ не меняет риск.

**Привязка Telegram:** одноразовый код (32 байта, хранится SHA-256, TTL 15
мин) в ссылке `https://t.me/{bot}?start=…`; `/start` только из личного чата
и организации, которой принадлежит код; `GET /telegram-link` не раскрывает
`telegram_user_id`/`chat_id`. Кнопки: белый список действий, проверка
привязки, чата, членства и принадлежности уведомления; идемпотентность по
`callback_query.id`; `ACKNOWLEDGE` вызывает `Radar.Acknowledge`, `SNOOZE`
помечает только текущее уведомление.

**Настройки:** `List` (всегда 5 записей, включая значения по умолчанию),
`Put` (полная замена, аудит `NOTIFICATION_POLICY_CHANGED`), `Reset`
(`NOTIFICATION_POLICY_RESET`); право — активное членство (`notification.manage`
входит только в набор владельца; настройки личные).

## 11. `ai` — очередь анализа и домашний узел

Полностью описан в [07-ai.md](07-ai.md). Кратко: владеет `ai_nodes`,
`ai_node_tenants`, `ai_node_request_nonces`, `ai_jobs`, `ai_runs`,
`conversation_summaries`; ставит одно ожидающее задание на переписку с
дебаунсом 60 с; узел забирает задания подписанными запросами с арендой 120 с
и потолком 15 мин; результат проходит строгую схему и проверку свежести;
факты с уверенностью ≥ 0,85 становятся доверенными и читаются правилами
`risk` и переходами `opportunity`.

## 12. `events` — транзакционный outbox

**Владеет:** `outbox_events` (ADR 0027). `AppendTx` пишет событие в
транзакции владельца изменения; идентичность события задаётся его `id`
(повтор с тем же содержимым — не ошибка, с другим — конфликт). Событие:
`type` + `version` → ключ `type.vN`, `data` — JSON-объект, `max_attempts = 5`.
Диспетчер (`RunOne`) захватывает одно событие под аренду 30 с, находит
обработчик по ключу (нет обработчика → сразу `DEAD` с
`UNSUPPORTED_EVENT_TYPE`), выполняет его в контексте организации события,
подтверждает или помечает `RETRY`/`DEAD`. Несколько подписчиков на один тип
— `ChainHandlers` по порядку.

## 13. `jobs` — очередь заданий и отложенные проверки

**Владеет:** `jobs`, `scheduled_checks` (ADR 0026). Задание: тип ≤ 100,
`dedup_key` ≤ 512 (уникален в паре с типом и организацией и **не истекает**),
payload — JSON-объект, приоритет, `max_attempts = 5`. `Enqueue` идемпотентен
по ключу (тот же payload → существующее задание, иной → конфликт). `Claim`
— `FOR UPDATE SKIP LOCKED`, порядок `priority DESC, available_at, created_at,
id`, аренда 30 с; истёкшие аренды с исчерпанными попытками помечаются `DEAD`
(`LEASE_EXPIRED_MAX_ATTEMPTS`). Обработчик выполняется в контексте
организации; успех → `SUCCEEDED`; ошибка классифицируется (`Permanent` →
`DEAD` сразу, иначе `RETRY` через 5 с / 30 с / 2 мин / 10 мин, после пятой
попытки `DEAD`). Проверка по расписанию: `check_type` + `dedup_key`
уникальны, `PromoteDue` одной транзакцией переводит до 100 просроченных в
задания с тем же `id`, `available_at = due_at` (`SCHEDULED` → `ENQUEUED`);
повторное планирование того же основания с другим сроком — конфликт.

## 14. `analytics` — сводка показателей

Собственных таблиц нет (ADR 0039). `Summary` — право `analytics.read`;
период — календарные даты `YYYY-MM-DD` в часовом поясе организации, по
умолчанию 30 дней, максимум 366; одна транзакция `REPEATABLE READ READ ONLY`.
Метрики: сообщения (всего, входящие, исходящие, переписки), сделки (созданы,
`BOOKED`, `WON`, `LOST` по истории), риски (обнаружены, с действием,
закрыты, ложные) по типам, исходы, деньги в валюте организации (потенциал
активных сделок, подтверждённая выручка, из неё `RECOVERED`, число платежей).
Все суммы — точные десятичные строки.

## 15. `admin` — платформенное администрирование

**Владеет:** `platform_admins`, `admin_audit_log`, признаком
`discarded_at/discarded_by` у четырёх очередей (ADR 0040). Работает вне
организации на пуле роли `lidradar_platform`; каждый вызов, кроме
`GET /admin/me`, требует активной строки в `platform_admins` (и `ACTIVE`
пользователя). Первый администратор — только CLI `platform-admin grant
--email`. Read-модели: организации со счётчиками, каналы, панель очередей,
последние задания, мёртвые элементы (не отложенные), AI-узлы и прогоны,
семантический результат переписки, потребление за окно (30 дней по
умолчанию, максимум 366), трасса «сообщение → задания → AI → риски →
уведомления → действия → исходы → выручка». Команды: `retry`/`replay`
(только `DEAD`: сброс попыток, `PENDING`, для AI — с учётом единственности
ожидающего задания), `discard` (только `DEAD` и ещё не отложенное). Каждая
команда пишет `admin_audit_log` в той же транзакции (`ADMIN_JOB_RETRIED`,
`ADMIN_EVENT_REPLAYED`, `ADMIN_AI_JOB_RETRIED`, `ADMIN_*_DISCARDED`,
`PLATFORM_ADMIN_GRANTED/REVOKED`). Read-модели никогда не отдают текст
сообщений, промпты, сырой вывод модели, текст резюме и тексты уведомлений.

## 16. `audit` — журналы

**Владеет:** `audit_log` (действия участника, актор обязан быть участником
организации по внешнему ключу) и `auth_audit_log` (вход, выход, регистрация;
из сетевых данных только IP ≤ 64 символов). Контракт `Recorder.Tenant` /
`Recorder.Auth`; чтения по HTTP нет. Имена операций и типов сущностей —
`^[A-Z][A-Z0-9_]{0,99}$`; оба журнала append-only (триггеры). Кто пишет:
identity (3 операции входа), tenant (`ORGANIZATION_UPDATED`, `MEMBER_ADDED`,
`ML_CONSENT_*`), connector (`INTEGRATION_*`), opportunity
(`OPPORTUNITY_STAGE_CHANGED`), risk (`RISK_ACKNOWLEDGED`, `RISK_RESOLVED`,
`RISK_FEEDBACK_RECORDED`), corrective (`ACTION_RECORDED`, `OUTCOME_RECORDED`),
revenue (`REVENUE_CONFIRMED`), notification (`NOTIFICATION_POLICY_*`).
`catalog` аудит не пишет.

## 17. Вспомогательные пакеты `internal`

- `testsupport` — изолированные схемы на тест, пулы четырёх ролей,
  трассировщик запросов (см. [11-testing.md](11-testing.md)).
- `loadgen` — генератор синтетического набора для нагрузочного испытания и
  staging; пишет таблицы всех модулей напрямую (ADR 0042), запрещён в
  `production`.
- `integration` — сквозные тесты с полным API-стеком в процессе.

## 18. Карта прямых чтений и записей чужих таблиц

| Модуль | Читает | Пишет | Основание |
|---|---|---|---|
| `conversation` | `channel_connections` (блокировка при приёме) | — | ADR 0025 |
| `opportunity` | `conversation_summaries`, `messages` (доверенные факты) | — | ADR 0008 |
| `risk` | таблицы переписок, сделок, точек, расписаний, фактов, исходов, действий, выручки | — | ADR 0007, 0038 |
| `corrective` | `risk_signals`, `opportunities`, `memberships` | `risk_signals.status → ACTED` | спецификация «Рекомендации, действия и исходы» |
| `revenue` | `risk_signals`, `actions`, `outcomes`, `opportunities`, `memberships` | — | спецификация «Выручка и атрибуция» |
| `notification` | `organizations`, `memberships`, `risk_signals`, `opportunities`, `conversations`, `contacts` | — | ADR 0029, 0037 |
| `ai` | `organizations`, `locations`, `conversations`, `messages` (контекст промпта) | — | ADR 0008 |
| `analytics` | таблицы фактов всех модулей | — | ADR 0039 |
| `admin` | все таблицы | статусы четырёх очередей | ADR 0040 |
| `loadgen` | — | все таблицы (испытания) | ADR 0042 |
