#!/usr/bin/env bash
# Учение по восстановлению (LR-BE-2408): свежая копия восстанавливается в
# отдельную базу, сравниваются число таблиц, последняя миграция и счётчики
# ключевых таблиц; база учения удаляется. Ненулевой код — восстановление
# не воспроизводит данные.
set -euo pipefail

MODE="${LIDRADAR_BACKUP_MODE:-compose}"
DB_NAME="${LIDRADAR_BACKUP_DB:-lidradar}"
DB_USER="${LIDRADAR_BACKUP_USER:-lidradar}"
DRILL_DB="${LIDRADAR_DRILL_DB:-lidradar_restore_drill}"
export LIDRADAR_BACKUP_DIR="${LIDRADAR_BACKUP_DIR:-backups}"

query() {
  local database="$1" sql="$2"
  case "${MODE}" in
    compose) docker compose exec -T postgres psql -At -v ON_ERROR_STOP=1 -U "${DB_USER}" -d "${database}" -c "${sql}" ;;
    container) docker exec -i "${LIDRADAR_BACKUP_CONTAINER:?}" psql -At -v ON_ERROR_STOP=1 -U "${DB_USER}" -d "${database}" -c "${sql}" ;;
    local) psql -At -v ON_ERROR_STOP=1 "${LIDRADAR_ADMIN_DATABASE_URL:?}" -d "${database}" -c "${sql}" ;;
  esac
}

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
"${SCRIPT_DIR}/backup.sh" >/dev/null
DUMP="$(ls -1t "${LIDRADAR_BACKUP_DIR}"/lidradar-*.dump | head -n 1)"
"${SCRIPT_DIR}/restore.sh" "${DUMP}" "${DRILL_DB}" >/dev/null

CHECK_SQL="SELECT (SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public') || '|' || (SELECT coalesce(max(version), '') FROM schema_migrations) || '|' || (SELECT count(*) FROM organizations) || '|' || (SELECT count(*) FROM messages) || '|' || (SELECT count(*) FROM risk_signals) || '|' || (SELECT count(*) FROM revenue_events) || '|' || (SELECT count(*) FROM audit_log)"
SOURCE="$(query "${DB_NAME}" "${CHECK_SQL}")"
RESTORED="$(query "${DRILL_DB}" "${CHECK_SQL}")"
query postgres "DROP DATABASE IF EXISTS \"${DRILL_DB}\"" >/dev/null

echo "source:   ${SOURCE}"
echo "restored: ${RESTORED}"
if [ "${SOURCE}" != "${RESTORED}" ]; then
  echo "restore drill FAILED: restored data differs from source" >&2
  exit 1
fi
echo "restore drill OK: ${DUMP}"
