# Артефакты реализации и оставшиеся доказательства

Редакция 2026-10-06 после сверки рабочих backend/frontend репозиториев.
Исходный статус `MISSING_FROM_DOCS_SNAPSHOT` от 2026-10-05 описывал комплект
документов, а не отсутствие реализации. Текущие объекты ниже найдены;
наличие файла не означает production-допуск.

<a id="openapi"></a>
## OpenAPI

- **Статус:** `AVAILABLE`; [машинный контракт](../../contracts/openapi/openapi.yaml).
- Содержит `RISK_CLOSED` и replay закрытого Risk. Snapshot frontend обновлён,
  типы сгенерированы и сверены командой `npm run api:check`.
- SHA-256 и версии рабочих деревьев: [сверка артефактов](evidence/2026-10-06/artifacts.json).
- **Владелец:** backend API. Production image/build ещё не связаны release manifest.

<a id="model-manifest"></a>
## Манифест модели

- **Статус:** `AVAILABLE`, полнота release tuple — `REQUIRED_NOT_VERIFIED`.
- [lidradar-main-v1](../../models/manifests/lidradar-main-v1.json) с 2026-10-10 относится к
  prompt v9 и контракту v2 (`FROZEN`): веса и hash (пересчитан на узле), параметры генерации и
  контекст 8192, сборка llama.cpp, образ и драйвер, сумма выбора (29 файлов tuple), dataset hashes
  400 GOLDEN + 100 DEV и наборов v2, результаты DEV, GOLDEN и оборудования, пороги. Числа собраны
  `scripts/ai-manifest.py` из отчётов ([ADR 0053](../adr/0053-ai-tuple-qualification.md),
  [runbook](../runbooks/ai-qualification.md)).
- Манифест v6 (QA-06, 2026-10-05) сохранён как
  [отчёт](../../models/reports/lidradar-main-v1-prompt-v6-manifest.json) и остаётся историей v6.
- Полный release tuple для общего выпуска остаётся неподтверждённым: нет независимой выборки для
  смыслов контракта v2, сверки tuple при запуске узла и независимого production-измерения.
- **Владелец:** AI; [сверка provenance](../../models/reports/lidradar-main-v1-prompt-v6-provenance-review.json).

<a id="model-report"></a>
## Отчёты контрольного прогона

- **Статус:** `AVAILABLE`; отчёты v6 от 2026-10-05 (история) и отчёты v9 от 2026-10-10.
- v6: [GOLDEN 400](../../models/reports/lidradar-main-v1-prompt-v6-golden.json),
  [DEV 100](../../models/reports/lidradar-main-v1-prompt-v6-dev.json),
  [GPU](../../models/reports/lidradar-main-v1-prompt-v6-hardware.json),
  [выбор до открытия GOLDEN](../../models/reports/lidradar-main-v1-prompt-v6-selection.json).
  Отчёты v5 не используются как доказательство v6, отчёты v6 — как доказательство v9.
- v9 (узел с контекстом 8192): [выбор до открытия GOLDEN](../../models/reports/lidradar-main-v1-prompt-v9-selection.json),
  [GOLDEN 400](../../models/reports/lidradar-main-v1-prompt-v9-golden.json), DEV:
  [100](../../models/reports/lidradar-main-v1-prompt-v9-dev.json),
  [намерения](../../models/reports/lidradar-main-v1-prompt-v9-dev-intents.json),
  [договорённости](../../models/reports/lidradar-main-v1-prompt-v9-dev-agreements.json),
  [независимые обещания](../../models/reports/lidradar-main-v1-prompt-v9-dev-independent.json),
  [зонд контекста](../../models/reports/lidradar-main-v1-prompt-v9-context-probe.json) (и
  [на контексте 4096](../../models/reports/lidradar-main-v1-prompt-v9-context-probe-4096.json)),
  [оборудование](../../models/reports/lidradar-main-v1-prompt-v9-hardware.json) со
  [снимками видеопамяти](../../models/reports/lidradar-main-v1-prompt-v9-vram-samples.csv),
  [хэш весов](../../models/reports/lidradar-main-v1-prompt-v9-model-hash.json).
- Файлы tuple v9 совпадают с выбором v9 (`make ai-selection-verify`); с хэшами выбора v6 они больше
  не совпадают: поставщик и валидатор изменены.
- **Владелец:** AI/QA. Независимое production-измерение и полный release tuple остаются открытыми.

<a id="repo-rules"></a>
## Правила репозитория

- **Статус:** `AVAILABLE`; [AGENTS.md](../../AGENTS.md),
  [CODEX_RULES](CODEX_RULES.md), [Definition of Done](DEFINITION_OF_DONE.md).
- Принятые ADR ограничивают архитектурные изменения. По решению владельца
  2026-10-06 H-04 остаётся открытым, [ADR 0046](../adr/0046-atomic-critical-command-audit.md)
  остаётся Proposed.

<a id="delivery-history"></a>
## История работ

Доступны датированные [отчёты roadmap](../roadmap/) и
[исходный QA](../front-end/09-qa-adversarial-2026-09-25.md).
Актуальная сверка: [HIGH_REVIEW](HIGH_REVIEW_2026_10_06.md).
Общего неизменяемого production release manifest пока нет; старые отчёты
не сертифицируют весь текущий продукт.

<a id="runtime"></a>
## Код и runtime

Backend, миграции и CI присутствуют в этом репозитории. Frontend находится
в отдельном локальном репозитории `lidradar-web`; его commit и source hash
указаны в artifacts.json. Dev API использует PostgreSQL 18, схема по
`000022_membership_invitations`; интерфейс доступен на localhost:5173.
Все проверки этого цикла относятся к незакоммиченным рабочим деревьям.

С 2026-10-10 образы выпуска воспроизводимо собирает `scripts/build-images.sh`
(тег — git sha), а [production-стек](../runbooks/production-deployment.md) описан в
`deploy/production` ([ADR 0050](../adr/0050-production-topology.md)) вместе со службой копий
по расписанию и вне хоста ([ADR 0051](../adr/0051-offhost-backups.md)) и с отдельными
логинами PostgreSQL для владельца и рабочих процессов
([ADR 0052](../adr/0052-database-roles-owner-and-runtime.md)); на настоящем хосте, у настоящего
поставщика хранилища и в управляемой базе он не разворачивался. Заполнить production manifest
образов, окружения, proxy/TLS, ключевых версий без секретов и внешних проверок по
[release gates](RELEASE_GATES.md) ещё предстоит.
