-- Роли PostgreSQL и права LidRadar для восстановленной базы (ADR 0034, ADR 0041).
--
-- pg_dump --no-owner --no-privileges не переносит роли кластера, их членство и
-- права на объекты и пропускает ALTER DEFAULT PRIVILEGES. После восстановления на
-- новом кластере API поэтому не стартует (role "lidradar_app" does not exist),
-- а миграция 000020 уже записана в журнале и повторно не выполнится. Файл
-- повторяет эту часть миграции 000020. Политики RLS и FORCE ROW LEVEL SECURITY
-- входят в дамп и здесь не затрагиваются.
--
-- Запускается scripts/bootstrap-roles.sh одной транзакцией под пользователем,
-- который владеет восстановленными объектами и запускает миграции; как и сама
-- миграция 000020, он создаёт роли либо имеет право администрировать готовые.
-- Повторный запуск безопасен. Файл ничего не отзывает и не удаляет.
--
-- Необязательная настройка сеанса lidradar.app_user — отдельный пользователь, под
-- которым работают API, worker и scheduler; ему тоже выдаётся членство в ролях.

DO $$
BEGIN
    PERFORM pg_advisory_xact_lock(1279541843);
END $$;

DO $$
DECLARE
    ledger regclass := to_regclass('schema_migrations');
    ledger_owner oid;
BEGIN
    IF ledger IS NULL THEN
        RAISE EXCEPTION 'нет журнала schema_migrations: это не база LidRadar, роли не настраиваются';
    END IF;
    SELECT relowner INTO ledger_owner FROM pg_class WHERE oid = ledger;
    IF NOT pg_has_role(current_user, ledger_owner, 'MEMBER') THEN
        RAISE EXCEPTION 'объекты базы принадлежат пользователю %, а скрипт запущен под %: восстановите базу и запускайте скрипт под пользователем миграций',
            pg_get_userbyid(ledger_owner), current_user;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM schema_migrations WHERE version = '000020_row_level_security') THEN
        RAISE EXCEPTION 'в журнале нет миграции 000020_row_level_security: база старше ролей LidRadar, примените миграции';
    END IF;
END $$;

DO $$
DECLARE
    role_name text;
BEGIN
    FOREACH role_name IN ARRAY ARRAY['lidradar_app', 'lidradar_worker', 'lidradar_platform'] LOOP
        IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = role_name) THEN
            BEGIN
                EXECUTE format('CREATE ROLE %I NOLOGIN', role_name);
            EXCEPTION WHEN duplicate_object THEN NULL;
            END;
        END IF;
    END LOOP;
END $$;

-- Пользователь миграций получает право переключаться в рабочие роли и сам входит
-- в lidradar_platform: миграции и CLI не ограничены политиками. Отдельный
-- пользователь приложения (lidradar.app_user) получает то же членство.
DO $$
DECLARE
    app_user text := NULLIF(current_setting('lidradar.app_user', true), '');
BEGIN
    GRANT lidradar_app, lidradar_worker, lidradar_platform TO CURRENT_USER;
    IF app_user IS NOT NULL AND app_user <> current_user THEN
        EXECUTE format('GRANT lidradar_app, lidradar_worker, lidradar_platform TO %I', app_user);
    END IF;
EXCEPTION WHEN insufficient_privilege THEN
    RAISE EXCEPTION 'не удалось выдать членство в ролях lidradar_*: нужны CREATEROLE либо ADMIN OPTION на эти роли'
        USING ERRCODE = 'insufficient_privilege';
END $$;

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
