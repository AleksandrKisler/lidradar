CREATE TABLE revenue_events (
    id UUID PRIMARY KEY,
    tenant_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    opportunity_id UUID NOT NULL,
    amount NUMERIC(14,2) NOT NULL CHECK (amount > 0),
    currency CHAR(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    status TEXT NOT NULL CHECK (status IN ('CONFIRMED')),
    source TEXT NOT NULL CHECK (source IN ('USER_CONFIRMED')),
    confirmed_by_user_id UUID NOT NULL,
    confirmed_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT revenue_events_tenant_id_unique UNIQUE (tenant_id, id),
    CONSTRAINT revenue_events_opportunity_fk
        FOREIGN KEY (tenant_id, opportunity_id)
        REFERENCES opportunities(tenant_id, id),
    CONSTRAINT revenue_events_actor_membership_fk
        FOREIGN KEY (tenant_id, confirmed_by_user_id)
        REFERENCES memberships(tenant_id, user_id),
    CONSTRAINT revenue_events_time_valid CHECK (created_at = confirmed_at)
);

CREATE INDEX revenue_events_opportunity_timeline_idx
    ON revenue_events(tenant_id, opportunity_id, confirmed_at DESC, id DESC);

CREATE INDEX revenue_events_confirmed_currency_idx
    ON revenue_events(tenant_id, currency, confirmed_at DESC)
    WHERE status = 'CONFIRMED';

CREATE TABLE revenue_attributions (
    id UUID PRIMARY KEY,
    tenant_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    revenue_event_id UUID NOT NULL,
    opportunity_id UUID NOT NULL,
    type TEXT NOT NULL CHECK (type IN ('RECOVERED', 'ORGANIC', 'UNKNOWN')),
    risk_id UUID,
    action_id UUID,
    outcome_id UUID,
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT revenue_attributions_tenant_id_unique UNIQUE (tenant_id, id),
    CONSTRAINT revenue_attributions_event_unique UNIQUE (tenant_id, revenue_event_id),
    CONSTRAINT revenue_attributions_event_fk
        FOREIGN KEY (tenant_id, revenue_event_id)
        REFERENCES revenue_events(tenant_id, id),
    CONSTRAINT revenue_attributions_opportunity_fk
        FOREIGN KEY (tenant_id, opportunity_id)
        REFERENCES opportunities(tenant_id, id),
    CONSTRAINT revenue_attributions_risk_fk
        FOREIGN KEY (tenant_id, risk_id)
        REFERENCES risk_signals(tenant_id, id),
    CONSTRAINT revenue_attributions_action_fk
        FOREIGN KEY (tenant_id, action_id)
        REFERENCES actions(tenant_id, id),
    CONSTRAINT revenue_attributions_outcome_fk
        FOREIGN KEY (tenant_id, outcome_id)
        REFERENCES outcomes(tenant_id, id),
    CONSTRAINT revenue_attributions_chain_valid CHECK (
        (type = 'RECOVERED' AND risk_id IS NOT NULL AND action_id IS NOT NULL AND outcome_id IS NOT NULL)
        OR
        (type IN ('ORGANIC', 'UNKNOWN') AND risk_id IS NULL AND action_id IS NULL AND outcome_id IS NULL)
    )
);

CREATE INDEX revenue_attributions_recovered_opportunity_idx
    ON revenue_attributions(tenant_id, opportunity_id, created_at DESC)
    WHERE type = 'RECOVERED';

CREATE FUNCTION validate_revenue_attribution()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    event_opportunity UUID;
    event_confirmed_at TIMESTAMPTZ;
    risk_opportunity UUID;
    risk_at TIMESTAMPTZ;
    action_opportunity UUID;
    action_at TIMESTAMPTZ;
    outcome_opportunity UUID;
    outcome_at TIMESTAMPTZ;
BEGIN
    SELECT opportunity_id, confirmed_at
    INTO event_opportunity, event_confirmed_at
    FROM revenue_events
    WHERE tenant_id = NEW.tenant_id AND id = NEW.revenue_event_id;

    IF event_opportunity IS NULL OR event_opportunity <> NEW.opportunity_id
       OR NEW.created_at <> event_confirmed_at THEN
        RAISE EXCEPTION 'событие выручки не соответствует атрибуции'
            USING ERRCODE = '23514';
    END IF;

    IF NEW.type = 'RECOVERED' THEN
        SELECT opportunity_id, detected_at INTO risk_opportunity, risk_at
        FROM risk_signals WHERE tenant_id = NEW.tenant_id AND id = NEW.risk_id;

        SELECT risk.opportunity_id, action.created_at INTO action_opportunity, action_at
        FROM actions AS action
        JOIN risk_signals AS risk
          ON risk.tenant_id = action.tenant_id AND risk.id = action.risk_id
        WHERE action.tenant_id = NEW.tenant_id AND action.id = NEW.action_id
          AND action.risk_id = NEW.risk_id;

        SELECT opportunity_id, created_at INTO outcome_opportunity, outcome_at
        FROM outcomes WHERE tenant_id = NEW.tenant_id AND id = NEW.outcome_id;

        IF risk_opportunity IS NULL OR action_opportunity IS NULL OR outcome_opportunity IS NULL
           OR risk_opportunity <> NEW.opportunity_id
           OR action_opportunity <> NEW.opportunity_id
           OR outcome_opportunity <> NEW.opportunity_id
           OR risk_at > event_confirmed_at OR action_at > event_confirmed_at OR outcome_at > event_confirmed_at
           OR risk_at < event_confirmed_at - INTERVAL '30 days'
           OR action_at < event_confirmed_at - INTERVAL '30 days'
           OR outcome_at < event_confirmed_at - INTERVAL '30 days' THEN
            RAISE EXCEPTION 'цепочка возврата выручки не соответствует возможности или окну атрибуции'
                USING ERRCODE = '23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER revenue_attributions_validate
    BEFORE INSERT ON revenue_attributions
    FOR EACH ROW EXECUTE FUNCTION validate_revenue_attribution();

CREATE TRIGGER revenue_events_append_only
    BEFORE UPDATE OR DELETE ON revenue_events
    FOR EACH ROW EXECUTE FUNCTION reject_append_only_mutation();

CREATE TRIGGER revenue_attributions_append_only
    BEFORE UPDATE OR DELETE ON revenue_attributions
    FOR EACH ROW EXECUTE FUNCTION reject_append_only_mutation();
