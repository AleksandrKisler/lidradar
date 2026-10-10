#!/usr/bin/env bash
# Сквозная проверка ролей PostgreSQL production-стека (ADR 0052) на настоящих файлах и
# миграциях: deploy/production/postgres/{10-roles.sql,pg_hba.conf} в образе postgres:18-alpine,
# все миграции владельцем без суперправ, api, worker и scheduler под рабочим логином, копия и
# восстановление, запуск файла ролей на «внешней» базе. Нужны Docker, Go, curl и jq; сеть —
# только чтобы скачать postgres:18-alpine и модули Go.
#
#   scripts/tests/e2e-db-roles.sh
#
# Код 0 — все проверки прошли. Контейнеры и временные файлы удаляются при любом выходе.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
POSTGRES_DIR="${ROOT}/deploy/production/postgres"
RUN="lrdb-e2e-$$"
WORK="$(mktemp -d)"
FAILURES=0
PIDS=()

ADMIN_PASSWORD=e2e-admin-password
OWNER_PASSWORD=e2e-owner-password
RUNTIME_PASSWORD=e2e-runtime-password

cleanup() {
  if [ "${#PIDS[@]}" -gt 0 ]; then kill "${PIDS[@]}" 2>/dev/null || true; fi
  docker rm -f "${RUN}-pg" "${RUN}-ext" >/dev/null 2>&1 || true
  rm -rf "${WORK}"
}
trap cleanup EXIT

ok() { echo "ok    $1"; }
bad() { echo "FAIL  $1" >&2; FAILURES=$((FAILURES + 1)); }
check() { # check ОПИСАНИЕ КОМАНДА...: команда обязана завершиться успешно
  local description="$1"
  shift
  if "$@" >/dev/null 2>&1; then ok "${description}"; else bad "${description}"; fi
}
check_fails() { # check_fails ОПИСАНИЕ КОМАНДА...: команда обязана завершиться ошибкой
  local description="$1"
  shift
  if "$@" >/dev/null 2>&1; then bad "${description}"; else ok "${description}"; fi
}
expect() { # expect ОПИСАНИЕ ФАКТ ОЖИДАЕМОЕ
  if [ "$2" = "$3" ]; then ok "$1"; else bad "$1: получено '$2', ожидалось '$3'"; fi
}

# start_database ИМЯ ПУБЛИКАЦИЯ(yes|no) ИНИЦИАЛИЗАЦИЯ_ФАЙЛАМИ(yes|no)
start_database() {
  local name="$1" publish="$2" init="$3"
  local -a args=(-d --name "${name}" -e POSTGRES_DB=lidradar -e "POSTGRES_PASSWORD=${ADMIN_PASSWORD}")
  if [ "${publish}" = yes ]; then args+=(-p 127.0.0.1::5432); fi
  if [ "${init}" = yes ]; then
    docker run "${args[@]}" -e "LIDRADAR_DB_OWNER_PASSWORD=${OWNER_PASSWORD}" -e "LIDRADAR_DB_RUNTIME_PASSWORD=${RUNTIME_PASSWORD}" \
      -v "${POSTGRES_DIR}/pg_hba.conf:/etc/lidradar/pg_hba.conf:ro" -v "${POSTGRES_DIR}/10-roles.sql:/docker-entrypoint-initdb.d/10-roles.sql:ro" \
      postgres:18-alpine postgres -c hba_file=/etc/lidradar/pg_hba.conf >/dev/null
  else
    docker run "${args[@]}" postgres:18-alpine >/dev/null
  fi
  # Образ сначала поднимает временный сервер для инициализации: ждём настоящий запуск.
  for _ in $(seq 1 90); do
    if docker logs "${name}" 2>&1 | grep -q 'init process complete' && docker exec "${name}" pg_isready -q -U postgres -d lidradar; then return 0; fi
    sleep 1
  done
  echo "PostgreSQL ${name} не запустился" >&2
  docker logs "${name}" 2>&1 | tail -20 >&2
  return 1
}

# sql КОНТЕЙНЕР ПОЛЬЗОВАТЕЛЬ БАЗА ЗАПРОС: запрос по сокету внутри контейнера (доверие по pg_hba.conf).
sql() { docker exec "$1" psql -X -At -v ON_ERROR_STOP=1 -U "$2" -d "$3" -c "$4"; }
pg() { sql "${RUN}-pg" "$1" lidradar "$2"; }
allowed() { pg "$1" "$2" >/dev/null 2>&1; }
denied() { ! allowed "$1" "$2"; }
role_flags() { # суперпользователь, CREATEROLE, CREATEDB, BYPASSRLS, вход, репликация
  sql "$1" postgres lidradar "SELECT rolsuper, rolcreaterole, rolcreatedb, rolbypassrls, rolcanlogin, rolreplication FROM pg_roles WHERE rolname = '$2'" | tr '|' ' '
}
# tcp КОНТЕЙНЕР ПОЛЬЗОВАТЕЛЬ ПАРОЛЬ: вход по сети с адреса контейнера. Через 127.0.0.1 нельзя: у образа
# по умолчанию loopback идёт под trust, и проверка пароля ничего бы не проверяла.
tcp() {
  local address
  address="$(docker exec "$1" hostname -i | awk '{print $1}')"
  docker exec -e "PGPASSWORD=$3" "$1" psql -X -At "host=${address} dbname=lidradar user=$2 connect_timeout=5" -c 'SELECT current_user'
}
free_port() { python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])'; }

# --- 1. инициализация образа ---------------------------------------------------------------
start_database "${RUN}-pg" yes yes
PG_PORT="$(docker port "${RUN}-pg" 5432/tcp | head -n 1 | sed 's/.*://')"
OWNER_DSN="postgres://lidradar:${OWNER_PASSWORD}@127.0.0.1:${PG_PORT}/lidradar?sslmode=disable"
RUNTIME_DSN="postgres://lidradar_runtime:${RUNTIME_PASSWORD}@127.0.0.1:${PG_PORT}/lidradar?sslmode=disable"

expect 'владелец lidradar: не суперпользователь, без CREATEROLE и BYPASSRLS, с CREATEDB и входом' "$(role_flags "${RUN}-pg" lidradar)" 'f f t f t f'
expect 'рабочий логин lidradar_runtime: ни одной привилегии, только вход' "$(role_flags "${RUN}-pg" lidradar_runtime)" 'f f f f t f'
for role in lidradar_app lidradar_worker lidradar_platform; do
  expect "роль ${role}: без входа и без привилегий" "$(role_flags "${RUN}-pg" ${role})" 'f f f f f f'
done
expect 'база lidradar принадлежит владельцу' "$(pg postgres "SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = 'lidradar'")" lidradar
expect 'владелец администрирует три роли приложения' "$(pg postgres "SELECT count(*) FROM pg_auth_members WHERE pg_get_userbyid(member) = 'lidradar' AND admin_option AND set_option")" 3
expect 'рабочий логин состоит в трёх ролях без права администрирования' "$(pg postgres "SELECT count(*) FROM pg_auth_members WHERE pg_get_userbyid(member) = 'lidradar_runtime' AND NOT admin_option AND set_option")" 3
check_fails 'администратор postgres по сети отклонён даже с верным паролем' tcp "${RUN}-pg" postgres "${ADMIN_PASSWORD}"
check 'владелец входит по сети с паролем' tcp "${RUN}-pg" lidradar "${OWNER_PASSWORD}"
check 'рабочий логин входит по сети с паролем' tcp "${RUN}-pg" lidradar_runtime "${RUNTIME_PASSWORD}"
check_fails 'неверный пароль владельца отклонён' tcp "${RUN}-pg" lidradar wrong
check 'администратор входит внутри контейнера по сокету' pg postgres 'SELECT 1'

# --- 2. миграции владельцем без суперправ --------------------------------------------------
migrate() { (cd "${ROOT}" && LIDRADAR_ENV=development LIDRADAR_DATABASE_URL="${OWNER_DSN}" go run ./backend/cmd/migrate); }
check 'все миграции проходят под владельцем без суперправ' migrate
check 'повторный запуск миграций ничего не меняет и не падает' migrate
expect 'схему создал владелец' "$(pg postgres "SELECT pg_get_userbyid(relowner) FROM pg_class WHERE relname = 'schema_migrations'")" lidradar
check 'проверка ролей и прав (владелец и рабочий логин) проходит' env LIDRADAR_BACKUP_MODE=container LIDRADAR_BACKUP_CONTAINER="${RUN}-pg" LIDRADAR_APP_DB_USER=lidradar_runtime \
  "${ROOT}/scripts/bootstrap-roles.sh" --verify-only lidradar

# --- 2a. генератор нагрузки под владельцем без суперправ (инструмент staging) -------------------
# COPY FROM не поддерживает таблицы с RLS: суперпользователь его обходит, а владелец под FORCE RLS нет.
generate() { (cd "${ROOT}" && LIDRADAR_ENV=staging LIDRADAR_DATABASE_ALLOW_PLAINTEXT=true LIDRADAR_DATABASE_URL="${OWNER_DSN}" \
  go run ./backend/cmd/load-generate --organizations 2 --conversations 20 --messages 3 --label rolesprobe); }
check 'load-generate работает под владельцем без суперправ (пакетные INSERT вместо COPY)' generate
expect 'генератор создал сообщения (2 организации × 20 переписок × 3 сообщения)' "$(pg postgres "SELECT count(*) FROM messages")" 120

# --- 3. что может и чего не может рабочий логин -----------------------------------------------
for statement in 'SET ROLE lidradar_app; SELECT count(*) FROM users' 'SET ROLE lidradar_worker; SELECT count(*) FROM jobs' 'SET ROLE lidradar_platform; SELECT count(*) FROM schema_migrations'; do
  check "рабочий логин: ${statement%%;*} и чтение через роль" allowed lidradar_runtime "${statement}"
done
for statement in 'ALTER TABLE audit_log DISABLE TRIGGER ALL' 'ALTER TABLE audit_log DISABLE TRIGGER audit_log_append_only' 'ALTER TABLE audit_log DISABLE ROW LEVEL SECURITY' 'ALTER TABLE audit_log NO FORCE ROW LEVEL SECURITY' \
  'DROP TABLE users' 'TRUNCATE users' 'CREATE TABLE evil (i int)' 'CREATE ROLE evil LOGIN' 'CREATE DATABASE evil' "COPY (SELECT 1) TO PROGRAM 'id'" \
  "SELECT pg_read_file('/etc/passwd')" "ALTER SYSTEM SET work_mem = '1GB'"; do
  check "рабочий логин не может: ${statement}" denied lidradar_runtime "${statement}"
done
for statement in 'CREATE ROLE evil LOGIN' "COPY (SELECT 1) TO PROGRAM 'id'" "SELECT pg_read_file('/etc/passwd')" "ALTER SYSTEM SET work_mem = '1GB'" 'CREATE EXTENSION file_fdw'; do
  check "владелец без суперправ не может: ${statement}" denied lidradar "${statement}"
done
check 'владелец может создать и удалить базу (восстановление, учения)' sh -c "docker exec ${RUN}-pg psql -X -q -U lidradar -d postgres -c 'CREATE DATABASE lr_probe' -c 'DROP DATABASE lr_probe'"

# --- 4. настоящие процессы под рабочим логином ------------------------------------------------
mkdir -p "${WORK}/bin"
(cd "${ROOT}" && go build -o "${WORK}/bin/" ./backend/cmd/api ./backend/cmd/worker ./backend/cmd/scheduler ./backend/cmd/platform-admin)
API_PORT="$(free_port)"
export LIDRADAR_ENV=development LIDRADAR_DATABASE_ALLOW_PLAINTEXT=true
LIDRADAR_DATABASE_URL="${RUNTIME_DSN}" LIDRADAR_HTTP_ADDRESS="127.0.0.1:${API_PORT}" "${WORK}/bin/api" >"${WORK}/api.log" 2>&1 &
PIDS+=($!)
LIDRADAR_DATABASE_URL="${RUNTIME_DSN}" "${WORK}/bin/worker" >"${WORK}/worker.log" 2>&1 &
PIDS+=($!)
LIDRADAR_DATABASE_URL="${RUNTIME_DSN}" "${WORK}/bin/scheduler" >"${WORK}/scheduler.log" 2>&1 &
PIDS+=($!)
for _ in $(seq 1 60); do curl -fsS "http://127.0.0.1:${API_PORT}/health/ready" >/dev/null 2>&1 && break; sleep 1; done
api() { curl -sS -b "${WORK}/jar" -c "${WORK}/jar" -X "$1" "http://127.0.0.1:${API_PORT}$2" -H 'Content-Type: application/json' -H "Origin: http://127.0.0.1:${API_PORT}" ${4:+-H "X-Tenant-ID: $4"} ${3:+-d "$3"} -o "${WORK}/response" -w '%{http_code}'; }
expect 'api готов: журнал миграций читается рабочим логином' "$(curl -sS "http://127.0.0.1:${API_PORT}/health/ready" | jq -r .migrations.applied)" "$(ls "${ROOT}/backend/platform/postgres/migrations" | tail -n 1 | sed 's/\.sql$//')"
expect 'регистрация пользователя через api' "$(api POST /api/v1/auth/register '{"email":"rt@example.test","password":"very-secure-password","displayName":"RT Owner"}')" 201
expect 'создание организации через api' "$(api POST /api/v1/organizations '{"name":"RT Studio","defaultTimezone":"Europe/Moscow"}')" 201
ORGANIZATION="$(jq -r .id "${WORK}/response")"
expect 'радар организации читается' "$(api GET /api/v1/radar '' "${ORGANIZATION}")" 200
expect 'подключение канала создаётся' "$(api POST /api/v1/integrations/GENERIC_WEBHOOK/connect '{"name":"RT webhook"}' "${ORGANIZATION}")" 201
check 'platform-admin выдаёт право под рабочим логином (права через членство, без SET ROLE)' env LIDRADAR_DATABASE_URL="${RUNTIME_DSN}" "${WORK}/bin/platform-admin" grant --email rt@example.test --note e2e
expect 'административный api отвечает после выдачи права' "$(api GET /api/v1/admin/me)" 200
sleep 5
expect 'все соединения процессов приложения идут от рабочего логина' "$(pg postgres "SELECT string_agg(DISTINCT usename, ',') FROM pg_stat_activity WHERE datname = 'lidradar' AND backend_type = 'client backend' AND pid <> pg_backend_pid()")" lidradar_runtime
expect 'суперпользователей среди клиентов базы нет' "$(pg postgres "SELECT count(*) FROM pg_stat_activity a JOIN pg_roles r ON r.rolname = a.usename WHERE r.rolsuper AND a.backend_type = 'client backend' AND a.pid <> pg_backend_pid()")" 0
check 'в журналах процессов нет отказов в правах' sh -c "! cat '${WORK}/api.log' '${WORK}/worker.log' '${WORK}/scheduler.log' | grep -qiE 'permission denied|must be owner|insufficient'"
kill "${PIDS[@]}" 2>/dev/null || true
wait 2>/dev/null || true
PIDS=()

# --- 5. копия рабочим логином и восстановление владельцем -----------------------------------------
export LIDRADAR_BACKUP_MODE=container LIDRADAR_BACKUP_CONTAINER="${RUN}-pg"
check 'копия снимается рабочим логином (он состоит в lidradar_platform)' env LIDRADAR_BACKUP_USER=lidradar_runtime LIDRADAR_BACKUP_DIR="${WORK}/dumps" "${ROOT}/scripts/backup.sh"
docker exec "${RUN}-pg" psql -X -q -U postgres -d lidradar -c 'CREATE ROLE lr_plain LOGIN' >/dev/null
check_fails 'копию обычной ролью без членства скрипт отказывается снимать' env LIDRADAR_BACKUP_USER=lr_plain LIDRADAR_BACKUP_DIR="${WORK}/plain" "${ROOT}/scripts/backup.sh"
DUMP="$(ls -1 "${WORK}/dumps"/lidradar-*.dump | head -n 1)"
check 'копия восстанавливается владельцем в новую базу (restore.sh)' env LIDRADAR_BACKUP_USER=lidradar "${ROOT}/scripts/restore.sh" "${DUMP}" lidradar_restored
check 'роли и права восстановленной базы выдаёт bootstrap-roles.sh под владельцем' env LIDRADAR_BACKUP_USER=lidradar LIDRADAR_APP_DB_USER=lidradar_runtime "${ROOT}/scripts/bootstrap-roles.sh" lidradar_restored
check 'рабочий логин читает восстановленную базу через роль' sh -c "docker exec ${RUN}-pg psql -X -At -U lidradar_runtime -d lidradar_restored -c 'SET ROLE lidradar_platform; SELECT count(*) FROM users'"
check 'владелец меняет базы местами переименованием (restore-offhost.sh --replace-database)' \
  docker exec "${RUN}-pg" psql -X -v ON_ERROR_STOP=1 -U lidradar -d postgres \
  -c 'ALTER DATABASE lidradar RENAME TO lidradar_replaced' -c 'ALTER DATABASE lidradar_restored RENAME TO lidradar'
expect 'после переименования рабочая база принадлежит владельцу' "$(pg postgres "SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = 'lidradar'")" lidradar

# --- 6. тот же файл на «внешней» базе, повторный запуск, отказы ------------------------------------
start_database "${RUN}-ext" no no
apply_roles() { # apply_roles ПАРОЛЬ_ВЛАДЕЛЬЦА [БАЗА]
  docker exec -i -e "LIDRADAR_DB_OWNER_PASSWORD=$1" -e "LIDRADAR_DB_RUNTIME_PASSWORD=${RUNTIME_PASSWORD}" "${RUN}-ext" \
    psql -X -v ON_ERROR_STOP=1 -U postgres -d "${2:-lidradar}" -f - <"${POSTGRES_DIR}/10-roles.sql"
}
check 'файл ролей применяется администратором к внешней базе' apply_roles "${OWNER_PASSWORD}"
expect 'внешняя база: владелец без суперправ' "$(role_flags "${RUN}-ext" lidradar)" 'f f t f t f'
expect 'внешняя база: база принадлежит владельцу' "$(sql "${RUN}-ext" postgres lidradar "SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = 'lidradar'")" lidradar
check 'повторное применение файла безопасно' apply_roles "${OWNER_PASSWORD}"
check 'повторное применение меняет пароль владельца' apply_roles "${OWNER_PASSWORD}-changed"
check_fails 'прежний пароль владельца после смены не действует' tcp "${RUN}-ext" lidradar "${OWNER_PASSWORD}"
check 'новый пароль владельца действует' tcp "${RUN}-ext" lidradar "${OWNER_PASSWORD}-changed"
check_fails 'файл ролей отказывает в служебной базе postgres' apply_roles "${OWNER_PASSWORD}" postgres
check_fails 'файл ролей отказывает без паролей' sh -c "docker exec -i ${RUN}-ext psql -X -v ON_ERROR_STOP=1 -U postgres -d lidradar -f - <'${POSTGRES_DIR}/10-roles.sql'"

echo
if [ "${FAILURES}" -eq 0 ]; then
  echo 'сквозная проверка ролей PostgreSQL: замечаний нет'
else
  echo "сквозная проверка ролей PostgreSQL: замечаний ${FAILURES}" >&2
  exit 1
fi
