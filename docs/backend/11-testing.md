# Тестирование и качество

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
LIDRADAR_DATABASE_URL='postgres://lidradar:lidradar@127.0.0.1:5432/lidradar?sslmode=disable' make test-db GO_TEST_FLAGS='-race -count=1'
```

намеренно падает без адреса базы и запускает всё с детектором гонок.

## 2. Инфраструктура тестов

`backend/internal/testsupport`:

- `Postgres(t)` — пул владельца схемы; `PostgresRoles(t)` — `Pools{Owner,
  App, Worker, Platform}` с хуками `SET ROLE` и контекста RLS.
- **Схема на тест**: `test_<16 hex>` через `CREATE SCHEMA` и `search_path`;
  миграции применяются **дважды** (проверка идемпотентности); `t.Cleanup`
  закрывает пулы и делает `DROP SCHEMA … CASCADE`.
- Без `LIDRADAR_DATABASE_URL` — `t.Skip`; при недоступной базе — `t.Skipf`.
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
4. `make test-db GO_TEST_FLAGS="-race -count=1"`;
5. `archcheck -root backend`;
6. `go run ./backend/cmd/migrate` (smoke миграций);
7. запуск собранного API и проверка `/health/ready` на строку
   `"latest":"000021_auth_audit"` — новая миграция без обновления ожидания
   ломает CI;
8. `npx @redocly/cli@1.34.5 lint contracts/openapi/openapi.yaml`;
9. `go build ./backend/cmd/...`;
10. `docker build` образа API.

Workflow `Architecture` дополнительно гоняет тесты самого archcheck и
компиляцию всего `backend/...`. Локальный аналог — `make check` (`vet`,
`test`, `ai-dataset-audit`, `archcheck`).

Правила archcheck — [02-architecture.md](02-architecture.md) § 3.

## 7. Правила для новых изменений

Из `docs/engineering/CODEX_RULES.md` и `DEFINITION_OF_DONE.md`: изменения в
рамках запрошенного поведения; архитектура, границы модулей, владение
данными и источник истины — только через принятый ADR; доменная логика
независима от транспорта и хранения; новая зависимость объясняется; тесты и
документация обновляются вместе с поведением; секреты и реквизиты не
коммитятся; `go test ./...` проходит из корня; ошибки и журналы не
раскрывают секретов; итог называет выполненную проверку и известные
ограничения. Практические соглашения этого репозитория: новая таблица с
`tenant_id` получает RLS в своей миграции; новое событие или задание — новая
версия в имени; в OpenAPI описания с запятыми и `: ` заключаются в кавычки;
в pgx повторно используемые параметры приводятся явно
(`$3::uuid`, `$1::timestamptz - INTERVAL`).
