# Карта экранов и макетов

Локальный пакет макетов сохранён целиком в [`mockups/`](mockups/), чтобы
задачи и PR не зависели от внешней ссылки: листы 01–16 — комплект v0.2
(3 сентября 2026), листы 17–32 — комплект v0.3 (25 сентября 2026), который
закрыл GAP-DESIGN-014/015/018: основной Radar, полная карточка риска и её
состояния, регистрация и выбор пространства, возобновление онбординга, диалоги
услуг, состояния переписки, подтверждения команды, данные и согласие,
администрирование, узкие экраны, планшет и масштаб 200 %, состояния форм.
SVG — редактируемый визуальный источник, PNG — эталон raster-сверки. Это
фиксированная геометрия, не нативные Figma components/auto-layout и не
доказательство наличия API. Комплект собирается из одного исходника
`mockups/source/build.mjs` и проверяется `mockups/source/verify.mjs`
(команды — в [README_RU.md](mockups/README_RU.md)); при пересборке v0.3 листы
09–12 получили пятую вкладку настроек «Данные», как в реализованном клиенте.

Быстрый просмотр: [галерея](mockups/index.html),
[общий PNG](mockups/preview/overview.png),
[инструкция импорта](mockups/README_RU.md),
[сценарии](mockups/SCENARIOS_RU.md).

<a id="screen-inventory"></a>
## 1. Инвентаризация листов

| № | Лист | Route / применение | API-готовность | Артефакты |
|---:|---|---|---|---|
| 01 | Вход | `/login` | готов | [SVG](mockups/svg/01-vhod.svg) · [PNG](mockups/preview/01-vhod.png) |
| 02 | Настройка компании | `/onboarding/company` | готов (статус по данным, ADR 0045) | [SVG](mockups/svg/02-nastroika-kompanii.svg) · [PNG](mockups/preview/02-nastroika-kompanii.png) |
| 03 | Рабочее время | `/onboarding/business-hours`, settings | готов | [SVG](mockups/svg/03-rabochee-vremia.svg) · [PNG](mockups/preview/03-rabochee-vremia.png) |
| 04 | Услуги и цены | `/onboarding/services` | готов; диалоги — лист 22 | [SVG](mockups/svg/04-uslugi-i-ceny.svg) · [PNG](mockups/preview/04-uslugi-i-ceny.png) |
| 05 | Подключение Telegram | `/onboarding/telegram` | готов (ADR 0045); форма токена — лист 32 | [SVG](mockups/svg/05-podkliuchenie-telegram.svg) · [PNG](mockups/preview/05-podkliuchenie-telegram.png) |
| 06 | Диалоги | `/conversations`, detail | готов (ADR 0044); состояния — лист 23 | [SVG](mockups/svg/06-dialogi.svg) · [PNG](mockups/preview/06-dialogi.png) |
| 07 | Аналитика | `/analytics` | готов (ADR 0044) | [SVG](mockups/svg/07-analitika.svg) · [PNG](mockups/preview/07-analitika.png) |
| 08 | Интеграции | `/integrations` | готов (ADR 0045) | [SVG](mockups/svg/08-integracii.svg) · [PNG](mockups/preview/08-integracii.png) |
| 09 | Компания и график | `/settings/company` | готов | [SVG](mockups/svg/09-nastroiki-kompanii.svg) · [PNG](mockups/preview/09-nastroiki-kompanii.png) |
| 10 | Настройки услуг | `/settings/services` | готов; диалоги — лист 22 | [SVG](mockups/svg/10-nastroiki-uslug.svg) · [PNG](mockups/preview/10-nastroiki-uslug.png) |
| 11 | Уведомления | `/settings/notifications` | готов; доступен OWNER и MANAGER (решение 2026-09-24) | [SVG](mockups/svg/11-nastroiki-uvedomlenii.svg) · [PNG](mockups/preview/11-nastroiki-uvedomlenii.png) |
| 12 | Команда | `/settings/team` | готов (ADR 0045); подтверждения — лист 24 | [SVG](mockups/svg/12-komanda.svg) · [PNG](mockups/preview/12-komanda.png) |
| 13 | Подтверждение оплаты | Risk Workspace dialog | готов при наличии evidence ids | [SVG](mockups/svg/13-podtverzhdenie-oplaty.svg) · [PNG](mockups/preview/13-podtverzhdenie-oplaty.png) |
| 14 | Radar без рисков | `/radar` zero-state | готов; основная лента — лист 17 | [SVG](mockups/svg/14-radar-bez-riskov.svg) · [PNG](mockups/preview/14-radar-bez-riskov.png) |
| 15 | Состояния | глобально | контракты готовы | [SVG](mockups/svg/15-sostoianiia.svg) · [PNG](mockups/preview/15-sostoianiia.png) |
| 16 | Основы | tokens/components | применимо | [SVG](mockups/svg/16-osnovy-interfeisa.svg) · [PNG](mockups/preview/16-osnovy-interfeisa.png) |
| 17 | Radar · лента рисков | `/radar` с фильтрами в адресе, сводкой и курсорной подгрузкой | готов | [SVG](mockups/svg/17-radar-osnovnoi.svg) · [PNG](mockups/preview/17-radar-osnovnoi.png) |
| 18 | Карточка риска · активный риск | `/risks/:riskId`: причина, переписка и сделка, этапы, рекомендация, оценка, действие, деньги | готов | [SVG](mockups/svg/18-kartochka-riska.svg) · [PNG](mockups/preview/18-kartochka-riska.png) |
| 19 | Карточка риска · состояния | загрузка, закрытый риск, конфликт этапа 409, потеря права и неизвестный результат | готов | [SVG](mockups/svg/19-kartochka-riska-sostoianiia.svg) · [PNG](mockups/preview/19-kartochka-riska-sostoianiia.png) |
| 20 | Регистрация и выбор пространства | `/register`, `/workspaces`, принятие приглашения | готов | [SVG](mockups/svg/20-registraciia-i-prostranstva.svg) · [PNG](mockups/preview/20-registraciia-i-prostranstva.png) |
| 21 | Онбординг · возобновление и итог | `/onboarding/*` resume по серверному статусу, необязательный шаг Telegram, завершение | готов | [SVG](mockups/svg/21-onbording-vozobnovlenie.svg) · [PNG](mockups/preview/21-onbording-vozobnovlenie.png) |
| 22 | Настройки · диалоги услуг | `/settings/services` создание/изменение, цена «точная, диапазон, не указана», отключение | готов | [SVG](mockups/svg/22-uslugi-dialogi.svg) · [PNG](mockups/preview/22-uslugi-dialogi.png) |
| 23 | Диалоги · состояния переписки | пустой поиск, удалённое сообщение, недоступное вложение, нет внешнего перехода, ранние сообщения | готов | [SVG](mockups/svg/23-perepiska-sostoianiia.svg) · [PNG](mockups/preview/23-perepiska-sostoianiia.png) |
| 24 | Команда · подтверждения | смена роли, отзыв доступа, защита последнего владельца, одноразовый код | готов | [SVG](mockups/svg/24-komanda-podtverzhdeniia.svg) · [PNG](mockups/preview/24-komanda-podtverzhdeniia.png) |
| 25 | Настройки · данные и согласие | `/settings/privacy` для всех участников, мутации владельца | готов | [SVG](mockups/svg/25-dannye-i-soglasie.svg) · [PNG](mockups/preview/25-dannye-i-soglasie.png) |
| 26 | Администрирование · обзор | `/admin` снимок очередей, ручное обновление, проверка права | готов | [SVG](mockups/svg/26-admin-obzor.svg) · [PNG](mockups/preview/26-admin-obzor.png) |
| 27 | Администрирование · мёртвые письма | `/admin/dead-letters` четыре очереди, одиночные команды, подтверждение, 409 | готов | [SVG](mockups/svg/27-admin-mertvye-pisma.svg) · [PNG](mockups/preview/27-admin-mertvye-pisma.png) |
| 28 | Администрирование · трассировка и AI | `/admin/trace`, факты анализа, администраторы платформы | готов | [SVG](mockups/svg/28-admin-trassirovka-i-ai.svg) · [PNG](mockups/preview/28-admin-trassirovka-i-ai.png) |
| 29 | Узкий экран · навигация и Radar | 375 CSS px: «Меню» как диалог, Radar в одну колонку, карточка риска секциями | спецификация | [SVG](mockups/svg/29-uzkii-ekran-navigaciia-radar.svg) · [PNG](mockups/preview/29-uzkii-ekran-navigaciia-radar.png) |
| 30 | Узкий экран · диалоги и формы | список и переписка как состояния маршрута, формы, модальные окна | спецификация | [SVG](mockups/svg/30-uzkii-ekran-dialogi-i-formy.svg) · [PNG](mockups/preview/30-uzkii-ekran-dialogi-i-formy.png) |
| 31 | Планшет 768 и масштаб 200 % | боковая панель с 768, одна колонка, прокрутка таблиц внутри области | спецификация | [SVG](mockups/svg/31-planshet-i-masshtab.svg) · [PNG](mockups/preview/31-planshet-i-masshtab.png) |
| 32 | Состояния форм | подключение источника, 409, потеря права, неизвестный результат, офлайн, отправка | готов | [SVG](mockups/svg/32-sostoianiia-form.svg) · [PNG](mockups/preview/32-sostoianiia-form.png) |

Статус «готов» означает, что нужные операции существуют и макет утверждён;
«спецификация» — лист задаёт правила поведения, а не отдельный экран. Номера
листов задают порядок просмотра, не route или sprint. Все листы v0.3 используют
один демонстрационный набор (§6).

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

| Route family | Desktop composition | Narrow fallback (листы 29–31) |
|---|---|---|
| Auth | центрированная card без app sidebar (01, 20) | card на всю доступную ширину |
| Onboarding | progress + single form/card (02–05, 21) | progress compact, одна колонка |
| Radar | sidebar + header/filters + metrics + feed (17) | «Меню»-диалог; сводка и лента в одну колонку (29) |
| Risk Workspace | header + context/action columns (18, 19) | последовательные секции в фиксированном порядке; без закреплённых элементов (29) |
| Conversations | list/detail split pane (06, 23) | list и detail как отдельные route states с «К списку» (30) |
| Settings | tabs + form/table (09–12, 22, 24, 25) | вкладки прокручиваются, формы в одну колонку, таблицы в labelled scroll region (30, 31) |
| Analytics | date controls + cards + chart/table (07) | chart/table scroll только внутри region |
| Admin | dense nav + filters + tables/detail (26–28) | «Меню»-диалог, карточки вместо строк; command controls не обрезаются |

Breakpoints — существующие токены Tailwind проекта (`sm` 640, `md` 768,
`lg` 1024); правила поведения — в §7. Независимо от breakpoint поддерживается
viewport 320 CSS px, 200% zoom и reflow без горизонтальной прокрутки всей
страницы; data table может иметь собственный labelled scroll region.

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
  полировку Audi A6, Risk Workspace — керамику BMW X5. Пакеты v0.2 и v0.3
  используют один набор: организация Detail Lab, точка «Студия на Пресне»,
  владелец Мария Владелец, менеджер Анна Смирнова, клиент Дмитрий Соколов с
  полировкой кузова на 31 000 ₽ (без марки автомобиля), второй риск — Елена
  Волкова, керамическое покрытие, 16 000 ₽; демонстрационный набор e2e
  веб-репозитория использует те же имена людей и услуг, отличаясь только
  названиями организации и точки. Это закрывает GAP-DESIGN-018; код не должен
  hardcode ни один пример.
- Термины денег фиксированы: «потенциальная», «подтверждённая»,
  «подтверждённо возвращённая». Нельзя сокращать последнюю до «возвращённой»
  без provenance.
- «Диалоги» read-only; CTA говорит «Открыть/Ответить в Telegram», но не создаёт
  ожидание in-app отправки.
- Error copy не повторяет произвольный server `message`, PII или secret.

<a id="responsive-spec"></a>
## 7. Адаптивная спецификация (листы 29–31)

Утверждена 25 сентября 2026 года вместе с комплектом v0.3 и закрывает
GAP-DESIGN-015. Контрольные точки — токены Tailwind проекта: `sm` 640,
`md` 768, `lg` 1024.

| Ширина области | Навигация | Раскладка |
|---|---|---|
| 320–767 CSS px (телефон, а также 200 % масштаба при 1280) | боковая панель скрыта; кнопка «Меню» в шапке открывает диалог с ловушкой фокуса: переключатель организации, разделы по правам, индикатор потока, ссылка администрирования, выход; Escape и «×» возвращают фокус на кнопку | одна колонка; действия на всю ширину высотой 44 px; сводка и лента Radar складываются вертикально; карточка риска — секции в порядке «статус и главная команда → причина → переписка и сделка → рекомендация → оценка → действие → исход → деньги» |
| 768–1023 CSS px (планшет) | боковая панель 232 px закреплена слева | содержимое в одну колонку; диалоги показывают либо список, либо переписку («К списку» возвращает с сохранёнными фильтрами); аналитика и карточка риска складывают секции вертикально |
| ≥ 1024 CSS px | боковая панель 232 px | десктопные листы 06–28; разделённая раскладка переписок и колонки карточки риска только отсюда |

Инварианты для любой ширины:

- Нет горизонтальной прокрутки страницы. Таблицы шире области получают
  собственную прокручиваемую область с именем (`role="region"`, `tabindex="0"`),
  кнопки действий остаются в строке.
- Диалоги Reka: ширина `100 % − 2 rem`, максимум 28 rem, ловушка фокуса,
  Escape закрывает, кроме неделимой операции в состоянии ожидания; фокус
  возвращается к триггеру.
- Подписи полей всегда видны, ошибки связаны через `aria-describedby`, цвет —
  не единственный признак; закреплённых элементов, перекрывающих фокус, нет.
- Длинные почты, названия и суммы переносятся или получают `break-all`; полное
  значение доступно в подсказке и копированием.
- Фокус: кольцо 2 px цвета `brand`, видимо после Tab на любом фоне.
  `prefers-reduced-motion` сводит анимации и переходы к 0,01 мс.
- Проверка автоматическая: матрица 320/375/768/1024/1440 и имитация 200 %
  (640 px) в `tests/e2e/responsive.spec.ts` веб-репозитория (нет
  горизонтальной прокрутки, главное действие видно, axe без нарушений, фокус
  видим) плюс профиль Playwright «mobile» (iPhone 13) для сценариев.

Отдельных макетов для 1024 и 1440 в планшетном режиме нет намеренно: с 1024 px
действуют десктопные листы.

<a id="missing-designs"></a>
## 8. Ранее отсутствовавшие макеты — закрыто комплектом v0.3

Список из версии документа от 14 сентября 2026 года и листы, которые его
закрыли (GAP-DESIGN-014/015/018 → `CLOSED` 2026-09-25):

1. Основной Radar с заполненной лентой и фильтрами — лист 17.
2. Полный Risk Workspace: активный риск, загрузка, закрытый риск, конфликт
   этапа, потеря права, неизвестный результат — листы 18 и 19; оплата — 13.
3. Registration и workspace selection, принятие приглашения — лист 20.
4. Onboarding resume/skip/completed — лист 21; форма токена источника —
   лист 32 (композиция «Подключение источника»), подключённое состояние — 05.
5. Add/edit/deactivate service dialogs — лист 22 (повторное включение —
   `PATCH active=true` без отдельного диалога).
6. Conversation: пустой поиск, удалённое сообщение, недоступное вложение,
   внешний переход недоступен, подгрузка ранних сообщений — лист 23.
7. Team role/revoke confirmations и last-owner protection, одноразовый код —
   лист 24.
8. Privacy/ML consent — лист 25.
9. Platform Admin: обзор, мёртвые письма с подтверждением и 409, трассировка,
   факты AI, администраторы — листы 26–28; списки организаций, подключений,
   заданий, AI-узлов и потребления используют ту же оболочку и таблицы без
   отдельного листа.
10. Narrow 320–767 и tablet — листы 29–31 и §7.
11. Validation, conflict, offline, permission-lost, unknown-result и pending
    states для форм — лист 32 (дополняет лист 15).

Новые продуктовые решения по-прежнему не принимаются в коде без макета или
записи в [backlog](TASKS.md); v0.3 описывает уже реализованное поведение
клиента и служит эталоном визуальной приёмки.

<a id="handoff-checklist"></a>
## 9. Design-to-development checklist

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
