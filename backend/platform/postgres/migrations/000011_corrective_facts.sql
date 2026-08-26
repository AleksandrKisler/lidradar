CREATE TABLE recommendations (
    id UUID PRIMARY KEY,
    tenant_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    risk_id UUID NOT NULL,
    text TEXT NOT NULL,
    source TEXT NOT NULL CHECK (source IN ('TEMPLATE')),
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT recommendations_tenant_id_unique UNIQUE (tenant_id, id),
    CONSTRAINT recommendations_risk_fk
        FOREIGN KEY (tenant_id, risk_id)
        REFERENCES risk_signals(tenant_id, id) ON DELETE CASCADE,
    CONSTRAINT recommendations_template_unique UNIQUE (tenant_id, risk_id, source),
    CONSTRAINT recommendations_text_valid CHECK (
        text = btrim(text) AND char_length(text) BETWEEN 1 AND 2000
    )
);

CREATE INDEX recommendations_risk_timeline_idx
    ON recommendations(tenant_id, risk_id, created_at, id);

CREATE TABLE actions (
    id UUID PRIMARY KEY,
    tenant_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    risk_id UUID NOT NULL,
    actor_user_id UUID NOT NULL,
    type TEXT NOT NULL CHECK (type IN (
        'OPEN_CONVERSATION', 'COPY_REPLY', 'MARK_CONTACTED',
        'CALL', 'SEND_MESSAGE', 'OTHER'
    )),
    note TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT actions_tenant_id_unique UNIQUE (tenant_id, id),
    CONSTRAINT actions_risk_fk
        FOREIGN KEY (tenant_id, risk_id)
        REFERENCES risk_signals(tenant_id, id),
    CONSTRAINT actions_actor_membership_fk
        FOREIGN KEY (tenant_id, actor_user_id)
        REFERENCES memberships(tenant_id, user_id),
    CONSTRAINT actions_note_valid CHECK (
        note = btrim(note) AND char_length(note) <= 2000
    )
);

CREATE INDEX actions_risk_timeline_idx
    ON actions(tenant_id, risk_id, created_at, id);

CREATE TABLE outcomes (
    id UUID PRIMARY KEY,
    tenant_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    opportunity_id UUID NOT NULL,
    actor_user_id UUID NOT NULL,
    status TEXT NOT NULL CHECK (status IN (
        'RESPONDED', 'BOOKED', 'PAID', 'LOST', 'THINKING', 'NOT_A_LEAD'
    )),
    note TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT outcomes_tenant_id_unique UNIQUE (tenant_id, id),
    CONSTRAINT outcomes_opportunity_fk
        FOREIGN KEY (tenant_id, opportunity_id)
        REFERENCES opportunities(tenant_id, id),
    CONSTRAINT outcomes_actor_membership_fk
        FOREIGN KEY (tenant_id, actor_user_id)
        REFERENCES memberships(tenant_id, user_id),
    CONSTRAINT outcomes_note_valid CHECK (
        note = btrim(note) AND char_length(note) <= 2000
    )
);

CREATE INDEX outcomes_opportunity_timeline_idx
    ON outcomes(tenant_id, opportunity_id, created_at DESC, id DESC);

CREATE TABLE idempotency_keys (
    tenant_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    key TEXT NOT NULL,
    operation TEXT NOT NULL,
    request_hash BYTEA NOT NULL,
    response_status SMALLINT NOT NULL CHECK (response_status BETWEEN 200 AND 599),
    response_body JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, key, operation),
    CONSTRAINT idempotency_keys_key_valid CHECK (
        key = btrim(key) AND char_length(key) BETWEEN 1 AND 255
    ),
    CONSTRAINT idempotency_keys_operation_valid CHECK (
        operation = btrim(operation) AND char_length(operation) BETWEEN 1 AND 100
    ),
    CONSTRAINT idempotency_keys_hash_valid CHECK (octet_length(request_hash) = 32)
);

CREATE INDEX idempotency_keys_created_at_idx
    ON idempotency_keys(created_at, tenant_id);

CREATE TABLE audit_log (
    id UUID PRIMARY KEY,
    tenant_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    actor_user_id UUID NOT NULL,
    operation TEXT NOT NULL,
    entity_type TEXT NOT NULL,
    entity_id UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT audit_log_tenant_id_unique UNIQUE (tenant_id, id),
    CONSTRAINT audit_log_actor_membership_fk
        FOREIGN KEY (tenant_id, actor_user_id)
        REFERENCES memberships(tenant_id, user_id),
    CONSTRAINT audit_log_operation_valid CHECK (
        operation = btrim(operation) AND operation ~ '^[A-Z][A-Z0-9_]{0,99}$'
    ),
    CONSTRAINT audit_log_entity_type_valid CHECK (
        entity_type = btrim(entity_type) AND entity_type ~ '^[A-Z][A-Z0-9_]{0,99}$'
    )
);

CREATE INDEX audit_log_tenant_timeline_idx
    ON audit_log(tenant_id, created_at DESC, id DESC);

CREATE FUNCTION reject_append_only_mutation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'неизменяемую запись журнала нельзя изменить или удалить'
        USING ERRCODE = '55000';
END;
$$;

CREATE TRIGGER actions_append_only
    BEFORE UPDATE OR DELETE ON actions
    FOR EACH ROW EXECUTE FUNCTION reject_append_only_mutation();

CREATE TRIGGER outcomes_append_only
    BEFORE UPDATE OR DELETE ON outcomes
    FOR EACH ROW EXECUTE FUNCTION reject_append_only_mutation();

CREATE TRIGGER idempotency_keys_append_only
    BEFORE UPDATE OR DELETE ON idempotency_keys
    FOR EACH ROW EXECUTE FUNCTION reject_append_only_mutation();

CREATE TRIGGER audit_log_append_only
    BEFORE UPDATE OR DELETE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION reject_append_only_mutation();
