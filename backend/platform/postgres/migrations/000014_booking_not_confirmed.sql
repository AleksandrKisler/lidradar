-- Этап 16: проверенные смысловые факты AI становятся версионированной
-- производной проекцией, а риск записи получает полную трассировку до AI-run.

ALTER TABLE conversation_summaries
    ADD COLUMN semantic_facts JSONB NOT NULL DEFAULT '[]'::jsonb,
    ADD CONSTRAINT conversation_summaries_facts_array
        CHECK (jsonb_typeof(semantic_facts) = 'array');

ALTER TABLE risk_signals
    ADD CONSTRAINT risk_signals_ai_run_fk
        FOREIGN KEY (tenant_id, ai_run_id)
        REFERENCES ai_runs(tenant_id, id),
    ADD CONSTRAINT risk_signals_hybrid_trace_valid CHECK (
        source <> 'HYBRID' OR (confidence IS NOT NULL AND ai_run_id IS NOT NULL)
    );

CREATE INDEX conversation_summaries_booking_fact_idx
    ON conversation_summaries(tenant_id, conversation_id)
    WHERE semantic_facts @> '[{"type":"BOOKING_INTENT","value":true}]'::jsonb;
