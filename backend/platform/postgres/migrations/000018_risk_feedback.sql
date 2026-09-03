-- Этап 21: обратная связь по рискам (LR-BE-2101…2106) и граница ML-согласия
-- (ТЗ §70, LR-BE-2107). Обратная связь хранится только как факт с неизменяемым
-- снимком риска; точность считается по последнему вердикту (LR-BE-RM-019).

-- Явное, активное и отзываемое согласие организации на использование реальных
-- переписок и обратной связи в наборах данных. Активное согласие — одно на
-- область; отзыв не удаляет строку, история выдач сохраняется.
CREATE TABLE ml_consents (
    id UUID PRIMARY KEY,
    tenant_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    scope TEXT NOT NULL CHECK (scope = 'DATASETS'),
    granted_by UUID NOT NULL,
    granted_at TIMESTAMPTZ NOT NULL,
    revoked_by UUID,
    revoked_at TIMESTAMPTZ,
    CONSTRAINT ml_consents_tenant_id_unique UNIQUE (tenant_id, id),
    CONSTRAINT ml_consents_granted_membership_fk
        FOREIGN KEY (tenant_id, granted_by) REFERENCES memberships(tenant_id, user_id),
    CONSTRAINT ml_consents_revoked_membership_fk
        FOREIGN KEY (tenant_id, revoked_by) REFERENCES memberships(tenant_id, user_id),
    CONSTRAINT ml_consents_revocation_consistent CHECK ((revoked_at IS NULL) = (revoked_by IS NULL)),
    CONSTRAINT ml_consents_revoked_after_grant CHECK (revoked_at IS NULL OR revoked_at >= granted_at)
);

CREATE UNIQUE INDEX ml_consents_one_active_idx
    ON ml_consents(tenant_id, scope)
    WHERE revoked_at IS NULL;

CREATE FUNCTION reject_ml_consent_mutation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'ml_consents is append-only' USING ERRCODE = 'restrict_violation';
    END IF;
    IF OLD.revoked_at IS NOT NULL OR NEW.revoked_at IS NULL
       OR NEW.id <> OLD.id OR NEW.tenant_id <> OLD.tenant_id OR NEW.scope <> OLD.scope
       OR NEW.granted_by <> OLD.granted_by OR NEW.granted_at <> OLD.granted_at THEN
        RAISE EXCEPTION 'ml_consents allows only a single revocation' USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER ml_consents_revoke_only
    BEFORE UPDATE OR DELETE ON ml_consents
    FOR EACH ROW EXECUTE FUNCTION reject_ml_consent_mutation();

-- Обратная связь append-only: несколько записей на один риск допустимы,
-- метрика читает последнюю. Снимок риска фиксирует, что именно оценивал
-- пользователь (LR-BE-2104); dataset_eligible — действовало ли ML-согласие.
CREATE TABLE risk_feedback (
    id UUID PRIMARY KEY,
    tenant_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    risk_id UUID NOT NULL,
    opportunity_id UUID NOT NULL,
    actor_user_id UUID NOT NULL,
    verdict TEXT NOT NULL CHECK (verdict IN ('TRUE_POSITIVE', 'FALSE_POSITIVE')),
    reason TEXT CHECK (reason IN (
        'CUSTOMER_ALREADY_BOOKED', 'CUSTOMER_ALREADY_ANSWERED', 'NOT_A_LEAD',
        'CUSTOMER_REJECTED', 'WRONG_INTERPRETATION', 'OTHER'
    )),
    note TEXT NOT NULL,
    risk_type TEXT NOT NULL CHECK (risk_type IN (
        'NO_RESPONSE', 'BOOKING_NOT_CONFIRMED', 'PROMISE_NOT_FULFILLED',
        'CUSTOMER_SILENT_AFTER_PRICE', 'FOLLOW_UP_CANDIDATE'
    )),
    severity TEXT NOT NULL CHECK (severity IN ('LOW', 'MEDIUM', 'HIGH', 'CRITICAL')),
    risk_status TEXT NOT NULL CHECK (risk_status IN (
        'OPEN', 'ACKNOWLEDGED', 'ACTED', 'RESOLVED', 'FALSE_POSITIVE', 'IGNORED', 'EXPIRED'
    )),
    source TEXT NOT NULL CHECK (source IN ('RULE', 'HYBRID', 'MANUAL')),
    risk_engine_version TEXT NOT NULL,
    ai_run_id UUID,
    trigger_message_id UUID NOT NULL,
    opportunity_stage TEXT NOT NULL CHECK (opportunity_stage IN (
        'NEW', 'ENGAGED', 'QUALIFYING', 'PRICE_SENT', 'WAITING_CUSTOMER', 'WAITING_BUSINESS',
        'BOOKING_INTENT', 'BOOKED', 'WON', 'LOST', 'ARCHIVED'
    )),
    detected_at TIMESTAMPTZ NOT NULL,
    dataset_eligible BOOLEAN NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT risk_feedback_tenant_id_unique UNIQUE (tenant_id, id),
    CONSTRAINT risk_feedback_risk_fk
        FOREIGN KEY (tenant_id, risk_id) REFERENCES risk_signals(tenant_id, id),
    CONSTRAINT risk_feedback_opportunity_fk
        FOREIGN KEY (tenant_id, opportunity_id) REFERENCES opportunities(tenant_id, id),
    CONSTRAINT risk_feedback_actor_membership_fk
        FOREIGN KEY (tenant_id, actor_user_id) REFERENCES memberships(tenant_id, user_id),
    CONSTRAINT risk_feedback_false_positive_needs_reason CHECK (verdict = 'TRUE_POSITIVE' OR reason IS NOT NULL),
    CONSTRAINT risk_feedback_note_valid CHECK (note = btrim(note) AND char_length(note) <= 1000),
    CONSTRAINT risk_feedback_version_valid CHECK (
        risk_engine_version = btrim(risk_engine_version) AND char_length(risk_engine_version) BETWEEN 1 AND 100
    )
);

CREATE INDEX risk_feedback_risk_idx
    ON risk_feedback(tenant_id, risk_id, created_at DESC, id DESC);

CREATE TRIGGER risk_feedback_append_only
    BEFORE UPDATE OR DELETE ON risk_feedback
    FOR EACH ROW EXECUTE FUNCTION reject_append_only_mutation();
