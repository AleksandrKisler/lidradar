# Эксплуатация

Конфигурация, сборка, развёртывание, запуск, наблюдение и ёмкость. Runbooks
с пошаговыми процедурами — в `docs/runbooks/`.

Сверка документации: 2026-10-05. Команды и значения ниже описывают
документированный baseline; исходники приложения и результаты нового
прогона в этом комплекте отсутствуют. Допуск к эксплуатации требует
[проверок выпуска](../engineering/RELEASE_GATES.md), а не только выполнения
последовательности команд.

## 1. Конфигурация

Все процессы читают переменные окружения через `platform/config` и целиком
валидируют их при старте (даже `cmd/migrate` отвергнет неверный
`LIDRADAR_AI_LLAMA_URL`). Для всех обязательна `LIDRADAR_ENV`; в `staging` и
`production` действуют дополнительные требования, помеченные в таблице: без них
`api` не запускается, а процессы с базой не открывают пул. Отсутствие
обязательного значения или неверный формат → код выхода 1 и событие
`runtime.configuration_invalid` (так же отказывает `api` без своих обязательных
настроек); отказ открыть пул без TLS приходит как `runtime.failed` с понятной
ошибкой. Значения секретов в текст ошибки не попадают.

| Ключ | По умолчанию | Валидация | Кто использует |
|---|---|---|---|
| `LIDRADAR_ENV` | — (обязателен) | `development` \| `test` \| `staging` \| `production` | все |
| `LIDRADAR_HTTP_ADDRESS` | `:8080` | непустой | `api` |
| `LIDRADAR_HTTP_RATE_LIMIT_PER_MINUTE` | `120` | ≥ 0, 0 выключает | `api`: `/api/v1/auth/*` |
| `LIDRADAR_HTTP_WEBHOOK_RATE_LIMIT_PER_MINUTE` | `1200` | ≥ 0 | `api`: `/api/v1/webhooks/*` |
| `LIDRADAR_HTTP_AI_NODE_RATE_LIMIT_PER_MINUTE` | `600` | ≥ 0 | `api`: `/internal/v1/ai/*` |
| `LIDRADAR_SHUTDOWN_TIMEOUT` | `10s` | > 0 | `api` |
| `LIDRADAR_DATABASE_URL` | в `development`/`test` — `postgres://lidradar:lidradar@127.0.0.1:5432/lidradar?sslmode=disable`, иначе пусто | проверяется при открытии пула; в `staging`/`production` строка обязана требовать TLS (`sslmode=require`, `verify-ca` или `verify-full`; `disable`, `allow`, `prefer` и отсутствие `sslmode` отвергаются) | все с базой |
| `LIDRADAR_DATABASE_MAX_CONNS` | `10` | > 0, ≥ min | все с базой (на каждый пул) |
| `LIDRADAR_DATABASE_MIN_CONNS` | `1` | ≥ 0 | то же |
| `LIDRADAR_DATABASE_TIMEOUT` | `5s` | > 0 | подключение и `Ping` |
| `LIDRADAR_DATABASE_ALLOW_PLAINTEXT` | `false` | bool; `true` разрешает в `staging`/`production` открытый канал к PostgreSQL, только для закрытой сети (например, база на том же хосте) | все с базой |
| `LIDRADAR_TRUSTED_PROXIES` | пусто | адреса и сети через запятую (`10.0.0.5`, `172.16.0.0/12`); `/0` и записи с битами узла отвергаются; **обязателен для `api` в `staging`/`production`**; адрес клиента из `X-Forwarded-For` берётся только от этих узлов (ADR 0049) | `api` |
| `LIDRADAR_ALLOWED_ORIGINS` | пусто | список `http(s)://host` без пути; **обязателен для `api` в `staging`/`production`** (иначе процесс не стартует), остальные процессы его не требуют | `api` (CSRF) |
| `LIDRADAR_SESSION_TTL` | `720h` | > 0 | `api` |
| `LIDRADAR_COOKIE_SECURE` | `true` в `staging`/`production`, иначе `false` | **обязан быть `true`** в `staging`/`production` | `api` (cookie и HSTS) |
| `LIDRADAR_PUBLIC_BASE_URL` | пусто | `https://host` без пути; только вместе с ключом шифрования; **обязателен для `api` в `staging`/`production`** | `api` (webhook Telegram) |
| `LIDRADAR_INTEGRATION_ENCRYPTION_KEY` | пусто | base64 ровно 32 байт; только вместе с URL; **обязателен для `api` в `staging`/`production`** | `api` |
| `LIDRADAR_TELEGRAM_TOKEN` (прежнее имя `LIDAR_TELEGRAM_TOKEN` принимается, новое приоритетнее) | пусто | `^[0-9]{5,20}:[A-Za-z0-9_-]{20,128}$` | `worker` (уведомления), помощник подключения |
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

Служба копий вне хоста (`backup`, не Go-процесс) читает свои переменные `LIDRADAR_BACKUP_*`
(хранилище, ключи доступа, получатели age, расписание, сигналы мониторинга): перечень и смысл —
в [runbook](../runbooks/offhost-backup.md) § 1.4 и `deploy/production/.env.example`.

Только для Compose (процессы их не читают): `LIDRADAR_AI_CREDENTIALS_HOST_FILE`,
`LIDRADAR_AI_MODEL_FILE`, `LIDRADAR_AI_MODELS_DIR`, `LIDRADAR_AI_AGENT_IMAGE`,
`LIDRADAR_AI_ENV`, `LIDRADAR_BUILD_VERSION`, `LIDRADAR_BUILD_REVISION`.
Образец — `.env.example`; настоящий `.env` в git не попадает.

Процесс-специфичные значения, не выносимые в окружение: аренды 30 с,
повторы 5 с…10 мин, пачка планировщика 100, паузы 500 мс / 1 с, дебаунс AI
60 с, таймаут Telegram 10 с, таймауты HTTP-сервера 5/30/30/60 с.

Каноническое имя ключа интеграций:
`LIDRADAR_INTEGRATION_ENCRYPTION_KEY`. Альтернативное имя из старого текста
ADR не является подтверждённым alias. Для восстановления существующей
базы нужен соответствующий её ciphertext ключ из защищённого recovery set;
генерация нового ключа его не заменяет. Проверка реального config loader
и decrypt после восстановления обязательна по
[RG-DEPLOY](../engineering/RELEASE_GATES.md#rg-deploy).

## 2. Сборка и образы

- `make build` — статические бинарники `bin/lidradar-{api,worker,scheduler,ai-agent,ai-node-register,ai-node-manage,migrate}`
  с `-ldflags -X buildinfo.Version/Revision` (`BUILD_VERSION`, `BUILD_REVISION`
  из `git rev-parse`).
- `Dockerfile` — двухстадийная сборка (`golang:${GO_VERSION}-alpine` →
  `alpine:3.22`; версия Go закреплена: `GO_VERSION=1.26.9`, плавающий тег уже
  давал сборки с уязвимостями stdlib), `CGO_ENABLED=0`, `-trimpath -s -w`,
  общий кэш сборки BuildKit; один образ на команду через
  `--build-arg COMMAND=<cmd>`; непривилегированный пользователь `lidradar`,
  `ca-certificates`, `tzdata`, метки OCI `title`, `version`, `revision`. Версия
  и ревизия видны в `/health/ready`.
- `scripts/build-images.sh` — образы выпуска `lidradar-<команда>:<первые 12
  символов git sha>` для восьми команд и `lidradar-web:<тег>` из каталога
  веб-клиента. Собирает только из чистого дерева обоих репозиториев
  (`--allow-dirty` — для проб, тег получает суффикс `-dirty`) и печатает
  `LIDRADAR_VERSION`, который задаёт production-стек.
- CI собирает образ `lidradar-api` с `REVISION=${{ github.sha }}`.

## 3. Развёртывание

`compose.yaml` в корне — среда разработки (§ 3.1). Стек пилота для `production` и
`staging` — отдельный набор файлов `deploy/production` (§ 3.4).

### 3.1. Compose (`compose.yaml`, проект `lidradar`)

| Сервис | Команда | Зависимости | Порты / проверка |
|---|---|---|---|
| `postgres` | `postgres:18-alpine`, том `lidradar-postgres` | — | `5432`; `pg_isready` каждые 2 с |
| `migrate` | `COMMAND=migrate`, `restart: "no"` | `postgres` здоров | однократно |
| `api` | `COMMAND=api` | `migrate` завершился успешно | `8080`; `wget /health/ready` каждые 3 с |
| `worker` | `COMMAND=worker` (`LIDRADAR_TELEGRAM_TOKEN`, флаг эскалации, имя бота) | `migrate` | без портов и healthcheck |
| `scheduler` | `COMMAND=scheduler` | `migrate` | без портов |
| `ai-agent` | профиль `ai`, `fake`-провайдер, `LIDRADAR_AI_CLOUD_URL=http://api:8080`, реквизиты из `./runtime/ai-node.json` | `api` здоров | локальная разработка |

Общее окружение: `LIDRADAR_ENV=development`, адрес базы внутри сети, пул
10/1. Политики перезапуска, кроме `migrate`, не заданы — в бою их задаёт
оркестратор.

Порядок запуска: база → миграции → API (готовность = совпадение миграций) →
worker и scheduler. Обновление: собрать образы новой ревизии → выполнить
`migrate` → перезапустить API/worker/scheduler той же ревизии (миграции
только вперёд, старая сборка с новой схемой выдаст `503` на готовности).

Поэтому откат только образа, даже после аддитивной миграции, не считается
безопасным. До выпуска нужна проверенная матрица build/schema; порядок
forward-fix либо восстановления совместимого recovery set описан в
[runbook восстановления](../runbooks/backup-restore.md).

### 3.2. Домашний AI-узел (`docker-compose.ai.yml`)

Отдельный хост с GPU: `llama-server` (только внутренняя сеть, `read_only`,
`cap_drop ALL`, healthcheck `/health`) и `ai-agent` (единственный с выходом
наружу, `LIDRADAR_ENV=production` по умолчанию, HTTPS до Cloud Core,
реквизиты `:ro`). Регистрация узла — `cmd/ai-node-register` на стороне Cloud
Core, файл реквизитов переносится на узел вручную. Управление —
`cmd/ai-node-manage allow-tenant|rotate|revoke`. Детали — [07-ai.md](07-ai.md) § 8.

### 3.3. Первый запуск в новом окружении

Эта процедура предназначена для пустой установки, не для disaster recovery.
До открытия внешнего трафика обязательны проверка
[reverse proxy и определения IP](../runbooks/proxy-deployment.md), проверка
изоляции tenant и готовности. Существующую базу восстанавливают только по
[отдельному runbook](../runbooks/backup-restore.md), с прежними ключами.

1. Задать `LIDRADAR_ENV`, `LIDRADAR_DATABASE_URL` (в `staging`/`production` с
   `sslmode=require` или строже, либо явно `LIDRADAR_DATABASE_ALLOW_PLAINTEXT=true`
   для закрытой сети), `LIDRADAR_COOKIE_SECURE=true` (вне development),
   `LIDRADAR_PUBLIC_BASE_URL` + `LIDRADAR_INTEGRATION_ENCRYPTION_KEY` (новый ключ
   через `openssl rand -base64 32` только для пустой установки) и
   `LIDRADAR_ALLOWED_ORIGINS` для фронтенда и `LIDRADAR_TRUSTED_PROXIES` с
   адресами proxy (в `staging`/`production` без них `api` не запустится),
   `LIDRADAR_TELEGRAM_TOKEN`. Для production-стека пилота значения задаёт
   `deploy/production/compose.yaml` из `.env` (§ 3.4).
2. `go run ./backend/cmd/migrate` (или сервис `migrate`).
3. Запустить `api`, `worker`, `scheduler`; убедиться, что `/health/ready`
   возвращает ожидаемую последнюю миграцию.
4. Зарегистрировать первого пользователя через API, выдать ему
   `PLATFORM_ADMIN`: `go run ./backend/cmd/platform-admin grant --email <email>`.
5. Для Telegram: OWNER использует secure-connect flow по ADR 0045 либо
   операторский `scripts/telegram-connect-safe.sh`; наличие и интерфейс
   helper проверяются в соответствующем build. Затем проверяет
   `GET /api/v1/integrations/{id}/health` = `ACTIVE`.
6. Для AI: `ai-node-register`, перенос реквизитов, запуск узла, проверка
   `GET /api/v1/admin/ai/nodes`.

### 3.4. Production-стек пилота (`deploy/production`)

[ADR 0050](../adr/0050-production-topology.md); пошаговая процедура, обновление и
откат — [runbook](../runbooks/production-deployment.md). Один хост, Docker
Compose, проект `lidradar-prod`, образы выпуска без `build:`.

| Сервис | Образ | Сети | Порты / проверка |
|---|---|---|---|
| `edge` | `caddy:2.11.7-alpine` | edge | наружу `80` и `443`; HTTPS (ACME), переход с HTTP, HSTS, маршруты `/api/*`, `/internal/v1/ai/*`, `/health/live`, остальное — `web`; адрес закреплён |
| `web` | `lidradar-web:<тег>` | edge | только через edge |
| `api` | `lidradar-api:<тег>` | edge, data | `/health/ready` каждые 10 с; `LIDRADAR_TRUSTED_PROXIES=<адрес edge>/32` |
| `worker` | `lidradar-worker:<тег>` | egress, data | выход к Telegram; токен бота только у него |
| `scheduler` | `lidradar-scheduler:<тег>` | data | — |
| `migrate` | `lidradar-migrate:<тег>` | data | однократно, `restart: "no"`; `api`, `worker`, `scheduler` ждут его |
| `postgres` | `postgres:18-alpine`, том `lidradar-postgres`, файлы `postgres/10-roles.sql` и `pg_hba.conf` | data | `pg_isready` по TCP (готов после инициализации тома); порт не публикуется; администратор по сети закрыт, входят `lidradar` (владелец, `migrate`) и `lidradar_runtime` (процессы приложения) |
| `backup` | `lidradar-backup:<тег>`, том `backup-spool` | data, egress | каждые 10 минут копия вне хоста; здоров, пока точка не старше 15 минут; без приватного ключа age |
| `platform-admin`, `ai-node-register`, `ai-node-manage` | образы одноимённых команд | data | профиль `tools`: `docker compose run --rm …` |

Сеть `data` объявлена `internal`: база и `scheduler` выхода наружу не имеют.
Контейнеры работают с корневой ФС только для чтения, `cap_drop: ALL`,
`no-new-privileges`, пределами памяти, ротацией журналов (10 МБ × 5) и
`restart: unless-stopped`. Конфигурация — один `.env` вне git (`chmod 600`),
шаблон `.env.example`; обязательные значения проверяет сам compose. Секреты
получает только тот сервис, которому они нужны.

Выпуск: собрать образы (`scripts/build-images.sh`), доставить на хост, снять
копию, остановить `api`, `worker`, `scheduler`, выполнить
`docker compose run --rm migrate`, запустить стек. Окно недоступности API около
10 с на пустой базе; откат — восстановление копии (миграции только вперёд).
Роли базы — [ADR 0052](../adr/0052-database-roles-owner-and-runtime.md): `postgres` (администратор, только по
сокету), `lidradar` (владелец без суперправ: миграции, восстановление), `lidradar_runtime` (рабочий логин без
владения); три пароля в `.env`, смена и переход — [runbook](../runbooks/production-deployment.md#11-роли-базы-данных).
Копии по расписанию и вне хоста — служба `backup` ([ADR 0051](../adr/0051-offhost-backups.md),
[runbook](../runbooks/offhost-backup.md)): без хранилища и ключа получателя стек не запускается.
Приёмка снаружи и изнутри (включая возраст последней копии) — `scripts/smoke-production.sh`. Инварианты compose и
Caddyfile закреплены тестами (`scripts/tests/test_production_compose.py`) и
проверкой Caddy в CI. На настоящем хосте стек не разворачивался; перечень
непроверенного — в ADR 0050 и runbook.

## 4. Командная строка

| Команда | Назначение | Особенности |
|---|---|---|
| `migrate` | применить встроенные миграции | владелец схемы; коды 0/1; сигналы не слушает |
| `platform-admin grant\|revoke\|list --email --note` | права платформенного администратора | единственный способ выдать первое право; код 2 при неверных аргументах |
| `ai-node-register --tenant-id --name --output` | регистрация узла | пишет файл реквизитов `0600`, секрет не печатает |
| `ai-node-manage allow-tenant\|rotate\|revoke` | допуски и секреты узла | |
| `load-generate --organizations --conversations --messages --label --webhook-secret` | синтетический набор для staging; пишет пакетными `INSERT` (COPY не работает с таблицами под RLS), поэтому подходит и владельцу без суперправ | отказывается в `production` |
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

Исторический baseline измерен нагрузочным испытанием этапа 25 на наборе 100 организаций
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

Исторический замер модели на RTX 4060 (этап 15, прежние prompt и dataset):
p50/p95/p99 1 799/3 848/4 183 мс, ≤ 5 360 МиБ. Он не подтверждает параметры
текущего AI-кандидата. Обратные величины p50/p95 не являются измеренной
пропускной способностью, а проценты загрузки и суточную ёмкость из них
выводить нельзя. Для расчёта нужны фактические arrivals после debounce,
распределение времени обслуживания, concurrency, очереди и повторные попытки.

Триггеры масштабирования (§73): AI queue p95 wait > 60 с стабильно или GPU
≈ 100 % при backlog → оценивать необходимость дополнительной AI-ёмкости
(до этого проверить окно тишины, приоритеты и отложенный анализ исходящих).
Это пороги реакции, не доказательство достаточной мощности. Утверждение
о запасе более 100× и отсутствии необходимости масштабирования до ста
организаций отозвано как неподтверждённая экстраполяция. Требуется
[RG-CAPACITY](../engineering/RELEASE_GATES.md#rg-capacity): смешанная нагрузка
через реальный proxy/TLS, фоновые процессы, PostgreSQL и текущий AI tuple.
Увеличение числа worker или API проверяется вместе с DB budget,
tenant fairness и общей политикой rate limiting.

Размер пулов выбирают по общему бюджету соединений и измеренному ожиданию
пула, а не приравнивают числу HTTP-запросов. Вложенный запрос с удержанием
всех соединений исправляют в коде; увеличение пула не доказывает устранение
deadlock. Лимиты вебхуков проверяют с корректным определением IP за proxy,
с burst и изоляцией арендаторов; отключать их ради прохождения теста нельзя.

## 7. Регламентные процедуры

| Процедура | Как | Когда |
|---|---|---|
| резервная копия | служба `backup` production-стека (ADR 0051): выгрузка, шифрование, загрузка вне хоста и сверка; ручная копия перед обновлением — `scripts/backup.sh` | каждые 10 минут; возраст точки и наибольший разрыв — `lidradar-backup status`, измеренный RPO ≤ 15 минут |
| восстановление из копии вне хоста | `scripts/restore-offhost.sh` ([runbook](../runbooks/offhost-backup.md) § 4) | потеря хоста, порча данных, учение на новом хосте |
| учение по копии вне хоста | `lidradar-backup drill` у оператора или на staging (нужен приватный ключ age) | после настройки, перед production и после изменения ключей, схемы или хранилища |
| роли и права после восстановления | `scripts/bootstrap-roles.sh <база>` ([описание](../runbooks/backup-restore.md#roles-after-restore)); `--verify-only` только проверяет | после каждого восстановления на новый кластер, до запуска `api`; проверка структурная, не заменяет межорганизационные сценарии |
| учение восстановления | `scripts/restore-drill.sh` как smoke-проверка плюс полный [DR на новом кластере](../runbooks/backup-restore.md) | перед production и после изменения recovery set; schema/count smoke не заменяет восстановление ролей, ключей, RLS и интеграций |
| нагрузочное испытание | `go test -tags load -run TestLoadCapacityBaseline` с переменными `LIDRADAR_LOAD_*` | перед пилотом и при росте нагрузки |
| ротация секрета узла | `ai-node-manage rotate` и замена файла на узле | при подозрении на утечку |
| разбор мёртвых элементов | панель `dead-letters`, `retry`/`replay`/`discard` | ежедневно в пилоте |
| проверка Telegram | `GET /integrations/{id}/health`, `deliveries_dead` | после смены токена или URL |

Локальная разработка: `docker compose up --build` поднимает базу, миграции,
API, worker, scheduler; профиль `ai` добавляет узел с `fake`-провайдером.
Полная проверка перед коммитом — `make check` плюс `make test-db` с
`-race` (см. [11-testing.md](11-testing.md)).
