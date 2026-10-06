# Нагрузочное испытание (этап 25)

Испытание живёт в тесте `TestLoadCapacityBaseline` с тегом `load`: оно
создаёт синтетический набор ТЗ §72 в изолированной схеме PostgreSQL, ходит в
API под ролью `lidradar_app` с включённым RLS, шлёт всплеск вебхуков, гонит
worker и планировщик, имитирует AI-узел и пишет отчёт JSON.

Уточнение 2026-10-05: это регрессионный in-process baseline, не полный
capacity gate. Наличие теста, флагов и helper сверяют в репозитории
реализации; текущий комплект их не содержит. Новый результат не перезаписывает
исторический отчёт этапа 25 и получает отдельные run ID и release tuple.

## Запуск

```bash
RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)"
REPORT_DIR="$(mktemp -d)"
LIDRADAR_DATABASE_URL="postgres://lidradar:lidradar@127.0.0.1:5432/lidradar?sslmode=disable" \
LIDRADAR_LOAD_TRACE=1 \
LIDRADAR_LOAD_ORGANIZATIONS=100 LIDRADAR_LOAD_CONVERSATIONS=500 LIDRADAR_LOAD_MESSAGES=10 \
LIDRADAR_LOAD_REQUESTS=300 LIDRADAR_LOAD_CONCURRENCY=16 LIDRADAR_LOAD_WEBHOOKS=400 \
LIDRADAR_LOAD_REPORT="$REPORT_DIR/capacity-$RUN_ID.json" \
go test -tags load -count=1 -run TestLoadCapacityBaseline -v -timeout 60m ./backend/internal/integration/...
```

Переменные: размер набора (`LIDRADAR_LOAD_ORGANIZATIONS`,
`LIDRADAR_LOAD_CONVERSATIONS`, `LIDRADAR_LOAD_MESSAGES`), число запросов на
конечную точку и параллелизм API, размер всплеска вебхуков, путь отчёта.
`LIDRADAR_LOAD_TRACE=1` включает клиентский профилировщик запросов pgx:
в журнал и отчёт попадают двенадцать самых медленных запросов по p95 и общий
DB p95 (LR-BE-2506). Схема испытания удаляется после теста.

Тест запускают только на выделенной тестовой базе, не production.
Созданный отчёт и полный test log сохраняют в хранилище результатов
вместе с run ID, commit, schema, конфигурацией и характеристиками стенда.
Недоступная БД, неисполненный `TestLoadCapacityBaseline` или неожиданный
`SKIP` являются провалом по [RG-TESTS](../engineering/RELEASE_GATES.md#rg-tests).

Измеряется путь middleware → сервис → PostgreSQL без сетевого перехода:
`httptest` вызывает обработчик напрямую, поэтому влияние сети и proxy
нужно измерять отдельно в end-to-end прогоне, а не складывать квантили.
Ограничитель частоты в
испытание не входит: всплеск вебхуков идёт с одного адреса, а в бою предел
для `/api/v1/webhooks/*` задаётся отдельно
(`LIDRADAR_HTTP_WEBHOOK_RATE_LIMIT_PER_MINUTE`, по умолчанию 1200 в минуту на
адрес). Его проверяют вместе с доверительной proxy-цепочкой, допустимым
профилем всплесков и retry; выключение лимита не считается приёмкой.

Результаты исторического прогона 2026-09-03 и разбор узких мест приведены в
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
задержку и использование ресурсов текущего AI tuple измеряют новым
benchmark; старый этап 15 не подставляют вместо него. Здесь узел имитируется,
а наличие отчёта само по себе не означает успех теста.

## Полный gate текущего выпуска

Статус: `REQUIRED_NOT_VERIFIED`. До заявления о production-ёмкости
выполняется [RG-CAPACITY](../engineering/RELEASE_GATES.md#rg-capacity).

- **Профиль:** согласовать arrivals, concurrency, длительность soak, peak,
  долю retries, размер истории, hot tenants, attachment metadata и фоновые
  операции. Указать ожидаемые границы ошибок и восстановление после пика.
- **Стенд:** реальный API через TLS/proxy/rate limits, worker/scheduler,
  PostgreSQL с RLS и подтверждённый AI release tuple, без подмены fake.
- **Измерение:** число завершений за wall-clock интервал, latency
  end-to-end и по фазам, очередь и её возраст, pool wait, CPU/RAM/VRAM,
  GPU utilization, ошибки/429, drops, повторные операции и tenant fairness.
- **Отказы:** потеря AI/БД/NOTIFY, restart процессов, пиковый backlog;
  после восстановления нет дублирования денег и нарушения freshness.
- **Результат:** неизменяемые входы и JSON/logs на один run ID; проверенные
  SLO и явные ограничения. Throughput не рассчитывается как `1 / p95`,
  а месячная сумма сообщений не объявляется числом AI-заданий.
