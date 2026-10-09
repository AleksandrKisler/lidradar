-- ADR 0047: retain semantic agreements and a separate response window.
ALTER TABLE conversation_summaries
    ADD COLUMN agreements JSONB NOT NULL DEFAULT '[]'::jsonb,
    ADD CONSTRAINT conversation_summaries_agreements_array CHECK (jsonb_typeof(agreements) = 'array');

ALTER TABLE locations
    ADD COLUMN agreement_threshold_minutes INTEGER NOT NULL DEFAULT 120,
    ADD CONSTRAINT locations_agreement_threshold_range CHECK (agreement_threshold_minutes BETWEEN 1 AND 1440);

ALTER TABLE risk_signals DROP CONSTRAINT risk_signals_type_check;
ALTER TABLE risk_signals ADD CONSTRAINT risk_signals_type_check CHECK (type IN (
    'NO_RESPONSE', 'BOOKING_NOT_CONFIRMED', 'PROMISE_NOT_FULFILLED',
    'CUSTOMER_SILENT_AFTER_PRICE', 'FOLLOW_UP_CANDIDATE', 'UNFINISHED_AGREEMENT'
));
ALTER TABLE notification_preferences DROP CONSTRAINT notification_preferences_risk_type_check;
ALTER TABLE notification_preferences ADD CONSTRAINT notification_preferences_risk_type_check CHECK (risk_type IN (
    'NO_RESPONSE', 'BOOKING_NOT_CONFIRMED', 'PROMISE_NOT_FULFILLED',
    'CUSTOMER_SILENT_AFTER_PRICE', 'FOLLOW_UP_CANDIDATE', 'UNFINISHED_AGREEMENT'
));
ALTER TABLE notification_digest_items DROP CONSTRAINT notification_digest_items_risk_type_check;
ALTER TABLE notification_digest_items ADD CONSTRAINT notification_digest_items_risk_type_check CHECK (risk_type IN (
    'NO_RESPONSE', 'BOOKING_NOT_CONFIRMED', 'PROMISE_NOT_FULFILLED',
    'CUSTOMER_SILENT_AFTER_PRICE', 'FOLLOW_UP_CANDIDATE', 'UNFINISHED_AGREEMENT'
));
ALTER TABLE risk_feedback DROP CONSTRAINT risk_feedback_risk_type_check;
ALTER TABLE risk_feedback ADD CONSTRAINT risk_feedback_risk_type_check CHECK (risk_type IN (
    'NO_RESPONSE', 'BOOKING_NOT_CONFIRMED', 'PROMISE_NOT_FULFILLED',
    'CUSTOMER_SILENT_AFTER_PRICE', 'FOLLOW_UP_CANDIDATE', 'UNFINISHED_AGREEMENT'
));
