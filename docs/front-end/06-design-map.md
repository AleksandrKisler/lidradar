# Карта экранов и макетов

Локальный пакет `LidRadar_Figma_Import_v0.2` сохранён целиком в
[`mockups/`](mockups/), чтобы задачи и PR не зависели от внешней ссылки. SVG —
редактируемый визуальный источник, PNG — эталон raster-сверки. Это фиксированная
геометрия, не нативные Figma components/auto-layout и не доказательство наличия
API.

Быстрый просмотр: [галерея](mockups/index.html),
[общий PNG](mockups/preview/overview.png),
[инструкция импорта](mockups/README_RU.md),
[сценарии](mockups/SCENARIOS_RU.md).

<a id="screen-inventory"></a>
## 1. Инвентаризация листов

| № | Лист | Route / применение | API-готовность | Артефакты |
|---:|---|---|---|---|
| 01 | Вход | `/login` | готов | [SVG](mockups/svg/01-vhod.svg) · [PNG](mockups/preview/01-vhod.png) |
| 02 | Настройка компании | `/onboarding/company` | частично: нет resume status | [SVG](mockups/svg/02-nastroika-kompanii.svg) · [PNG](mockups/preview/02-nastroika-kompanii.png) |
| 03 | Рабочее время | `/onboarding/business-hours`, settings | готов | [SVG](mockups/svg/03-rabochee-vremia.svg) · [PNG](mockups/preview/03-rabochee-vremia.png) |
| 04 | Услуги и цены | `/onboarding/services` | готов для строк; dialog не задан | [SVG](mockups/svg/04-uslugi-i-ceny.svg) · [PNG](mockups/preview/04-uslugi-i-ceny.png) |
| 05 | Подключение Telegram | `/onboarding/telegram` | два разных flow; source connect требует решения | [SVG](mockups/svg/05-podkliuchenie-telegram.svg) · [PNG](mockups/preview/05-podkliuchenie-telegram.png) |
| 06 | Диалоги | `/conversations`, detail | list/search/deeplink API неполны | [SVG](mockups/svg/06-dialogi.svg) · [PNG](mockups/preview/06-dialogi.png) |
| 07 | Аналитика | `/analytics` | агрегаты готовы; series/payments/split нет | [SVG](mockups/svg/07-analitika.svg) · [PNG](mockups/preview/07-analitika.png) |
| 08 | Интеграции | `/integrations` | persisted health готов; safe connect/live probe не решены | [SVG](mockups/svg/08-integracii.svg) · [PNG](mockups/preview/08-integracii.png) |
| 09 | Компания и график | `/settings/company` | готов | [SVG](mockups/svg/09-nastroiki-kompanii.svg) · [PNG](mockups/preview/09-nastroiki-kompanii.png) |
| 10 | Настройки услуг | `/settings/services` | API готов; dialogs не заданы | [SVG](mockups/svg/10-nastroiki-uslug.svg) · [PNG](mockups/preview/10-nastroiki-uslug.png) |
| 11 | Уведомления | `/settings/notifications` | готов, но доступен обеим tenant roles | [SVG](mockups/svg/11-nastroiki-uvedomlenii.svg) · [PNG](mockups/preview/11-nastroiki-uvedomlenii.png) |
| 12 | Команда | `/settings/team` | HTTP API отсутствует | [SVG](mockups/svg/12-komanda.svg) · [PNG](mockups/preview/12-komanda.png) |
| 13 | Подтверждение оплаты | Risk Workspace dialog | готов при наличии evidence ids | [SVG](mockups/svg/13-podtverzhdenie-oplaty.svg) · [PNG](mockups/preview/13-podtverzhdenie-oplaty.png) |
| 14 | Radar без рисков | `/radar` zero-state | zero state готов; основной feed API неполон | [SVG](mockups/svg/14-radar-bez-riskov.svg) · [PNG](mockups/preview/14-radar-bez-riskov.png) |
| 15 | Состояния | глобально | контракты готовы | [SVG](mockups/svg/15-sostoianiia.svg) · [PNG](mockups/preview/15-sostoianiia.png) |
| 16 | Основы | tokens/components | применимо | [SVG](mockups/svg/16-osnovy-interfeisa.svg) · [PNG](mockups/preview/16-osnovy-interfeisa.png) |

Статус «API готов» означает, что нужные операции существуют, а не что экран
реализован. Номера листов задают порядок просмотра, не route или sprint.

<a id="design-tokens"></a>
## 2. Токены

Канонический машинный файл: [design-tokens.json](mockups/design-tokens.json).
Первичная CSS mapping:

```css
:root {
  --lr-bg: #f5f7fb;
  --lr-paper: #ffffff;
  --lr-ink: #191e2b;
  --lr-muted: #63718a;
  --lr-line: #e1e7f0;
  --lr-nav: #0e1422;
  --lr-nav-active: #1c2435;
  --lr-nav-text: #bac2d2;
  --lr-brand: #6c55ff;
  --lr-brand-dark: #5640d8;
  --lr-brand-pale: #f0edff;
  --lr-success: #087d50;
  --lr-success-pale: #ecfbf3;
  --lr-danger: #d22a25;
  --lr-danger-pale: #fff3f2;
  --lr-warning: #ad5b11;
  --lr-warning-pale: #fff6e8;
  --lr-info: #1768c6;
  --lr-info-pale: #edf6ff;
  --lr-space: 8px;
}
```

Baseline: Inter Variable; desktop canvas 1440; sidebar 232; content padding 48;
spacing grid 8; radii 10/12/18; button height 44; input height 46. Значения
переносятся в существующую token/theme систему проекта, а не дублируются по
components. Font assets и OFL находятся в [`mockups/fonts`](mockups/fonts).

Цвет не является единственным носителем status: badge содержит текст/icon.
Все фактические foreground/background пары, включая disabled/focus/error,
проверяются на WCAG-контраст после переноса; наличие цвета в макете не
гарантирует допустимый contrast в любой комбинации.

<a id="component-map"></a>
## 3. Компонентная карта

| Уровень | Компоненты | Варианты, которые обязательны |
|---|---|---|
| Foundations | Typography, Icon, Divider, Surface, Stack/Grid | text scale, weights, semantic colors, focus ring |
| Inputs | TextInput, PasswordInput, MoneyInput, Select, DateRange, TimeInput, Checkbox, Switch, Textarea | idle/focus/invalid/disabled/read-only/loading |
| Actions | Button, IconButton, Link, MenuItem | primary/secondary/danger/ghost; pending; keyboard |
| Feedback | Alert, InlineError, Toast/LiveRegion, Skeleton, EmptyState, StaleBadge | info/success/warning/error; retry and traceId |
| Data display | Badge, StatusDot, Money, RelativeTime, DefinitionList, Table, Pagination sentinel | null/unknown, overflow, copy ID |
| Domain | SeverityBadge, RiskTypeLabel, RiskCard, OpportunityStage, MessageBubble, ConnectionCard, PreferenceRow | every enum and unknown fallback |
| Overlay | Dialog, ConfirmDialog, Drawer | focus trap/restore, Escape, destructive label, pending lock |
| Layout | AppShell, Sidebar, PageHeader, FilterBar, SplitPane | owner/manager nav, narrow layouts, scroll ownership |

Компонент создаётся после инвентаризации состояний, а не только по default
variant листа 16. Domain components принимают view model, а не transport DTO.

<a id="route-layout"></a>
## 4. Композиция маршрутов

| Route family | Desktop composition | Narrow fallback |
|---|---|---|
| Auth | центрированная card без app sidebar | card на всю доступную ширину |
| Onboarding | progress + single form/card | progress scroll/compact, одна колонка |
| Radar | sidebar + header/filters + metrics + feed | drawer nav; metrics wrap; cards one column |
| Risk Workspace | header + context/action columns | последовательные sections; sticky action только без перекрытия |
| Conversations | list/detail split pane | list и detail как отдельные route states/back navigation |
| Settings | settings nav + form/table | drawer/combobox nav; rows превращаются в cards при необходимости |
| Analytics | date controls + cards + chart/table | horizontal chart/table scroll только внутри region |
| Admin | dense nav + filters + tables/detail | card/detail fallback; без обрезания command controls |

Breakpoints не изобретаются в документации: используются уже существующие
project tokens. Независимо от breakpoint поддерживается viewport 320 CSS px,
200% zoom и reflow без горизонтальной прокрутки всей страницы; data table может
иметь собственный labelled scroll region.

<a id="state-coverage"></a>
## 5. Матрица визуальных состояний

Лист [15](mockups/svg/15-sostoianiia.svg) задаёт только примеры. Каждый
data-block должен иметь:

| State | Визуальное требование |
|---|---|
| initial loading | skeleton с устойчивой геометрией, без фальшивых значений |
| success with data | данные + время snapshot там, где актуальность важна |
| success empty | предметный zero-state и следующий разрешённый шаг |
| filtered empty | сохранённые filters + «Сбросить фильтры» |
| partial failure | успешные sections остаются; failed section содержит retry |
| total error | объяснение, retry, optional technical details/traceId |
| stale/offline | последний успешный snapshot явно помечен временем |
| 401 | защищённый content убран, переход на login |
| 403 | «Раздел недоступен» без tenant/object details |
| 404 | нейтральный resource-not-found |
| 409 | conflict copy + refetch/новое решение пользователя |
| 429 | countdown по `Retry-After` |
| mutation pending | disabled duplicate submit + progress/live announcement |
| mutation unknown | не заявлять успех; safe retry/verification path |

Нулевые показатели допустимы только после успешного API response. Ошибка
источника, отсутствие source и отсутствие рисков — три разные композиции.

<a id="copy-and-demo-data"></a>
## 6. Тексты и демонстрационные данные

- Имена, автомобили, цены, dates, team и connection state в SVG — примеры, не
  fixtures/API contract.
- В исходном Product UI v0.1 пример Дмитрия расходится: Radar упоминает
  полировку Audi A6, Risk Workspace — керамику BMW X5. Новый пакет использует
  полировку Audi A6 и 31 000 ₽; код не должен hardcode ни один пример.
- Термины денег фиксированы: «потенциальная», «подтверждённая»,
  «подтверждённо возвращённая». Нельзя сокращать последнюю до «возвращённой»
  без provenance.
- «Диалоги» read-only; CTA говорит «Открыть/Ответить в Telegram», но не создаёт
  ожидание in-app отправки.
- Error copy не повторяет произвольный server `message`, PII или secret.

<a id="missing-designs"></a>
## 7. Отсутствующие и неполные макеты

До начала соответствующей UI-задачи нужны утверждённые desktop states и узкие
варианты:

1. Основной Radar с заполненной лентой и filters. Пакет ссылается на Figma
   «LidRadar — Product UI v0.1», но локального `.fig`/URL/экспорта нет.
2. Полный Risk Workspace: loading, active statuses, terminal statuses,
   recommendation/action/outcome/stage/revenue и conflicts.
3. Registration и workspace selection.
4. Onboarding resume/skip/completed и initial source secret flow.
5. Add/edit/deactivate/reactivate service dialogs.
6. Conversation: filter/search errors, deleted message, unsupported/missing
   attachment, external deeplink unavailable.
7. Team invite/role/revoke confirmations и last-owner protection.
8. Privacy/ML consent.
9. Все Platform Admin маршруты и recovery confirmations.
10. Mobile/narrow 320–767 для всех основных маршрутов; tablet split behavior.
11. Полный набор validation, conflict, offline, permission-lost и unknown-result
    states для форм.

Отсутствие макета не разрешает разработчику принимать продуктовые решения в
коде. Оно отмечается design prerequisite в [backlog](TASKS.md).

<a id="handoff-checklist"></a>
## 8. Design-to-development checklist

Перед реализацией экрана:

1. Есть ссылка на конкретный SVG/Figma frame и все состояния либо ссылка на
   documented missing-design prerequisite.
2. Все видимые значения сопоставлены с полями [сущностей](02-entities.md);
   для отсутствующего поля создан API gap, а не client fiction.
3. У каждого действия указан endpoint, permission, pending/error/success и
   invalidation.
4. Определены keyboard order, labels, live regions, focus restore и zoom/reflow.
5. Определены desktop, narrow, long text, empty, partial error и stale states.
6. Demo copy/data удалены из production state и существуют только в story/test.
7. SVG сравнивается с одноимённым PNG после импорта; размеры 1440×1024 или
   1440×1180 сохранены.
