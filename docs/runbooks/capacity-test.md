# Нагрузочное испытание (этап 25)

Испытание живёт в тесте `TestLoadCapacityBaseline` с тегом `load`: оно
создаёт синтетический набор ТЗ §72 в изолированной схеме PostgreSQL, ходит в
API под ролью `lidradar_app` с включённым RLS, шлёт всплеск вебхуков, гонит
worker и планировщик, имитирует AI-узел и пишет отчёт JSON.

## Запуск

```bash
LIDRADAR_DATABASE_URL="postgres://lidradar:lidradar@127.0.0.1:5432/lidradar?sslmode=disable" \
LIDRADAR_LOAD_TRACE=1 \
LIDRADAR_LOAD_ORGANIZATIONS=100 LIDRADAR_LOAD_CONVERSATIONS=500 LIDRADAR_LOAD_MESSAGES=10 \
LIDRADAR_LOAD_REQUESTS=300 LIDRADAR_LOAD_CONCURRENCY=16 LIDRADAR_LOAD_WEBHOOKS=400 \
LIDRADAR_LOAD_REPORT="$PWD/docs/roadmap/STAGE_25_CAPACITY_REPORT.json" \
go test -tags load -count=1 -run TestLoadCapacityBaseline -v -timeout 60m ./backend/internal/integration/...
```

Переменные: размер набора (`LIDRADAR_LOAD_ORGANIZATIONS`,
`LIDRADAR_LOAD_CONVERSATIONS`, `LIDRADAR_LOAD_MESSAGES`), число запросов на
конечную точку и параллелизм API, размер всплеска вебхуков, путь отчёта.
`LIDRADAR_LOAD_TRACE=1` включает клиентский профилировщик запросов pgx:
в журнал и отчёт попадают двенадцать самых медленных запросов по p95 и общий
DB p95 (LR-BE-2506). Схема испытания удаляется после теста.

Измеряется путь middleware → сервис → PostgreSQL без сетевого перехода:
`httptest` вызывает обработчик напрямую, поэтому сетевые задержки прокси и
клиента прибавляются к приведённым числам отдельно. Ограничитель частоты в
испытание не входит: всплеск вебхуков идёт с одного адреса, а в бою предел
для `/api/v1/webhooks/*` задаётся отдельно
(`LIDRADAR_HTTP_WEBHOOK_RATE_LIMIT_PER_MINUTE`, по умолчанию 1200 в минуту на
адрес) и должен быть выше ожидаемого пика провайдера.

Результаты последнего прогона и разбор узких мест — в
[`../roadmap/STAGE_25_CAPACITY_REPORT.md`](../roadmap/STAGE_25_CAPACITY_REPORT.md).

## Набор на стенде

Для staging тот же набор создаётся командой:

```bash
LIDRADAR_ENV=staging LIDRADAR_DATABASE_URL=... \
go run ./backend/cmd/load-generate --organizations 100 --conversations 500 --messages 10 --label stage
```

Команда отказывается работать в `production`. Владельцы набора получают
почту `<label>-owner-<n>@load.test`; пароль не задан, входить нужно через
сессии, созданные напрямую, либо задать пароль отдельно.

## Пороги (ТЗ §72, §73)

| Показатель | Цель |
|---|---|
| API p95 без AI | < 300 мс |
| Webhook persist p95 | < 200 мс |
| Rule Risk после срока | < 10 с |
| AI queue p95 wait | < 60 с, иначе триггер масштабирования AI |

Тест помечает превышение как ошибку, но отчёт записывается всегда. Реальную
задержку вывода модели и загрузку GPU даёт бенчмарк этапа 15 на RTX 4060;
здесь узел имитируется.
