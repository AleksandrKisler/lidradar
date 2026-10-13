-- Почта и безопасность учётной записи (ADR 0058): мягкое подтверждение адреса,
-- одноразовые токены подтверждения и сброса пароля, очередь исходящих писем и
-- новые пределы частоты. Таблицы без tenant_id: учётная запись и почта не
-- принадлежат организации, RLS на них нет.

-- Подтверждение мягкое: пустое значение означает «не подтверждён» и ничего не
-- блокирует. У существующих пользователей значение остаётся пустым.
ALTER TABLE users ADD COLUMN email_verified_at TIMESTAMPTZ;

-- Одноразовые токены. В базе только SHA-256 токена, сам токен уходит в письмо.
-- Строка не удаляется при использовании: погашение и отзыв фиксируются отметками.
-- Персональных данных, кроме ссылки на пользователя, таблица не хранит.
CREATE TABLE account_tokens (
    id UUID PRIMARY KEY,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    purpose TEXT NOT NULL,
    token_hash CHAR(64) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    used_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ,
    CONSTRAINT account_tokens_hash_unique UNIQUE (token_hash),
    CONSTRAINT account_tokens_purpose_valid CHECK (purpose IN ('EMAIL_VERIFICATION', 'PASSWORD_RESET')),
    CONSTRAINT account_tokens_hash_valid CHECK (token_hash ~ '^[0-9a-f]{64}$'),
    CONSTRAINT account_tokens_lifetime_valid CHECK (expires_at > created_at),
    CONSTRAINT account_tokens_single_outcome CHECK (used_at IS NULL OR revoked_at IS NULL)
);

-- Одновременно действует не больше одного неиспользованного токена на пользователя
-- и назначение: выпуск нового сначала отзывает прежний в той же транзакции.
CREATE UNIQUE INDEX account_tokens_active_idx
    ON account_tokens(user_id, purpose)
    WHERE used_at IS NULL AND revoked_at IS NULL;

CREATE INDEX account_tokens_user_idx ON account_tokens(user_id, created_at DESC);

-- Очередь исходящих писем (владелец — модуль mail). Текст и адрес получателя
-- нужны только пока письмо ждёт отправки: при любом завершении они стираются, а
-- строка хранит вид, тему, домен получателя, число попыток и код сбоя.
CREATE TABLE mail_outbox (
    id UUID PRIMARY KEY,
    kind TEXT NOT NULL,
    subject TEXT NOT NULL,
    recipient TEXT,
    recipient_domain TEXT NOT NULL,
    body TEXT,
    status TEXT NOT NULL,
    attempts SMALLINT NOT NULL DEFAULT 0,
    available_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    leased_by TEXT,
    lease_until TIMESTAMPTZ,
    failure_code TEXT,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    sent_at TIMESTAMPTZ,
    CONSTRAINT mail_outbox_kind_valid CHECK (kind ~ '^[A-Z][A-Z0-9_]{0,49}$'),
    CONSTRAINT mail_outbox_status_valid CHECK (status IN ('PENDING', 'PROCESSING', 'SENT', 'DEAD')),
    CONSTRAINT mail_outbox_subject_valid CHECK (
        subject = btrim(subject) AND char_length(subject) BETWEEN 1 AND 200 AND subject !~ '[\r\n]'
    ),
    CONSTRAINT mail_outbox_recipient_valid CHECK (
        recipient IS NULL OR (
            recipient = lower(btrim(recipient)) AND char_length(recipient) BETWEEN 3 AND 254 AND recipient !~ '[\r\n]'
        )
    ),
    CONSTRAINT mail_outbox_domain_valid CHECK (char_length(recipient_domain) BETWEEN 1 AND 253),
    CONSTRAINT mail_outbox_body_valid CHECK (body IS NULL OR char_length(body) BETWEEN 1 AND 20000),
    CONSTRAINT mail_outbox_attempts_valid CHECK (attempts BETWEEN 0 AND 20),
    CONSTRAINT mail_outbox_failure_valid CHECK (failure_code IS NULL OR failure_code ~ '^[A-Z][A-Z0-9_]{0,99}$'),
    CONSTRAINT mail_outbox_lifetime_valid CHECK (expires_at > created_at),
    -- Тело и адрес есть у письма, которое ещё может уйти, и стёрты у завершённого.
    CONSTRAINT mail_outbox_content_consistent CHECK (
        (status IN ('PENDING', 'PROCESSING') AND recipient IS NOT NULL AND body IS NOT NULL)
        OR (status IN ('SENT', 'DEAD') AND recipient IS NULL AND body IS NULL)
    ),
    CONSTRAINT mail_outbox_lease_consistent CHECK (
        (status = 'PROCESSING' AND leased_by IS NOT NULL AND lease_until IS NOT NULL)
        OR (status <> 'PROCESSING' AND leased_by IS NULL AND lease_until IS NULL)
    ),
    CONSTRAINT mail_outbox_sent_consistent CHECK ((status = 'SENT') = (sent_at IS NOT NULL)),
    CONSTRAINT mail_outbox_dead_has_reason CHECK (status <> 'DEAD' OR failure_code IS NOT NULL)
);

CREATE INDEX mail_outbox_due_idx ON mail_outbox(available_at, id) WHERE status = 'PENDING';
CREATE INDEX mail_outbox_lease_idx ON mail_outbox(lease_until) WHERE status = 'PROCESSING';
CREATE INDEX mail_outbox_created_idx ON mail_outbox(created_at, id);

-- Пределы сброса пароля, смены пароля и подтверждения почты (ADR 0058).
ALTER TABLE auth_rate_limits DROP CONSTRAINT auth_rate_limits_scope_check;
ALTER TABLE auth_rate_limits ADD CONSTRAINT auth_rate_limits_scope_check CHECK (scope IN (
    'REGISTER_IP', 'LOGIN_IP', 'LOGIN_ACCOUNT', 'REFRESH_IP',
    'PASSWORD_RESET_IP', 'PASSWORD_RESET_ACCOUNT', 'PASSWORD_RESET_CONFIRM_IP',
    'PASSWORD_CHANGE_ACCOUNT', 'EMAIL_VERIFICATION_ACCOUNT', 'EMAIL_VERIFY_IP'
));
