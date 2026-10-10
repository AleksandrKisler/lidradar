# Тестирование и качество

Сверка runtime 2026-10-06: исходники, CI и обязательный DB gate доступны.
Текущий прогон: 568 pass events (включая subtests), 0 failed, один ожидаемый
skip служебного дочернего теста. [Машинные доказательства](../engineering/evidence/2026-10-06/db-summary.json)
и [границы допуска](../engineering/HIGH_REVIEW_2026_10_06.md) отделены от списка тестов.

## 1. Пирамида проверок

| Уровень | Что проверяет | Где |
|---|---|---|
| доменные unit-тесты | правила риска и рабочее время, переходы этапов, деньги, политика уведомлений, контракт AI, парсер обещаний | `backend/internal/*/domain/*_test.go` — без базы |
| прикладные тесты с in-memory адаптерами | сценарии сервисов, права, идемпотентность, классификация ошибок | `backend/internal/*/application/*_test.go` (адаптеры `NewMemory*`/`NewTestMemory*` только для тестов, archcheck запрещает их в `cmd`) |
| PostgreSQL-интеграция модуля | SQL, ограничения, гонки, аренды, RLS | `backend/internal/*/infrastructure/*_test.go`, `backend/platform/postgres/*_test.go` |
| контрактные тесты | коннекторы (наборы фикстур провайдеров, Telegram Bot API через поддельный сервер), AI (llama.cpp-провайдер, схема результата, клиент узла, реквизиты), OpenAPI (Redocly) | `connector/infrastructure/connectors_test.go`, `ai/infrastructure/*_test.go`, CI |
| сквозные (E2E golden path) | полный API-стек в процессе: webhook → worker → scheduler → риск → уведомление → действие → исход → выручка | `backend/internal/integration/*_test.go` |
| изоляция организаций | RLS по ролям, матрица межорганизационных атак, кросс-tenant идентификаторы | `platform/postgres/rls_test.go`, `integration/security_hardening_test.go` |
| отказы и восстановление | `kill -9` дочернего процесса после захвата, истёкшие аренды, `RETRY`/`DEAD`, откат outbox вместе с фактом, гонка свежести AI | `jobs/infrastructure/postgres_test.go`, `events/infrastructure/postgres_test.go`, `ai/infrastructure/*_test.go` |
| нагрузочные (тег `load`) | всплеск вебхуков, конкуренция кандидатов, конкурентные захваты, baseline §72 | `integration/load_stage_1_7_test.go`, `integration/load_stage_25_test.go`, `jobs/infrastructure/load_test.go` |
| архитектура | направление зависимостей и запрет in-memory адаптеров в командах | `backend/tools/archcheck` |

Канонические команды:

```bash
make test
```

пропускает тесты с базой, если `LIDRADAR_DATABASE_URL` не задан;

```bash
LIDRADAR_DATABASE_URL='postgres://lidradar:lidradar@127.0.0.1:5432/lidradar_frontend?sslmode=disable' make test-db GO_TEST_FLAGS='-race -count=1'
```

запускает `backend/tools/testgate`, который требует выделенную БД
`lidradar_frontend`, выставляет `LIDRADAR_TEST_DATABASE_REQUIRED=1`, собирает
`go test -json` в `runtime/test-db/events.jsonl` и `summary.json`. Отсутствие/
недоступность PostgreSQL, setup failure, неожиданный skip или отсутствие
обязательного sentinel-теста дают failure. Helper разрешает skip при отсутствии
DSN только в необязательном `go test ./...`; заданный недоступный DSN всегда failure.
Единственное исключение — helper crash-test: родитель реально выполняет его
в дочернем процессе и сам входит в обязательные проверки. CI хранит положительный
и отрицательный артефакты отдельно. Требования: [RG-TESTS](../engineering/RELEASE_GATES.md#rg-tests).

## 2. Инфраструктура тестов

`backend/internal/testsupport`:

- `Postgres(t)` — пул владельца схемы; `PostgresRoles(t)` — `Pools{Owner,
  App, Worker, Platform}` с хуками `SET ROLE` и контекста RLS.
- **Схема на тест**: `test_<16 hex>` через `CREATE SCHEMA` и `search_path`;
  миграции применяются **дважды** (проверка идемпотентности); `t.Cleanup`
  закрывает пулы и делает `DROP SCHEMA … CASCADE`.
- Историческое поведение helper: без `LIDRADAR_DATABASE_URL` используется
  `t.Skip`, при недоступной базе `t.Skipf`. Оно допустимо только для явно
  необязательного локального unit-only запуска, который нельзя назвать
  полной проверкой. В обязательном DB/CI режиме обе ситуации должны давать
  failure. Наличие URL и поздний smoke миграций не доказывают выполнение
  ранее пропущенных тестов.
- `TwoTenants(t, ctx, pool)` — две организации с владельцем, точкой
  (`Europe/Moscow`, порог 45) и членством.
- `LoadTrace` — `pgx.QueryTracer` (включается `LIDRADAR_LOAD_TRACE=1`),
  собирает p50/p95 по нормализованному тексту запроса.

`backend/internal/integration/identity_tenant_test.go` содержит `newAPIFixture`
— сборку почти полного API-стека в процессе: те же маршруты и middleware,
что `cmd/api`, диспетчер, worker и планировщик, пулы ролей (`App` для
репозиториев, `Platform` для захвата, доставок и админки, `Owner` для
прямых проверок), заглушка Telegram (`StubTransport`), нулевой дебаунс AI,
`SessionTTL = 24h`. Отличия от боевой сборки: нет лимита частоты и списка
origin, инвалидатор — хаб в памяти вместо `pg_notify`.

Тег сборки `load` — единственный в репозитории; обычный `go test ./...` эти
файлы не собирает.

## 3. Сквозные сценарии (`backend/internal/integration`)

| Файл | Сценарий |
|---|---|
| `identity_tenant_test.go` | перебор пароля → `429` + `Retry-After`; полный поток OWNER (организация, точка, права, изоляция); одноразовая Telegram-ссылка хранит только хеш |
| `service_catalog_test.go` | CRUD каталога, точные деньги и `NULL`-цены, права MANAGER, изоляция |
| `connector_core_test.go` | управление подключениями, persist-first, дедупликация, изоляция |
| `conversation_core_test.go` | `GENERIC_WEBHOOK` → RawEvent → worker → каноническая переписка → REST |
| `opportunity_stage_test.go` | услуга с ценой → входящее сообщение → кандидат → этапы и история |
| `no_response_risk_test.go` | webhook → сделка → проверка → `NO_RESPONSE` без AI → рекомендация → действие → исход → `PAID` 47 000 → `RECOVERED`; ответ бизнеса закрывает риск |
| `semantic_booking_risk_test.go`, `semantic_promise_risk_test.go`, `semantic_price_risk_test.go`, `semantic_follow_up_risk_test.go` | факт AI → этап → проверка → риск R3/R4/R2/R5 → Radar → уведомление; повтор результата без дубликата; исходы закрывают риск |
| `notification_policy_test.go` | настройки по типу риска, тихие часы, одна сводка в оба канала |
| `risk_feedback_test.go` | вердикты append-only, `FALSE_POSITIVE` закрывает риск, `NOT_A_LEAD` закрывает сделку, precision по типам |
| `analytics_summary_test.go` | сводка совпадает с Radar и выручкой, окно в часовом поясе организации |
| `admin_observability_test.go` | администратор диагностирует и чинит сломанный контур через API без доступа к базе |
| `security_hardening_test.go` | аудит всех критических действий, заголовки безопасности и cookie; матрица межорганизационных атак под RLS |
| `smoke_security_stage_1_7_test.go` | `/health`, враждебный `X-Tenant-ID`, недоверенный `Origin` без мутаций |
| `load_stage_1_7_test.go`, `load_stage_25_test.go` (`load`) | всплеск 150 вебхуков → ровно 150 сырых событий и событий outbox; 120 конкурентных кандидатов → одна сделка; baseline §72 с отчётом JSON |

## 4. Проверки платформы и схемы

- `platform/postgres/migrate_test.go` — порядок и контрольные суммы
  встроенных миграций.
- `platform/postgres/forward_test.go` — схема прежнего выпуска (по
  `000019`) принимает следующие миграции, повтор ничего не меняет, ≥ 30
  политик RLS, подмена суммы останавливает запуск, неизвестная версия
  отвергается.
- `platform/postgres/rls_test.go` — fail-closed по ролям.
- `platform/postgres/bootstrap_roles_test.go` — `scripts/sql/bootstrap-roles.sql`
  возвращает схеме, лишённой прав (как после восстановления без привилегий),
  ровно те права и права по умолчанию, что выдали миграции; повтор ничего не
  меняет; `verify-roles.sql` без замечаний на свежей схеме (RLS принудителен и
  политика есть у каждой таблицы с `tenant_id`) и замечает отсутствующие права и
  несуществующего пользователя приложения; оба теста входят в обязательные для
  `testgate`.
- `platform/http/clientaddr_test.go` — адрес клиента (ADR 0049): заголовок
  читается только от доверенного соседа, цепочка справа налево, подделка слева
  не читается, несколько строк, порт, IPv6, повреждённые записи и длинная
  цепочка; клиенты за proxy не делят предел, подмена заголовка недоверенным
  узлом не обходит предел и не переносит его на постороннего; предупреждение
  об игнорируемом заголовке не чаще раза в минуту и без адресов.
- `identity/transport/client_address_test.go`,
  `integration/trusted_proxy_test.go` — сеанс, журнал входа и постоянные
  пределы PostgreSQL получают адрес клиента, а не proxy.
- `platform/postgres/transport_test.go` — пул не открывается без TLS, когда он
  обязателен: `disable`, `allow`, `prefer`, строка без `sslmode` отвергаются,
  `require`, `verify-ca`, `verify-full` проходят проверку; ошибка не раскрывает
  пароль.
- `scripts/tests` (`unittest`, шаг CI «Documentation and recovery helper tests») —
  `backup.sh`, `restore.sh` и `bootstrap-roles.sh` на подставных инструментах:
  порядок вызовов, отказ до обращения к базе при неверных входных данных, SQL не
  содержит отзыва прав, удаления и ослабления RLS.
- `scripts/tests/test_production_compose.py` (ADR 0050) — `docker compose config`
  production-стека без запуска контейнеров: наружу публикуются только `80` и `443`
  у edge, сегментация сетей (`data` внутренняя), `api` доверяет ровно закреплённому
  адресу edge за пределами динамического пула, образы выпуска берут
  `LIDRADAR_VERSION` и не собираются на месте, у каждого сервиса усиление,
  предел памяти и ротация журналов, секреты достигают только своих сервисов,
  открытый канал к базе включается только явно, инструменты существуют лишь под
  профилем, пропущенное обязательное значение называет переменную, `.env.example`
  совпадает с тем, что требует стек; текстовые проверки Caddyfile (поток событий
  без буфера и сжатия, снаружи только `/health/live`, заголовки пересылки не
  доверяются). Тест пропускается, если нет `docker compose`.
  `test_build_images.py` — сборка образов на подставном `docker` и временных
  репозиториях (тег из sha, отказ на изменённом дереве, метки, неизвестная
  команда), `test_smoke_production.py` — приёмочный скрипт на подставных `curl` и
  `docker`: каждый из семи дефектов замечается, а исправный стек не даёт замечаний.
- Шаг CI «Production Caddyfile» проверяет `caddy validate` на образе, закреплённом в
  production-файле compose.
- `scripts/tests/test_offhost_backup.py` (ADR 0051) — контейнер копий вне хоста на подставных `pg_dump`,
  `psql`, `age`, `rclone` и `curl`: цикл (файл копии, затем манифест, сверка скачиванием, открытый текст
  удалён до загрузки), ярус daily раз в сутки и его сбой без отмены точки, отказы на каждом шаге и счётчик
  неудач, сигналы успеха и сбоя без утечки секретов, разрыв между точками (с учётом времени цикла),
  пробный цикл `check`, перекрытие циклов, проверка настройки, расписание на сетке времени, таймаут и
  аварийное завершение цикла, `status`, `list`, `fetch` (самая новая полная копия, подмена, чужой ключ),
  `drill` (счётчики между замерами, удаление учебной базы). `test_restore_offhost.py` — порядок
  шагов и защитные проверки `restore-offhost.sh`; `test_production_compose.py` дополнен инвариантами службы
  (сети, права, секреты, обязательные переменные); `test_smoke_production.py` — возраст копии в приёмке.
- `scripts/tests/e2e-db-roles.sh` (шаг CI «PostgreSQL roles of the production stack», ADR 0052) на настоящих
  файлах `postgres/10-roles.sql` и `pg_hba.conf` в `postgres:18-alpine`: атрибуты и членство ролей,
  владелец базы, отказ администратору по сети, вход владельца и рабочего логина, все миграции под владельцем
  без суперправ (повтор безопасен), `bootstrap-roles.sh --verify-only` с рабочим логином, матрица прав
  (рабочий логин: переключение в роли и чтение разрешены, DDL, отключение триггеров и RLS, `TRUNCATE`,
  `CREATE ROLE/DATABASE`, `COPY PROGRAM`, `pg_read_file`, `ALTER SYSTEM` отклонены; владелец: суперправ нет),
  настоящие api, worker, scheduler и `platform-admin` под рабочим логином (готовность, регистрация,
  организация, радар, подключение, право администратора, все соединения от `lidradar_runtime`, нет отказов
  в правах), `load-generate` под владельцем, копия рабочим логином и восстановление владельцем с
  переименованием баз, запуск файла ролей на «внешней» базе (повтор, смена пароля, отказ в служебной базе и
  без паролей). `test_production_compose.py` дополнен кругом обладателей паролей, пользователями в строках
  подключения и инвариантами файлов ролей и сетевых правил; `test_smoke_production.py` — проверкой ролей на
  живом стеке.
- Шаг CI «Off-host backup pipeline» собирает образ копий и запускает `scripts/tests/e2e-offhost-backup.sh`
  на настоящих PostgreSQL 18, age, `pg_dump`/`pg_restore` и S3 (`rclone serve s3`): цикл, ярусы,
  шифротекст в хранилище, `list`, `drill`, чужой ключ, подмена объекта, неверный ключ хранилища.
- `platform/postgres/schema_invariants_test.go` — связи между таблицами
  организации включают `tenant_id`.
- `platform/postgres/readiness_test.go` — дрейф миграций даёт «не готов».
- `platform/http/*_test.go` — конверт ошибки с trace, сокрытие паники,
  `Origin`, заголовки безопасности (в том числе на 404), независимые правила
  лимита частоты, IPv6-адреса, строгий JSON и fuzz-тест декодера.
- `platform/config/config_test.go` — небезопасные cookie в production,
  парная настройка Telegram, токен не утекает в ошибку, небезопасная
  конфигурация AI отвергается.
- `platform/crypto/*_test.go` — отклонение опасных параметров Argon2id,
  привязка шифротекста к AAD.

## 5. Отказы и гонки, закреплённые тестами

- `jobs/infrastructure/postgres_test.go`: жизненный цикл аренды и `DEAD`,
  пропуск заблокированной строки, однократное продвижение проверки,
  однократный побочный эффект после восстановления аренды, **реальный
  `kill -9`** дочернего процесса того же тестового бинарника после захвата с
  повторным захватом другим владельцем и запретом подтверждения прежним.
- `events/infrastructure/postgres_test.go`: восстановление аренды outbox,
  идемпотентный `Append`, неподдерживаемое событие → `DEAD`.
- `conversation/infrastructure/postgres_test.go`: откат переписки вместе с
  событием outbox, пространство имён личности, страница сообщений под пулом
  из двух соединений и шестью читателями.
- `opportunity/infrastructure/postgres_test.go`: одна активная сделка при
  конкурентных кандидатах, один переход при параллельных одинаковых командах.
- `corrective`/`revenue` `postgres_store_test.go`: параллельный повтор с
  ключом идемпотентности и атомарный откат, вторая `RECOVERED` отвергается
  базой, цепочка одной сделки.
- `identity/infrastructure/postgres_rate_limiter_test.go`: 40 параллельных
  попыток — ровно 5 разрешены.
- `notification` тесты: отказ Telegram создаёт повтор и не меняет риск,
  недоступный Telegram не дублирует уведомление.
- `ai` тесты: перехват аренды после разрыва, потолок аренды при живом
  heartbeat, гонка свежести внутри финализации, один анализ на всплеск,
  старый прогон не перетирает новую проекцию.

## 6. Статический контроль и CI

Workflow `Backend` (`.github/workflows/backend.yml`) на `pull_request` и
`push main` с сервис-контейнером `postgres:18-alpine`:

1. `gofmt -l backend` пуст;
2. `go vet ./...`;
3. `staticcheck@v0.6.1 ./...`;
4. `make test-db GO_TEST_FLAGS="-race -count=1"` в обязательном режиме:
   PostgreSQL доступен, схема подготовлена, все обязательные RLS, money,
   lease/freshness и crash-recovery сценарии реально выполнены; неожиданных
   skips нет. Сохраняются machine-readable test events, число выполненных,
   список skips с причинами и привязка к build/schema;
5. `archcheck -root backend`;
6. `go run ./backend/cmd/migrate` (smoke миграций);
7. запуск собранного API и проверка `/health/ready` на строку
   `"latest":"000023_unfinished_agreements"` — новая миграция без обновления ожидания
   ломает CI;
8. `npx @redocly/cli@1.34.5 lint contracts/openapi/openapi.yaml`;
9. `go build ./backend/cmd/...`;
10. `docker build` образа API.

Workflow `Architecture` дополнительно гоняет тесты самого archcheck и
компиляцию всего `backend/...`. Локальный аналог — `make check` (`vet`,
`test`, `ai-dataset-audit`, `archcheck`).

Правила archcheck — [02-architecture.md](02-architecture.md) § 3.

Негативная проверка CI: временно недоступный PostgreSQL должен делать именно
DB-test job красным, а не оставлять зелёный exit code с `SKIP`. Затем
повторяется успешный прогон и сверяется ожидаемый набор сценариев. До
получения обоих результатов H-17 не закрыт на уровне реализации.

## 7. Правила для новых изменений

Из `docs/engineering/CODEX_RULES.md` и `DEFINITION_OF_DONE.md`: изменения в
рамках запрошенного поведения; архитектура, границы модулей, владение
данными и источник истины — только через принятый ADR; доменная логика
независима от транспорта и хранения; новая зависимость объясняется; тесты и
документация обновляются вместе с поведением; секреты и реквизиты не
коммитятся; `go test ./...` проходит из корня, но не заменяет обязательный
DB gate без пропусков; ошибки и журналы не
раскрывают секретов; итог называет выполненную проверку и известные
ограничения. Практические соглашения этого репозитория: новая таблица с
`tenant_id` получает RLS в своей миграции; новое событие или задание — новая
версия в имени; в OpenAPI описания с запятыми и `: ` заключаются в кавычки;
в pgx повторно используемые параметры приводятся явно
(`$3::uuid`, `$1::timestamptz - INTERVAL`).
