CREATE TABLE notifications (
    id UUID PRIMARY KEY,
    tenant_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    user_id UUID NOT NULL,
    risk_id UUID NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('RISK_OPENED')),
    dedup_key TEXT NOT NULL,
    title TEXT NOT NULL,
    body TEXT NOT NULL,
    snoozed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT notifications_tenant_id_unique UNIQUE (tenant_id, id),
    CONSTRAINT notifications_dedup_unique UNIQUE (tenant_id, dedup_key),
    CONSTRAINT notifications_membership_fk
        FOREIGN KEY (tenant_id, user_id) REFERENCES memberships(tenant_id, user_id),
    CONSTRAINT notifications_risk_fk
        FOREIGN KEY (tenant_id, risk_id) REFERENCES risk_signals(tenant_id, id),
    CONSTRAINT notifications_dedup_valid CHECK (
        dedup_key = btrim(dedup_key) AND char_length(dedup_key) BETWEEN 1 AND 512
    ),
    CONSTRAINT notifications_title_valid CHECK (
        title = btrim(title) AND char_length(title) BETWEEN 1 AND 200
    ),
    CONSTRAINT notifications_body_valid CHECK (
        body = btrim(body) AND char_length(body) BETWEEN 1 AND 2000
    )
);

CREATE INDEX notifications_user_timeline_idx
    ON notifications(tenant_id, user_id, created_at DESC, id DESC);

CREATE TABLE notification_deliveries (
    id UUID PRIMARY KEY,
    tenant_id UUID NOT NULL,
    notification_id UUID NOT NULL,
    channel TEXT NOT NULL CHECK (channel IN ('IN_APP', 'TELEGRAM')),
    destination TEXT NOT NULL,
    title TEXT NOT NULL,
    body TEXT NOT NULL,
    attempt INTEGER NOT NULL CHECK (attempt BETWEEN 1 AND 5),
    status TEXT NOT NULL CHECK (status IN ('PENDING', 'PROCESSING', 'SUCCEEDED', 'RETRY', 'DEAD')),
    available_at TIMESTAMPTZ NOT NULL,
    lease_owner TEXT,
    lease_until TIMESTAMPTZ,
    attempted_at TIMESTAMPTZ,
    provider_message_id TEXT,
    failure_code TEXT,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT notification_deliveries_attempt_unique
        UNIQUE (tenant_id, notification_id, channel, attempt),
    CONSTRAINT notification_deliveries_notification_fk
        FOREIGN KEY (tenant_id, notification_id)
        REFERENCES notifications(tenant_id, id) ON DELETE CASCADE,
    CONSTRAINT notification_deliveries_destination_valid CHECK (
        destination = btrim(destination) AND char_length(destination) BETWEEN 1 AND 255
    ),
    CONSTRAINT notification_deliveries_failure_code_valid CHECK (
        failure_code IS NULL OR failure_code ~ '^[A-Z][A-Z0-9_]{0,99}$'
    ),
    CONSTRAINT notification_deliveries_lifecycle_consistent CHECK (
        (status = 'PENDING' AND lease_owner IS NULL AND lease_until IS NULL
            AND attempted_at IS NULL AND provider_message_id IS NULL AND failure_code IS NULL)
        OR (status = 'PROCESSING' AND lease_owner IS NOT NULL AND lease_until IS NOT NULL
            AND attempted_at IS NULL AND provider_message_id IS NULL AND failure_code IS NULL)
        OR (status = 'SUCCEEDED' AND lease_owner IS NULL AND lease_until IS NULL
            AND attempted_at IS NOT NULL AND provider_message_id IS NOT NULL AND failure_code IS NULL)
        OR (status = 'RETRY' AND lease_owner IS NULL AND lease_until IS NULL
            AND attempted_at IS NOT NULL AND provider_message_id IS NULL AND failure_code IS NOT NULL)
        OR (status = 'DEAD' AND lease_owner IS NULL AND lease_until IS NULL
            AND attempted_at IS NOT NULL AND provider_message_id IS NULL AND failure_code IS NOT NULL)
    )
);

CREATE INDEX notification_deliveries_claim_idx
    ON notification_deliveries(status, available_at, created_at, id)
    WHERE status IN ('PENDING', 'PROCESSING');

CREATE TABLE telegram_user_links (
    id UUID PRIMARY KEY,
    tenant_id UUID NOT NULL,
    user_id UUID NOT NULL,
    telegram_user_id BIGINT NOT NULL CHECK (telegram_user_id > 0),
    chat_id BIGINT NOT NULL,
    linked_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    disabled_at TIMESTAMPTZ,
    CONSTRAINT telegram_user_links_membership_fk
        FOREIGN KEY (tenant_id, user_id) REFERENCES memberships(tenant_id, user_id) ON DELETE CASCADE,
    CONSTRAINT telegram_user_links_user_unique UNIQUE (tenant_id, user_id),
    CONSTRAINT telegram_user_links_telegram_unique UNIQUE (tenant_id, telegram_user_id)
);

CREATE INDEX telegram_user_links_active_owner_idx
    ON telegram_user_links(tenant_id, user_id, linked_at)
    WHERE disabled_at IS NULL;

CREATE TABLE telegram_link_tokens (
    id UUID PRIMARY KEY,
    tenant_id UUID NOT NULL,
    user_id UUID NOT NULL,
    token_hash CHAR(64) NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    used_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT telegram_link_tokens_membership_fk
        FOREIGN KEY (tenant_id, user_id) REFERENCES memberships(tenant_id, user_id) ON DELETE CASCADE,
    CONSTRAINT telegram_link_tokens_hash_unique UNIQUE (token_hash),
    CONSTRAINT telegram_link_tokens_hash_valid CHECK (token_hash ~ '^[0-9a-f]{64}$'),
    CONSTRAINT telegram_link_tokens_expiry_valid CHECK (expires_at > created_at),
    CONSTRAINT telegram_link_tokens_use_valid CHECK (used_at IS NULL OR used_at >= created_at)
);

CREATE INDEX telegram_link_tokens_active_idx
    ON telegram_link_tokens(expires_at, token_hash)
    WHERE used_at IS NULL;

CREATE TABLE telegram_callback_commands (
    id UUID PRIMARY KEY,
    tenant_id UUID NOT NULL,
    user_id UUID NOT NULL,
    notification_id UUID NOT NULL,
    risk_id UUID NOT NULL,
    idempotency_key TEXT NOT NULL,
    action TEXT NOT NULL CHECK (action IN ('OPEN_RISK', 'ACKNOWLEDGE', 'SNOOZE')),
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT telegram_callback_commands_notification_fk
        FOREIGN KEY (tenant_id, notification_id)
        REFERENCES notifications(tenant_id, id) ON DELETE CASCADE,
    CONSTRAINT telegram_callback_commands_risk_fk
        FOREIGN KEY (tenant_id, risk_id) REFERENCES risk_signals(tenant_id, id),
    CONSTRAINT telegram_callback_commands_membership_fk
        FOREIGN KEY (tenant_id, user_id) REFERENCES memberships(tenant_id, user_id),
    CONSTRAINT telegram_callback_commands_idempotency_unique UNIQUE (tenant_id, idempotency_key),
    CONSTRAINT telegram_callback_commands_idempotency_valid CHECK (
        idempotency_key = btrim(idempotency_key) AND char_length(idempotency_key) BETWEEN 1 AND 255
    )
);

CREATE INDEX telegram_callback_commands_notification_idx
    ON telegram_callback_commands(tenant_id, notification_id, created_at, id);
