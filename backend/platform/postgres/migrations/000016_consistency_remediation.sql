-- Этап R — CONSISTENCY REMEDIATION (Errata v1.2.2). Одна forward-only
-- миграция: только ALTER, CREATE INDEX и переименования. ТЗ называет файл
-- 000018; по правилу нумерации занят следующий свободный номер 000016, карта
-- миграций сдвинута на два.

-- ---------------------------------------------------------------------------
-- LR-BE-RM-001 — целостность цепочки атрибуции выручки.
-- Каждое звено Risk → Action → Outcome → RevenueEvent связано с Opportunity
-- составным ключом, поэтому чужая Opportunity внутри своей организации
-- отклоняется внешним ключом, а не только триггером.
-- ---------------------------------------------------------------------------
ALTER TABLE revenue_events
    ADD CONSTRAINT revenue_events_tenant_opportunity_unique UNIQUE (tenant_id, id, opportunity_id);

ALTER TABLE risk_signals
    ADD CONSTRAINT risk_signals_tenant_opportunity_unique UNIQUE (tenant_id, id, opportunity_id);

ALTER TABLE outcomes
    ADD CONSTRAINT outcomes_tenant_opportunity_unique UNIQUE (tenant_id, id, opportunity_id);

-- Действие принадлежит Opportunity через свой риск. Денормализованный столбец
-- позволяет составному ключу проверять это без соединения.
ALTER TABLE actions ADD COLUMN opportunity_id UUID;

ALTER TABLE actions DISABLE TRIGGER actions_append_only;
UPDATE actions AS action
SET opportunity_id = risk.opportunity_id
FROM risk_signals AS risk
WHERE risk.tenant_id = action.tenant_id AND risk.id = action.risk_id;
ALTER TABLE actions ENABLE TRIGGER actions_append_only;

ALTER TABLE actions
    ALTER COLUMN opportunity_id SET NOT NULL,
    ADD CONSTRAINT actions_tenant_opportunity_unique UNIQUE (tenant_id, id, opportunity_id),
    ADD CONSTRAINT actions_risk_opportunity_fk
        FOREIGN KEY (tenant_id, risk_id, opportunity_id)
        REFERENCES risk_signals(tenant_id, id, opportunity_id);

ALTER TABLE revenue_attributions
    DROP CONSTRAINT revenue_attributions_event_fk,
    DROP CONSTRAINT revenue_attributions_risk_fk,
    DROP CONSTRAINT revenue_attributions_action_fk,
    DROP CONSTRAINT revenue_attributions_outcome_fk;

ALTER TABLE revenue_attributions
    ADD CONSTRAINT revenue_attributions_event_fk
        FOREIGN KEY (tenant_id, revenue_event_id, opportunity_id)
        REFERENCES revenue_events(tenant_id, id, opportunity_id),
    ADD CONSTRAINT revenue_attributions_risk_fk
        FOREIGN KEY (tenant_id, risk_id, opportunity_id)
        REFERENCES risk_signals(tenant_id, id, opportunity_id),
    ADD CONSTRAINT revenue_attributions_action_fk
        FOREIGN KEY (tenant_id, action_id, opportunity_id)
        REFERENCES actions(tenant_id, id, opportunity_id),
    ADD CONSTRAINT revenue_attributions_outcome_fk
        FOREIGN KEY (tenant_id, outcome_id, opportunity_id)
        REFERENCES outcomes(tenant_id, id, opportunity_id);

-- Одна Opportunity даёт не более одной атрибуции RECOVERED. При оплате частями
-- RECOVERED получает первое событие, остальные — ORGANIC. Уже существующие
-- дубликаты останавливают миграцию: атрибуции неизменяемы, и такое состояние
-- требует ручного решения, а не тихого удаления.
CREATE UNIQUE INDEX revenue_attributions_one_recovered_per_opportunity_idx
    ON revenue_attributions(tenant_id, opportunity_id)
    WHERE type = 'RECOVERED';

-- ---------------------------------------------------------------------------
-- LR-BE-RM-006 — не более одного ожидающего AI-задания на сущность.
-- Захваченное или выполняющееся задание в индекс не входит: узел уже получил
-- его инструкцию, и снимок такого задания менять нельзя (см. ADR 0036).
-- ---------------------------------------------------------------------------
UPDATE ai_jobs AS job
SET status = 'DEAD', completed_at = now(), updated_at = now(),
    last_error_code = 'SUPERSEDED_BY_NEWER_SNAPSHOT'
WHERE job.status IN ('PENDING', 'RETRY')
  AND job.id NOT IN (
    SELECT DISTINCT ON (tenant_id, entity_type, entity_id) id
    FROM ai_jobs
    WHERE status IN ('PENDING', 'RETRY')
    ORDER BY tenant_id, entity_type, entity_id,
             base_conversation_revision DESC, created_at DESC, id DESC
  );

CREATE UNIQUE INDEX ai_jobs_one_queued_per_entity_idx
    ON ai_jobs(tenant_id, entity_type, entity_id)
    WHERE status IN ('PENDING', 'RETRY');

-- ---------------------------------------------------------------------------
-- LR-BE-RM-016 — абсолютный потолок аренды AI-задания (§3.5).
-- leased_at ставится при захвате и никогда не продлевается heartbeat.
-- ---------------------------------------------------------------------------
ALTER TABLE ai_jobs ADD COLUMN leased_at TIMESTAMPTZ;

UPDATE ai_jobs SET leased_at = updated_at WHERE status IN ('LEASED', 'RUNNING');

ALTER TABLE ai_jobs
    DROP CONSTRAINT ai_jobs_lease_valid,
    ADD CONSTRAINT ai_jobs_lease_valid CHECK (
        (status IN ('LEASED', 'RUNNING')
            AND leased_by IS NOT NULL AND lease_until IS NOT NULL AND leased_at IS NOT NULL
            AND completed_at IS NULL)
        OR
        (status IN ('PENDING', 'RETRY')
            AND leased_by IS NULL AND lease_until IS NULL AND leased_at IS NULL
            AND completed_at IS NULL)
        OR
        (status IN ('SUCCEEDED', 'DEAD')
            AND leased_by IS NULL AND lease_until IS NULL AND leased_at IS NULL
            AND completed_at IS NOT NULL)
    );

CREATE INDEX ai_jobs_lease_cap_idx
    ON ai_jobs(leased_at)
    WHERE status IN ('LEASED', 'RUNNING');

-- ---------------------------------------------------------------------------
-- LR-BE-RM-017 — уровни уверенности (§59) зафиксированы в самом факте.
-- trusted вычисляет Cloud Core; модель это поле прислать не может.
-- ---------------------------------------------------------------------------
UPDATE conversation_summaries
SET semantic_facts = COALESCE((
    SELECT jsonb_agg(
        fact || jsonb_build_object('trusted', (fact ->> 'confidence')::numeric >= 0.85)
        ORDER BY ordinality
    )
    FROM jsonb_array_elements(semantic_facts) WITH ORDINALITY AS facts(fact, ordinality)
), '[]'::jsonb)
WHERE jsonb_array_length(semantic_facts) > 0;

CREATE FUNCTION semantic_facts_carry_trust(facts JSONB)
RETURNS BOOLEAN
LANGUAGE sql
IMMUTABLE
AS $$
    -- bool_and пропускает NULL, поэтому отсутствие ключа сводится к FALSE явно.
    SELECT COALESCE(bool_and(COALESCE(jsonb_typeof(fact -> 'trusted') = 'boolean', FALSE)), TRUE)
    FROM jsonb_array_elements(facts) AS fact
$$;

ALTER TABLE conversation_summaries
    ADD CONSTRAINT conversation_summaries_facts_trusted
        CHECK (semantic_facts_carry_trust(semantic_facts));

-- ---------------------------------------------------------------------------
-- LR-BE-RM-009 — членство не удаляется физически.
-- На membership ссылаются неизменяемые факты (revenue_events, actions,
-- outcomes, audit_log), поэтому отзыв доступа — только revoked_at.
-- ---------------------------------------------------------------------------
ALTER TABLE memberships ADD COLUMN revoked_at TIMESTAMPTZ;

UPDATE memberships SET revoked_at = updated_at WHERE status = 'DISABLED';

ALTER TABLE memberships
    ADD CONSTRAINT memberships_revocation_consistent CHECK (
        (status = 'DISABLED') = (revoked_at IS NOT NULL)
    );

CREATE FUNCTION reject_membership_delete()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'членство не удаляется физически: отзыв доступа выполняется через revoked_at'
        USING ERRCODE = '55000';
END;
$$;

CREATE TRIGGER memberships_no_delete
    BEFORE DELETE ON memberships
    FOR EACH ROW EXECUTE FUNCTION reject_membership_delete();

-- ---------------------------------------------------------------------------
-- LR-BE-RM-010 — единый словарь аренды (§53): leased_by / lease_until.
-- Проверочные ограничения ссылаются на столбец по идентификатору и
-- переименовываются вместе с ним.
-- ---------------------------------------------------------------------------
ALTER TABLE jobs RENAME COLUMN lease_owner TO leased_by;
ALTER TABLE outbox_events RENAME COLUMN lease_owner TO leased_by;
ALTER TABLE notification_deliveries RENAME COLUMN lease_owner TO leased_by;
