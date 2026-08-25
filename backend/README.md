# Устройство серверной части

Серверная часть — модульный монолит с пятью независимо собираемыми командами:

- `cmd/api` — HTTP API;
- `cmd/worker` — фоновая обработка сохранённых событий;
- `cmd/scheduler` — планировщик;
- `cmd/ai-agent` — узел локального анализа;
- `cmd/migrate` — применение миграций PostgreSQL.

Собрать все рабочие файлы из корня репозитория:

```sh
make build
```

Перед запуском каждая команда проверяет типизированные настройки. Обязательная
переменная `LIDRADAR_ENV` принимает `development`, `test`, `staging` или
`production`:

```sh
LIDRADAR_ENV=development ./bin/lidradar-api
```

Командам, работающим с базой, также нужна `LIDRADAR_DATABASE_URL`. Для локальной
разработки и тестов используется адрес из Compose; для испытательного и
рабочего окружений значения по умолчанию нет. Дополнительно настраиваются адрес
HTTP, размеры пула, время подключения и время плавной остановки. Проверка
личности использует `LIDRADAR_SESSION_TTL` (по умолчанию 30 дней),
`LIDRADAR_COOKIE_SECURE` (обязательно вне локальной среды) и список доверенных
источников `LIDRADAR_ALLOWED_ORIGINS`.

При отсутствии обязательной настройки команда завершается с ненулевым кодом.
Долго работающие процессы обрабатывают `SIGINT` и `SIGTERM`. API, фоновый
обработчик и планировщик проверяют PostgreSQL при запуске. Команда миграций
применяет встроенные неизменяемые SQL-файлы в транзакциях и отклоняет изменение
контрольной суммы уже применённого файла.

## Локальный запуск

```sh
docker compose up --build
```

Команда запускает PostgreSQL 18, миграции, API, фоновый обработчик, планировщик
и узел анализа с локальной заглушкой. Готовность доступна по адресам
`GET /health/live` и `GET /health/ready` на порту 8080.

Организация выбирается явно заголовком `X-Tenant-ID`, значение которого
возвращает `GET /api/v1/auth/me`. OWNER управляет организацией, точками,
расписанием, каталогом услуг и подключениями. MANAGER имеет только разрешения на
рабочие бизнес-операции.

Подключения каналов доступны в `/api/v1/integrations`. Событие поставщика входит
через `/api/v1/webhooks/{provider}/{tenantId}/{connectionId}`, проверяется и
сохраняется до постановки работы по преобразованию. TEST, IMPORT и
GENERIC_WEBHOOK используют локальные адаптеры. Telegram Connected Business
Connector умеет зарегистрировать настоящий webhook через Bot API, проверить
его адрес и удалить при отключении. Без двух специальных настроек Telegram
остаётся недоступным, а остальные каналы продолжают работать локально. Узел
искусственного интеллекта по-прежнему использует заглушку.

## Подключение Telegram для испытания

API должен быть доступен Telegram по публичному HTTPS-адресу без дополнительного
пути. Скопируйте `.env.example` в локальный `.env`, не добавляемый в Git, и
задайте:

```text
LIDRADAR_PUBLIC_BASE_URL=https://ваш-публичный-адрес
LIDRADAR_INTEGRATION_ENCRYPTION_KEY=<32 случайных байта в Base64>
```

Ключ создаётся один раз командой `openssl rand -base64 32`. Его потеря лишит
сервер возможности расшифровать сохранённые токены; смена ключа требует отдельной
процедуры перешифрования. Токен бота в `.env`, журнале или репозитории хранить не
нужно: OWNER передаёт его только в теле запроса подключения. Поле является
одноразовым входом и никогда не возвращается API.

```sh
read -rs TELEGRAM_BOT_TOKEN
read -rs TELEGRAM_WEBHOOK_SECRET
curl --fail-with-body \
  -X POST 'http://127.0.0.1:8080/api/v1/integrations/CONNECTED_BUSINESS_BOT/connect' \
  -H 'Content-Type: application/json' \
  -H "X-Tenant-ID: $LIDRADAR_TENANT_ID" \
  -H "Cookie: lidradar_session=$LIDRADAR_SESSION" \
  --data-binary @- <<JSON
{"name":"Telegram разработки","webhookSecret":"$TELEGRAM_WEBHOOK_SECRET","botToken":"$TELEGRAM_BOT_TOKEN"}
JSON
unset TELEGRAM_BOT_TOKEN TELEGRAM_WEBHOOK_SECRET
```

`TELEGRAM_WEBHOOK_SECRET` должен содержать 16–256 латинских букв, цифр,
подчёркиваний или дефисов; его удобно создать командой `openssl rand -hex 32`.
Успешный ответ имеет состояние `ACTIVE`. Ошибка внешней настройки оставляет
запись подключения в `ERROR/TELEGRAM_WEBHOOK_SETUP_FAILED`, не раскрывая ответ
Telegram или токен. Перед повтором следует удалить это подключение и создать
новое после устранения причины.

Это ещё не означает, что проверка применимости пройдена. Живые входящие и
исходящие сообщения, варианты файлов, повтор доставки, переподключение,
одноразовая привязка и уведомление фиксируются в
[`../docs/roadmap/TELEGRAM_SPIKE_REPORT.md`](../docs/roadmap/TELEGRAM_SPIKE_REPORT.md).

Фоновый обработчик преобразует ожидающие события в независимую от канала модель
контактов и переписок. Доступны маршруты:

```text
GET /api/v1/conversations
GET /api/v1/conversations/{conversationId}
GET /api/v1/conversations/{conversationId}/messages
```

Повтор одного события безопасен. Изменение сообщения обновляет его состояние,
удаление оставляет историю с отметкой времени, а версия переписки увеличивается
только при фактическом изменении. PostgreSQL хранит лишь метаданные вложений;
передача двоичных файлов в S3-совместимое хранилище пока заменена заглушкой.

## Границы модулей

Бизнес-возможности находятся в `internal`. Эталонное направление зависимостей:

```text
transport -> application -> domain
infrastructure ---------> domain ports
```

- `domain` — правила предметной области и порты хранения;
- `application` — сценарии использования и согласование операций;
- `infrastructure` — PostgreSQL и адаптеры внешних систем;
- `transport` — HTTP, команды и структуры обмена;
- `platform` — общие технические средства;
- `contracts` — версионированные внешние контракты.

Проверка направлений зависимостей:

```sh
go run ./backend/tools/archcheck -root backend
```

Она также обязательна в `.github/workflows/architecture.yml`. Перед расширением
архитектуры проверьте [запреты MVP](../docs/architecture/NON_GOALS.md). Изменение
зафиксированного решения требует принятого ADR.

## Проверки

Полная локальная проверка с работающей PostgreSQL:

```sh
LIDRADAR_DATABASE_URL='postgres://lidradar:lidradar@127.0.0.1:5432/lidradar?sslmode=disable' go test -race -count=1 ./...
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@v0.6.1 ./...
go run ./backend/tools/archcheck -root backend
npx --yes @redocly/cli@1.34.5 lint contracts/openapi/openapi.yaml
```

Актуальный аудит выполнения задач находится в [`../DONE.md`](../DONE.md), а
сроки подключения Telegram и локального узла анализа — в
[`../docs/roadmap/EXTERNAL_SERVICES.md`](../docs/roadmap/EXTERNAL_SERVICES.md).
