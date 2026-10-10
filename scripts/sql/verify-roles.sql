-- Проверка ролей и прав LidRadar в текущей схеме (только чтение).
--
-- Каждая найденная проблема возвращается отдельной строкой; пустой результат
-- означает, что база готова к работе API, worker и scheduler: роли созданы,
-- пользователи могут в них переключаться, права на схему, таблицы и
-- последовательности выданы, права по умолчанию для будущих миграций заданы,
-- объекты принадлежат пользователю миграций, а RLS принудителен и политики на
-- месте. Использует ту же настройку lidradar.app_user, что и bootstrap-roles.sql.
-- Поведение RLS (изоляция организаций) проверяют тесты схемы, здесь только
-- структура: восстановленная база не должна терять политики.

WITH
roles(role_name) AS (
    VALUES ('lidradar_app'::name), ('lidradar_worker'::name), ('lidradar_platform'::name)
),
members(member_name) AS (
    SELECT current_user::name
    UNION
    SELECT COALESCE(NULLIF(current_setting('lidradar.app_user', true), ''), current_user)::name
),
relations AS (
    SELECT c.oid, c.relname::text AS name, c.relkind, c.relowner, c.relrowsecurity, c.relforcerowsecurity
    FROM pg_class c
    JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = current_schema() AND c.relkind IN ('r', 'p', 'S')
),
tenant_tables AS (
    SELECT r.*
    FROM relations r
    WHERE r.relkind IN ('r', 'p')
      AND (r.name = 'organizations' OR EXISTS (
            SELECT 1 FROM pg_attribute a
            WHERE a.attrelid = r.oid AND a.attname = 'tenant_id' AND a.attnum > 0 AND NOT a.attisdropped))
)
SELECT problem
FROM (
    SELECT 1 AS section, format('нет роли %s', role_name) AS problem
    FROM roles
    WHERE to_regrole(role_name) IS NULL

    UNION ALL
    SELECT 2, format('нет пользователя %s', member_name)
    FROM members
    WHERE to_regrole(member_name) IS NULL

    UNION ALL
    SELECT 3, format('пользователь %s не может переключаться в роль %s', m.member_name, r.role_name)
    FROM members m
    CROSS JOIN roles r
    WHERE to_regrole(m.member_name) IS NOT NULL AND to_regrole(r.role_name) IS NOT NULL
      AND NOT (
          pg_has_role(m.member_name, r.role_name, 'MEMBER')
          AND CASE WHEN current_setting('server_version_num')::int >= 160000
                   THEN pg_has_role(m.member_name, r.role_name, 'SET') ELSE true END)

    UNION ALL
    SELECT 4, format('у роли %s нет USAGE на схему %s', role_name, current_schema())
    FROM roles
    WHERE to_regrole(role_name) IS NOT NULL
      AND NOT has_schema_privilege(role_name, current_schema(), 'USAGE')

    UNION ALL
    SELECT 5, format('у роли %s нет %s на таблицу %s', r.role_name, p.privilege, t.name)
    FROM relations t
    CROSS JOIN roles r
    CROSS JOIN (VALUES ('SELECT'), ('INSERT'), ('UPDATE'), ('DELETE')) AS p(privilege)
    WHERE t.relkind IN ('r', 'p') AND to_regrole(r.role_name) IS NOT NULL
      AND NOT has_table_privilege(r.role_name, t.oid, p.privilege)

    UNION ALL
    SELECT 6, format('у роли %s нет %s на последовательность %s', r.role_name, p.privilege, t.name)
    FROM relations t
    CROSS JOIN roles r
    CROSS JOIN (VALUES ('USAGE'), ('SELECT'), ('UPDATE')) AS p(privilege)
    WHERE t.relkind = 'S' AND to_regrole(r.role_name) IS NOT NULL
      AND NOT has_sequence_privilege(r.role_name, t.oid, p.privilege)

    UNION ALL
    SELECT 7, format('у роли %s нет права по умолчанию %s на %s: объекты будущих миграций пользователя %s останутся недоступны',
                     r.role_name, d.privilege, CASE d.objtype WHEN 'S' THEN 'последовательности' ELSE 'таблицы' END, current_user)
    FROM roles r
    CROSS JOIN (VALUES ('r', 'SELECT'), ('r', 'INSERT'), ('r', 'UPDATE'), ('r', 'DELETE'),
                       ('S', 'USAGE'), ('S', 'SELECT'), ('S', 'UPDATE')) AS d(objtype, privilege)
    WHERE to_regrole(r.role_name) IS NOT NULL
      AND NOT EXISTS (
          SELECT 1
          FROM pg_default_acl a
          CROSS JOIN LATERAL aclexplode(a.defaclacl) AS x
          WHERE a.defaclrole = (SELECT oid FROM pg_roles WHERE rolname = current_user)
            AND a.defaclnamespace = to_regnamespace(current_schema())
            AND a.defaclobjtype = d.objtype::"char"
            AND x.grantee = to_regrole(r.role_name)::oid
            AND x.privilege_type = d.privilege)

    UNION ALL
    SELECT 8, format('%s %s принадлежит %s, а не %s: восстанавливайте под пользователем миграций',
                     CASE t.relkind WHEN 'S' THEN 'последовательность' ELSE 'таблица' END,
                     t.name, pg_get_userbyid(t.relowner), current_user)
    FROM relations t
    WHERE NOT pg_has_role(current_user, t.relowner, 'MEMBER')

    UNION ALL
    SELECT 9, format('таблица %s: RLS не включён принудительно', t.name)
    FROM tenant_tables t
    WHERE NOT (t.relrowsecurity AND t.relforcerowsecurity)

    UNION ALL
    SELECT 10, format('таблица %s: нет политики tenant_isolation', t.name)
    FROM tenant_tables t
    WHERE NOT EXISTS (
        SELECT 1 FROM pg_policies p
        WHERE p.schemaname = current_schema() AND p.tablename = t.name AND p.policyname = 'tenant_isolation')
) AS problems
ORDER BY section, problem;
