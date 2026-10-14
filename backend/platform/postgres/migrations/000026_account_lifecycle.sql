-- Выгрузка данных и закрытие учётной записи (ADR 0059): назначенное и выполненное
-- обезличивание пользователя и новые пределы частоты. Новых таблиц нет: выгрузки
-- считаются на лету, а закрытие хранится отметками на строке пользователя.

-- deletion_requested_at / deletion_due_at задаются вместе при запросе пользователя и
-- снимаются при отмене. deleted_at ставит исполнитель обезличивания или операторская
-- команда; у обезличенного пользователя отметки назначения остаются как история.
ALTER TABLE users
    ADD COLUMN deletion_requested_at TIMESTAMPTZ,
    ADD COLUMN deletion_due_at TIMESTAMPTZ,
    ADD COLUMN deleted_at TIMESTAMPTZ;

-- Обе проверки на NOT NULL нужны явно: сравнение с NULL даёт NULL, а CHECK пропускает NULL.
ALTER TABLE users ADD CONSTRAINT users_deletion_schedule_valid CHECK (
    (deletion_requested_at IS NULL AND deletion_due_at IS NULL)
    OR (deletion_requested_at IS NOT NULL AND deletion_due_at IS NOT NULL AND deletion_due_at > deletion_requested_at)
);

-- Обезличенный пользователь никогда не остаётся активным.
ALTER TABLE users ADD CONSTRAINT users_deleted_is_disabled CHECK (
    deleted_at IS NULL OR status = 'DISABLED'
);

-- Исполнитель выбирает назначенные и ещё не выполненные удаления по сроку.
CREATE INDEX users_deletion_due_idx
    ON users(deletion_due_at, id)
    WHERE deletion_due_at IS NOT NULL AND deleted_at IS NULL;

-- Пределы выгрузок и запросов на закрытие (ADR 0059).
ALTER TABLE auth_rate_limits DROP CONSTRAINT auth_rate_limits_scope_check;
ALTER TABLE auth_rate_limits ADD CONSTRAINT auth_rate_limits_scope_check CHECK (scope IN (
    'REGISTER_IP', 'LOGIN_IP', 'LOGIN_ACCOUNT', 'REFRESH_IP',
    'PASSWORD_RESET_IP', 'PASSWORD_RESET_ACCOUNT', 'PASSWORD_RESET_CONFIRM_IP',
    'PASSWORD_CHANGE_ACCOUNT', 'EMAIL_VERIFICATION_ACCOUNT', 'EMAIL_VERIFY_IP',
    'ACCOUNT_EXPORT_ACCOUNT', 'ACCOUNT_DELETION_ACCOUNT', 'ORGANIZATION_EXPORT_TENANT'
));
