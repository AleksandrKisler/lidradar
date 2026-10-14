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
- идентификаторы из пути запроса проверяются на формат UUID в `transport`
  (`pathID`) до вызова `application`: неверное значение получает
  `400 INVALID_ARGUMENT` и не доходит до хранилища;
- обязательный аудит является требованием ТЗ §65, но граница зависит от команды:
  денежные факты, ML-согласие и admin-команды описаны как транзакционные;
  post-commit запись ADR 0041 не гарантирует след при отказе.
  См. [матрицу аудита и открытый gate](12-critical-commands.md#audit);
- идентификаторы организаций (`tenantId`) наружу в JSON не отдаются
  (`json:"-"`).

## 1. `identity` — пользователи и сессии

**Владеет:** `users` (включая `organization_rights`, ADR 0057,
`email_verified_at`, ADR 0058, и колонки закрытия учётной записи
`deletion_requested_at`, `deletion_due_at`, `deleted_at`, ADR 0059), `sessions`,
`auth_rate_limits`, `account_tokens` (ADR 0058). **Не владеет** организациями,
ролями и правами: `/auth/me` получает членства через узкий контракт
`MembershipLister` модуля `tenant`. При регистрации читает и погашает
`platform_invitations` (таблица модуля `admin`), а коды сотрудников проверяет и
принимает через узкий контракт `MemberInvitations`, который реализует `tenant`.
Письма ставит в очередь через контракт `Mailer` (`Enqueue`), который реализует
модуль `mail`. Данные для выгрузки личных данных и причины, мешающие закрыть
учётную запись, получает через контракт `AccountData`, который реализует
`lifecycle` (ADR 0059).

**Регистрация по приглашениям** (ADR 0057). Режим `RegistrationMode` задаёт
конфигурация (`open` в `development` и `test`, `invite_only` в `staging` и
`production`); нулевое значение службы считается закрытым. `Register` принимает
`RegistrationInput` с необязательным `InviteCode`: пустой код допустим только в
`open`; код определяет вид без расхода — сначала `platform_invitations`
(`PlatformInvitationUsable`), затем контракт `CheckMemberInvitation`. Код платформы
гасится в одной транзакции с вставкой пользователя и сессии: строка кода
блокируется до вставки (недействительный код отвечает раньше проверки почты),
пользователь получает `organization_rights = 1`. Код сотрудника: пользователь
создаётся, затем `AcceptMemberInvitation`; однозначный отказ приёма (гонка)
удаляет только что созданного пользователя (`DeleteNewUser`, только без следов),
неоднозначный сбой оставляет его. Занятая почта код не расходует. Любая причина
недействительности даёт `ErrInvitationInvalid`, отсутствие кода в закрытом
режиме — `ErrInvitationRequired`.

**Правила домена.** Email нормализуется (`ToLower(TrimSpace)`), должен
разбираться `net/mail` без имени отправителя и быть не длиннее 254 символов.
Имя пользователя 1…200 символов. Пароль 12…1024 байт. Сессия хранит только
`sha256(token)` (64 hex), срок обязан быть позже создания, `User-Agent`
обрезается до 1024 символов. Статусы пользователя `ACTIVE`/`DISABLED`.

**Сервис** (`application.Service`): `Register`, `Login`, `Authenticate`,
`Logout`, `Refresh`, `Mode`, а также команды безопасности учётной записи (ADR 0058):
`ChangePassword`, `RequestPasswordReset`, `ResetPassword`,
`RequestEmailVerification`, `VerifyEmail`, и команды выгрузки и закрытия учётной
записи (ADR 0059): `ExportAccountData`, `RequestAccountDeletion`,
`CancelAccountDeletion`. Троттлинг в PostgreSQL (ADR 0032), отпечаток субъекта —
`sha256(lower(trim(value)))`:

| Scope | Лимит | Окно | Где |
|---|---|---|---|
| `REGISTER_IP` | 5 | 1 ч | регистрация |
| `LOGIN_IP` | 20 | 1 мин | вход |
| `LOGIN_ACCOUNT` | 5 | 15 мин | вход, сбрасывается при успехе |
| `REFRESH_IP` | 60 | 1 мин | ротация сессии |
| `PASSWORD_RESET_IP` | 10 | 1 ч | запрос сброса |
| `PASSWORD_RESET_ACCOUNT` | 3 | 1 ч | запрос сброса, субъект — адрес почты, до поиска пользователя |
| `PASSWORD_RESET_CONFIRM_IP` | 20 | 1 ч | завершение сброса |
| `PASSWORD_CHANGE_ACCOUNT` | 5 | 15 мин | смена пароля и подтверждение паролем выгрузки и закрытия (один счётчик на все три команды), субъект — пользователь, до проверки пароля, сбрасывается при успехе |
| `EMAIL_VERIFICATION_ACCOUNT` | 3 | 1 ч | повторное письмо подтверждения |
| `EMAIL_VERIFY_IP` | 30 | 1 ч | подтверждение по токену |
| `ACCOUNT_EXPORT_ACCOUNT` | 3 | 1 ч | выгрузка личных данных, субъект — пользователь (ADR 0059) |
| `ACCOUNT_DELETION_ACCOUNT` | 5 | 1 ч | запрос и отмена закрытия учётной записи: каждое действие шлёт письмо (ADR 0059) |

Вход по неизвестному email выполняет холостое хеширование пароля, чтобы время
ответа не выдавало существование учётной записи; неверный пароль и `DISABLED`
дают один и тот же `401 INVALID_CREDENTIALS`. Ошибка троттлера (недоступная
база) не пропускает запрос. Аудит: `USER_REGISTERED`, `USER_LOGGED_IN`,
`USER_LOGGED_OUT` в `auth_audit_log`; у `Refresh` аудита нет. Команды безопасности
учётной записи пишут свои операции (см. ниже) в той же транзакции.

**Безопасность учётной записи** ([ADR 0058](../adr/0058-account-security-and-mail.md)).
*Токены:* `account_tokens` (`EMAIL_VERIFICATION` — 7 суток, `PASSWORD_RESET` — 60
минут), 256 бит `base64url`, в базе только SHA-256, одноразовые; выпуск нового
отзывает неиспользованные прежние того же назначения (частичный уникальный
индекс гарантирует один действующий токен на пользователя и назначение). Ссылка
в письме несёт токен во фрагменте (`#token=`). *Смена пароля* — одна транзакция:
пароль меняется условно по прежнему хэшу (иначе `ErrConflict` →
`INVALID_CURRENT_PASSWORD`), остальные сессии отзываются, токены сброса
отзываются, пишется `PASSWORD_CHANGED`. *Запрос сброса* отвечает одинаково для
любого адреса; для действующего пользователя токен и `PASSWORD_RESET_REQUESTED`
пишутся одной транзакцией, затем письмо ставится в очередь (ошибки после поиска
пользователя только логируются). *Завершение сброса:* сначала дешёвая проверка
токена (`AccountTokenUsable`), затем хэш пароля, затем транзакция — токен гасится,
пароль меняется, все сессии и прочие токены сброса отзываются, адрес
подтверждается, пишется `PASSWORD_RESET_COMPLETED`. *Подтверждение почты* мягкое:
`CompleteEmailVerification` гасит токен и ставит `email_verified_at` (аудит
`EMAIL_VERIFIED` только при изменении). Порядок блокировок во всех командах один —
строка пользователя, затем строки токенов: взаимная блокировка исключена
(тест `TestIssueResetAndChangeDoNotDeadlock`). Аудит пишется в
`auth_audit_log` в той же транзакции (документированная прямая запись, раздел 20);
вход, выход и регистрация по-прежнему пишутся после фиксации.

**Выгрузка личных данных и закрытие учётной записи** ([ADR 0059](../adr/0059-data-export-and-account-closure.md)).
Обе опасные команды подтверждаются текущим паролем (`confirmPassword`: общий предел
`PASSWORD_CHANGE_ACCOUNT`, счётчик расходуется до проверки). *Личная выгрузка*
(`ExportAccountData`): пароль, предел `ACCOUNT_EXPORT_ACCOUNT`, запись аудита
`ACCOUNT_DATA_EXPORTED` **до** выдачи тела, затем документ собирает
`AccountData.PersonalExport`. *Закрытие* (`RequestAccountDeletion`): пароль, предел
`ACCOUNT_DELETION_ACCOUNT`; уже назначенное удаление возвращает прежний срок без
записи и письма; иначе `AccountData.DeletionBlockers` (единственный владелец
организации, платформенный администратор → `DeletionBlockedError`), затем
`ScheduleDeletion` одной транзакцией с аудитом `ACCOUNT_DELETION_REQUESTED`: срок —
`AccountDeletionGrace` (30 суток, константа политики `retention-v1`). Отправка письма
о запросе — после фиксации, её сбой действие не отменяет. *Отмена*
(`CancelAccountDeletion`): без пароля, идемпотентна, `CancelDeletion` пишет
`ACCOUNT_DELETION_CANCELLED` в той же транзакции. Сама работа обезличивания — в
модуле `lifecycle` (раздел 18). Условие `status = 'ACTIVE'` в `INSERT` сессии
(`FOR SHARE` на строку пользователя) не даёт параллельному входу оставить сессию у
пользователя, которого именно сейчас обезличивают.

**Транспорт:** `/api/v1/auth/*`, cookie `lidradar_session` (`HttpOnly`,
`SameSite=Strict`, `Secure` по конфигурации, `MaxAge = LIDRADAR_SESSION_TTL`,
по умолчанию 30 суток). `Resolver.User`/`Resolver.Principal` — то, чем
остальные модули узнают актора и организацию.

**Хранение:** токен — 32 случайных байта в `base64.RawURLEncoding`; ротация
под `FOR UPDATE OF s` с проверкой `RowsAffected == 1` (гонка ротации не даёт
двух действующих токенов). Таблицы модуля без `tenant_id`, RLS на них нет.

## 2. `tenant` — организации, членства, точки, ML-согласие

**Владеет:** `organizations`, `memberships`, `membership_invitations`,
`locations`, `location_business_hours`, `ml_consents` и **единственной
таблицей прав** (`PermissionService`), которой пользуются все модули. Для
списка команды читает `users` (почта, имя), для статуса онбординга —
`service_catalog_items`, `channel_connections`, `telegram_user_links`
(ADR 0045).

**Правила домена.** Организация: имя 1…200, валидная IANA-зона, валюта из трёх
букв `A–Z` (по умолчанию `RUB`), статусы `ACTIVE`/`SUSPENDED`/`ARCHIVED`.
Членство: роли `OWNER`/`MANAGER`, статусы `ACTIVE`/`INVITED`/`DISABLED`;
физически не удаляется (триггер), отзыв = `DISABLED` + `revoked_at`. Точка:
имя 1…200, зона, порог ответа 1…1440 мин (по умолчанию 45), часы работы —
ровно 7 строк, `weekday` 1…7, `opens < closes` формата `HH:MM`, закрытый день
без границ. ML-согласие: единственная область `DATASETS`, одно действующее
согласие на организацию, история хранится, повторный отзыв и удаление
запрещены триггером. Для новой ML-партии исторического `dataset_eligible`
недостаточно: активное согласие, отзыв и retention проверяются заново по
[политике данных](13-data-lifecycle.md#consent).

**Права по ролям** (`application/service.go`):

| Право | OWNER | MANAGER |
|---|---|---|
| `risks.read`, `risks.manage`, `conversation.read`, `opportunity.manage`, `action.manage`, `outcome.manage`, `revenue.confirm` | ✓ | ✓ |
| `revenue.read`, `analytics.read`, `integration.manage`, `organization.manage`, `location.manage`, `service.manage`, `notification.manage`, `member.manage`, `organization.export` (ADR 0059) | ✓ | — |

`Allowed` требует членство `ACTIVE` **и** организацию `ACTIVE`: приостановленная
или архивная организация теряет все права разом.

**Сервис:** `CreateOrganization` (только аутентификация; создаёт организацию и
`OWNER`-членство атомарно и подставляет новый `tenant_id` в контекст, чтобы
прошёл `WITH CHECK` RLS), `GetOrganization`, `UpdateOrganization` (аудит
`ORGANIZATION_UPDATED`), `AddMember` (аудит `MEMBER_ADDED`; HTTP-маршрута нет,
вызывается из тестов), `MembershipsForUser`, `ListLocations`, `CreateLocation`,
`UpdateLocation`, `ReplaceBusinessHours`, `MLConsent`, `GrantMLConsent`
(аудит `ML_CONSENT_GRANTED` в той же транзакции), `RevokeMLConsent`
(`ML_CONSENT_REVOKED`).

**Команда и приглашения** (ADR 0045, право `member.manage`): `ListMembers`
(членства с почтой и именем, включая `DISABLED`; у обезличенной учётной записи
адрес пустой, а имя «Удалён», ADR 0059), `ChangeMemberRole` (аудит
`MEMBER_ROLE_CHANGED`), `RevokeMember` (`DISABLED` + `revoked_at`, аудит
`MEMBER_REVOKED`, повтор идемпотентен) — обе команды блокируют все членства
организации `FOR UPDATE` и отклоняют понижение/отзыв последнего активного
владельца (`ErrLastOwner`), включая собственное; `CreateInvitation` (256-битный
код `base64url`, хранится SHA-256, срок 7 дней, аудит `MEMBER_INVITED` в той же
транзакции), `ListInvitations` (статус `PENDING`/`ACCEPTED`/`REVOKED`/`EXPIRED`
выводится из отметок), `RevokeInvitation` (принятое → `ErrInvitationUsed`,
аудит `INVITATION_REVOKED`), `AcceptInvitation` (сеанс без организации: строка
находится по хешу кода через политику `invitation_by_code`, затем локальный
контекст организации; создаёт членство или восстанавливает отозванное с ролью
приглашения; активный участник → `ErrAlreadyMember`; аудит
`INVITATION_ACCEPTED` от имени нового участника). Статус `INVITED` членства
по-прежнему не присваивается: до приёма кода членства нет. Вставка и
восстановление членства (`insertMembership`) выполняются только для пользователя
со статусом `ACTIVE` (`FOR SHARE` на его строку): приём приглашения не оставит
членства у учётной записи, которую в этот момент обезличивают (ADR 0059).

**Онбординг:** `Onboarding` (активное членство) — `OnboardingFacts` одним
запросом и детерминированное правило `OnboardingFrom`: обязательные шаги
`ORGANIZATION`, `LOCATION` (активная точка с семью строками графика),
`SERVICES` (активная услуга), `CHANNEL` (подключение не `DISCONNECTED`);
`TELEGRAM_LINK` необязателен и считается по текущему пользователю.

**Транспорт:** `/api/v1/organizations`, `/api/v1/organization`,
`/api/v1/organization/onboarding`, `/api/v1/organization/members`,
`/api/v1/organization/invitations`, `/api/v1/invitations/accept`,
`/api/v1/locations` (поле `active` в `POST` отклоняется),
`/api/v1/organization/ml-consent` (монтируется на `/api/v1` последним,
поэтому более специфичные префиксы имеют приоритет).

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

**Сервис** (`application.Service`): `Connect` (секрет 16…256 байт либо
пустой — тогда сервер выпускает 256-битный `base64url`-секрет и возвращает его
один раз в `ConnectResult.WebhookSecret` для провайдеров без удалённой
регистрации; для Telegram — токен по маске, шифрование AES-256-GCM с AAD
`lidradar:v1:{tenant}:{provider}:{connection}`, запись в базу до сетевого
вызова, затем `setWebhook`; ошибка провижининга сохраняется как
`ERROR/TELEGRAM_WEBHOOK_SETUP_FAILED`), `List`, `Health` (сохранённое
состояние), `CheckHealth` (живая проверка: `Provisioner.Verify` →
`getWebhookInfo`, сравнение адреса, результат сохраняется — `ACTIVE`,
`ERROR/TELEGRAM_WEBHOOK_MISMATCH` или `ERROR/TELEGRAM_WEBHOOK_CHECK_FAILED`,
`verification: REMOTE`; провайдеры без регистрации и `DISCONNECTED` —
сохранённое состояние, `LOCAL`; ADR 0045), `Disconnect`
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

**Внешняя ссылка на собеседника** (`domain.ContactDeepLink`, ADR 0044):
единственная разрешённая схема — `tg://user?id=<число>` для
`CONNECTED_BUSINESS_BOT` по внешнему идентификатору контакта в канале; иначе
причины `PROVIDER_UNSUPPORTED` / `IDENTITY_UNKNOWN`. Правило используют модели
чтения `risk` и `conversation`.

**Прямые связи:** `conversation` читает `channel_connections` при приёме
(`FOR UPDATE`) и в списке, `risk` — в карточке риска, `notification` —
служебные обновления через `ControlSink`.

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
курсорная пагинация (`base64url({"at","id","f"?})`, `limit` 1…100, по
умолчанию 50; `f` — отпечаток фильтров списка, курсор с другими фильтрами
отвергается). `List` отдаёт `ConversationListItem`: переписка, контакт
(`id`, `displayName`), снимок канала, последнее не удалённое у поставщика
сообщение с превью ≤ 140 символов, сводка активных рисков (`count`,
`maxSeverity`) и внешняя ссылка — одним запросом с `LATERAL`-подзапросами
(читает `channel_connections`, `external_identities`, `risk_signals`,
`opportunities`; ADR 0044). Фильтры: `search` (имя, телефон по цифрам, почта;
`ILIKE` с экранированием метасимволов, ≤ 100 символов), `withRisk`,
`locationId`, `connectionId`, `status`. `Detail` дополняет контакт снимком
канала и внешней ссылкой; каноническая `Conversation` от канала не зависит.
Страница сообщений вычитывается целиком, вложения загружаются одним
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
с поддержкой регулярных русских падежных форм (например, «на полировку»)
**ровно одной** активной услуги каталога (глобальной или той же точки);
сумма ставится только при `priceFrom == priceTo` с уверенностью 1.
Неоднозначность, в том числе между точным написанием и формой другой услуги, —
без сделки. Порядок и полный набор слов названия сохраняются; синонимы,
опечатки и нерегулярные формы не угадываются. Границы поддержанного правила
описаны в [спецификации](../spec/BACKEND_SPEC.md#коммерческие-возможности).

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
Читает только для чтения `opportunities`, `conversations`, `contacts`,
`channel_connections`, `external_identities`, `service_catalog_items`,
`locations`, `location_business_hours`, `messages`, `conversation_summaries`,
`outcomes`, `recommendations`, `actions`, `revenue_*`, `organizations`
(ADR 0007, 0028, 0038, 0044).

**Типы, важность, статусы.** Пять типов; важность по типу ограничена:
`NO_RESPONSE` → `HIGH`/`CRITICAL`; `BOOKING_NOT_CONFIRMED` → `CRITICAL`;
`PROMISE_NOT_FULFILLED` → `HIGH` и только `HYBRID` (или `MANUAL`, когда риск заводит человек); `CUSTOMER_SILENT_AFTER_PRICE`
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
закрытии рисков) и `List` с общими фильтрами `statuses` (несколько статусов;
в HTTP — повторяемый `status` или `active=true|false`), `locationId`,
`severity`, `riskType`; набор статусов нормализуется и входит в ключ курсора;
порядок `severity DESC, booking_intent DESC, estimated_amount DESC, due_at,
detected_at, id`. Модель чтения `Detail` обогащена одним запросом (ADR 0044):
контакт, услуга сделки, канал, превью последнего сообщения и внешняя ссылка;
ранжирование и курсор считаются только по `risk_signals` и `opportunities`,
обогащение — для строк отобранной страницы, корректирующие факты — пакетно.
Все связи в JSON присутствуют всегда (`null` при отсутствии). Денежные поля
видны любому обладателю `risks.read`. `Acknowledge`/`Resolve` — право
`risks.manage`, аудит `RISK_ACKNOWLEDGED` / `RISK_RESOLVED`, сигналы
`risk.acknowledged` / `risk.resolved`. SSE-хаб при переполнении буфера
подписчика (16) отбрасывает сигналы и посылает `resync.required`.

**Обратная связь** (ADR 0038): вердикт `TRUE_POSITIVE`/`FALSE_POSITIVE`, для
ложного обязательна причина (`CUSTOMER_ALREADY_BOOKED`,
`CUSTOMER_ALREADY_ANSWERED`, `NOT_A_LEAD`, `CUSTOMER_REJECTED`,
`WRONG_INTERPRETATION`, `OTHER`), заметка ≤ 1000; append-only со снимком
риска и этапа; `dataset_eligible` фиксирует наличие ML-согласия в момент
вердикта; `FALSE_POSITIVE` закрывает активный риск, `NOT_A_LEAD` дополнительно
переводит сделку в `LOST`; аудит `RISK_FEEDBACK_RECORDED`. Точность по типам
за окно обнаружения: `precision = TP/(TP+FP)`, покрытие = доля рисков с
вердиктом, `reliable` при покрытии ≥ 0,5; право `analytics.read`. Сигнал
`risk.false_positive` входит в допустимые SSE-сигналы и требует перечитать
Radar (см. [06-async-processing.md](06-async-processing.md)).

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
`THINKING`, `NOT_A_LEAD`; заметки ≤ 2000 рун. Права: рекомендация —
`risks.manage`, действие — `action.manage`, исход — `outcome.manage` (у OWNER
и MANAGER есть все три; ADR 0044); активное членство дополнительно
проверяется в самом SQL
(`EXISTS memberships … status='ACTIVE'`). `Idempotency-Key` обязателен для
действий и исходов (≤ 255), хеш запроса — `sha256(actor, id, тип, заметка)`:
точный повтор возвращает сохранённый ответ (`200`), другое содержимое —
`409 IDEMPOTENCY_CONFLICT`. Всё пишется одной транзакцией: ключ, факт, перевод
риска в `ACTED` (идемпотентно, только из активных статусов), аудит
`ACTION_RECORDED` / `OUTCOME_RECORDED`. Журналы неизменяемы на уровне базы
(триггеры). После действия публикуется сигнал `risk.changed`.

Новый Action допустим только при `OPEN`, `ACKNOWLEDGED`, `ACTED`;
`RESOLVED`, `FALSE_POSITIVE`, `IGNORED`, `EXPIRED` дают `409 RISK_CLOSED`.
Идемпотентность проверяется до статуса: точный replay успешного Action
возвращает прежний `200` даже после закрытия. Статус и новая запись проверяются
под блокировкой Risk в одной транзакции; close, зафиксированный первым,
не оставляет Action, ключа или аудита. Это норма QA-07;
текущее выполнение проверяется через [денежный gate](../engineering/RELEASE_GATES.md#rg-money).

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
активных сделок, подтверждённая выручка, из неё `RECOVERED`, число платежей),
дневной ряд `series` (по каждой дате окна в часовом поясе организации:
входящие, исходящие, обнаруженные риски, подтверждённые деньги, из них
`RECOVERED`, число оплат; нули заполняются) и разбивка `attribution`
(`RECOVERED`, `ORGANIC`, `UNKNOWN` — сумма и число). `Payments` — курсорный
список подтверждённых событий окна во всех валютах с контактом, услугой и
атрибуцией (`confirmed_at DESC, id DESC`; читает `contacts`,
`conversations`, `service_catalog_items`). Все суммы — точные десятичные
строки.

## 15. `admin` — платформенное администрирование

**Владеет:** `platform_admins`, `admin_audit_log`, `platform_invitations`
(коды платформы, ADR 0057), признаком `discarded_at/discarded_by` у четырёх
очередей (ADR 0040). Работает вне
организации на пуле роли `lidradar_platform`; каждый вызов, кроме
`GET /admin/me`, требует активной строки в `platform_admins` (и `ACTIVE`
пользователя). Первый администратор — только CLI `platform-admin grant
--email`. Остальные команды CLI (ADR 0057): `invite [--note] [--days]` выпускает код
платформы (открытый код печатается один раз), `invitations` перечисляет без кодов,
`revoke-invitation --id` отзывает ожидающий код, `allow-organization --email`
добавляет право создать организацию; каждая пишет `admin_audit_log` с источником
`CLI` в своей транзакции (`PLATFORM_INVITATION_ISSUED/REVOKED`,
`ORGANIZATION_RIGHT_GRANTED`), заметка в аудит не попадает. Read-модели: организации со счётчиками, каналы, панель очередей,
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
identity (3 операции входа, 4 операции безопасности учётной записи: `PASSWORD_CHANGED`,
`PASSWORD_RESET_REQUESTED`, `PASSWORD_RESET_COMPLETED`, `EMAIL_VERIFIED`; ADR 0058, и 3
операции выгрузки и закрытия: `ACCOUNT_DATA_EXPORTED`, `ACCOUNT_DELETION_REQUESTED`,
`ACCOUNT_DELETION_CANCELLED`; ADR 0059), lifecycle (`ACCOUNT_DELETION_BLOCKED`,
`ACCOUNT_ANONYMIZED` в `auth_audit_log`, `ORGANIZATION_DATA_EXPORTED` в `audit_log`;
ADR 0059), tenant (`ORGANIZATION_UPDATED`, `MEMBER_ADDED`, `ML_CONSENT_*`), connector (`INTEGRATION_*`), opportunity
(`OPPORTUNITY_STAGE_CHANGED`), risk (`RISK_ACKNOWLEDGED`, `RISK_RESOLVED`,
`RISK_FEEDBACK_RECORDED`), corrective (`ACTION_RECORDED`, `OUTCOME_RECORDED`),
revenue (`REVENUE_CONFIRMED`), notification (`NOTIFICATION_POLICY_*`).
`catalog` аудит не пишет.

## 17. `mail` — исходящая почта

**Владеет:** `mail_outbox` ([ADR 0058](../adr/0058-account-security-and-mail.md))
и транспортами писем. HTTP-слоя нет. Другие модули ставят письмо в очередь узким
контрактом `Enqueue(kind, to, subject, text, expiresAt)` (строки и время, без типов
почты), саму таблицу не читают и не пишут. Письма — обычный текст UTF-8 без HTML,
без имени получателя и без персональных данных в теме и тексте.

**Правила домена.** Адрес — простой адрес `net/mail` без имени, ≤ 254 символов, без
управляющих символов (защита от внедрения заголовков); тема 1…200 символов без
управляющих; текст ≤ 20 000 байт, допустимы только `\n`, `\r`, `\t`; срок
(`expires_at`) позже создания. Состояния `PENDING → PROCESSING → SENT | DEAD`;
повтор возвращает письмо в `PENDING` с отложенным `available_at`.

**Доставка** (`application.Service.DispatchOne`, цепочка воркера, не больше
одной попытки за вызов). `Claim` арендует самое раннее готовое письмо
(`FOR UPDATE SKIP LOCKED`; ожидающее с наступившим сроком или арендованное с
истёкшей арендой, 2 минуты). Письмо с истёкшим `expires_at` не отправляется
(`EXPIRED`); максимум 8 попыток (`ATTEMPTS_EXHAUSTED`); паузы 5 с, 30 с, 2 мин,
затем по 10 мин; повтор, который начался бы после срока ссылки, не планируется.
Временный сбой (SMTP `4xx`, сеть, таймаут, TLS, отказ входа `530/534/535`) повторяется,
постоянный (остальные `5xx`) завершает письмо как `DEAD`. Результат принимается
только от арендатора (`domain.ErrLeaseLost` иначе): доставка «хотя бы раз».
**При любом завершении адрес и текст стираются** (CHECK таблицы), остаются вид,
тема, домен получателя, число попыток, код сбоя и время. В журнале воркера — только
идентификатор, вид, номер попытки и код (без адресов и ссылок): события
`mail.sent`, `mail.retry`, `mail.failed`, `mail.lease_lost`, раз в минуту
`mail.queue.status`.

**Транспорты.** `SMTPTransport` (стандартная библиотека): TLS обязателен —
`starttls` (по умолчанию, порт 587; сервер обязан предложить STARTTLS, возврата к
открытому каналу нет, пароль до TLS не уходит) или `tls` (порт 465); проверка
сертификата и TLS 1.2 не отключаются; вход PLAIN; срок одной отправки 30 с;
письмо собирается из проверенных значений (тема по RFC 2047, тело base64).
`security=none` допустим только для loopback-хоста вне staging и production.
`Check` проверяет доступ без отправки (воркер вызывает её при старте и пишет
`mail.smtp.ready` или `mail.smtp.check_failed`). `LogTransport` пишет письмо с
адресом и ссылкой в журнал воркера (`mail.logged`) и допустим только в
`development` и `test` при пустом `LIDRADAR_MAIL_SMTP_HOST`.

**Пул и роль.** Таблица без `tenant_id` и RLS: API ставит письма под ролью
приложения, воркер доставляет под рабочей ролью.

## 18. `lifecycle` — выгрузки и обезличивание

**Владеет:** реестром данных и тремя сценариями ([ADR 0059](../adr/0059-data-export-and-account-closure.md)):
выгрузкой данных организации, личной выгрузкой пользователя и исполнителем
обезличивания закрытых учётных записей. Собственных таблиц у модуля нет: он читает и
пишет таблицы других модулей по реестру (карта — раздел 20). Команды пользователя
(подтверждение паролем, пределы, письма о запросе и отмене) принадлежат `identity`;
`lifecycle` реализует его узкий контракт `AccountData` (`DeletionBlockers`,
`PersonalExport`). Права на выгрузку организации проверяет через
`tenant.PermissionService`, аудит пишет через `audit.Recorder`, письма ставит в очередь
контрактом `Mailer` модуля `mail`.

**Реестр данных** (`domain/registry.go`) — код, а не договорённость. Три списка:
`UserReferences` (31 колонка, указывающая на пользователя или членство, с судьбой
строк при обезличивании: `ERASE`, `ANONYMIZE`, `REVOKE`, `RETAIN` — и разделом личной
выгрузки), `OrganizationDatasets` (23 набора выгрузки организации: таблица, файл,
**явный список колонок**, порядок строк) и `ExcludedTables` (16 таблиц с `tenant_id`) с
`ExcludedColumns` (секреты и служебные колонки выгружаемых таблиц) и причиной каждого
исключения. Тесты на живой схеме (`TestRegistryClassifiesEveryReferenceToUsers`,
`TestRegistryClassifiesEveryTenantTable`,
`TestDatasetColumnsAreExactlyTheSchemaColumnsMinusTheExcludedOnes`) падают, если
появилась колонка, указывающая на `users` или на членство, таблица с `tenant_id` или
колонка выгружаемой таблицы без записи в реестре: новая таблица не остаётся молча вне
выгрузки и обезличивания, а новая колонка по умолчанию не выгружается. Все три рабочие
роли имеют полный DML (миграция 000020), поэтому полнота держится на этом коде.

**Личная выгрузка** (`Store.PersonalExport`, платформенный пул, всё по идентификатору
пользователя). Один JSON-документ `lidradar.account-export/1`: учётная запись,
членства, сессии без хэшей, события входа и безопасности, настройки уведомлений,
привязки Telegram, решения по согласию на наборы данных и **числа** записей, внесённых
пользователем в организациях (содержимое записей принадлежит организации). Каждый
список ограничен 50 000 строк (`domain.SectionLimit`) и при превышении помечается
`truncated: true`; пустые разделы всегда `[]`, не `null`. Чтение — одна транзакция
`REPEATABLE READ` только по чтению.

**Выгрузка организации** (`Service.ExportOrganization`, `transport`: `POST
/api/v1/organization/export`). Порядок проверок до первого байта: право
`organization.export` (только `OWNER`), свободный слот (не больше 2 выгрузок в
процессе, `domain.MaxConcurrentExports`), предел `ORGANIZATION_EXPORT_TENANT` (3 в час
на организацию, `auth_rate_limits`), запись аудита `ORGANIZATION_DATA_EXPORTED`, открытие
снимка. Снимок — одна транзакция `REPEATABLE READ` только по чтению под ролью
приложения в контексте организации (RLS остаётся страховкой к условию `tenant_id`);
строки читаются потоком (`to_json` от явного списка колонок: порядок полей задан реестром, в отличие от `jsonb`; постоянная память).
Архив ZIP с Deflate собирается по ходу чтения: `README.txt`, `organization.json`,
файлы `data/<набор>.jsonl` и `manifest.json` **последним** (число строк, размер и
SHA-256 каждого файла, момент снимка). `tenant_id`, секреты и служебные колонки в него
не попадают. Заголовки уходят при первой записи (`streamWriter`), поэтому всё, что
может отказать заранее, отвечает обычной ошибкой (`403 FORBIDDEN`, `429 RATE_LIMITED` с
`Retry-After`, `429 EXPORT_BUSY`). Срок записи продлевается на каждый кусок (30 с,
`http.ResponseController`), общий срок 15 минут (`domain.ExportTimeout`), отключение
клиента отменяет чтение. Ошибка после первого байта обрывает соединение
(`panic(http.ErrAbortHandler)`, посредник восстановления её пробрасывает) и пишет
`organization.export.aborted` в журнал: клиент получает архив без манифеста.

**Исполнитель обезличивания** (`Service.ProcessDueDeletions`, цепочка `worker`
`lifecycle.deletion_failed` раз в минуту, пачка до 20 пользователей, платформенный
пул). Пользователь берётся, когда `deletion_due_at <= now()` и `deleted_at` не задан
(частичный индекс `users_deletion_due_idx`). Одна транзакция на пользователя
(`PostgresStore.Erase`): `SET LOCAL lock_timeout = '5s'`; строка пользователя `FOR
UPDATE SKIP LOCKED`; неиспользованные приглашения пользователя; все членства каждой
организации, где у него есть членство (`ORDER BY tenant_id, id`, как
`lockMemberships`); перепроверка блокировок (единственный владелец `SOLE_OWNER`,
платформенный администратор `PLATFORM_ADMIN`). *Есть блокировка:* назначение
снимается, пишется `ACCOUNT_DELETION_BLOCKED`, после фиксации уходит письмо.
*Блокировок нет:* удаляются `telegram_*`, `notification_*`, `sessions`,
`account_tokens`; неиспользованные приглашения пользователя отзываются; членства
получают `DISABLED` и `revoked_at`; `users` обезличивается (адрес
`<id>@deleted.invalid`, имя «Удалён», хэш пароля от неизвестного случайного секрета,
`status = DISABLED`, `email_verified_at` и `organization_rights` обнулены, `deleted_at`
задан); пишется `ACCOUNT_ANONYMIZED`. После фиксации — письмо `ACCOUNT_DELETED` на
прежний адрес и событие `account.deletion.anonymized` (идентификатор и режим, без адреса). Сбой
одного пользователя не мешает остальным (`account.deletion.failed`), он остаётся в
очереди до следующего прохода. Раз в минуту диагностика пишет
`lifecycle.deletions.status` (`deletions_scheduled`, `deletions_due`,
`deletions_overdue`). *Оператор:* `platform-admin erase-user` вызывает тот же код в
режиме `IMMEDIATE` (без отсрочки, при блокировке ничего не меняет, дополнительно
пишет `USER_ERASED` в `admin_audit_log`); `--dry-run` исполняет и откатывает
транзакцию, поэтому счётчики совпадают с боевым запуском.

**Транспорт:** `POST /api/v1/organization/export` (модуль `lifecycle`);
`/api/v1/auth/account/export` и `/api/v1/auth/account/deletion` — в транспорте
`identity`.

## 19. Вспомогательные пакеты `internal`

- `testsupport` — изолированные схемы на тест, пулы четырёх ролей,
  трассировщик запросов (см. [11-testing.md](11-testing.md)).
- `loadgen` — генератор синтетического набора для нагрузочного испытания и
  staging; пишет таблицы всех модулей напрямую (ADR 0042), запрещён в
  `production`.
- `integration` — сквозные тесты с полным API-стеком в процессе.

## 20. Карта прямых чтений и записей чужих таблиц

| Модуль | Читает | Пишет | Основание |
|---|---|---|---|
| `tenant` | `users` (список команды; статус и блокировка строки `FOR SHARE` при создании и восстановлении членства), `service_catalog_items`, `channel_connections`, `telegram_user_links` (факты онбординга) | `users.organization_rights` (уменьшение в транзакции создания организации) | ADR 0045, ADR 0057, ADR 0059 |
| `identity` | `platform_invitations` (проверка кода) | `platform_invitations.consumed_*` (погашение кода платформы в транзакции регистрации); `auth_audit_log` (записи `PASSWORD_CHANGED`, `PASSWORD_RESET_REQUESTED`, `PASSWORD_RESET_COMPLETED`, `EMAIL_VERIFIED`, `ACCOUNT_DELETION_REQUESTED`, `ACCOUNT_DELETION_CANCELLED` в транзакциях своих команд) | ADR 0057, ADR 0058, ADR 0059 |
| `admin` (команды кодов) | — | `users.organization_rights` (увеличение командой `allow-organization`) | ADR 0057 |
| `lifecycle` | по реестру данных: `users`, `sessions`, `memberships`, `organizations`, 23 набора выгрузки организации, `platform_admins`, `auth_audit_log` (личная выгрузка и проверка блокировок) | исполнитель: удаляет `telegram_callback_commands`, `telegram_link_tokens`, `telegram_user_links`, `notification_digest_items`, `notification_preferences`, `notifications`, `sessions`, `account_tokens`; отзывает `membership_invitations` и `memberships` пользователя; обезличивает `users`; пишет `auth_audit_log` (`ACCOUNT_DELETION_BLOCKED`, `ACCOUNT_ANONYMIZED`) и `admin_audit_log` (`USER_ERASED`, только операторский режим) | ADR 0059; прецеденты ADR 0039, 0040 |
| `conversation` | `channel_connections` (блокировка при приёме; снимок канала в списке), `external_identities`, `risk_signals`, `opportunities` (сводка активных рисков списка) | — | ADR 0025, 0044 |
| `opportunity` | `conversation_summaries`, `messages` (доверенные факты) | — | ADR 0008 |
| `risk` | таблицы переписок, контактов, каналов, внешних личностей, каталога услуг, сделок, точек, расписаний, фактов, исходов, действий, выручки | — | ADR 0007, 0038, 0044 |
| `corrective` | `risk_signals`, `opportunities`, `memberships` | `risk_signals.status → ACTED` | спецификация «Рекомендации, действия и исходы» |
| `revenue` | `risk_signals`, `actions`, `outcomes`, `opportunities`, `memberships` | — | спецификация «Выручка и атрибуция» |
| `notification` | `organizations`, `memberships`, `risk_signals`, `opportunities`, `conversations`, `contacts` | — | ADR 0029, 0037 |
| `ai` | `organizations`, `locations`, `conversations`, `messages` (контекст промпта) | — | ADR 0008 |
| `analytics` | таблицы фактов всех модулей | — | ADR 0039 |
| `admin` | все таблицы | статусы четырёх очередей | ADR 0040 |
| `loadgen` | — | все таблицы (испытания) | ADR 0042 |
