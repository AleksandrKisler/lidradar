CREATE TABLE risk_signals (
    id UUID PRIMARY KEY,
    tenant_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    opportunity_id UUID NOT NULL,
    location_id UUID NOT NULL,
    type TEXT NOT NULL CHECK (type IN (
        'NO_RESPONSE', 'BOOKING_NOT_CONFIRMED', 'PROMISE_NOT_FULFILLED',
        'CUSTOMER_SILENT_AFTER_PRICE', 'FOLLOW_UP_CANDIDATE'
    )),
    severity TEXT NOT NULL CHECK (severity IN ('LOW', 'MEDIUM', 'HIGH', 'CRITICAL')),
    status TEXT NOT NULL CHECK (status IN (
        'OPEN', 'ACKNOWLEDGED', 'ACTED', 'RESOLVED',
        'FALSE_POSITIVE', 'IGNORED', 'EXPIRED'
    )),
    reason_code TEXT NOT NULL,
    reason_text TEXT NOT NULL,
    confidence NUMERIC(4,3),
    source TEXT NOT NULL CHECK (source IN ('RULE', 'HYBRID', 'MANUAL')),
    risk_engine_version TEXT NOT NULL,
    ai_run_id UUID,
    trigger_message_id UUID NOT NULL,
    detected_at TIMESTAMPTZ NOT NULL,
    due_at TIMESTAMPTZ NOT NULL,
    acknowledged_at TIMESTAMPTZ,
    acted_at TIMESTAMPTZ,
    resolved_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT risk_signals_tenant_id_unique UNIQUE (tenant_id, id),
    CONSTRAINT risk_signals_opportunity_fk
        FOREIGN KEY (tenant_id, opportunity_id)
        REFERENCES opportunities(tenant_id, id),
    CONSTRAINT risk_signals_location_fk
        FOREIGN KEY (tenant_id, location_id)
        REFERENCES locations(tenant_id, id),
    CONSTRAINT risk_signals_trigger_message_fk
        FOREIGN KEY (tenant_id, trigger_message_id)
        REFERENCES messages(tenant_id, id),
    CONSTRAINT risk_signals_reason_code_valid CHECK (
        reason_code = btrim(reason_code) AND reason_code ~ '^[A-Z][A-Z0-9_]{0,99}$'
    ),
    CONSTRAINT risk_signals_reason_text_valid CHECK (
        reason_text = btrim(reason_text) AND char_length(reason_text) BETWEEN 1 AND 2000
    ),
    CONSTRAINT risk_signals_engine_version_valid CHECK (
        risk_engine_version = btrim(risk_engine_version)
        AND char_length(risk_engine_version) BETWEEN 1 AND 100
    ),
    CONSTRAINT risk_signals_confidence_valid CHECK (
        confidence IS NULL OR confidence BETWEEN 0 AND 1
    ),
    CONSTRAINT risk_signals_rule_without_ai CHECK (
        source <> 'RULE' OR (confidence IS NULL AND ai_run_id IS NULL)
    ),
    CONSTRAINT risk_signals_lifecycle_consistent CHECK (
        (status = 'OPEN' AND acknowledged_at IS NULL AND acted_at IS NULL AND resolved_at IS NULL)
        OR (status = 'ACKNOWLEDGED' AND acknowledged_at IS NOT NULL AND acted_at IS NULL AND resolved_at IS NULL)
        OR (status = 'ACTED' AND acknowledged_at IS NOT NULL AND acted_at IS NOT NULL AND resolved_at IS NULL)
        OR (status IN ('RESOLVED', 'FALSE_POSITIVE', 'IGNORED', 'EXPIRED') AND resolved_at IS NOT NULL)
    ),
    CONSTRAINT risk_signals_due_not_after_detection CHECK (due_at <= detected_at)
);

CREATE UNIQUE INDEX risk_signals_one_active_type_idx
    ON risk_signals(tenant_id, opportunity_id, type)
    WHERE status IN ('OPEN', 'ACKNOWLEDGED', 'ACTED');

CREATE INDEX risk_signals_radar_idx
    ON risk_signals(tenant_id, status, severity, detected_at, id);

CREATE INDEX risk_signals_opportunity_timeline_idx
    ON risk_signals(tenant_id, opportunity_id, detected_at DESC, id DESC);
