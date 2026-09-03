-- Этап 24: роли PostgreSQL и fail-closed RLS (ADR 0034, LR-BE-2401,
-- LR-BE-RM-018). Политика читает организацию из настройки сеанса
-- lidradar.tenant_id; пустая настройка не совпадает ни с одной строкой.
-- Член lidradar_platform (владелец схемы, захват заданий, диспетчер,
-- администрирование) не ограничен политикой.

SELECT pg_advisory_xact_lock(1279541843);

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'lidradar_app') THEN
        BEGIN
            CREATE ROLE lidradar_app NOLOGIN;
        EXCEPTION WHEN duplicate_object THEN NULL;
        END;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'lidradar_worker') THEN
        BEGIN
            CREATE ROLE lidradar_worker NOLOGIN;
        EXCEPTION WHEN duplicate_object THEN NULL;
        END;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'lidradar_platform') THEN
        BEGIN
            CREATE ROLE lidradar_platform NOLOGIN;
        EXCEPTION WHEN duplicate_object THEN NULL;
        END;
    END IF;
END $$;

-- Пользователь миграций получает право переключаться в рабочие роли и сам
-- входит в lidradar_platform: миграции и CLI не ограничены политиками.
GRANT lidradar_app, lidradar_worker, lidradar_platform TO CURRENT_USER;

DO $$
DECLARE
    schema_name text := current_schema();
BEGIN
    EXECUTE format('GRANT USAGE ON SCHEMA %I TO lidradar_app, lidradar_worker, lidradar_platform', schema_name);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA %I TO lidradar_app, lidradar_worker, lidradar_platform', schema_name);
    EXECUTE format('GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA %I TO lidradar_app, lidradar_worker, lidradar_platform', schema_name);
    EXECUTE format('ALTER DEFAULT PRIVILEGES IN SCHEMA %I GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO lidradar_app, lidradar_worker, lidradar_platform', schema_name);
    EXECUTE format('ALTER DEFAULT PRIVILEGES IN SCHEMA %I GRANT USAGE, SELECT, UPDATE ON SEQUENCES TO lidradar_app, lidradar_worker, lidradar_platform', schema_name);
END $$;

-- Каждая таблица с tenant_id получает одну разрешающую политику: строка
-- организации из контекста либо член lidradar_platform.
DO $$
DECLARE
    tenant_table record;
BEGIN
    FOR tenant_table IN
        SELECT c.table_name
        FROM information_schema.columns AS c
        JOIN information_schema.tables AS t
          ON t.table_schema = c.table_schema AND t.table_name = c.table_name
        WHERE c.table_schema = current_schema() AND c.column_name = 'tenant_id' AND t.table_type = 'BASE TABLE'
    LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', tenant_table.table_name);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', tenant_table.table_name);
        EXECUTE format('DROP POLICY IF EXISTS tenant_isolation ON %I', tenant_table.table_name);
        EXECUTE format($policy$
            CREATE POLICY tenant_isolation ON %I AS PERMISSIVE FOR ALL
            USING (
                tenant_id = NULLIF(current_setting('lidradar.tenant_id', true), '')::uuid
                OR pg_has_role(current_user, 'lidradar_platform', 'MEMBER')
            )
            WITH CHECK (
                tenant_id = NULLIF(current_setting('lidradar.tenant_id', true), '')::uuid
                OR pg_has_role(current_user, 'lidradar_platform', 'MEMBER')
            )$policy$, tenant_table.table_name);
    END LOOP;
END $$;

-- Организация видна по контексту организации, члену платформы и участнику,
-- чей пользователь задан в контексте (список организаций пользователя).
ALTER TABLE organizations ENABLE ROW LEVEL SECURITY;
ALTER TABLE organizations FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON organizations;
CREATE POLICY tenant_isolation ON organizations AS PERMISSIVE FOR ALL
    USING (
        id = NULLIF(current_setting('lidradar.tenant_id', true), '')::uuid
        OR pg_has_role(current_user, 'lidradar_platform', 'MEMBER')
        OR EXISTS (
            SELECT 1 FROM memberships AS membership
            WHERE membership.tenant_id = organizations.id
              AND membership.user_id = NULLIF(current_setting('lidradar.user_id', true), '')::uuid
        )
    )
    WITH CHECK (
        id = NULLIF(current_setting('lidradar.tenant_id', true), '')::uuid
        OR pg_has_role(current_user, 'lidradar_platform', 'MEMBER')
    );

-- Пользователь видит собственные членства во всех организациях.
DROP POLICY IF EXISTS member_self ON memberships;
CREATE POLICY member_self ON memberships AS PERMISSIVE FOR SELECT
    USING (user_id = NULLIF(current_setting('lidradar.user_id', true), '')::uuid);
