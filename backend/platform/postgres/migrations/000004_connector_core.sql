CREATE TABLE channel_connections (
    id UUID PRIMARY KEY,
    tenant_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    location_id UUID,
    provider TEXT NOT NULL CHECK (
        provider IN ('TEST', 'IMPORT', 'GENERIC_WEBHOOK', 'CONNECTED_BUSINESS_BOT')
    ),
    name TEXT NOT NULL,
    status TEXT NOT NULL CHECK (
        status IN ('ACTIVE', 'DEGRADED', 'ERROR', 'DISCONNECTED')
    ),
    capabilities JSONB NOT NULL DEFAULT '[]'::jsonb,
    encrypted_credentials BYTEA,
    verification_secret_hash CHAR(64) NOT NULL,
    last_event_at TIMESTAMPTZ,
    last_success_at TIMESTAMPTZ,
    last_error_at TIMESTAMPTZ,
    last_error_code TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT channel_connections_tenant_id_unique UNIQUE (tenant_id, id),
    CONSTRAINT channel_connections_tenant_provider_unique UNIQUE (tenant_id, id, provider),
    CONSTRAINT channel_connections_location_fk
        FOREIGN KEY (tenant_id, location_id)
        REFERENCES locations(tenant_id, id),
    CONSTRAINT channel_connections_name_valid CHECK (
        name = btrim(name) AND char_length(name) BETWEEN 1 AND 200
    ),
    CONSTRAINT channel_connections_capabilities_valid CHECK (
        jsonb_typeof(capabilities) = 'array' AND jsonb_array_length(capabilities) > 0
    ),
    CONSTRAINT channel_connections_secret_hash_valid CHECK (
        verification_secret_hash ~ '^[0-9a-f]{64}$'
    ),
    CONSTRAINT channel_connections_error_code_valid CHECK (
        last_error_code IS NULL OR char_length(last_error_code) BETWEEN 1 AND 100
    )
);

CREATE INDEX channel_connections_tenant_idx
    ON channel_connections(tenant_id, created_at, id);

CREATE INDEX channel_connections_health_idx
    ON channel_connections(status, last_success_at);

CREATE TABLE raw_events (
    id UUID PRIMARY KEY,
    tenant_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    connection_id UUID NOT NULL,
    provider TEXT NOT NULL,
    external_event_id TEXT NOT NULL,
    payload JSONB NOT NULL,
    payload_hash CHAR(64) NOT NULL,
    status TEXT NOT NULL CHECK (
        status IN ('RECEIVED', 'PROCESSING', 'PROCESSED', 'FAILED')
    ),
    error_code TEXT,
    received_at TIMESTAMPTZ NOT NULL,
    processed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT raw_events_tenant_id_unique UNIQUE (tenant_id, id),
    CONSTRAINT raw_events_tenant_connection_unique UNIQUE (tenant_id, id, connection_id),
    CONSTRAINT raw_events_connection_provider_fk
        FOREIGN KEY (tenant_id, connection_id, provider)
        REFERENCES channel_connections(tenant_id, id, provider),
    CONSTRAINT raw_event_external_unique UNIQUE (connection_id, external_event_id),
    CONSTRAINT raw_events_external_id_valid CHECK (
        external_event_id = btrim(external_event_id)
        AND char_length(external_event_id) BETWEEN 1 AND 512
    ),
    CONSTRAINT raw_events_payload_hash_valid CHECK (payload_hash ~ '^[0-9a-f]{64}$'),
    CONSTRAINT raw_events_failure_consistent CHECK (
        (status = 'FAILED' AND error_code IS NOT NULL AND processed_at IS NOT NULL)
        OR
        (status <> 'FAILED' AND error_code IS NULL)
    )
);

CREATE INDEX raw_events_processing_idx
    ON raw_events(status, received_at);

CREATE INDEX raw_events_tenant_time_idx
    ON raw_events(tenant_id, received_at DESC, id);

-- Stage 4 owns only the durable handoff record. Stage 6 will provide the
-- generic leased jobs runtime that consumes this pending work.
CREATE TABLE raw_event_normalization_work (
    id UUID PRIMARY KEY,
    tenant_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    connection_id UUID NOT NULL,
    raw_event_id UUID NOT NULL,
    status TEXT NOT NULL CHECK (status = 'PENDING'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT normalization_work_connection_fk
        FOREIGN KEY (tenant_id, connection_id)
        REFERENCES channel_connections(tenant_id, id),
    CONSTRAINT normalization_work_raw_event_fk
        FOREIGN KEY (tenant_id, raw_event_id, connection_id)
        REFERENCES raw_events(tenant_id, id, connection_id)
        ON DELETE CASCADE,
    CONSTRAINT normalization_work_raw_event_unique UNIQUE (raw_event_id)
);

CREATE INDEX normalization_work_pending_idx
    ON raw_event_normalization_work(status, created_at, id);
