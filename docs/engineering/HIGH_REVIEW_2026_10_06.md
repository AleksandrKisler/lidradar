# Сверка H-01…H-20 с кодом, 6 октября 2026

Замечания релевантны: они обнаружили реальные дефекты клиента и проверок,
а также верно отделили требования от production-доказательств. Часть выводов
об отсутствии реализации относится только к переданному документационному
snapshot: в рабочих репозиториях код, OpenAPI и реальные отчёты AI v6 есть.

Работа выполнена в незакоммиченных backend/frontend деревьях, без публикации
и production-развёртывания. [Commit/source/API hashes](evidence/2026-10-06/artifacts.json)
фиксируют проверенное состояние; commit сам по себе не описывает эти правки.
Нормы и исторические отчёты пользователя сохранены, добавлена датированная сверка.

## Что изменено

- Action/Outcome/Revenue сохраняют исходные key/body до отправки и восстанавливают
  unknown после reload. Изменение формы, cancel/logout и конфликт не создают новый
  ключ для незавершённого намерения. Недоступный/повреждённый журнал блокирует POST.
  Ошибка обновления кеша после успеха не переводит уже подтверждённую команду
  в unknown. Убран обход `RECOVERED_ALREADY_ATTRIBUTED` через тот же платёж ORGANIC.
- Новый Action терминального риска отклоняется атомарно; успешный replay остаётся
  доступным. Frontend обновляет устаревшую карточку после `RISK_CLOSED`, а закрытая
  карточка с pending-командой позволяет восстановить именно её.
- В форму точки и критические формы добавлен synchronous submit guard до async
  validation; форма услуги уже имела его после QA-02. Поля блокируются на отправке.
- Radar автоматически сверяет активные REST-запросы выбранной организации каждые
  15 секунд. Hidden/offline вкладка не опрашивает сервер; возврат запускает сверку.
  Перекрывающиеся safety refetch не отменяют друг друга.
- Обязательные DB-тесты больше не зеленеют из-за недоступного PostgreSQL/skip.
  Добавлен testgate с JSONL, summary, source/migration hashes и проверкой покрытия
  обязательных сценариев; CI сохраняет положительное и отрицательное доказательства.
- Backup публикует только завершённый приватный файл и лишь затем ротирует копии.
  Restore проверяет ввод/TOC/URL до CREATE и никогда не удаляет существующую базу.
- Восстановлен односторонний сборщик документов: 14 представлений из канона,
  проверка ссылок/anchors/LFS, manifest, детерминированные архивы, режим `--check`.
  Старые графики ёмкости больше не генерируются как новые доказательства.

## Матрица релевантности

| High | Вывод по реализации | Результат и граница |
|---|---|---|
| H-01 | Реальный дефект: draft жил в памяти, reset/изменение тела могли дать новый ключ, был shortcut ORGANIC | Исправлено в клиенте. Unit/component проверяют commit с потерей ответа, неизменяемость тела, reload/logout/actor scope, replay, конфликт и storage failure. Серверная идемпотентность покрыта DB suite. Полный browser E2E с fault proxy не выполнялся |
| H-02 | Реальный QA-07; правило в документах корректно | Исправлено и проверено: четыре терминальных статуса × шесть типов, точный replay и конфликт; DB гонки close-first (commit/rollback) и action-first; ручной API/UI PASS |
| H-03 | Услуга уже исправлена QA-02; форма точки оставалась уязвимой | Guard добавлен. Create/update с двумя submit до async validation и при удерживаемом ответе создают одну запись. Отдельный browser-прогон с throttling ровно 1,5 с в этом цикле не проводился |
| H-04 | Post-commit окно действительно есть; повтор no-op не обязан восстановить след | **OPEN по решению владельца 2026-10-06.** [ADR 0046](../adr/0046-atomic-critical-command-audit.md) Proposed. Архитектура аудита не изменена, пооперационный fault injection остаётся |
| H-05 | Суточный dump не доказывает RPO ≤ 15 минут; пригодная внешняя точка не подтверждена | Исправлена безопасность локального backup. Независимая доставка, мониторинг возраста и измеренный RPO остаются открытыми |
| H-06 | Старый helper удалял цель до проверки файла; global roles/ключи не входят в dump | Небезопасный DROP устранён, preflight и URL-guards проверены. Полное восстановление на новом кластере, grants/owners/decrypt/RLS и RTO ещё не выполнены |
| H-07 | Строгая схема действительно блокирует старую сборку на новом journal | Forward migration/checksum/readiness тесты проходят; матрица старый binary × новая схема и полное восстановление не квалифицированы. Обход readiness не добавлялся |
| H-08 | Запрет аварийного отключения RLS корректен | RLS/tenant attacks/pool reuse проходят обязательный DB gate; отдельный restore под реальными production-ролями остаётся в RG-DR |
| H-09 | Код использует `LIDRADAR_INTEGRATION_ENCRYPTION_KEY`; замечание об альтернативном имени полезно | Config/crypto/write-only тесты проходят. Restore с прежним ключом на новом хосте не проверен. Legacy alias Telegram bot token относится к другому параметру |
| H-10 | Limiter берёт RemoteAddr; за обычным proxy клиенты могут делить адрес | Deployment-gap подтверждён. Код не доверяет произвольному forwarded header; новую доверительную цепочку/env без топологии не вводили. Нужен реальный edge и anti-spoof/shared-IP/multi-instance прогон |
| H-11 | Исторический dataset_eligible не разрешает новый экспорт после revoke | Корректное требование. Действующего экспортёра ML-партий в проверенных cmd/AI/risk/tenant не найдено; benchmark синтетический. Grant/revoke/feedback тесты есть; export/revoke race относится к будущей реализации |
| H-12 | Сроки retention не утверждены | **BLOCKED_DECISION.** Purge и новые сроки не придуманы. Нужна заполненная матрица классов/исключений/backup/offboarding до реализации |
| H-13 | Нельзя переносить v5 freeze на v6; утверждение «v6 отчётов нет» к рабочему repo неприменимо | Реальные v6 DEV/GOLDEN/GPU отчёты найдены, selection hashes совпали, аудит 400/100 прошёл. [Provenance review](../../models/reports/lidradar-main-v1-prompt-v6-provenance-review.json) фиксирует отсутствующие исторические runtime/driver/build bindings; общий release tuple не объявлен полным |
| H-14 | Правка документа соответствует уже принятому ADR 0036 и коду | Реализован queued-only upsert цельного snapshot; leased/running не меняется. DB tests burst/out-of-order/debounce/5→7/STALE/finalization/lease cap проходят; новой миграции не требуется |
| H-15 | Старый DDL противоречил принятому ADR 0043 | Все пять правил используют business-time; порог NO_RESPONSE — Location, остальные — versioned constants. Domain/DB tests границ, выходных и DST проходят; таблица risk_policy_configs отсутствует намеренно |
| H-16 | Реальный пробел: живой SSE не гарантировал актуальность при silent NOTIFY loss | SLA 30 с принят, safety refetch реализован. Ручной dev тест без NOTIFY: изменение видно ≤ 6,322 с, SSE online, без кликов. Unit проверяет новые snapshots/ошибку REST/восстановление/tenant scope/hidden/offline/logout. Multi-API/LISTEN, backpressure и SLA под нагрузкой остаются |
| H-17 | Реальный дефект тестового helper и недостаточный gate | Исправлено. 568 passed events, 0 failed, 1 ожидаемый subprocess-helper skip; missing/unreachable DB отдельно дают failure. CI workflow обновлён, удалённый CI здесь не запускался |
| H-18 | Вывод о ёмкости через 1/p95 и «100× запас» необоснован | Документальная корректировка верна. Старые числа сохранены как история. Текущие mixed-workload/edge/real AI/soak/burst и утверждённый workload отсутствуют; новая ёмкость не заявлена |
| H-19 | Реестр и release manifest нужны; отсутствие всех артефактов было свойством docs snapshot | [Реестр](EXTERNAL_ARTIFACTS.md) исправлен, найдены реальные объекты и hashes. Production image/config manifest и живая Telegram-матрица не закрыты |
| H-20 | Перенесённые тесты и старый builder расходились; README разрешал обратное зеркалирование | До исправления 10/10 tooling tests падали, после — 10/10 PASS. Производные страницы/архивы собираются из канона и проверяются без изменений. Реальный Notion import/page IDs/права/export round-trip не выполнялись |

## Проверки и доказательства

- [Frontend](evidence/2026-10-06/frontend-final.log): 60 файлов, **363 теста PASS**.
  Dev build/typecheck, ESLint, OpenAPI generation check, FSD и 13 architecture cases PASS.
  Первый FSD-прогон упёрся в лимит file watchers; штатный режим Chokidar polling прошёл.
- [Обязательный PostgreSQL gate](evidence/2026-10-06/db-summary.json): **568 pass events**,
  включая subtests, **0 failed**, один документированный helper skip. Сам crash-test
  выполняется в дочернем процессе. Summary содержит commit/source/migration hashes.
- [Недоступная БД](evidence/2026-10-06/db-unavailable-summary.json): ожидаемый FAIL.
  Missing DSN в обязательном helper также проверен отдельно и не превращается в skip.
- Canonical `go test ./...`, `go vet ./...`, backend archcheck — PASS.
  Закреплённый в CI `staticcheck@v0.6.1` — PASS после повторной проверки разрешения;
  первая попытка не выполнялась из-за лимита сервиса автоматического approval.
- Полный DB-прогон сначала обнаружил race в ожиданиях старого SSE-теста:
  второй законный resync мог заменить контрольный сигнал. Проверка исправлена
  по контракту потока, [50 повторов с race detector](evidence/2026-10-06/sse-repeat.log)
  и повторный полный gate прошли. Runtime SSE этим изменением не менялся.
- [Ручной silent-NOTIFY тест](evidence/2026-10-06/silent-notify-result.json): один dev API,
  видимый Radar, изменён только reason_text синтетического QA-риска, затем восстановлен.
  Это верхняя граница задержки наблюдения, а не нагрузочная оценка p95.
- [QA-07 API/UI](evidence/2026-10-06/qa07-live-result.json): после закрытия 6×409,
  replay 200, изменённый replay 409, число Actions/keys/audits неизменно.
- [Recovery helper tests](evidence/2026-10-06/recovery-helper-tests.log): **8 PASS**
  с поддельными pg tools, без удаления реальных БД. Не заменяют restore drill.
- [Docs tooling](evidence/2026-10-06/docs-tooling-tests.log): **10 PASS**.
  `build.py` и `build.py --check` — локальная приёмка; фактический inventory,
  hashes и ZIP находятся в `runtime/docs-package/`. Секреты и runtime credentials
  в пакет не включаются.

## Ограничения клиентской защиты

Локальный журнал ограничен браузером и actor/tenant/resource. Очистка site data,
новое устройство и два независимо подтверждённых намерения в разных вкладках
не покрываются гарантией одного pending draft. Он может содержать пользовательскую
заметку и сумму, не содержит токенов; скрытие после logout не шифрует локальные данные.
Конфликт и потерянный журнал требуют сверки истории. Это не глобальный запрет
нескольких действительных оплат одной сделки.

Safety refetch имеет максимум четыре тика в минуту на видимую вкладку, затрагивает
только активные tenant queries; одна infinite-query цепочка может прочитать несколько
страниц. Это ограничение запуска, а не согласованный production QPS budget. Поведение
при timeout/retry и нагрузке должно входить в оставшийся RG-SSE/RG-CAPACITY.

## Готовность и следующий этап

Семь исходных дефектов QA-01…QA-07 устранены в своей проверенной области.
Рабочий dev/demo контур стал устойчивее; это не закрытие всех release gates.
H-04 оставлен открытым по указанию владельца. Для полного допуска остаются:

1. Утвердить retention и workload, отдельно принять решение по H-04.
2. Подготовить recovery set и провести новый кластер/ключи/RLS/money/queues drill,
   измерить внешнюю точку RPO и полное RTO; проверить матрицу обновления/отката.
3. Пройти production proxy/TLS/rate limits и полную Telegram/notification матрицу
   в согласованном тестовом контуре.
4. Закрепить полный AI release tuple; измерить mixed workload, multi-API/LISTEN,
   SLA восстановления и нагрузочный бюджет.
5. Дополнить browser E2E lost response/reload/двойная отправка с задержкой;
   отдельно принять реальный перенос документации в Notion.
