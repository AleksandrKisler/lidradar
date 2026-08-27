-- Этап 13: домашний AI-узел остаётся заменяемым вычислительным ресурсом,
-- а PostgreSQL хранит авторитетные узлы, задания, попытки и производные итоги.

CREATE TABLE ai_nodes (
    id UUID PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    secret_digest BYTEA NOT NULL CHECK (octet_length(secret_digest) = 32),
    status TEXT NOT NULL CHECK (status IN ('OFFLINE', 'READY', 'REVOKED')),
    model_version TEXT,
    available_slots SMALLINT NOT NULL DEFAULT 0 CHECK (available_slots BETWEEN 0 AND 1),
    max_inflight SMALLINT NOT NULL DEFAULT 1 CHECK (max_inflight = 1),
    last_heartbeat_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT ai_nodes_name_valid CHECK (
        name = btrim(name) AND char_length(name) BETWEEN 1 AND 100
    ),
    CONSTRAINT ai_nodes_model_version_valid CHECK (
        model_version IS NULL OR (model_version = btrim(model_version) AND char_length(model_version) BETWEEN 1 AND 200)
    ),
    CONSTRAINT ai_nodes_revocation_valid CHECK (
        (status = 'REVOKED' AND revoked_at IS NOT NULL AND available_slots = 0)
        OR (status <> 'REVOKED' AND revoked_at IS NULL)
    )
);

CREATE INDEX ai_nodes_heartbeat_idx
    ON ai_nodes(status, last_heartbeat_at DESC NULLS LAST);

CREATE TABLE ai_jobs (
    id UUID PRIMARY KEY,
    tenant_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    job_type TEXT NOT NULL CHECK (job_type IN ('ANALYZE_CONVERSATION')),
    entity_type TEXT NOT NULL CHECK (entity_type IN ('CONVERSATION')),
    entity_id UUID NOT NULL,
    priority SMALLINT NOT NULL DEFAULT 0 CHECK (priority BETWEEN -100 AND 100),
    payload JSONB NOT NULL,
    model_requirement TEXT NOT NULL,
    schema_version TEXT NOT NULL,
    prompt_version TEXT NOT NULL,
    base_conversation_revision BIGINT NOT NULL CHECK (base_conversation_revision > 0),
    analysis_through_message_id UUID NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('PENDING', 'LEASED', 'RUNNING', 'SUCCEEDED', 'RETRY', 'DEAD')),
    attempts SMALLINT NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 5),
    max_attempts SMALLINT NOT NULL DEFAULT 5 CHECK (max_attempts BETWEEN 1 AND 5),
    available_at TIMESTAMPTZ NOT NULL,
    leased_by UUID REFERENCES ai_nodes(id),
    lease_until TIMESTAMPTZ,
    last_error_code TEXT,
    completed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT ai_jobs_tenant_id_unique UNIQUE (tenant_id, id),
    CONSTRAINT ai_jobs_conversation_fk
        FOREIGN KEY (tenant_id, entity_id)
        REFERENCES conversations(tenant_id, id),
    CONSTRAINT ai_jobs_message_fk
        FOREIGN KEY (tenant_id, analysis_through_message_id, entity_id)
        REFERENCES messages(tenant_id, id, conversation_id),
    CONSTRAINT ai_jobs_snapshot_unique UNIQUE (
        tenant_id, job_type, entity_type, entity_id,
        base_conversation_revision, model_requirement, schema_version, prompt_version
    ),
    CONSTRAINT ai_jobs_payload_object CHECK (jsonb_typeof(payload) = 'object'),
    CONSTRAINT ai_jobs_versions_valid CHECK (
        model_requirement = btrim(model_requirement) AND char_length(model_requirement) BETWEEN 1 AND 200
        AND schema_version = btrim(schema_version) AND char_length(schema_version) BETWEEN 1 AND 200
        AND prompt_version = btrim(prompt_version) AND char_length(prompt_version) BETWEEN 1 AND 200
    ),
    CONSTRAINT ai_jobs_error_code_valid CHECK (
        last_error_code IS NULL OR last_error_code ~ '^[A-Z0-9_]{1,100}$'
    ),
    CONSTRAINT ai_jobs_lease_valid CHECK (
        (status IN ('LEASED', 'RUNNING') AND leased_by IS NOT NULL AND lease_until IS NOT NULL AND completed_at IS NULL)
        OR
        (status IN ('PENDING', 'RETRY') AND leased_by IS NULL AND lease_until IS NULL AND completed_at IS NULL)
        OR
        (status IN ('SUCCEEDED', 'DEAD') AND leased_by IS NULL AND lease_until IS NULL AND completed_at IS NOT NULL)
    )
);

CREATE INDEX ai_jobs_claim_idx
    ON ai_jobs(priority DESC, available_at, created_at, id)
    WHERE status IN ('PENDING', 'RETRY', 'LEASED', 'RUNNING');

CREATE INDEX ai_jobs_tenant_entity_idx
    ON ai_jobs(tenant_id, entity_type, entity_id, created_at DESC);

CREATE INDEX ai_jobs_lease_idx
    ON ai_jobs(lease_until)
    WHERE status IN ('LEASED', 'RUNNING');

CREATE TABLE ai_runs (
    id UUID PRIMARY KEY,
    tenant_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    job_id UUID NOT NULL,
    node_id UUID NOT NULL REFERENCES ai_nodes(id),
    entity_type TEXT NOT NULL CHECK (entity_type IN ('CONVERSATION')),
    entity_id UUID NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('RUNNING', 'SUCCEEDED', 'FAILED')),
    application_status TEXT NOT NULL CHECK (application_status IN ('PENDING', 'APPLIED', 'STALE', 'REJECTED')),
    base_conversation_revision BIGINT NOT NULL CHECK (base_conversation_revision > 0),
    analysis_through_message_id UUID NOT NULL,
    model_version TEXT NOT NULL,
    prompt_version TEXT NOT NULL,
    schema_version TEXT NOT NULL,
    raw_output TEXT,
    error_code TEXT,
    validation_error TEXT,
    started_at TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ,
    CONSTRAINT ai_runs_tenant_id_unique UNIQUE (tenant_id, id),
    CONSTRAINT ai_runs_job_fk
        FOREIGN KEY (tenant_id, job_id)
        REFERENCES ai_jobs(tenant_id, id),
    CONSTRAINT ai_runs_conversation_fk
        FOREIGN KEY (tenant_id, entity_id)
        REFERENCES conversations(tenant_id, id),
    CONSTRAINT ai_runs_message_fk
        FOREIGN KEY (tenant_id, analysis_through_message_id, entity_id)
        REFERENCES messages(tenant_id, id, conversation_id),
    CONSTRAINT ai_runs_completion_valid CHECK (
        (status = 'RUNNING' AND application_status = 'PENDING' AND completed_at IS NULL AND raw_output IS NULL AND error_code IS NULL)
        OR
        (status = 'SUCCEEDED' AND application_status IN ('APPLIED', 'STALE', 'REJECTED') AND completed_at IS NOT NULL AND raw_output IS NOT NULL AND error_code IS NULL)
        OR
        (status = 'FAILED' AND application_status = 'REJECTED' AND completed_at IS NOT NULL AND raw_output IS NULL AND error_code IS NOT NULL)
    ),
    CONSTRAINT ai_runs_error_code_valid CHECK (
        error_code IS NULL OR error_code ~ '^[A-Z0-9_]{1,100}$'
    )
);

CREATE UNIQUE INDEX ai_runs_active_job_idx
    ON ai_runs(job_id)
    WHERE status = 'RUNNING';

CREATE INDEX ai_runs_tenant_entity_idx
    ON ai_runs(tenant_id, entity_type, entity_id, started_at DESC);

CREATE INDEX ai_runs_node_timeline_idx
    ON ai_runs(node_id, started_at DESC);

CREATE TABLE conversation_summaries (
    tenant_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    conversation_id UUID NOT NULL,
    summary_text TEXT NOT NULL,
    base_conversation_revision BIGINT NOT NULL CHECK (base_conversation_revision > 0),
    analysis_through_message_id UUID NOT NULL,
    model_version TEXT NOT NULL,
    prompt_version TEXT NOT NULL,
    schema_version TEXT NOT NULL,
    ai_run_id UUID NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, conversation_id),
    CONSTRAINT conversation_summaries_conversation_fk
        FOREIGN KEY (tenant_id, conversation_id)
        REFERENCES conversations(tenant_id, id),
    CONSTRAINT conversation_summaries_message_fk
        FOREIGN KEY (tenant_id, analysis_through_message_id, conversation_id)
        REFERENCES messages(tenant_id, id, conversation_id),
    CONSTRAINT conversation_summaries_run_fk
        FOREIGN KEY (tenant_id, ai_run_id)
        REFERENCES ai_runs(tenant_id, id),
    CONSTRAINT conversation_summaries_text_valid CHECK (
        summary_text = btrim(summary_text) AND char_length(summary_text) BETWEEN 1 AND 2000
    )
);

CREATE TABLE ai_node_request_nonces (
    node_id UUID NOT NULL REFERENCES ai_nodes(id) ON DELETE CASCADE,
    request_id UUID NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (node_id, request_id),
    CONSTRAINT ai_node_request_nonce_expiry CHECK (expires_at > created_at)
);

CREATE INDEX ai_node_request_nonces_expiry_idx
    ON ai_node_request_nonces(expires_at);
