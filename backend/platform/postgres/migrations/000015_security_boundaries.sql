-- Ограничение частоты аутентификации разделяется всеми экземплярами API.
-- В базе хранятся только необратимые SHA-256 отпечатки адресов и учётных
-- записей, поэтому таблица не становится новым источником персональных данных.
CREATE TABLE auth_rate_limits (
    scope TEXT NOT NULL CHECK (scope IN ('REGISTER_IP', 'LOGIN_IP', 'LOGIN_ACCOUNT', 'REFRESH_IP')),
    subject_hash BYTEA NOT NULL CHECK (octet_length(subject_hash) = 32),
    attempts BIGINT NOT NULL CHECK (attempts > 0),
    window_started_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (scope, subject_hash),
    CONSTRAINT auth_rate_limits_window_valid CHECK (expires_at > window_started_at)
);

CREATE INDEX auth_rate_limits_expiry_idx ON auth_rate_limits(expires_at);

-- AI-узел получает только задания явно разрешённых организаций. Таблица
-- допускает несколько явных назначений, но отсутствие строк означает полный
-- запрет. Исторические связи восстанавливаются только ради целостности уже
-- существующих запусков и действующих аренд.
CREATE TABLE ai_node_tenants (
    node_id UUID NOT NULL REFERENCES ai_nodes(id) ON DELETE CASCADE,
    tenant_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (node_id, tenant_id)
);

INSERT INTO ai_node_tenants(node_id, tenant_id, created_at)
SELECT node_id, tenant_id, MIN(started_at)
FROM ai_runs
GROUP BY node_id, tenant_id
ON CONFLICT DO NOTHING;

INSERT INTO ai_node_tenants(node_id, tenant_id, created_at)
SELECT leased_by, tenant_id, MIN(updated_at)
FROM ai_jobs
WHERE leased_by IS NOT NULL
GROUP BY leased_by, tenant_id
ON CONFLICT DO NOTHING;

CREATE INDEX ai_node_tenants_tenant_idx ON ai_node_tenants(tenant_id, node_id);

ALTER TABLE ai_jobs
    ADD CONSTRAINT ai_jobs_leased_node_tenant_fk
    FOREIGN KEY (leased_by, tenant_id)
    REFERENCES ai_node_tenants(node_id, tenant_id);

ALTER TABLE ai_runs
    ADD CONSTRAINT ai_runs_node_tenant_fk
    FOREIGN KEY (node_id, tenant_id)
    REFERENCES ai_node_tenants(node_id, tenant_id);

CREATE INDEX ai_jobs_tenant_claim_idx
    ON ai_jobs(tenant_id, priority DESC, available_at, created_at, id)
    WHERE status IN ('PENDING', 'RETRY', 'LEASED', 'RUNNING');
