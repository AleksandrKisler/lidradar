-- Этап 20: политика уведомлений (ТЗ §3.7, §45–§47, LR-BE-RM-015/020).
-- Настройки хранятся как самостоятельная сущность на пользователя и тип риска;
-- отложенные уведомления (дайджест и тихие часы) накапливаются в очереди
-- элементов и доставляются одним сообщением в срок, вычисленный в часовом
-- поясе организации.

CREATE TABLE notification_preferences (
    id UUID PRIMARY KEY,
    tenant_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    user_id UUID NOT NULL,
    risk_type TEXT NOT NULL CHECK (risk_type IN (
        'NO_RESPONSE', 'BOOKING_NOT_CONFIRMED', 'PROMISE_NOT_FULFILLED',
        'CUSTOMER_SILENT_AFTER_PRICE', 'FOLLOW_UP_CANDIDATE'
    )),
    minimum_severity TEXT NOT NULL CHECK (minimum_severity IN ('LOW', 'MEDIUM', 'HIGH', 'CRITICAL')),
    delivery_mode TEXT NOT NULL CHECK (delivery_mode IN ('IMMEDIATE', 'DIGEST', 'DISABLED')),
    in_app_enabled BOOLEAN NOT NULL,
    telegram_enabled BOOLEAN NOT NULL,
    quiet_hours_enabled BOOLEAN NOT NULL,
    quiet_hours_start TIME,
    quiet_hours_end TIME,
    digest_time TIME NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT notification_preferences_tenant_id_unique UNIQUE (tenant_id, id),
    CONSTRAINT notification_preferences_membership_fk
        FOREIGN KEY (tenant_id, user_id) REFERENCES memberships(tenant_id, user_id),
    CONSTRAINT notification_preferences_user_type_unique UNIQUE (tenant_id, user_id, risk_type),
    -- LR-BE-RM-020: совпадающие границы тихих часов неоднозначны и запрещены.
    CONSTRAINT notification_preferences_quiet_not_degenerate CHECK (
        quiet_hours_start IS NULL OR quiet_hours_start <> quiet_hours_end
    ),
    CONSTRAINT notification_preferences_quiet_pair CHECK (
        (quiet_hours_start IS NULL) = (quiet_hours_end IS NULL)
    ),
    CONSTRAINT notification_preferences_quiet_enabled_needs_bounds CHECK (
        NOT quiet_hours_enabled OR quiet_hours_start IS NOT NULL
    ),
    CONSTRAINT notification_preferences_minute_precision CHECK (
        (quiet_hours_start IS NULL OR EXTRACT(SECOND FROM quiet_hours_start) = 0)
        AND (quiet_hours_end IS NULL OR EXTRACT(SECOND FROM quiet_hours_end) = 0)
        AND EXTRACT(SECOND FROM digest_time) = 0
    ),
    CONSTRAINT notification_preferences_updated_valid CHECK (updated_at >= created_at)
);

-- Логическое уведомление получает вид: немедленное открытие риска, сводка
-- (дайджест или тихие часы) и эскалация владельцу. Сводка объединяет несколько
-- рисков и не привязана к одному risk_id.
ALTER TABLE notifications
    DROP CONSTRAINT notifications_kind_check,
    ADD CONSTRAINT notifications_kind_check CHECK (
        kind IN ('RISK_OPENED', 'RISK_DIGEST', 'RISK_ESCALATED')
    ),
    ALTER COLUMN risk_id DROP NOT NULL,
    ADD CONSTRAINT notifications_kind_risk_consistent CHECK (
        (kind = 'RISK_DIGEST') = (risk_id IS NULL)
    );

-- Ключ дедупликации становится персональным: один риск даёт один видимый
-- пользователю факт на каждого получателя (ТЗ §47).
UPDATE notifications
SET dedup_key = dedup_key || ':user:' || user_id::text
WHERE kind = 'RISK_OPENED' AND dedup_key = 'risk:' || risk_id::text || ':opened';

ALTER TABLE notification_deliveries
    ADD COLUMN kind TEXT NOT NULL DEFAULT 'RISK_OPENED'
        CHECK (kind IN ('RISK_OPENED', 'RISK_DIGEST', 'RISK_ESCALATED'));

ALTER TABLE notification_deliveries
    ALTER COLUMN kind DROP DEFAULT;

-- Очередь отложенных элементов. Один риск попадает в очередь пользователя не
-- более одного раза; элемент считается доставленным, когда его забрала сводка.
CREATE TABLE notification_digest_items (
    id UUID PRIMARY KEY,
    tenant_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    user_id UUID NOT NULL,
    risk_id UUID NOT NULL,
    risk_type TEXT NOT NULL CHECK (risk_type IN (
        'NO_RESPONSE', 'BOOKING_NOT_CONFIRMED', 'PROMISE_NOT_FULFILLED',
        'CUSTOMER_SILENT_AFTER_PRICE', 'FOLLOW_UP_CANDIDATE'
    )),
    reason TEXT NOT NULL CHECK (reason IN ('DIGEST', 'QUIET_HOURS')),
    slot TEXT NOT NULL,
    deliver_at TIMESTAMPTZ NOT NULL,
    in_app_enabled BOOLEAN NOT NULL,
    telegram_enabled BOOLEAN NOT NULL,
    notification_id UUID,
    consumed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT notification_digest_items_tenant_id_unique UNIQUE (tenant_id, id),
    CONSTRAINT notification_digest_items_membership_fk
        FOREIGN KEY (tenant_id, user_id) REFERENCES memberships(tenant_id, user_id),
    CONSTRAINT notification_digest_items_risk_fk
        FOREIGN KEY (tenant_id, risk_id) REFERENCES risk_signals(tenant_id, id),
    CONSTRAINT notification_digest_items_notification_fk
        FOREIGN KEY (tenant_id, notification_id) REFERENCES notifications(tenant_id, id),
    CONSTRAINT notification_digest_items_once_per_user UNIQUE (tenant_id, user_id, risk_id),
    CONSTRAINT notification_digest_items_slot_valid CHECK (
        slot ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}$'
    ),
    CONSTRAINT notification_digest_items_channel_present CHECK (in_app_enabled OR telegram_enabled),
    CONSTRAINT notification_digest_items_consumed_valid CHECK (
        consumed_at IS NULL OR consumed_at >= created_at
    ),
    CONSTRAINT notification_digest_items_notification_consumed CHECK (
        notification_id IS NULL OR consumed_at IS NOT NULL
    )
);

CREATE INDEX notification_digest_items_pending_idx
    ON notification_digest_items(tenant_id, user_id, slot, created_at)
    WHERE consumed_at IS NULL;
