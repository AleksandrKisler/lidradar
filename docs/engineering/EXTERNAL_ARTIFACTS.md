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
- [lidradar-main-v1](../../models/manifests/lidradar-main-v1.json) содержит
  prompt v6, веса/hash, параметры, dataset hashes 400 GOLDEN + 100 DEV,
  RTX 4060, quality/performance gates и статус `FROZEN` от QA-06.
- Это фиксация выбранной конфигурации и результатов QA-06; полный runtime/driver/
  validator provenance для общего выпуска ещё не подтверждён.
- **Владелец:** AI; [сверка provenance](../../models/reports/lidradar-main-v1-prompt-v6-provenance-review.json).

<a id="model-report"></a>
## Отчёты контрольного прогона

- **Статус:** `AVAILABLE`; реальные отчёты v6 от 2026-10-05, не новый benchmark 6 октября.
- [GOLDEN 400](../../models/reports/lidradar-main-v1-prompt-v6-golden.json),
  [DEV 100](../../models/reports/lidradar-main-v1-prompt-v6-dev.json),
  [GPU](../../models/reports/lidradar-main-v1-prompt-v6-hardware.json),
  [выбор до открытия GOLDEN](../../models/reports/lidradar-main-v1-prompt-v6-selection.json).
- Текущие provider/generation schema/price validator совпадают с хэшами selection.
  Исторические отчёты v5 не используются как доказательство v6.
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

Заполнить production manifest образов, окружения, proxy/TLS, ключевых версий
без секретов и внешних проверок по [release gates](RELEASE_GATES.md) ещё предстоит.
