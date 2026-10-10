-- Роли PostgreSQL production-стека LidRadar (ADR 0052).
--
-- Образ postgres выполняет файл один раз, при первой инициализации тома, под
-- суперпользователем postgres в базе lidradar (POSTGRES_DB). Для внешней базы
-- тот же файл запускает её администратор в базе приложения:
--   LIDRADAR_DB_OWNER_PASSWORD=… LIDRADAR_DB_RUNTIME_PASSWORD=… psql -d БАЗА -f 10-roles.sql
-- (нужен psql 15 или новее: пароли читаются из окружения командой \getenv, в
-- командную строку и журнал они не попадают). Повторный запуск безопасен: он
-- возвращает атрибуты, членство и пароли к описанным здесь.
--
-- Три уровня доступа:
--   postgres          администратор кластера. Его пароль нужен только образу, по сети
--                     администратор не входит (pg_hba.conf), процессы LidRadar его не знают.
--   lidradar          владелец базы и схемы: миграции, восстановление копий, роли. Не
--                     суперпользователь, без CREATEROLE и BYPASSRLS. CREATEDB нужен
--                     восстановлению (новая база, переименование) и учениям.
--   lidradar_runtime  рабочий логин api, worker, scheduler, копий и инструментов. Не
--                     владелец: схему менять, отключать триггеры и RLS он не может. Только
--                     членство в ролях lidradar_app, lidradar_worker, lidradar_platform,
--                     в которые процессы переключаются командой SET ROLE (ADR 0041).
-- Роли lidradar_app, lidradar_worker и lidradar_platform создаёт тот же файл: миграции
-- 000020 тогда не нужен CREATEROLE, ей достаточно ADMIN OPTION на них у владельца.

\set ON_ERROR_STOP on

\getenv owner_password LIDRADAR_DB_OWNER_PASSWORD
\getenv runtime_password LIDRADAR_DB_RUNTIME_PASSWORD
\if :{?owner_password}
\else
  \set owner_password ''
\endif
\if :{?runtime_password}
\else
  \set runtime_password ''
\endif
SELECT (:'owner_password' = '' OR :'runtime_password' = '') AS empty_password \gset
\if :empty_password
  DO $$ BEGIN RAISE EXCEPTION 'задайте непустые LIDRADAR_DB_OWNER_PASSWORD и LIDRADAR_DB_RUNTIME_PASSWORD'; END $$;
\endif
SELECT current_database() IN ('postgres', 'template0', 'template1') AS service_database \gset
\if :service_database
  DO $$ BEGIN RAISE EXCEPTION 'файл запускают в базе LidRadar (psql -d БАЗА), а не в служебной базе'; END $$;
\endif

DO $$
DECLARE
    role_name text;
BEGIN
    FOREACH role_name IN ARRAY ARRAY['lidradar_app', 'lidradar_worker', 'lidradar_platform'] LOOP
        IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = role_name) THEN
            EXECUTE format('CREATE ROLE %I NOLOGIN', role_name);
        END IF;
        EXECUTE format('ALTER ROLE %I WITH NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS', role_name);
    END LOOP;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'lidradar') THEN
        CREATE ROLE lidradar LOGIN;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'lidradar_runtime') THEN
        CREATE ROLE lidradar_runtime LOGIN;
    END IF;
END $$;

ALTER ROLE lidradar WITH LOGIN NOSUPERUSER NOCREATEROLE CREATEDB NOREPLICATION NOBYPASSRLS INHERIT PASSWORD :'owner_password';
ALTER ROLE lidradar_runtime WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS INHERIT PASSWORD :'runtime_password';

-- Владелец может выдавать эти роли сам себе (GRANT … TO CURRENT_USER в миграции 000020 и
-- в scripts/sql/bootstrap-roles.sql), рабочий логин только состоит в них.
GRANT lidradar_app, lidradar_worker, lidradar_platform TO lidradar WITH ADMIN OPTION;
GRANT lidradar_app, lidradar_worker, lidradar_platform TO lidradar_runtime;

SELECT format('ALTER DATABASE %I OWNER TO lidradar', current_database()) \gexec
