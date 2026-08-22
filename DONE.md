# Аудит выполнения задач LR-BE-0001 — LR-BE-1509

Дата аудита: 2026-08-22. Статусы сверены с кодом и тестами текущей ветки, а не
только с историей merge. `Частично` означает, что контракт или in-memory
реализация есть, но production-требование (прежде всего PostgreSQL) не закрыто.

## Этап 0 — Architecture / Repository Freeze

- **LR-BE-0001 — частично.** Созданы базовые каталоги backend, contracts и docs;
  предусмотренные картой `frontend` и `infra` отсутствуют, часть platform-каталогов
  остаётся пустой.
- **LR-BE-0002 — частично.** На реализованных модулях видны слои domain,
  application, infrastructure и transport, но шаблон не создан для большинства
  доменов из спецификации.
- **LR-BE-0003 — выполнено.** Есть статический architecture check и его тесты.
- **LR-BE-0004 — выполнено.** Приняты 31 ADR, фиксирующие исходные решения.
- **LR-BE-0005 — выполнено.** MVP non-goals вынесены в отдельный документ.

## Этап 1 — Foundation

- **LR-BE-0101 — выполнено.** Go-модуль и пять обязательных runtime-команд
  компилируются.
- **LR-BE-0102 — выполнено.** Реализована типизированная конфигурация с
  валидацией и тестами.
- **LR-BE-0103 — частично.** Bootstrap использует `slog`, но сквозной JSON
  logging и политика редактирования чувствительных полей не проверены.
- **LR-BE-0104 — не выполнено.** PostgreSQL platform и pgx-подключение
  отсутствуют.
- **LR-BE-0105 — не выполнено.** Нет migration framework и SQL-миграций.
- **LR-BE-0106 — частично.** HTTP server поднимается, но общей platform-обвязки
  chi/middleware нет.
- **LR-BE-0107 — частично.** Отдельные handlers формируют JSON-ошибки, единого
  error envelope с trace ID нет.
- **LR-BE-0108 — не выполнено.** Docker Compose отсутствует.
- **LR-BE-0109 — частично.** Есть GitHub workflow architecture check; полной CI
  проверки build/test/migrations нет.

## Telegram feasibility spike

- **LR-BE-TG-001 — LR-BE-TG-012 — не выполнены.** Development bot, проверка
  non-Premium аккаунта, реальные `business_connection`/incoming/manual outgoing,
  варианты событий, reconnect, duplicate delivery, stable identifiers, linking,
  тестовое уведомление и spike report в репозитории отсутствуют. Без Telegram
  credentials и внешнего аккаунта эти проверки нельзя достоверно заявить.

## Этапы 2–7 — базовые бизнес-модули

- **LR-BE-0201 — LR-BE-0212 — не выполнены.** Нет User, Argon2id-аутентификации,
  opaque sessions, Organization, Membership, permission service, Location,
  Business Hours, Auth/Organization/Location API и tenant test harness.
- **LR-BE-0301 — LR-BE-0305 — не выполнены.** Service Catalog, денежная
  валидация, миграция, CRUD API и tenant-тесты отсутствуют.
- **LR-BE-0401 — LR-BE-0412 — не выполнены.** Нет ChannelConnection,
  connection health, connector interface/implementations, RawEvent persist-first
  хранения, Integration API и обязательных duplicate/invalid-payload тестов.
- **LR-BE-0501 — LR-BE-0512 — не выполнены.** Contact, ExternalIdentity,
  Conversation, Message, canonical ingestion, revision/read API и connector
  fixtures отсутствуют.
- **LR-BE-0601 — LR-BE-0612 — не выполнены.** Общая PostgreSQL job queue,
  scheduled checks, transactional outbox, dispatcher, retry/dead/crash semantics
  отсутствуют; наличие пустого scheduler runtime эти требования не закрывает.
- **LR-BE-0701 — LR-BE-0708 — не выполнены.** Opportunity aggregate, stage
  machine/history, one-active invariant, potential revenue, processor и API
  отсутствуют.

## Этап 8 — NO_RESPONSE

- **LR-BE-0801 — выполнено на уровне domain.** Определены Risk aggregate,
  severity/status и evidence.
- **LR-BE-0802 — частично.** Интерфейс и in-memory repository tenant-aware, но
  PostgreSQL repository и DB constraints отсутствуют.
- **LR-BE-0803 — выполнено.** NO_RESPONSE использует версионированную
  детерминированную policy.
- **LR-BE-0804 — выполнено.** Реализован расчёт business time с timezone и
  недельным расписанием.
- **LR-BE-0805 — выполнено на уровне policy.** Проверяются направление,
  активность Opportunity, ответ и severity thresholds без AI.
- **LR-BE-0806 — частично.** Due time вычисляется в domain, но scheduled check
  не сохраняется.
- **LR-BE-0807 — частично.** Evaluator работает с переданным актуальным
  snapshot; production re-read из PostgreSQL отсутствует.
- **LR-BE-0808 — частично.** In-memory repository дедуплицирует активный риск;
  атомарного partial unique index нет.
- **LR-BE-0809 — выполнено в in-memory flow.** Ответ или неактивная Opportunity
  разрешают активный риск.
- **LR-BE-0810 — выполнено.** Есть boundary/timezone/closed-hours тесты.

## Этап 9 — Radar API / SSE

- **LR-BE-0901 — LR-BE-0903 — выполнены в памяти.** Реализованы Radar query,
  стабильная приоритизация и summary, включая decimal-строки сумм.
- **LR-BE-0904 — выполнено.** Есть tenant-scoped risk list handler с cursor.
- **LR-BE-0905 — частично.** Detail API и составной read model есть, но реальные
  связи с отсутствующими Conversation/Opportunity модулями не интегрированы.
- **LR-BE-0906 — LR-BE-0907 — выполнены в памяти.** Acknowledge и Resolve
  идемпотентны и tenant-scoped.
- **LR-BE-0908 — LR-BE-0909 — выполнены.** SSE hub отправляет только invalidation
  signals; durable данные через SSE не передаются.
- **LR-BE-0910 — выполнено на handler-уровне.** Тесты проверяют `risks.read` и
  `risks.manage`; полноценного Membership permission service пока нет.

## Этап 10 — Telegram notifications

- **LR-BE-1001 — LR-BE-1004 — выполнены на domain/in-memory уровне.** Есть
  Notification, Delivery, Telegram link и одноразовые SHA-256 token с TTL/used_at;
  PostgreSQL storage отсутствует.
- **LR-BE-1005 — выполнено.** Telegram transport вызывает Bot API и не меняет
  Risk при ошибке.
- **LR-BE-1006 — частично.** Intent и delivery создаются согласованно in-memory,
  но transactional Outbox handler отсутствует.
- **LR-BE-1007 — выполнено в сервисе.** Попытки доставки и retry schedule
  сохраняются как отдельные записи в памяти.
- **LR-BE-1008 — выполнено.** Используется детерминированный dedup key открытия
  Risk.
- **LR-BE-1009 — выполнено.** Разрешены только OPEN_RISK, ACKNOWLEDGE и SNOOZE,
  с tenant link и idempotency.
- **LR-BE-1010 — выполнено.** Тесты покрывают ошибку Telegram и повторную
  доставку без изменения Risk.

## Этап 11 — Recommendation / Action / Outcome

- **LR-BE-1101 — LR-BE-1102 — выполнены.** Есть deterministic recommendation
  templates, не зависящие от AI.
- **LR-BE-1103 — выполнено в памяти.** Action моделируется append-only.
- **LR-BE-1104 — выполнено.** Есть tenant-scoped Action API.
- **LR-BE-1105 — выполнено в памяти.** Outcome моделируется append-only.
- **LR-BE-1106 — выполнено.** Есть tenant-scoped Outcome API.
- **LR-BE-1107 — выполнено.** Idempotency-Key replay возвращает прежний ответ,
  несовпадающий payload даёт conflict.
- **LR-BE-1108 — частично.** Audit создаётся вместе с fact в in-memory store;
  транзакционной PostgreSQL-гарантии нет.

## Этап 12 — Revenue / Attribution

- **LR-BE-1201 — выполнено в памяти.** Есть RevenueEvent с точной строковой
  decimal-суммой и валютой.
- **LR-BE-1202 — выполнено.** Confirmation API требует permission и
  Idempotency-Key.
- **LR-BE-1203 — выполнено в памяти.** Создаётся формальная attribution.
- **LR-BE-1204 — выполнено.** Проверяется совпадение tenant и Opportunity у
  Risk/Action/Outcome/Revenue.
- **LR-BE-1205 — выполнено.** Централизовано окно attribution 30 дней.
- **LR-BE-1206 — частично.** Уникальность обеспечена in-memory, SQL UNIQUE нет.
- **LR-BE-1207 — выполнено.** Confirmed Recovered Revenue суммируется отдельно
  по валютам только для RECOVERED attribution.
- **LR-BE-1208 — частично.** Audit формируется атомарно только внутри in-memory
  mutex, не PostgreSQL transaction.
- **LR-BE-1209 — выполнено как service integration test.** Проверен money loop
  и сумма 47 000; полного API/DB E2E нет.

## Этап 13 — Home AI Node

- **LR-BE-1301 — LR-BE-1304 — выполнены в памяти.** Есть Node, Job, Run и
  freshness snapshots; durable PostgreSQL tables отсутствуют.
- **LR-BE-1305 — выполнено.** Node secret хранится как SHA-256 digest и
  проверяется constant-time сравнением.
- **LR-BE-1306 — LR-BE-1309 — выполнены в сервисе.** Heartbeat, atomic claim,
  start/complete/fail, 120-second lease, renewal и reclaim проверены тестами;
  durability между процессами отсутствует.
- **LR-BE-1310 — выполнено.** Go AI Agent использует outbound polling и не пишет
  customer text на диск.
- **LR-BE-1311 — выполнено.** Реализован клиент OpenAI-compatible llama.cpp.
- **LR-BE-1312 — частично.** Agent после запуска продолжает polling, но Docker/
  systemd automatic reboot recovery не настроен.
- **LR-BE-1313 — выполнено.** Есть deterministic Fake AI Provider.
- **LR-BE-1314 — выполнено на unit-уровне.** Проверены expiry/reclaim и запрет
  завершения job прежним owner; реального node disconnect E2E нет.

## Этап 14 — AI Conversation Analysis

- **LR-BE-1401 — LR-BE-1403 — выполнены.** ContextBuilder ограничивает последние
  20 сообщений/примерно 3000 токенов, request и JSON Schema версионированы.
- **LR-BE-1404 — LR-BE-1406 — выполнены.** Строгий decoder отвергает invalid
  JSON, неизвестные поля/enums, неверные ranges/evidence и противоречивые price
  facts.
- **LR-BE-1407 — выполнено.** Заданы strong/weak/untrusted thresholds; факты ниже
  0.65 не поступают policy.
- **LR-BE-1408 — LR-BE-1409 — выполнены в памяти.** Job/Run сохраняют model,
  prompt/schema versions и сырой output; durable audit отсутствует.
- **LR-BE-1410 — LR-BE-1411 — выполнены.** Revision и through-message freshness
  проверяются, stale result сохраняется как STALE и ставит replacement job.
- **LR-BE-1412 — выполнено в памяти.** Fresh result обновляет только derived
  ConversationSummary.
- **LR-BE-1413 — выполнено.** Contract tests покрывают invalid/missing/unknown,
  low confidence, stale revision и provider failures; реальный timeout зависит
  от переданного context.

## Этап 15 — AI Benchmark / Model Freeze

- **LR-BE-1501 — выполнено.** Добавлен строгий versioned JSONL format с
  синтетическими input и ожидаемыми semantic facts.
- **LR-BE-1502 — не выполнено.** Цель 300–500 размеченных случаев не достигнута:
  в golden fixture четыре синтетических случая. Реальные данные нельзя выдумывать
  или добавлять без разметки и consent.
- **LR-BE-1503 — выполнено на уровне формата.** Runner принимает только TRAIN,
  VALIDATION или GOLDEN и запрещает повтор ID; наполнение всех трёх split ещё не
  выполнено.
- **LR-BE-1504 — выполнено.** Golden JSONL защищён reviewable SHA-256; runner
  прекращает запуск при несовпадении.
- **LR-BE-1505 — частично.** Добавлен manifest с contract/prompt/dataset/runtime,
  target hardware и местами под gates. Числа gate, model artifact и checksum
  намеренно `null`: они не заданы в Tasks.md и должны появиться после решения и
  измерений, а не быть выдуманы при реализации.
- **LR-BE-1506 — выполнено.** `ai-benchmark` вызывает llama.cpp, валидирует каждый
  результат существующим production contract и выдаёт JSON report/ненулевой
  exit code при провале gate.
- **LR-BE-1507 — выполнено.** Считаются TP/FP/FN, precision, recall, F1, exact
  match rate и invalid results только по trusted facts.
- **LR-BE-1508 — выполнено.** Считаются p50/p95/p99 latency и throughput; runner
  требует явно передать утверждённый максимальный p95.
- **LR-BE-1509 — не выполнено по объективной причине.** `lidradar-main-v1`
  оставлен `CANDIDATE_NOT_FROZEN`: нет утверждённых значений gate, 300–500
  случаев, выбранного model artifact и измерения quality/performance на целевой
  RTX 4060. Ложная фиксация нарушила бы Exit Gate и требование выбирать модель
  измерениями.

## Итог аудита

Этап 15 предоставляет воспроизводимый benchmark-контур, но его Exit Gate ещё не
пройден. Главный общий долг этапов 1–14 — отсутствие PostgreSQL persistence,
миграций, tenant RLS/composite FK и реальных connector E2E. Поэтому имеющиеся
in-memory реализации и unit tests нельзя считать production-ready вертикальным
slice, даже когда отдельная логика задачи реализована полностью.
