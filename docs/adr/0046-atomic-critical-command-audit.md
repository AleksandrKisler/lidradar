# 0046: Атомарный аудит критических команд

- Status: Proposed
- Date: 2026-10-06
- Связь: H-04; заменяет только post-commit исключение пункта 5 ADR 0041 после принятия.

## Context

`audit.Recorder` вызывается после изменения в identity, tenant, connector,
opportunity, risk/Radar и notification/preferences. Ошибка Recorder возвращается
после commit. Повтор перехода или закрытия может оказаться no-op и уже не вызвать
Recorder: отсутствие следа нельзя устранить одним повтором HTTP.
Action/Outcome/Revenue, consent, приглашения, feedback и admin уже передают
audit metadata в транзакцию соответствующего хранилища.

## Decision proposed

Распространить существующую транзакционную модель на оставшиеся команды.
Application создаёт metadata (actor, tenant, operation, entity, timestamp, audit ID)
до записи. Репозиторий владельца выполняет доменное изменение, его историю/outbox
и INSERT аудита на одном pgx.Tx. Общий infrastructure writer аудита принимает
ограниченный SQL executor; application/domain не получают pgx и не читают чужие
таблицы. Ошибка INSERT или commit возвращает ошибку и откатывает всю транзакцию.
Отдельный post-commit Recorder для этих операций удаляется из wiring.

Область перехода:

- identity: регистрация пользователя, выдача и отзыв сессии + auth_audit_log;
- tenant: организация, добавление участника, смена роли, отзыв членства;
- connector: сохранение подключения/отключения + audit_log;
- opportunity: ручной переход и stage history;
- risk: acknowledge/resolve; feedback уже транзакционен;
- notification: установка/удаление политики уведомлений.

Внешний вызов Telegram не становится частью PostgreSQL-транзакции.
Его исход и локальное изменение — разные границы: при ошибке после внешнего
успеха сохраняются reconciliation/retry правила интеграции. Этот ADR не обещает
распределённую атомарность или exactly-once во внешнем сервисе.

## Alternatives considered

- Сохранить post-commit запись: простая реализация, но остаётся H-04.
- Audit outbox: обеспечивает восстановление после commit, но добавляет доставку,
  дедупликацию и отдельный lag/SLO; для одной PostgreSQL базы сейчас не нужен.
- Повторить Recorder при HTTP replay: не защищает от падения процесса между
  commit и записью, поэтому не закрывает обязательный след.

## Consequences and acceptance

Новая схема хранения не требуется; меняются порты репозиториев и wiring.
Нельзя молча назначать system actor вместо реального пользователя. Для каждой
операции обязательны успешная запись, ошибка audit INSERT с rollback, ошибка
commit, повтор/no-op без лишнего аудита, tenant isolation и отзыв полномочий.
Один общий тест не доказывает остальные классы.

Переход можно выполнять по модулям, но H-04 и RG-MONEY не закрываются, пока
остаётся хотя бы одна применимая post-commit операция. Старые пробелы аудита
отмечаются отдельно; выдумывать задним числом событие пользователя нельзя.

## Migration and rollback

Выпускать совместно application, repository и wiring каждого модуля. Проверить
canonical Go и обязательный PostgreSQL gate. Откат к старому binary возвращает
ограничение post-commit и не считается безопасным восстановлением гарантии;
предпочтителен forward-fix. Таблицы и RLS не изменяются.
