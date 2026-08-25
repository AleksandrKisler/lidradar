CREATE TABLE jobs (
    id UUID PRIMARY KEY,
    tenant_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    job_type TEXT NOT NULL,
    dedup_key TEXT NOT NULL,
    payload JSONB NOT NULL,
    status TEXT NOT NULL CHECK (
        status IN ('PENDING', 'PROCESSING', 'RETRY', 'SUCCEEDED', 'DEAD')
    ),
    priority SMALLINT NOT NULL DEFAULT 0,
    available_at TIMESTAMPTZ NOT NULL,
    attempt_count SMALLINT NOT NULL DEFAULT 0,
    max_attempts SMALLINT NOT NULL DEFAULT 5,
    lease_owner TEXT,
    lease_until TIMESTAMPTZ,
    last_error_code TEXT,
    completed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT jobs_tenant_id_unique UNIQUE (tenant_id, id),
    CONSTRAINT jobs_dedup_unique UNIQUE (tenant_id, job_type, dedup_key),
    CONSTRAINT jobs_type_valid CHECK (
        job_type = btrim(job_type) AND char_length(job_type) BETWEEN 1 AND 100
    ),
    CONSTRAINT jobs_dedup_key_valid CHECK (
        dedup_key = btrim(dedup_key) AND char_length(dedup_key) BETWEEN 1 AND 512
    ),
    CONSTRAINT jobs_payload_object CHECK (jsonb_typeof(payload) = 'object'),
    CONSTRAINT jobs_attempts_valid CHECK (
        max_attempts BETWEEN 1 AND 20
        AND attempt_count BETWEEN 0 AND max_attempts
    ),
    CONSTRAINT jobs_error_code_valid CHECK (
        last_error_code IS NULL
        OR last_error_code ~ '^[A-Z][A-Z0-9_]{0,99}$'
    ),
    CONSTRAINT jobs_lifecycle_consistent CHECK (
        (status = 'PROCESSING' AND lease_owner IS NOT NULL AND lease_until IS NOT NULL AND completed_at IS NULL)
        OR
        (status IN ('PENDING', 'RETRY') AND lease_owner IS NULL AND lease_until IS NULL AND completed_at IS NULL)
        OR
        (status IN ('SUCCEEDED', 'DEAD') AND lease_owner IS NULL AND lease_until IS NULL AND completed_at IS NOT NULL)
    )
);

CREATE INDEX jobs_claim_idx
    ON jobs(priority DESC, available_at, created_at, id)
    WHERE status IN ('PENDING', 'RETRY');

CREATE INDEX jobs_expired_lease_idx
    ON jobs(lease_until, id)
    WHERE status = 'PROCESSING';

CREATE INDEX jobs_tenant_status_idx
    ON jobs(tenant_id, status, created_at, id);

CREATE TABLE scheduled_checks (
    id UUID PRIMARY KEY,
    tenant_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    check_type TEXT NOT NULL,
    subject_type TEXT NOT NULL,
    subject_id UUID NOT NULL,
    job_type TEXT NOT NULL,
    dedup_key TEXT NOT NULL,
    payload JSONB NOT NULL,
    due_at TIMESTAMPTZ NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('SCHEDULED', 'ENQUEUED', 'CANCELLED')),
    job_id UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT scheduled_checks_tenant_id_unique UNIQUE (tenant_id, id),
    CONSTRAINT scheduled_checks_dedup_unique UNIQUE (tenant_id, check_type, dedup_key),
    CONSTRAINT scheduled_checks_job_fk
        FOREIGN KEY (tenant_id, job_id) REFERENCES jobs(tenant_id, id),
    CONSTRAINT scheduled_checks_names_valid CHECK (
        check_type = btrim(check_type) AND char_length(check_type) BETWEEN 1 AND 100
        AND subject_type = btrim(subject_type) AND char_length(subject_type) BETWEEN 1 AND 100
        AND job_type = btrim(job_type) AND char_length(job_type) BETWEEN 1 AND 100
        AND dedup_key = btrim(dedup_key) AND char_length(dedup_key) BETWEEN 1 AND 512
    ),
    CONSTRAINT scheduled_checks_payload_object CHECK (jsonb_typeof(payload) = 'object'),
    CONSTRAINT scheduled_checks_lifecycle_consistent CHECK (
        (status = 'ENQUEUED' AND job_id IS NOT NULL)
        OR
        (status IN ('SCHEDULED', 'CANCELLED') AND job_id IS NULL)
    )
);

CREATE INDEX scheduled_checks_due_idx
    ON scheduled_checks(due_at, created_at, id)
    WHERE status = 'SCHEDULED';

CREATE TABLE outbox_events (
    id UUID PRIMARY KEY,
    tenant_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    event_type TEXT NOT NULL,
    event_version INTEGER NOT NULL,
    aggregate_type TEXT NOT NULL,
    aggregate_id UUID NOT NULL,
    trace_id UUID NOT NULL,
    data JSONB NOT NULL,
    status TEXT NOT NULL CHECK (
        status IN ('PENDING', 'PROCESSING', 'RETRY', 'PUBLISHED', 'DEAD')
    ),
    available_at TIMESTAMPTZ NOT NULL,
    attempt_count SMALLINT NOT NULL DEFAULT 0,
    max_attempts SMALLINT NOT NULL DEFAULT 5,
    lease_owner TEXT,
    lease_until TIMESTAMPTZ,
    last_error_code TEXT,
    occurred_at TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT outbox_events_tenant_id_unique UNIQUE (tenant_id, id),
    CONSTRAINT outbox_events_name_valid CHECK (
        event_type = btrim(event_type) AND char_length(event_type) BETWEEN 1 AND 100
        AND aggregate_type = btrim(aggregate_type) AND char_length(aggregate_type) BETWEEN 1 AND 100
    ),
    CONSTRAINT outbox_events_version_valid CHECK (event_version > 0),
    CONSTRAINT outbox_events_data_object CHECK (jsonb_typeof(data) = 'object'),
    CONSTRAINT outbox_events_attempts_valid CHECK (
        max_attempts BETWEEN 1 AND 20
        AND attempt_count BETWEEN 0 AND max_attempts
    ),
    CONSTRAINT outbox_events_error_code_valid CHECK (
        last_error_code IS NULL
        OR last_error_code ~ '^[A-Z][A-Z0-9_]{0,99}$'
    ),
    CONSTRAINT outbox_events_lifecycle_consistent CHECK (
        (status = 'PROCESSING' AND lease_owner IS NOT NULL AND lease_until IS NOT NULL AND completed_at IS NULL)
        OR
        (status IN ('PENDING', 'RETRY') AND lease_owner IS NULL AND lease_until IS NULL AND completed_at IS NULL)
        OR
        (status IN ('PUBLISHED', 'DEAD') AND lease_owner IS NULL AND lease_until IS NULL AND completed_at IS NOT NULL)
    )
);

CREATE INDEX outbox_events_claim_idx
    ON outbox_events(available_at, occurred_at, id)
    WHERE status IN ('PENDING', 'RETRY');

CREATE INDEX outbox_events_expired_lease_idx
    ON outbox_events(lease_until, id)
    WHERE status = 'PROCESSING';

-- Ожидающие записи этапа 4 становятся долговечными версионированными событиями
-- до удаления временной таблицы. Диспетчер создаст одно задание без повторов.
INSERT INTO outbox_events(
    id, tenant_id, event_type, event_version, aggregate_type, aggregate_id,
    trace_id, data, status, available_at, occurred_at, created_at, updated_at
)
SELECT
    work.id,
    work.tenant_id,
    'connector.raw-event.received',
    1,
    'raw_event',
    work.raw_event_id,
    work.raw_event_id,
    jsonb_build_object('rawEventId', work.raw_event_id::text),
    'PENDING',
    work.created_at,
    work.created_at,
    work.created_at,
    work.created_at
FROM raw_event_normalization_work AS work;

DROP TABLE raw_event_normalization_work;
