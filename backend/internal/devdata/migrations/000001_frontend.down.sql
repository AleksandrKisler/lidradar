-- Удаляются ТОЛЬКО три организации из манифеста и их данные, в том числе
-- созданные при ручной разработке. Чужие организации, пользователи, общий
-- журнал миграций и общие счётчики ограничения частоты не очищаются.
-- Внешние ключи, проверки вставок и RLS никогда не отключаются.
DO $$
DECLARE
    target TEXT;
    pair RECORD;
    tables TEXT[] := ARRAY[
        'telegram_callback_commands','notification_digest_items','notification_deliveries',
        'notifications','notification_preferences','telegram_link_tokens','telegram_user_links',
        'revenue_attributions','revenue_events','risk_feedback','actions','outcomes',
        'recommendations','risk_signals','opportunity_stage_history','conversation_summaries',
        'ai_runs','ai_jobs','ai_node_tenants','scheduled_checks','jobs','outbox_events',
        'attachments','messages','opportunities','conversations','external_identities',
        'contacts','raw_events','channel_connections','service_catalog_items',
        'location_business_hours','locations','idempotency_keys','audit_log','ml_consents','memberships'
    ];
BEGIN
    -- Блокировки держатся до фиксации или отката. Другая транзакция не может
    -- изменить данные в коротком окне обслуживания неизменяемых журналов.
    LOCK TABLE users, organizations, platform_admins, admin_audit_log,
        auth_audit_log, sessions IN ACCESS EXCLUSIVE MODE;
    FOREACH target IN ARRAY tables LOOP
        EXECUTE format('LOCK TABLE %I IN ACCESS EXCLUSIVE MODE',target);
    END LOOP;
    IF (SELECT count(*) FROM users u JOIN frontend_profiles p ON u.id=p.user_id AND u.email=p.email)<>3
       OR (SELECT count(*) FROM organizations o JOIN frontend_profiles p ON o.id=p.tenant_id)<>3
       OR EXISTS(SELECT 1 FROM memberships WHERE user_id IN (SELECT user_id FROM frontend_profiles)
                 AND tenant_id NOT IN (SELECT tenant_id FROM frontend_profiles))
       OR EXISTS(SELECT 1 FROM platform_admins WHERE user_id IN (SELECT user_id FROM frontend_profiles)
                 OR granted_by IN (SELECT user_id FROM frontend_profiles) OR revoked_by IN (SELECT user_id FROM frontend_profiles))
       OR EXISTS(SELECT 1 FROM admin_audit_log WHERE actor_user_id IN (SELECT user_id FROM frontend_profiles))
       OR EXISTS(SELECT 1 FROM channel_connections WHERE tenant_id IN (SELECT tenant_id FROM frontend_profiles)
                 AND encrypted_credentials IS NOT NULL) THEN
        RAISE EXCEPTION 'границы учебных кабинетов изменены; автоматический откат запрещён' USING ERRCODE='55000';
    END IF;
    -- Только поимённые запреты изменения/удаления. Это не DISABLE TRIGGER ALL:
    -- внешние ключи и проверка цепочки выручки остаются включены.
    FOR pair IN SELECT * FROM (VALUES
        ('actions','actions_append_only'),('outcomes','outcomes_append_only'),
        ('idempotency_keys','idempotency_keys_append_only'),('audit_log','audit_log_append_only'),
        ('opportunity_stage_history','opportunity_stage_history_append_only'),
        ('revenue_events','revenue_events_append_only'),('revenue_attributions','revenue_attributions_append_only'),
        ('risk_feedback','risk_feedback_append_only'),('ml_consents','ml_consents_revoke_only'),
        ('memberships','memberships_no_delete'),('auth_audit_log','auth_audit_log_append_only')
    ) AS protection(table_name,trigger_name) LOOP
        IF NOT EXISTS(SELECT 1 FROM pg_trigger WHERE tgrelid=pair.table_name::regclass
                      AND tgname=pair.trigger_name AND tgenabled='O') THEN
            RAISE EXCEPTION 'защита журналов отличается от ожидаемой' USING ERRCODE='55000';
        END IF;
        EXECUTE format('ALTER TABLE %I DISABLE TRIGGER %I',pair.table_name,pair.trigger_name);
    END LOOP;
    FOREACH target IN ARRAY tables LOOP
        EXECUTE format('DELETE FROM %I WHERE tenant_id IN (SELECT tenant_id FROM frontend_profiles)',target);
    END LOOP;
    DELETE FROM auth_audit_log WHERE user_id IN (SELECT user_id FROM frontend_profiles);
    DELETE FROM sessions WHERE user_id IN (SELECT user_id FROM frontend_profiles);
    -- Сбрасываются только корзины трёх тестовых адресов, не ограничения IP.
    DELETE FROM auth_rate_limits WHERE scope='LOGIN_ACCOUNT'
        AND subject_hash IN (SELECT sha256(email::BYTEA) FROM frontend_profiles);
    DELETE FROM organizations WHERE id IN (SELECT tenant_id FROM frontend_profiles);
    DELETE FROM users WHERE id IN (SELECT user_id FROM frontend_profiles);
    FOR pair IN SELECT * FROM (VALUES
        ('actions','actions_append_only'),('outcomes','outcomes_append_only'),
        ('idempotency_keys','idempotency_keys_append_only'),('audit_log','audit_log_append_only'),
        ('opportunity_stage_history','opportunity_stage_history_append_only'),
        ('revenue_events','revenue_events_append_only'),('revenue_attributions','revenue_attributions_append_only'),
        ('risk_feedback','risk_feedback_append_only'),('ml_consents','ml_consents_revoke_only'),
        ('memberships','memberships_no_delete'),('auth_audit_log','auth_audit_log_append_only')
    ) AS protection(table_name,trigger_name) LOOP
        EXECUTE format('ALTER TABLE %I ENABLE TRIGGER %I',pair.table_name,pair.trigger_name);
    END LOOP;
END $$;
