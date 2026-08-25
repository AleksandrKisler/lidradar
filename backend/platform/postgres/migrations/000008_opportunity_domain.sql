CREATE TABLE opportunities (
    id UUID PRIMARY KEY,
    tenant_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    conversation_id UUID NOT NULL,
    service_id UUID,
    stage TEXT NOT NULL CHECK (stage IN (
        'NEW', 'ENGAGED', 'QUALIFYING', 'PRICE_SENT', 'WAITING_CUSTOMER',
        'WAITING_BUSINESS', 'BOOKING_INTENT', 'BOOKED', 'WON', 'LOST', 'ARCHIVED'
    )),
    estimated_amount NUMERIC(14,2),
    estimated_amount_confidence NUMERIC(4,3),
    currency CHAR(3) NOT NULL DEFAULT 'RUB',
    opened_at TIMESTAMPTZ NOT NULL,
    closed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT opportunities_tenant_id_unique UNIQUE (tenant_id, id),
    CONSTRAINT opportunities_conversation_fk
        FOREIGN KEY (tenant_id, conversation_id)
        REFERENCES conversations(tenant_id, id),
    CONSTRAINT opportunities_service_fk
        FOREIGN KEY (tenant_id, service_id)
        REFERENCES service_catalog_items(tenant_id, id),
    CONSTRAINT opportunities_amount_nonnegative CHECK (
        estimated_amount IS NULL OR estimated_amount >= 0
    ),
    CONSTRAINT opportunities_confidence_valid CHECK (
        estimated_amount_confidence IS NULL
        OR (estimated_amount IS NOT NULL AND estimated_amount_confidence BETWEEN 0 AND 1)
    ),
    CONSTRAINT opportunities_currency_valid CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT opportunities_lifecycle_consistent CHECK (
        (stage NOT IN ('WON', 'LOST', 'ARCHIVED') AND closed_at IS NULL)
        OR (stage IN ('WON', 'LOST', 'ARCHIVED') AND closed_at IS NOT NULL)
    )
);

CREATE UNIQUE INDEX opportunities_one_active_per_conversation_idx
    ON opportunities(tenant_id, conversation_id)
    WHERE stage NOT IN ('WON', 'LOST', 'ARCHIVED');

CREATE INDEX opportunities_tenant_stage_updated_idx
    ON opportunities(tenant_id, stage, updated_at DESC, id DESC);

CREATE INDEX opportunities_tenant_conversation_opened_idx
    ON opportunities(tenant_id, conversation_id, opened_at DESC, id DESC);

CREATE TABLE opportunity_stage_history (
    id UUID PRIMARY KEY,
    tenant_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    opportunity_id UUID NOT NULL,
    from_stage TEXT CHECK (from_stage IS NULL OR from_stage IN (
        'NEW', 'ENGAGED', 'QUALIFYING', 'PRICE_SENT', 'WAITING_CUSTOMER',
        'WAITING_BUSINESS', 'BOOKING_INTENT', 'BOOKED', 'WON', 'LOST', 'ARCHIVED'
    )),
    to_stage TEXT NOT NULL CHECK (to_stage IN (
        'NEW', 'ENGAGED', 'QUALIFYING', 'PRICE_SENT', 'WAITING_CUSTOMER',
        'WAITING_BUSINESS', 'BOOKING_INTENT', 'BOOKED', 'WON', 'LOST', 'ARCHIVED'
    )),
    source TEXT NOT NULL CHECK (source IN ('RULE', 'AI', 'USER', 'IMPORT')),
    confidence NUMERIC(4,3),
    ai_run_id UUID,
    actor_user_id UUID REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT opportunity_stage_history_tenant_id_unique UNIQUE (tenant_id, id),
    CONSTRAINT opportunity_stage_history_opportunity_fk
        FOREIGN KEY (tenant_id, opportunity_id)
        REFERENCES opportunities(tenant_id, id)
        ON DELETE CASCADE,
    CONSTRAINT opportunity_stage_history_transition_valid CHECK (
        from_stage IS NULL OR from_stage <> to_stage
    ),
    CONSTRAINT opportunity_stage_history_confidence_valid CHECK (
        confidence IS NULL OR confidence BETWEEN 0 AND 1
    ),
    CONSTRAINT opportunity_stage_history_source_context CHECK (
        (source <> 'USER' OR actor_user_id IS NOT NULL)
        AND (source <> 'AI' OR confidence IS NOT NULL)
    )
);

CREATE INDEX opportunity_stage_history_timeline_idx
    ON opportunity_stage_history(tenant_id, opportunity_id, created_at, id);

-- История этапов является журналом: исправление выполняется новой записью,
-- поэтому прямые UPDATE и DELETE запрещены на уровне источника истины.
CREATE FUNCTION reject_opportunity_stage_history_mutation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'opportunity_stage_history is append-only' USING ERRCODE = '55000';
END;
$$;

CREATE TRIGGER opportunity_stage_history_append_only
BEFORE UPDATE OR DELETE ON opportunity_stage_history
FOR EACH ROW EXECUTE FUNCTION reject_opportunity_stage_history_mutation();
