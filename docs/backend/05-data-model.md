# Модель данных

PostgreSQL 18 — единственный источник истины (ADR 0003). Схема создаётся
только встроенными миграциями `backend/platform/postgres/migrations/*.sql`;
ручные изменения и ORM отсутствуют (ADR 0013).

## 1. Принципы схемы

- **Идентификаторы** — `UUID` (новые записи UUIDv7, `platform/ids`), время —
  `TIMESTAMPTZ` в UTC (ADR 0015).
- **Организация — граница арендатора** (ADR 0018): почти каждая таблица несёт
  `tenant_id`; связи между таблицами организации идут **составными внешними
  ключами**, включающими `tenant_id` (`(tenant_id, id)` → `UNIQUE
  (tenant_id, id)` на родителе), поэтому строка не может сослаться на объект
  другой организации даже при ошибке в коде (ADR 0019; проверяется тестом
  `schema_invariants_test.go`).
- **Деньги** — `NUMERIC(14,2)` для сумм, `NUMERIC(4,3)` для уверенности,
  агрегаты `numeric(20,2)`; читаются и пишутся как десятичные строки (ADR
  0016).
- **JSONB** только для расширяемых данных: payload сырых событий, метаданные
  сообщений, payload заданий и событий, семантические факты, детали аудита
  администратора; каждое поле ограничено `CHECK (jsonb_typeof(...) = 'object'
  | 'array')` (ADR 0017).
- **Инварианты в базе**: перечисления через `CHECK (… IN (…))`, длины и
  форматы через `btrim`/`char_length`/regex, согласованность жизненного цикла
  через составные `CHECK`, единственность активного объекта через **частичные
  уникальные индексы**, неизменяемость журналов через триггеры
  `reject_append_only_mutation()` (SQLSTATE `55000`).
- **Row Level Security** на каждой таблице с `tenant_id` (§ 4).

## 2. Миграции

Механизм (`platform/postgres/migrate.go`):

- файлы вшиты в бинарь (`//go:embed migrations/*.sql`), версия — имя файла
  без `.sql`, контрольная сумма — SHA-256 содержимого;
- журнал `schema_migrations(version TEXT PK, checksum TEXT, applied_at)`;
- всё применение — одна транзакция под `pg_advisory_xact_lock(1279541842)`;
  уже применённая миграция с другой суммой останавливает запуск
  (`migration … checksum changed`); down-миграций нет — только вперёд;
- `MigrateUpTo(version)` применяет по указанную версию (для проверки, что
  схема прежнего выпуска принимает следующие миграции);
- `/health/ready` сравнивает журнал с набором сборки поэлементно и по
  последней версии — расхождение даёт `503`; CI дополнительно проверяет
  строку `"latest":"000021_auth_audit"`.

Правила: новая миграция получает следующий свободный номер, содержит
`ENABLE`/`FORCE ROW LEVEL SECURITY` и политику для новых таблиц с `tenant_id`
(цикл миграции 000020 их не догоняет), сопровождается обновлением ожидаемой
версии в `.github/workflows/backend.yml` и записывается идемпотентно
(`testsupport` применяет набор дважды).

| Миграция | Содержимое |
|---|---|
| `000001_platform_baseline` | `platform_metadata` |
| `000002_identity_tenant` | `users`, `sessions`, `organizations`, `memberships`, `locations`, `location_business_hours` |
| `000003_service_catalog` | `service_catalog_items` |
| `000004_connector_core` | `channel_connections`, `raw_events` (и временная `raw_event_normalization_work`) |
| `000005_conversation_core` | `contacts`, `external_identities`, `conversations`, `messages`, `attachments` |
| `000006_telegram_connector_credentials` | `channel_connections.encrypted_credentials` |
| `000007_background_infrastructure` | `jobs`, `scheduled_checks`, `outbox_events`; перенос работы нормализации в outbox, удаление `raw_event_normalization_work` |
| `000008_opportunity_domain` | `opportunities`, `opportunity_stage_history` |
| `000009_no_response_risk` | `risk_signals` |
| `000010_notification_delivery` | `notifications`, `notification_deliveries`, `telegram_user_links`, `telegram_link_tokens`, `telegram_callback_commands` |
| `000011_corrective_facts` | `recommendations`, `actions`, `outcomes`, `idempotency_keys`, `audit_log`, функция `reject_append_only_mutation` |
| `000012_revenue_attribution` | `revenue_events`, `revenue_attributions`, триггер `validate_revenue_attribution` |
| `000013_home_ai_node` | `ai_nodes`, `ai_jobs`, `ai_runs`, `conversation_summaries`, `ai_node_request_nonces` |
| `000014_booking_not_confirmed` | `conversation_summaries.semantic_facts`, `risk_signals.ai_run_id`, трассируемость HYBRID |
| `000015_security_boundaries` | `auth_rate_limits`, `ai_node_tenants` |
| `000016_consistency_remediation` | составные ключи цепочки выручки, единственность `RECOVERED`, `memberships.revoked_at` и запрет удаления, `ai_jobs.leased_at`, одно ожидающее AI-задание, функция `semantic_facts_carry_trust`, переименование `lease_owner → leased_by` |
| `000017_notification_policy` | `notification_preferences`, `notification_digest_items`, `notifications.kind` |
| `000018_risk_feedback` | `ml_consents`, `risk_feedback` |
| `000019_platform_admin` | `platform_admins`, `admin_audit_log`, `discarded_at/discarded_by` в четырёх очередях |
| `000020_row_level_security` | роли, гранты, политики RLS |
| `000021_auth_audit` | `auth_audit_log` |

Итого 48 таблиц и 68 индексов (без учёта первичных ключей и `UNIQUE`-ограничений).

## 3. Таблицы по модулям

Ниже — назначение, ключевые колонки, ограничения (без повторения очевидных
`id`, `tenant_id`, `created_at`, `updated_at`). Полные определения — в
файлах миграций.

### 3.1. Платформа, пользователи, организации

| Таблица | Назначение и ключевые правила |
|---|---|
| `platform_metadata` | служебные метаданные платформы |
| `schema_migrations` | журнал миграций (версия, контрольная сумма, время) |
| `users` | `email` канонический (`= lower(btrim)`), `UNIQUE`; `password_hash` (Argon2id PHC); `status IN (ACTIVE, DISABLED)`; **без `tenant_id`** |
| `sessions` | `token_hash CHAR(64) UNIQUE`, `expires_at`, `ip INET`, `user_agent`, `revoked_at`; индекс по сроку для живых сессий; без `tenant_id` |
| `auth_rate_limits` | PK `(scope, subject_hash BYTEA(32))`, `scope IN (REGISTER_IP, LOGIN_IP, LOGIN_ACCOUNT, REFRESH_IP)`, `attempts > 0`, `expires_at > window_started_at`; без `tenant_id` |
| `organizations` | `name`, `default_timezone`, `default_currency CHAR(3) DEFAULT 'RUB'`, `status IN (ACTIVE, SUSPENDED, ARCHIVED)`; собственная политика RLS по `id` |
| `memberships` | `UNIQUE (tenant_id, user_id)`; `role IN (OWNER, MANAGER)`; `status IN (ACTIVE, INVITED, DISABLED)`; `(status='DISABLED') = (revoked_at IS NOT NULL)`; триггер запрещает `DELETE` (на членство ссылаются факты); политика `member_self` |
| `locations` | `UNIQUE (tenant_id, id)`; `timezone`; `response_threshold_minutes DEFAULT 45 CHECK 1..1440`; `active` |
| `location_business_hours` | FK `(tenant_id, location_id)`; `UNIQUE (tenant_id, location_id, weekday)`; `weekday 1..7`; закрытый день без границ, открытый — `opens_at < closes_at` |
| `ml_consents` | `scope = 'DATASETS'`; FK на членства выдавшего и отозвавшего; `(revoked_at IS NULL) = (revoked_by IS NULL)`; `revoked_at >= granted_at`; частичный уникальный индекс `ml_consents_one_active_idx (tenant_id, scope) WHERE revoked_at IS NULL`; триггер: только один отзыв, без `DELETE` |
| `service_catalog_items` | `UNIQUE (tenant_id, id)`; FK `(tenant_id, location_id)`; `name`/`normalized_name` 1..200 с `btrim`; `price_from/price_to NUMERIC(14,2) >= 0`, `price_from <= price_to`; `currency ~ '^[A-Z]{3}$'`; индексы по `(tenant_id, active, normalized_name, id)` и частичный по точке |

### 3.2. Каналы и переписки

| Таблица | Назначение и ключевые правила |
|---|---|
| `channel_connections` | `provider IN (TEST, IMPORT, GENERIC_WEBHOOK, CONNECTED_BUSINESS_BOT)`; `status IN (ACTIVE, DEGRADED, ERROR, DISCONNECTED)`; `capabilities JSONB` непустой массив; `verification_secret_hash CHAR(64) ~ hex`; `encrypted_credentials BYTEA` 1..8192; `UNIQUE (tenant_id, id)` и `UNIQUE (tenant_id, id, provider)` (цель составного ключа событий); FK на точку; индекс здоровья `(status, last_success_at)` |
| `raw_events` | `payload JSONB`, `payload_hash CHAR(64)`; `status IN (RECEIVED, PROCESSING, PROCESSED, FAILED)`; **`UNIQUE (connection_id, external_event_id)`** — дедупликация провайдера; `FAILED` ⇔ есть `error_code` и `processed_at`; FK `(tenant_id, connection_id, provider)` → подключение |
| `contacts` | `display_name` 1..200, `phone_normalized` 1..40, `email_normalized` канонический 3..254; частичные индексы по телефону и почте |
| `external_identities` | **`UNIQUE NULLS NOT DISTINCT (tenant_id, provider, connection_id, external_id)`** — личность в пространстве подключения; FK на подключение |
| `conversations` | `UNIQUE (connection_id, external_id)`; `status IN (ACTIVE, ARCHIVED)`; `revision >= 0`; границы (`first_message_at`, `last_message_at`, `last_message_direction`) заданы вместе и `first <= last`; индекс `(tenant_id, updated_at DESC, id DESC)` под курсор |
| `messages` | `UNIQUE (connection_id, external_id)`; `direction IN (INCOMING, OUTGOING, SYSTEM)`; `type IN (TEXT, IMAGE, VOICE, AUDIO, VIDEO, DOCUMENT, OTHER)`; `metadata JSONB` объект; `provider_deleted_at` (мягкое удаление); self-FK ответа `(tenant_id, reply_to_message_id, conversation_id)` — ответ не может указывать в чужую переписку; индекс `messages_conversation_time_idx (tenant_id, conversation_id, sent_at DESC, id DESC)` |
| `attachments` | только метаданные: `object_key` 1..1024, `mime_type`, `size_bytes >= 0`, `sha256 ~ hex`, `provider_file_id`; `ON DELETE CASCADE` от сообщения |

### 3.3. Сделки, риски, обратная связь

| Таблица | Назначение и ключевые правила |
|---|---|
| `opportunities` | `stage` из 11 значений; `estimated_amount NUMERIC(14,2)`, `estimated_amount_confidence NUMERIC(4,3)` только вместе с суммой и в 0..1; `currency CHAR(3)`; `stage NOT IN (WON, LOST, ARCHIVED) ⇔ closed_at IS NULL`; **`opportunities_one_active_per_conversation_idx UNIQUE (tenant_id, conversation_id) WHERE активная`**; FK на переписку и услугу |
| `opportunity_stage_history` | `source IN (RULE, AI, USER, IMPORT)`; `from_stage <> to_stage`; `USER` ⇒ `actor_user_id`, `AI` ⇒ `confidence`; `ai_run_id`; append-only триггер |
| `risk_signals` | `type` из 5, `severity` из 4, `status` из 7, `source IN (RULE, HYBRID, MANUAL)`; `reason_code ~ '^[A-Z][A-Z0-9_]{0,99}$'`, `reason_text` 1..2000, `risk_engine_version`; `RULE` без `confidence`/`ai_run_id`, `HYBRID` с обоими; жизненный цикл отметок `acknowledged_at`/`acted_at`/`resolved_at` согласован со статусом; `due_at <= detected_at`; **`risk_signals_one_active_type_idx UNIQUE (tenant_id, opportunity_id, type) WHERE status IN (OPEN, ACKNOWLEDGED, ACTED)`**; FK на сделку, точку, сообщение-триггер, прогон AI; индексы `risk_signals_radar_idx (tenant_id, status, severity, detected_at, id)` и по временной шкале сделки; `UNIQUE (tenant_id, id, opportunity_id)` для цепочки выручки |
| `risk_feedback` | `verdict IN (TRUE_POSITIVE, FALSE_POSITIVE)`, `verdict='TRUE_POSITIVE' OR reason IS NOT NULL`, `note <= 1000`; снимок риска (`risk_type`, `severity`, `status`, `source`, `policy_version`, `ai_run_id`, `trigger_message_id`, `opportunity_stage`, `detected_at`); `dataset_eligible`; append-only |

### 3.4. Корректирующие факты и деньги

| Таблица | Назначение и ключевые правила |
|---|---|
| `recommendations` | `source IN ('TEMPLATE')`; **`UNIQUE (tenant_id, risk_id, source)`**; текст 1..2000; `ON DELETE CASCADE` от риска |
| `actions` | `type` из 6; `note` ≤ 2000; денормализованный `opportunity_id` с FK `(tenant_id, risk_id, opportunity_id)` → риск; FK актора → `memberships`; `UNIQUE (tenant_id, id, opportunity_id)`; append-only |
| `outcomes` | `status IN (RESPONDED, BOOKED, PAID, LOST, THINKING, NOT_A_LEAD)`; FK на сделку и членство; `UNIQUE (tenant_id, id, opportunity_id)`; append-only |
| `idempotency_keys` | PK `(tenant_id, key, operation)`; `request_hash BYTEA(32)`; `response_status 200..599`; `response_body JSONB`; `key` 1..255, `operation` 1..100; append-only; операции `corrective.action.create`, `corrective.outcome.create`, `revenue.confirm` |
| `revenue_events` | `amount NUMERIC(14,2) > 0`; `currency ~ '^[A-Z]{3}$'`; `status IN ('CONFIRMED')`, `source IN ('USER_CONFIRMED')`; `created_at = confirmed_at`; FK на сделку и подтвердившее членство; `UNIQUE (tenant_id, id, opportunity_id)`; append-only |
| `revenue_attributions` | `type IN (RECOVERED, ORGANIC, UNKNOWN)`; `UNIQUE (tenant_id, revenue_event_id)`; `RECOVERED` ⇒ три идентификатора цепочки, иначе все `NULL`; составные FK на событие, риск, действие, исход **той же сделки**; **`revenue_attributions_one_recovered_per_opportunity_idx UNIQUE (tenant_id, opportunity_id) WHERE type='RECOVERED'`**; триггер `validate_revenue_attribution`: цепочка не позже подтверждения и не старше 30 дней; append-only |
| `audit_log` | `actor_user_id` обязан быть участником (FK на `memberships`); `operation`, `entity_type ~ '^[A-Z][A-Z0-9_]{0,99}$'`; `entity_id UUID`; индекс `(tenant_id, created_at DESC, id DESC)`; append-only |
| `auth_audit_log` | `user_id`, `operation`, `ip_address` 1..64 или `NULL`; без `tenant_id`; append-only |

### 3.5. Фоновая обработка и уведомления

| Таблица | Назначение и ключевые правила |
|---|---|
| `outbox_events` | `event_type`, `event_version > 0`, `aggregate_type/id`, `trace_id`, `data JSONB` объект; `status IN (PENDING, PROCESSING, RETRY, PUBLISHED, DEAD)`; `max_attempts 1..20`, `attempt_count 0..max`; `leased_by`, `lease_until`; `last_error_code` по regex; `discarded_at/by` только при `DEAD`; индексы захвата `(available_at, occurred_at, id) WHERE PENDING/RETRY` и истёкших аренд |
| `jobs` | `job_type` ≤ 100; **`UNIQUE (tenant_id, job_type, dedup_key)`**, `dedup_key` ≤ 512; `payload JSONB` объект; `priority`; статусы `PENDING, PROCESSING, RETRY, SUCCEEDED, DEAD`; аренда; `discarded_at/by`; индексы `jobs_claim_idx (priority DESC, available_at, created_at, id) WHERE PENDING/RETRY`, истёкших аренд, `(tenant_id, status, created_at, id)` |
| `scheduled_checks` | `check_type`; **`UNIQUE (tenant_id, check_type, dedup_key)`**; `subject_type/id`, `job_type`, `payload`, `due_at`; `status IN (SCHEDULED, ENQUEUED, CANCELLED)`, `ENQUEUED ⇔ job_id IS NOT NULL` (FK `(tenant_id, job_id)`); индекс `(due_at, created_at, id) WHERE SCHEDULED` |
| `notifications` | `kind IN (RISK_OPENED, RISK_DIGEST, RISK_ESCALATED)`; **`UNIQUE (tenant_id, dedup_key)`**; `title` 1..200, `body` 1..2000; `(kind='RISK_DIGEST') = (risk_id IS NULL)`; FK на членство получателя и риск; `snoozed_at` |
| `notification_deliveries` | **`UNIQUE (tenant_id, notification_id, channel, attempt)`** — строка на попытку, `attempt 1..5`; `channel IN (IN_APP, TELEGRAM)`; статусы `PENDING, PROCESSING, SUCCEEDED, RETRY, DEAD` с согласованными полями; `destination` ≤ 255; `provider_message_id`; `failure_code` по regex; `discarded_at/by`; индекс захвата `(status, available_at, created_at, id)` |
| `notification_preferences` | `UNIQUE (tenant_id, user_id, risk_type)`; режим, порог, флаги каналов; тихие часы задаются парой, не равны друг другу, с минутной точностью; `digest_time` |
| `notification_digest_items` | **`UNIQUE (tenant_id, user_id, risk_id)`** — риск в очереди получателя один раз; `reason IN (DIGEST, QUIET_HOURS)`; `slot` формата `YYYY-MM-DDTHH:MM`; хотя бы один канал; `notification_id` только вместе с `consumed_at`; частичный индекс ожидающих |
| `telegram_user_links` | `UNIQUE (tenant_id, user_id)`, `UNIQUE (tenant_id, telegram_user_id)`; `chat_id`; `disabled_at` |
| `telegram_link_tokens` | `token_hash CHAR(64) UNIQUE`; `expires_at > created_at`; `used_at`; частичный индекс неиспользованных |
| `telegram_callback_commands` | `UNIQUE (tenant_id, idempotency_key)`; `action IN (OPEN_RISK, ACKNOWLEDGE, SNOOZE)` |

### 3.6. AI

| Таблица | Назначение и ключевые правила |
|---|---|
| `ai_nodes` | `name UNIQUE` 1..100; `secret_digest BYTEA(32)`; `status IN (OFFLINE, READY, REVOKED)`; `model_version`; `available_slots 0..1`; `max_inflight = 1`; `last_heartbeat_at`; `revoked_at` ⇔ `REVOKED`; без `tenant_id` |
| `ai_node_tenants` | PK `(node_id, tenant_id)`, оба `ON DELETE CASCADE` — допуск узла к организации |
| `ai_node_request_nonces` | PK `(node_id, request_id)`, `expires_at > created_at` — одноразовые идентификаторы запросов; без `tenant_id` |
| `ai_jobs` | `job_type IN ('ANALYZE_CONVERSATION')`, `entity_type IN ('CONVERSATION')`; `priority -100..100`; `payload JSONB`; версии модели/схемы/промпта; `base_conversation_revision > 0`; `analysis_through_message_id`; статусы `PENDING, LEASED, RUNNING, SUCCEEDED, RETRY, DEAD`; `attempts 0..5`, `max_attempts 1..5`; аренда `leased_by/lease_until/leased_at` согласована со статусом; **`ai_jobs_one_queued_per_entity_idx UNIQUE (tenant_id, entity_type, entity_id) WHERE PENDING/RETRY`**; `UNIQUE` снимка `(tenant, job_type, entity_type, entity_id, revision, model, schema, prompt)`; FK на переписку, сообщение и **на допуск `(leased_by, tenant_id)` → `ai_node_tenants`**; `discarded_at/by`; индексы захвата, аренды, потолка аренды |
| `ai_runs` | `status IN (RUNNING, SUCCEEDED, FAILED)`, `application_status IN (PENDING, APPLIED, STALE, REJECTED)` с CHECK согласованности (`RUNNING ⇔ PENDING`; `SUCCEEDED` ⇒ `raw_output` и нет `error_code`; `FAILED` ⇒ `REJECTED` и `error_code`); снимок ревизии и сообщения; `raw_output`, `validation_error`; FK на задание, переписку, сообщение и допуск `(node_id, tenant_id)`; **`ai_runs_active_job_idx UNIQUE (job_id) WHERE RUNNING`** |
| `conversation_summaries` | PK `(tenant_id, conversation_id)`; `summary_text` 1..2000; ревизия, сообщение, версии, `ai_run_id`; `semantic_facts JSONB` массив, каждый элемент с булевым `trusted` (функция `semantic_facts_carry_trust`); частичный индекс по факту `BOOKING_INTENT=true` |

### 3.7. Администрирование

| Таблица | Назначение и ключевые правила |
|---|---|
| `platform_admins` | `user_id`, `granted_by` (`NULL` для CLI), `note` ≤ 500, `revoked_at/by`; **`platform_admins_one_active_idx UNIQUE (user_id) WHERE revoked_at IS NULL`**; триггер: без `DELETE`, ровно один отзыв |
| `admin_audit_log` | `source IN (API, CLI)`; `API` ⇒ актор, `CLI` ⇒ `NULL`; `operation`, `entity_type` по regex; `tenant_id` объекта (может быть `NULL`); `details JSONB` объект; append-only |

## 4. Row Level Security

Реализация — ADR 0034 и 0041 (`000020_row_level_security.sql`,
`platform/postgres/postgres.go`).

- **Роли** `lidradar_app`, `lidradar_worker`, `lidradar_platform` — `NOLOGIN`,
  выдаются пользователю миграций (`GRANT … TO CURRENT_USER`) с правами на все
  таблицы и последовательности схемы и `ALTER DEFAULT PRIVILEGES` для будущих.
- **Политика** `tenant_isolation` на каждой базовой таблице с колонкой
  `tenant_id` (`ENABLE` + `FORCE ROW LEVEL SECURITY`, `FOR ALL`):

  ```sql
  USING (tenant_id = NULLIF(current_setting('lidradar.tenant_id', true), '')::uuid
         OR pg_has_role(current_user, 'lidradar_platform', 'MEMBER'))
  WITH CHECK (то же)
  ```

  `organizations` дополнительно видна участнику по `lidradar.user_id`
  (`EXISTS memberships`), `memberships` имеет политику `member_self` (`FOR
  SELECT`) — это нужно `/auth/me` до выбора организации.
- **Контекст** переносится хуками пула: `AfterConnect` делает `SET ROLE`,
  `PrepareConn` перед каждой выдачей соединения выполняет
  `set_config('lidradar.tenant_id', $1, false), set_config('lidradar.user_id',
  $2, false)` из `tenantctx` (с кэшем состояния соединения). Пустой контекст
  записывается пустыми строками — запрос без организации не видит ни одной
  строки (**fail-closed**). Источники контекста: `X-Tenant-ID`, путь вебхука,
  создание организации, актор для `/auth/me`, `tenant_id` захваченного
  задания или события в worker.
- **Обход** только у `lidradar_platform` через предикат политики (без
  `BYPASSRLS`): захват заданий и событий, планировщик, доставки, API AI-узла,
  административные модели. Владелец схемы (`postgres.Open`) не ограничен —
  им пользуются миграции и CLI.
- **Без RLS** остаются таблицы без `tenant_id`: `users`, `sessions`,
  `auth_rate_limits`, `auth_audit_log`, `ai_nodes`, `ai_node_request_nonces`,
  `platform_metadata`, `schema_migrations`. `admin_audit_log` имеет
  `tenant_id` и потому под политикой, но пишется платформенной ролью.
- Соединение `LISTEN` для SSE открывается вне пула (без `SET ROLE` и
  контекста) и таблиц не читает.

Проверки: `rls_test.go` (0 строк без контекста, `42501` при записи в чужую
организацию, платформенная роль видит всё, контекст пользователя открывает
членства, но не контакты), `security_hardening_test.go` (матрица
межорганизационных атак), `forward_test.go` (≥ 30 политик после миграции).

## 5. Неизменяемость и единственность

| Гарантия | Механизм |
|---|---|
| журналы нельзя править и удалять | триггеры `BEFORE UPDATE OR DELETE` на `actions`, `outcomes`, `idempotency_keys`, `audit_log`, `auth_audit_log`, `opportunity_stage_history`, `revenue_events`, `revenue_attributions`, `risk_feedback`, `admin_audit_log` |
| членство, согласие, право администратора не удаляются, отзываются один раз | триггеры на `memberships`, `ml_consents`, `platform_admins` |
| одна активная сделка на переписку, один активный риск на сделку и тип, одна `RECOVERED` на сделку, одно действующее согласие, один активный администратор, одно ожидающее AI-задание на переписку, один активный прогон на задание, одна попытка доставки с номером | частичные уникальные индексы |
| событие провайдера обрабатывается один раз | `UNIQUE (connection_id, external_event_id)` |
| задание и проверка ставятся один раз | `UNIQUE (tenant_id, job_type|check_type, dedup_key)` |
| цепочка выручки принадлежит одной сделке и укладывается в 30 дней | составные FK с `opportunity_id` и триггер `validate_revenue_attribution` |

## 6. Индексы под рабочие запросы

- Radar и списки рисков — `risk_signals_radar_idx`; хронология сделки —
  `risk_signals_opportunity_timeline_idx`.
- Сообщения переписки и счётчики аналитики — `messages_conversation_time_idx`
  (нагрузочное испытание подтверждает индексный план и отсутствие
  последовательного сканирования `messages` на 500 000 строк).
- Списки переписок — `conversation_tenant_updated_idx`.
- Очереди — частичные индексы захвата по статусу; истёкшие аренды — отдельные
  частичные индексы; сводки — индекс ожидающих элементов.
- Каталог — `(tenant_id, active, normalized_name, id)`.

## 7. Что хранится вне PostgreSQL

Двоичные вложения — только метаданные в `attachments`; объектное хранилище
(`platform/objectstorage`) пока не реализовано (каталог пуст). Модели,
наборы и отчёты измерений AI лежат файлами в `models/`. Резервные копии —
`pg_dump -Fc` (см. [09-reliability.md](09-reliability.md)).
