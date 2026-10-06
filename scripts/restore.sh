#!/usr/bin/env bash
# Восстановление логической копии в указанную базу (по умолчанию —
# lidradar_restore) внутри контейнера compose `postgres` либо локальным
# pg_restore (LIDRADAR_BACKUP_MODE=local). Создаёт только НОВУЮ базу.
set -euo pipefail

DUMP="${1:?использование: restore.sh <dump> [target_db]}"
TARGET_DB="${2:-lidradar_restore}"
MODE="${LIDRADAR_BACKUP_MODE:-compose}"
DB_USER="${LIDRADAR_BACKUP_USER:-lidradar}"
[[ "${TARGET_DB}" =~ ^[a-zA-Z_][a-zA-Z0-9_]{0,62}$ ]] || { echo 'invalid target database name' >&2; exit 2; }
[[ -r "${DUMP}" && -s "${DUMP}" ]] || { echo 'dump is missing, empty or unreadable' >&2; exit 2; }
case "${TARGET_DB}" in postgres|template0|template1) echo 'reserved target database' >&2; exit 2 ;; esac

# Validate format and configuration BEFORE creating anything. Existing databases
# are never dropped. A failed restore leaves its new database for diagnosis.
case "${MODE}" in
  compose) docker compose exec -T postgres pg_restore --list < "${DUMP}" >/dev/null ;;
  container) docker exec -i "${LIDRADAR_BACKUP_CONTAINER:?}" pg_restore --list < "${DUMP}" >/dev/null ;;
  local)
    : "${LIDRADAR_ADMIN_DATABASE_URL:?LIDRADAR_ADMIN_DATABASE_URL is required}"
    RESTORE_URL="$(LIDRADAR_RESTORE_TARGET="${TARGET_DB}" python3 - <<'PY'
import os
from urllib.parse import urlsplit, urlunsplit, parse_qsl
admin = urlsplit(os.environ['LIDRADAR_ADMIN_DATABASE_URL'])
if admin.scheme not in ('postgres', 'postgresql') or not admin.hostname:
    raise SystemExit('admin database URL must be a PostgreSQL URI')
if any(k in ('dbname', 'host', 'port', 'user', 'service') for k, _ in parse_qsl(admin.query)):
    raise SystemExit('connection overrides in admin URL are not supported')
target = urlunsplit(admin._replace(path='/' + os.environ['LIDRADAR_RESTORE_TARGET']))
legacy = os.environ.get('LIDRADAR_RESTORE_DATABASE_URL')
if legacy and urlsplit(legacy) != urlsplit(target):
    raise SystemExit('restore URL disagrees with admin cluster and target database')
print(target)
PY
    )"
    pg_restore --list "${DUMP}" >/dev/null
    ;;
  *) echo 'unknown LIDRADAR_BACKUP_MODE' >&2; exit 2 ;;
esac

run_psql() {
  case "${MODE}" in
    compose) docker compose exec -T postgres psql -v ON_ERROR_STOP=1 -U "${DB_USER}" -d postgres "$@" ;;
    container) docker exec -i "${LIDRADAR_BACKUP_CONTAINER:?}" psql -v ON_ERROR_STOP=1 -U "${DB_USER}" -d postgres "$@" ;;
    local) psql -v ON_ERROR_STOP=1 "${LIDRADAR_ADMIN_DATABASE_URL:?LIDRADAR_ADMIN_DATABASE_URL is required}" "$@" ;;
  esac
}

run_psql -c "CREATE DATABASE \"${TARGET_DB}\""
case "${MODE}" in
  compose) docker compose exec -T postgres pg_restore -U "${DB_USER}" -d "${TARGET_DB}" --no-owner --no-privileges --exit-on-error < "${DUMP}" ;;
  container) docker exec -i "${LIDRADAR_BACKUP_CONTAINER:?}" pg_restore -U "${DB_USER}" -d "${TARGET_DB}" --no-owner --no-privileges --exit-on-error < "${DUMP}" ;;
  local) pg_restore --dbname="${RESTORE_URL}" --no-owner --no-privileges --exit-on-error "${DUMP}" ;;
esac
echo "restored ${DUMP} into ${TARGET_DB}"
