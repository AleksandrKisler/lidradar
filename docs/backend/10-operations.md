# Эксплуатация

Конфигурация, сборка, развёртывание, запуск, наблюдение и ёмкость. Runbooks
с пошаговыми процедурами — в `docs/runbooks/`.

## 1. Конфигурация

Все процессы читают переменные окружения через `platform/config` и целиком
валидируют их при старте (даже `cmd/migrate` отвергнет неверный
`LIDRADAR_AI_LLAMA_URL`). Единственная обязательная — `LIDRADAR_ENV`.
Отсутствие обязательного значения или неверный формат → код выхода 1 и
событие `runtime.configuration_invalid`; значения секретов в текст ошибки
не попадают.

| Ключ | По умолчанию | Валидация | Кто использует |
|---|---|---|---|
| `LIDRADAR_ENV` | — (обязателен) | `development` \| `test` \| `staging` \| `production` | все |
| `LIDRADAR_HTTP_ADDRESS` | `:8080` | непустой | `api` |
| `LIDRADAR_HTTP_RATE_LIMIT_PER_MINUTE` | `120` | ≥ 0, 0 выключает | `api`: `/api/v1/auth/*` |
| `LIDRADAR_HTTP_WEBHOOK_RATE_LIMIT_PER_MINUTE` | `1200` | ≥ 0 | `api`: `/api/v1/webhooks/*` |
| `LIDRADAR_SHUTDOWN_TIMEOUT` | `10s` | > 0 | `api` |
| `LIDRADAR_DATABASE_URL` | в `development`/`test` — `postgres://lidradar:lidradar@127.0.0.1:5432/lidradar?sslmode=disable`, иначе пусто | проверяется при открытии пула | все с базой |
| `LIDRADAR_DATABASE_MAX_CONNS` | `10` | > 0, ≥ min | все с базой (на каждый пул) |
| `LIDRADAR_DATABASE_MIN_CONNS` | `1` | ≥ 0 | то же |
| `LIDRADAR_DATABASE_TIMEOUT` | `5s` | > 0 | подключение и `Ping` |
| `LIDRADAR_ALLOWED_ORIGINS` | пусто | список `http(s)://host` без пути | `api` (CSRF) |
| `LIDRADAR_SESSION_TTL` | `720h` | > 0 | `api` |
| `LIDRADAR_COOKIE_SECURE` | `true` в `staging`/`production`, иначе `false` | **обязан быть `true`** в `staging`/`production` | `api` (cookie и HSTS) |
| `LIDRADAR_PUBLIC_BASE_URL` | пусто | `https://host` без пути; только вместе с ключом шифрования | `api` (webhook Telegram) |
| `LIDRADAR_INTEGRATION_ENCRYPTION_KEY` | пусто | base64 ровно 32 байт; только вместе с URL | `api` |
| `LIDAR_TELEGRAM_TOKEN` (имя без `RA`) | пусто | `^[0-9]{5,20}:[A-Za-z0-9_-]{20,100}$` | `worker` (уведомления), помощник подключения |
| `LIDRADAR_TELEGRAM_BOT_USERNAME` | `LidRadarDevBot` | `^[A-Za-z0-9_]{5,32}$` | `api` (ссылка `/start`) |
| `LIDRADAR_NOTIFICATIONS_OWNER_ESCALATION` | `false` | bool | `worker` |
| `LIDRADAR_NOTIFICATIONS_OWNER_ESCALATION_AFTER` | `30m` | > 0 | `worker` |
| `LIDRADAR_AI_MODEL_VERSION` | `lidradar-main-v1` | ≤ 200 | `api`, `worker`, `ai-agent` — должны совпадать |
| `LIDRADAR_AI_SIGNATURE_WINDOW` | `60s` | > 0 | `api` |
| `LIDRADAR_AI_CLOUD_URL` | пусто | `http(s)://host`, в `staging`/`production` только `https` | `ai-agent` |
| `LIDRADAR_AI_CREDENTIALS_FILE` | пусто | абсолютный путь вне development/test | `ai-agent` |
| `LIDRADAR_AI_PROVIDER` | `fake` | `fake` \| `llama`; `fake` запрещён вне development/test | `ai-agent` |
| `LIDRADAR_AI_LLAMA_URL` | `http://llama-server:8080/v1/chat/completions` | URL | `ai-agent` |
| `LIDRADAR_AI_POLL_INTERVAL` / `…_HEARTBEAT_INTERVAL` / `…_HTTP_TIMEOUT` | `1s` / `10s` / `5m` | > 0 | `ai-agent` |

Только для Compose (процессы их не читают): `LIDRADAR_AI_CREDENTIALS_HOST_FILE`,
`LIDRADAR_AI_MODEL_FILE`, `LIDRADAR_AI_MODELS_DIR`, `LIDRADAR_AI_AGENT_IMAGE`,
`LIDRADAR_AI_ENV`, `LIDRADAR_BUILD_VERSION`, `LIDRADAR_BUILD_REVISION`.
Образец — `.env.example`; настоящий `.env` в git не попадает.

Процесс-специфичные значения, не выносимые в окружение: аренды 30 с,
повторы 5 с…10 мин, пачка планировщика 100, паузы 500 мс / 1 с, дебаунс AI
60 с, таймаут Telegram 10 с, таймауты HTTP-сервера 5/30/30/60 с.

## 2. Сборка и образы

- `make build` — статические бинарники `bin/lidradar-{api,worker,scheduler,ai-agent,ai-node-register,ai-node-manage,migrate}`
  с `-ldflags -X buildinfo.Version/Revision` (`BUILD_VERSION`, `BUILD_REVISION`
  из `git rev-parse`).
- `Dockerfile` — двухстадийная сборка (`golang:1.26-alpine` → `alpine:3.22`,
  `CGO_ENABLED=0`, `-trimpath -s -w`), один образ на команду через
  `--build-arg COMMAND=<cmd>`; непривилегированный пользователь `lidradar`,
  `ca-certificates`, `tzdata`. Версия и ревизия видны в `/health/ready`.
- CI собирает образ `lidradar-api` с `REVISION=${{ github.sha }}`.

## 3. Развёртывание

### 3.1. Compose (`compose.yaml`, проект `lidradar`)

| Сервис | Команда | Зависимости | Порты / проверка |
|---|---|---|---|
| `postgres` | `postgres:18-alpine`, том `lidradar-postgres` | — | `5432`; `pg_isready` каждые 2 с |
| `migrate` | `COMMAND=migrate`, `restart: "no"` | `postgres` здоров | однократно |
| `api` | `COMMAND=api` | `migrate` завершился успешно | `8080`; `wget /health/ready` каждые 3 с |
| `worker` | `COMMAND=worker` (`LIDAR_TELEGRAM_TOKEN`, флаг эскалации, имя бота) | `migrate` | без портов и healthcheck |
| `scheduler` | `COMMAND=scheduler` | `migrate` | без портов |
| `ai-agent` | профиль `ai`, `fake`-провайдер, `LIDRADAR_AI_CLOUD_URL=http://api:8080`, реквизиты из `./runtime/ai-node.json` | `api` здоров | локальная разработка |

Общее окружение: `LIDRADAR_ENV=development`, адрес базы внутри сети, пул
10/1. Политики перезапуска, кроме `migrate`, не заданы — в бою их задаёт
оркестратор.

Порядок запуска: база → миграции → API (готовность = совпадение миграций) →
worker и scheduler. Обновление: собрать образы новой ревизии → выполнить
`migrate` → перезапустить API/worker/scheduler той же ревизии (миграции
только вперёд, старая сборка с новой схемой выдаст `503` на готовности).

### 3.2. Домашний AI-узел (`docker-compose.ai.yml`)

Отдельный хост с GPU: `llama-server` (только внутренняя сеть, `read_only`,
`cap_drop ALL`, healthcheck `/health`) и `ai-agent` (единственный с выходом
наружу, `LIDRADAR_ENV=production` по умолчанию, HTTPS до Cloud Core,
реквизиты `:ro`). Регистрация узла — `cmd/ai-node-register` на стороне Cloud
Core, файл реквизитов переносится на узел вручную. Управление —
`cmd/ai-node-manage allow-tenant|rotate|revoke`. Детали — [07-ai.md](07-ai.md) § 8.

### 3.3. Первый запуск в новом окружении

1. Задать `LIDRADAR_ENV`, `LIDRADAR_DATABASE_URL`, `LIDRADAR_COOKIE_SECURE=true`
   (вне development), при необходимости `LIDRADAR_PUBLIC_BASE_URL` +
   `LIDRADAR_INTEGRATION_ENCRYPTION_KEY` (`openssl rand -base64 32`),
   `LIDAR_TELEGRAM_TOKEN`, `LIDRADAR_ALLOWED_ORIGINS` для фронтенда.
2. `go run ./backend/cmd/migrate` (или сервис `migrate`).
3. Запустить `api`, `worker`, `scheduler`; убедиться, что `/health/ready`
   возвращает ожидаемую последнюю миграцию.
4. Зарегистрировать первого пользователя через API, выдать ему
   `PLATFORM_ADMIN`: `go run ./backend/cmd/platform-admin grant --email <email>`.
5. Для Telegram: OWNER выпускает подключение через
   `scripts/telegram-connect-safe.sh` (см. `backend/README.md`), проверяет
   `GET /api/v1/integrations/{id}/health` = `ACTIVE`.
6. Для AI: `ai-node-register`, перенос реквизитов, запуск узла, проверка
   `GET /api/v1/admin/ai/nodes`.

## 4. Командная строка

| Команда | Назначение | Особенности |
|---|---|---|
| `migrate` | применить встроенные миграции | владелец схемы; коды 0/1; сигналы не слушает |
| `platform-admin grant\|revoke\|list --email --note` | права платформенного администратора | единственный способ выдать первое право; код 2 при неверных аргументах |
| `ai-node-register --tenant-id --name --output` | регистрация узла | пишет файл реквизитов `0600`, секрет не печатает |
| `ai-node-manage allow-tenant\|rotate\|revoke` | допуски и секреты узла | |
| `load-generate --organizations --conversations --messages --label --webhook-secret` | синтетический набор для staging | отказывается в `production` |
| `ai-dataset-generate`, `ai-dataset-audit`, `ai-benchmark` | наборы и измерение модели | `make ai-dataset-audit` входит в `make check` |

## 5. Наблюдаемость

- **Логи** — `slog` JSON в stderr с полями `service`, `environment`,
  `event`, `request_id`, `trace_id`. Ключевые события: `runtime.starting` /
  `runtime.stopped` / `runtime.failed` / `runtime.configuration_invalid`,
  `http.server.started`, `http.request.completed` (метод, путь, статус,
  `duration_ms`), `http.panic`, `outbox.failed`, `job.failed` /
  `job.succeeded` (`job_id`, `job_type`, `tenant_id`, `attempt`,
  `error_code`, `retryable`, `duration_ms`), `notification.delivery_failed`,
  `notification.telegram.disabled`, `scheduler.failed`,
  `risk.invalidation.reconnect`, `postgres.migrations.applied`,
  `ai.agent.configured` / `ai.agent.operation_failed`. Раз в минуту worker
  пишет `background.queue.status` и `notification.queue.status` со
  счётчиками очередей — это основной сигнал «очередь растёт».
- **Корреляция** — входящий `X-Request-ID`/`Traceparent` или сгенерированные
  значения; `X-Request-ID` возвращается в ответе, `traceId` — в конверте
  ошибки.
- **Готовность** — `/health/live`, `/health/ready` (версия сборки, применённая
  и ожидаемая миграция).
- **Административная панель** (`/api/v1/admin/*`): очереди по статусам,
  истёкшие аренды, просроченные проверки, мёртвые элементы, узлы AI и их
  heartbeat, прогоны и статусы применения, потребление по организациям,
  трасса от сообщения до выручки, здоровье каналов.
- **Метрик Prometheus и трассировки OpenTelemetry нет**; базис допускает их
  добавление без ADR, экспортёр должен переиспользовать запросы панели.
- **Что смотреть в бою**: рост `jobs_pending`/`scheduled_checks_overdue`
  (worker/scheduler отстают), `deliveries_dead` (Telegram), `aiJobs.pending`
  и `nodesReady = 0` (узел), доля `429` на вебхуках (предел частоты),
  `503` на `/health/ready` после деплоя (миграции), p95 сводки аналитики.

## 6. Ёмкость и масштабирование

Baseline измерен нагрузочным испытанием этапа 25 на наборе 100 организаций
× 500 переписок × 10 сообщений (500 000 сообщений) в процессе без сети
(`docs/roadmap/STAGE_25_CAPACITY_REPORT.md`, runbook
`docs/runbooks/capacity-test.md`):

| Показатель | Измерено | Цель |
|---|---|---|
| API p95 без AI | 5–123 мс (тяжелее всего сводка аналитики) | < 300 мс |
| сохранение вебхука p95 | 21 мс при 32 параллельных | < 200 мс |
| worker | 171 задание/с одним процессом, 480 четырьмя | — |
| риск по правилу после срока p95 | 3,1 с | < 10 с |
| DB p95 | 1,7 мс (17 380 запросов) | — |
| очередь AI | 179 заданий/с при имитированном узле | предел — вывод модели |

Вывод модели на RTX 4060 (этап 15): p50/p95/p99 1 799/3 848/4 183 мс,
≤ 5 360 МиБ, один слот → 0,26–0,56 задания/с. При средней нагрузке
≈ 0,19 задания/с узел занят на 35–75 %, в пик спрос превышает ёмкость.

Триггеры масштабирования (§73): AI queue p95 wait > 60 с стабильно или GPU
≈ 100 % при backlog → добавлять AI-ёмкость (до этого — окно тишины,
приоритеты, отложенный анализ исходящих). API, worker, scheduler и
PostgreSQL до ста организаций масштабировать не требуется (запас > 100×);
при росте — несколько экземпляров worker (безопасно по `SKIP LOCKED`),
1–2 API за балансировщиком (помнить про per-process лимит частоты).

Держите `LIDRADAR_DATABASE_MAX_CONNS` не ниже параллелизма API; лимит
вебхуков — выше ожидаемого пика провайдера с общих адресов.

## 7. Регламентные процедуры

| Процедура | Как | Когда |
|---|---|---|
| резервная копия | `scripts/backup.sh` (`LIDRADAR_BACKUP_MODE`, `LIDRADAR_BACKUP_KEEP`) | по расписанию хоста, например 03:00 UTC |
| учение восстановления | `scripts/restore-drill.sh` | перед релизом и в чек-листе пилота |
| нагрузочное испытание | `go test -tags load -run TestLoadCapacityBaseline` с переменными `LIDRADAR_LOAD_*` | перед пилотом и при росте нагрузки |
| ротация секрета узла | `ai-node-manage rotate` и замена файла на узле | при подозрении на утечку |
| разбор мёртвых элементов | панель `dead-letters`, `retry`/`replay`/`discard` | ежедневно в пилоте |
| проверка Telegram | `GET /integrations/{id}/health`, `deliveries_dead` | после смены токена или URL |

Локальная разработка: `docker compose up --build` поднимает базу, миграции,
API, worker, scheduler; профиль `ai` добавляет узел с `fake`-провайдером.
Полная проверка перед коммитом — `make check` плюс `make test-db` с
`-race` (см. [11-testing.md](11-testing.md)).
