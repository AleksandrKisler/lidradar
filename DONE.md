# Аудит выполнения задач LR-BE-0001 — LR-BE-1509

Дата аудита: 2026-08-25. Статусы сверены с кодом, миграциями и тестами текущей
ветки, а не только с историей слияний. `Частично` означает, что контракт или
реализация в памяти есть, но производственное требование (прежде всего
PostgreSQL либо настоящий внешний сервис) не закрыто.

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
- **LR-BE-0103 — выполнено.** Все runtime используют JSON `slog` с полями
  service/environment/event; HTTP middleware добавляет request/trace correlation.
- **LR-BE-0104 — выполнено.** Добавлен `pgxpool` с типизированными limits,
  connect timeout, startup ping, readiness и graceful close.
- **LR-BE-0105 — выполнено.** Embedded SQL migrations применяются транзакционно,
  сериализуются advisory lock и защищены immutable SHA-256 checksum.
- **LR-BE-0106 — выполнено.** Общий `chi` HTTP platform поднимает live/ready,
  middleware и graceful shutdown с таймаутом.
- **LR-BE-0107 — выполнено.** Platform и feature handlers используют единый
  error envelope с безопасным message и trace ID.
- **LR-BE-0108 — выполнено.** `compose.yaml` поднимает PostgreSQL 18, migrate,
  API, worker, scheduler и AI agent со stub provider; конфигурация Compose валидна.
- **LR-BE-0109 — выполнено.** Backend CI проверяет gofmt, vet, staticcheck,
  race tests, architecture, migrations, API readiness, OpenAPI, binaries и Docker image.

## Telegram feasibility spike

- **LR-BE-TG-001 — LR-BE-TG-012 — не выполнены.** Development bot, проверка
  non-Premium аккаунта, реальные `business_connection`/incoming/manual outgoing,
  варианты событий, reconnect, duplicate delivery, stable identifiers, linking,
  тестовое уведомление и spike report в репозитории отсутствуют. Без Telegram
  credentials и внешнего аккаунта эти проверки нельзя достоверно заявить.

## Этапы 2–7 — базовые бизнес-модули

- **LR-BE-0201 — выполнено.** User и tenant-independent PostgreSQL repository
  используют UUIDv7-compatible IDs, canonical email и UTC timestamps.
- **LR-BE-0202 — выполнено.** Пароли хэшируются Argon2id в PHC format;
  plaintext не сохраняется и не попадает в публичные ответы.
- **LR-BE-0203 — выполнено.** Реализованы server-side opaque sessions с 256-bit
  token, SHA-256 digest в PostgreSQL, expiry, revocation и atomic rotation.
- **LR-BE-0204 — выполнено.** Organization является tenant и создаётся
  транзакционно вместе с OWNER Membership.
- **LR-BE-0205 — выполнено.** Membership поддерживает OWNER/MANAGER и active,
  invited, disabled statuses.
- **LR-BE-0206 — выполнено.** Центральный permission resolver переводит роль
  Membership в named permissions; MANAGER не получает settings permissions.
- **LR-BE-0207 — выполнено.** Location хранится tenant-scoped с IANA timezone,
  active state и response threshold 1-1440 минут (default 45).
- **LR-BE-0208 — выполнено.** Полное недельное расписание из семи дней
  валидируется и заменяется атомарно вместе с timezone Location.
- **LR-BE-0209 — выполнено.** Подключены register/login/logout/refresh/me,
  HttpOnly SameSite=Strict cookie и browser Origin validation.
- **LR-BE-0210 — выполнено.** Подключены create/get/update Organization API;
  update защищён `organization.manage`.
- **LR-BE-0211 — выполнено.** Подключены list/create/update Location и replace
  business-hours API с явным `X-Tenant-ID`.
- **LR-BE-0212 — выполнено.** Есть isolated PostgreSQL harness для Tenant A/B и
  integration exit-gate test: onboarding, relogin persistence, MANAGER denial и
  cross-tenant 404/403 без раскрытия данных.
- **LR-BE-0301 — выполнено.** Добавлен `ServiceCatalogItem` domain с display и
  normalized name, optional Location, active state и nullable price range.
- **LR-BE-0302 — выполнено.** Миграция `service_catalog_items` использует
  tenant/location composite FK, `NUMERIC(14,2)`, DB checks и tenant indexes.
- **LR-BE-0303 — выполнено.** Точная decimal-модель запрещает JSON number,
  отрицательные/слишком точные суммы и диапазон `price_from > price_to`;
  REST всегда возвращает две дробные цифры.
- **LR-BE-0304 — выполнено.** Подключены OWNER-only list/create/update и
  idempotent soft-deactivate API `/api/v1/services` с явным `X-Tenant-ID`.
- **LR-BE-0305 — выполнено.** Unit и isolated PostgreSQL tests покрывают
  nullable цены без догадок, OWNER/MANAGER, tenant A/B, foreign Location,
  cross-tenant 404 и отсутствие чужих данных в list.
- **LR-BE-0401 — выполнено.** Добавлен tenant-scoped `ChannelConnection` с
  provider, capabilities, optional Location, безопасным verifier hash и
  PostgreSQL constraints/composite tenant FK.
- **LR-BE-0402 — выполнено.** Health хранит `ACTIVE`, `DEGRADED`, `ERROR` или
  `DISCONNECTED`, timestamps последнего события/успеха/ошибки и безопасный error
  code; valid event восстанавливает здоровье development-коннектора.
- **LR-BE-0403 — выполнено.** Domain-контракт Connector определяет Provider,
  VerifyEvent, NormalizeEvent и Health; отдельная capability извлекает внешний
  event ID до постановки normalize work.
- **LR-BE-0404 — выполнено.** `raw_events` хранит JSONB payload и SHA-256,
  tenant/connection/provider scope, processing status и уникальность
  `(connection_id, external_event_id)`.
- **LR-BE-0405 — выполнено.** Короткая PostgreSQL-транзакция атомарно сохраняет
  новый RawEvent, ровно один `raw_event_normalization_work` и health; webhook
  отвечает до Normalize/AI. Общая leased job queue остаётся задачей этапа 6.
- **LR-BE-0406 — LR-BE-0408 — выполнены.** TEST, IMPORT и GENERIC_WEBHOOK имеют
  детерминированные local adapters с secret verification, строгим fixture
  envelope, external ID и normalization fixture.
- **LR-BE-0409 — частично.** Connected Business Bot stub проверяет официальный
  secret-token header и fixture updates `business_connection`,
  `business_message`, edit/delete без сетевых вызовов. Health намеренно
  `DEGRADED/TELEGRAM_SPIKE_NOT_VERIFIED`: обязательный real-account spike выше
  по-прежнему не выполнен.
- **LR-BE-0410 — выполнено.** OWNER-only connect/list/idempotent disconnect/health
  API подключён к runtime; cross-tenant ID возвращается как missing. Webhook
  endpoint аутентифицируется provider secret без user session.
- **LR-BE-0411 — выполнено.** Unit, repository и сквозной HTTP/PostgreSQL tests
  доказывают, что 10 одинаковых доставок дают один RawEvent и одну normalize
  work запись; тот же external ID с другими bytes даёт conflict.
- **LR-BE-0412 — выполнено.** Неверный secret ничего не сохраняет; корректно
  аутентифицированный malformed payload сохраняется один раз как `FAILED` без
  normalize work, включая lossless base64 wrapper для non-JSON bytes.
- **Exit Gate этапа 4 — частично.** Persist-once, быстрый `202` и независимость
  от downstream AI доказаны для local/fixture adapters. Настоящий Telegram
  update нельзя подтвердить до выполнения feasibility spike с credentials.
- **LR-BE-0501 — выполнено.** Определены строгие канонические события
  `message.received.v1`, `message.edited.v1` и `message.deleted.v1` с направлением,
  видом сообщения, ссылкой на исходное событие, контактом, ответом, вложениями и
  произвольными метаданными в объекте JSON.
- **LR-BE-0502 — выполнено.** Контакт создаётся при первой встрече внешней
  личности и затем разрешается через неё; имя, телефон и почта обновляются только
  данными не старше уже сохранённых.
- **LR-BE-0503 — выполнено.** Уникальность внешней личности учитывает
  организацию, поставщика, подключение и внешний идентификатор; два подключения
  с одинаковым идентификатором контакта не склеиваются.
- **LR-BE-0504 — выполнено.** Переписка создаётся один раз на пару
  `(connection_id, external_id)` и повторно используется следующими сообщениями.
- **LR-BE-0505 — выполнено.** Каноническое сообщение сохраняется в PostgreSQL;
  повтор с тем же содержимым безопасен, а противоречащее содержимое даёт конфликт.
- **LR-BE-0506 — выполнено для локальных адаптеров и макета Telegram.** TEST,
  IMPORT и GENERIC_WEBHOOK принимают явное направление. Макет Connected Business
  Bot различает входящие сообщения и ручные исходящие по `sender_business_bot`
  и соотношению отправителя с личным чатом. Настоящий Telegram всё ещё требует
  проверки отдельного этапа применимости.
- **LR-BE-0507 — выполнено.** Изменение обновляет каноническое состояние
  существующего сообщения, включая ответ и вложения; точный повтор ничего не
  меняет.
- **LR-BE-0508 — выполнено.** Удаление сохраняет историю и выставляет
  `provider_deleted_at`; повтор удаления безопасен.
- **LR-BE-0509 — частично.** Метаданные вложений, ключ объекта, MIME-тип, размер,
  SHA-256 и внешний идентификатор сохраняются отдельно от сообщения. Двоичные
  данные не попадают в PostgreSQL, но фактическая загрузка в S3-совместимое
  объектное хранилище пока заменена явными ключами-заглушками.
- **LR-BE-0510 — выполнено.** `conversations.revision` атомарно увеличивается в
  той же транзакции, что фактическое создание, изменение или удаление сообщения;
  безопасный повтор счётчик не увеличивает.
- **LR-BE-0511 — выполнено.** Подключены чтение списка переписок, одной
  переписки с контактом и сообщений с вложениями; OWNER и MANAGER используют
  `conversation.read`, страницы имеют непрозрачный курсор, чужие данные не
  раскрываются.
- **LR-BE-0512 — выполнено.** Для TEST, IMPORT, GENERIC_WEBHOOK и макета Telegram
  добавлено по пять файлов-примеров: новое, изменённое, удалённое сообщение,
  вложение и повтор события.
- **Exit Gate этапа 5 — выполнено для канонического контура.** Сквозной тест
  `webhook → RawEvent → обработчик → Contact + ExternalIdentity + Conversation +
  Message → HTTP-чтение` проходит на PostgreSQL 18. Второе сообщение создаёт
  только Message; модуль переписок не содержит логики конкретного поставщика.
  Производственная готовность двоичных вложений и настоящего Telegram остаётся
  ограниченной заглушками, как указано выше.
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
- **LR-BE-0910 — выполнено на уровне обработчика.** Тесты проверяют `risks.read`
  и `risks.manage`; централизованная проверка разрешений уже существует, но сам
  модуль Radar пока не подключён к производственному PostgreSQL-контуру.

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

Этапы 2–5 теперь образуют связный PostgreSQL-контур от регистрации организации
до чтения канонической переписки. Проверки ограничений базы, повторных событий,
ролей и изоляции организаций проходят на PostgreSQL 18. Не закрыты настоящий
Telegram, перенос двоичных вложений в объектное хранилище и RLS, который по ТЗ
относится к этапу 24.

Главный общий долг этапов 6–15 — существующие реализации в памяти ещё не заменяют
очередь с арендой, PostgreSQL-хранилища и полностью связанный производственный
путь. Этап 15 даёт воспроизводимый контур измерений, но его выходной критерий не
пройден без размеченного набора, выбранной модели и измерения на целевом
оборудовании.

## Проверки текущего аудита

Проверки выполнены 2026-08-25 на PostgreSQL 18:

- весь набор `go test -race -count=1 ./...`, включая изолированные схемы двух
  организаций и сквозной путь этапа 5;
- `go vet ./...` и `staticcheck ./...`;
- проверка направлений зависимостей `archcheck`;
- двукратное применение всех миграций, подтверждающее повторяемость запуска;
- проверка OpenAPI через Redocly;
- сборка всех команд `backend/cmd/...`;
- запуск API с успешными `/health/ready` и `/health/live`;
- запуск и штатная остановка фонового обработчика;
- сборка контейнерного образа API.

Ни одна из этих локальных проверок не подменяет обязательные испытания на
настоящем Telegram, загрузку двоичных файлов в объектное хранилище и измерения
локальной модели на целевом графическом ускорителе.
