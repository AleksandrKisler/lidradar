-- Этап 24: аудит входа и выхода пользователей (ТЗ §65, LR-BE-2406). События
-- входа не принадлежат организации, поэтому живут отдельно от audit_log.
CREATE TABLE auth_audit_log (
    id UUID PRIMARY KEY,
    user_id UUID NOT NULL REFERENCES users(id),
    operation TEXT NOT NULL,
    ip_address TEXT,
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT auth_audit_log_operation_valid CHECK (
        operation = btrim(operation) AND operation ~ '^[A-Z][A-Z0-9_]{0,99}$'
    ),
    CONSTRAINT auth_audit_log_ip_valid CHECK (ip_address IS NULL OR char_length(ip_address) BETWEEN 1 AND 64)
);

CREATE INDEX auth_audit_log_user_timeline_idx
    ON auth_audit_log(user_id, created_at DESC, id DESC);

CREATE TRIGGER auth_audit_log_append_only
    BEFORE UPDATE OR DELETE ON auth_audit_log
    FOR EACH ROW EXECUTE FUNCTION reject_append_only_mutation();
