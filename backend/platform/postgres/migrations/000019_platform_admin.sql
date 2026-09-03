-- Этап 23: платформенные администраторы (LR-BE-2301, LR-BE-RM-008),
-- append-only журнал административных команд (§65) и признак «отложено»
-- для мёртвых элементов очередей (LR-BE-2305/2309).

-- Право PLATFORM_ADMIN принадлежит пользователю, а не членству. Повторная
-- выдача после отзыва создаёт новую строку; история выдач сохраняется.
CREATE TABLE platform_admins (
    id UUID PRIMARY KEY,
    user_id UUID NOT NULL REFERENCES users(id),
    granted_by UUID REFERENCES users(id),
    granted_at TIMESTAMPTZ NOT NULL,
    revoked_by UUID REFERENCES users(id),
    revoked_at TIMESTAMPTZ,
    note TEXT NOT NULL DEFAULT '',
    CONSTRAINT platform_admins_revocation_valid CHECK (revoked_at IS NOT NULL OR revoked_by IS NULL),
    CONSTRAINT platform_admins_revoked_after_grant CHECK (revoked_at IS NULL OR revoked_at >= granted_at),
    CONSTRAINT platform_admins_note_valid CHECK (note = btrim(note) AND char_length(note) <= 500)
);

CREATE UNIQUE INDEX platform_admins_one_active_idx
    ON platform_admins(user_id)
    WHERE revoked_at IS NULL;

CREATE FUNCTION reject_platform_admin_mutation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'platform_admins is append-only' USING ERRCODE = 'restrict_violation';
    END IF;
    IF OLD.revoked_at IS NOT NULL OR NEW.revoked_at IS NULL
       OR NEW.id <> OLD.id OR NEW.user_id <> OLD.user_id OR NEW.granted_at <> OLD.granted_at
       OR NEW.granted_by IS DISTINCT FROM OLD.granted_by OR NEW.note <> OLD.note THEN
        RAISE EXCEPTION 'platform_admins allows only a single revocation' USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER platform_admins_revoke_only
    BEFORE UPDATE OR DELETE ON platform_admins
    FOR EACH ROW EXECUTE FUNCTION reject_platform_admin_mutation();

-- Административные команды не принадлежат организации и не требуют членства,
-- поэтому живут отдельно от tenant-аудита. Источник CLI не имеет актора.
CREATE TABLE admin_audit_log (
    id UUID PRIMARY KEY,
    actor_user_id UUID REFERENCES users(id),
    source TEXT NOT NULL CHECK (source IN ('API', 'CLI')),
    operation TEXT NOT NULL,
    entity_type TEXT NOT NULL,
    entity_id UUID NOT NULL,
    tenant_id UUID,
    details JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT admin_audit_log_actor_by_source CHECK (
        (source = 'API' AND actor_user_id IS NOT NULL) OR (source = 'CLI' AND actor_user_id IS NULL)
    ),
    CONSTRAINT admin_audit_log_operation_valid CHECK (
        operation = btrim(operation) AND operation ~ '^[A-Z][A-Z0-9_]{0,99}$'
    ),
    CONSTRAINT admin_audit_log_entity_type_valid CHECK (
        entity_type = btrim(entity_type) AND entity_type ~ '^[A-Z][A-Z0-9_]{0,99}$'
    ),
    CONSTRAINT admin_audit_log_details_object CHECK (jsonb_typeof(details) = 'object')
);

CREATE INDEX admin_audit_log_timeline_idx
    ON admin_audit_log(created_at DESC, id DESC);

CREATE TRIGGER admin_audit_log_append_only
    BEFORE UPDATE OR DELETE ON admin_audit_log
    FOR EACH ROW EXECUTE FUNCTION reject_append_only_mutation();

-- «Отложено» — осознанное решение администратора не повторять мёртвый
-- элемент; строка остаётся для истории, но уходит из панели мёртвых.
ALTER TABLE jobs
    ADD COLUMN discarded_at TIMESTAMPTZ,
    ADD COLUMN discarded_by UUID,
    ADD CONSTRAINT jobs_discard_only_dead CHECK (discarded_at IS NULL OR status = 'DEAD');

ALTER TABLE outbox_events
    ADD COLUMN discarded_at TIMESTAMPTZ,
    ADD COLUMN discarded_by UUID,
    ADD CONSTRAINT outbox_events_discard_only_dead CHECK (discarded_at IS NULL OR status = 'DEAD');

ALTER TABLE ai_jobs
    ADD COLUMN discarded_at TIMESTAMPTZ,
    ADD COLUMN discarded_by UUID,
    ADD CONSTRAINT ai_jobs_discard_only_dead CHECK (discarded_at IS NULL OR status = 'DEAD');

ALTER TABLE notification_deliveries
    ADD COLUMN discarded_at TIMESTAMPTZ,
    ADD COLUMN discarded_by UUID,
    ADD CONSTRAINT notification_deliveries_discard_only_dead CHECK (discarded_at IS NULL OR status = 'DEAD');
