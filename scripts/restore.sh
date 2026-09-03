#!/usr/bin/env bash
# Восстановление логической копии в указанную базу (по умолчанию —
# lidradar_restore) внутри контейнера compose `postgres` либо локальным
# pg_restore (LIDRADAR_BACKUP_MODE=local). Целевая база пересоздаётся.
set -euo pipefail

DUMP="${1:?использование: restore.sh <dump> [target_db]}"
TARGET_DB="${2:-lidradar_restore}"
MODE="${LIDRADAR_BACKUP_MODE:-compose}"
DB_USER="${LIDRADAR_BACKUP_USER:-lidradar}"

run_psql() {
  case "${MODE}" in
    compose) docker compose exec -T postgres psql -v ON_ERROR_STOP=1 -U "${DB_USER}" -d postgres "$@" ;;
    container) docker exec -i "${LIDRADAR_BACKUP_CONTAINER:?}" psql -v ON_ERROR_STOP=1 -U "${DB_USER}" -d postgres "$@" ;;
    local) psql -v ON_ERROR_STOP=1 "${LIDRADAR_ADMIN_DATABASE_URL:?LIDRADAR_ADMIN_DATABASE_URL is required}" "$@" ;;
  esac
}

run_psql -c "DROP DATABASE IF EXISTS \"${TARGET_DB}\""
run_psql -c "CREATE DATABASE \"${TARGET_DB}\""
case "${MODE}" in
  compose) docker compose exec -T postgres pg_restore -U "${DB_USER}" -d "${TARGET_DB}" --no-owner --no-privileges --exit-on-error < "${DUMP}" ;;
  container) docker exec -i "${LIDRADAR_BACKUP_CONTAINER:?}" pg_restore -U "${DB_USER}" -d "${TARGET_DB}" --no-owner --no-privileges --exit-on-error < "${DUMP}" ;;
  local) pg_restore --dbname="${LIDRADAR_RESTORE_DATABASE_URL:?LIDRADAR_RESTORE_DATABASE_URL is required}" --no-owner --no-privileges --exit-on-error "${DUMP}" ;;
esac
echo "restored ${DUMP} into ${TARGET_DB}"
