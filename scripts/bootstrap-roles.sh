#!/usr/bin/env bash
# Роли PostgreSQL и права LidRadar для восстановленной базы.
#
# pg_dump --no-owner --no-privileges не переносит роли кластера, их членство и
# права на объекты, поэтому после scripts/restore.sh на новом кластере API не
# стартует (role "lidradar_app" does not exist). cmd/migrate не поможет: миграция
# 000020 уже записана в журнале и повторно не выполняется.
#
#   scripts/bootstrap-roles.sh [--verify-only] [база]      (по умолчанию lidradar_restore)
#
# Скрипт выполняет scripts/sql/bootstrap-roles.sql (одна транзакция, повторный
# запуск безопасен, ничего не отзывает и не удаляет) и затем
# scripts/sql/verify-roles.sql: роли, членство, права на схему, таблицы и
# последовательности, права по умолчанию для будущих миграций, владельцы объектов,
# принудительный RLS и политики. Любая найденная проблема даёт код 1.
# --verify-only только проверяет и ничего не меняет.
#
# Режимы и переменные те же, что у restore.sh: LIDRADAR_BACKUP_MODE=compose
# (по умолчанию), container (LIDRADAR_BACKUP_CONTAINER) или local
# (LIDRADAR_ADMIN_DATABASE_URL, нужны psql и python3); LIDRADAR_BACKUP_USER.
# Запускать под пользователем, который владеет восстановленными объектами и
# запускает миграции: восстанавливайте базу под тем же пользователем.
# LIDRADAR_APP_DB_USER — отдельный пользователь, под которым работают API, worker
# и scheduler; он тоже получает членство в ролях.
set -euo pipefail

VERIFY_ONLY=0
if [ "${1:-}" = "--verify-only" ]; then
  VERIFY_ONLY=1
  shift
fi
[ "$#" -le 1 ] || { echo 'использование: bootstrap-roles.sh [--verify-only] [база]' >&2; exit 2; }
TARGET_DB="${1:-lidradar_restore}"
MODE="${LIDRADAR_BACKUP_MODE:-compose}"
DB_USER="${LIDRADAR_BACKUP_USER:-lidradar}"
APP_USER="${LIDRADAR_APP_DB_USER:-}"
SQL_DIR="$(cd "$(dirname "$0")" && pwd)/sql"
[[ "${TARGET_DB}" =~ ^[a-zA-Z_][a-zA-Z0-9_]{0,62}$ ]] || { echo 'invalid target database name' >&2; exit 2; }
case "${TARGET_DB}" in postgres|template0|template1) echo 'reserved target database' >&2; exit 2 ;; esac
[[ -z "${APP_USER}" || "${APP_USER}" =~ ^[a-zA-Z_][a-zA-Z0-9_]{0,62}$ ]] || { echo 'invalid LIDRADAR_APP_DB_USER' >&2; exit 2; }

# Всё проверяется до первого обращения к базе.
case "${MODE}" in
  compose | container) ;;
  local)
    : "${LIDRADAR_ADMIN_DATABASE_URL:?LIDRADAR_ADMIN_DATABASE_URL is required}"
    TARGET_URL="$(LIDRADAR_RESTORE_TARGET="${TARGET_DB}" python3 - <<'PY'
import os
from urllib.parse import urlsplit, urlunsplit, parse_qsl
admin = urlsplit(os.environ['LIDRADAR_ADMIN_DATABASE_URL'])
if admin.scheme not in ('postgres', 'postgresql') or not admin.hostname:
    raise SystemExit('admin database URL must be a PostgreSQL URI')
if any(k in ('dbname', 'host', 'port', 'user', 'service') for k, _ in parse_qsl(admin.query)):
    raise SystemExit('connection overrides in admin URL are not supported')
print(urlunsplit(admin._replace(path='/' + os.environ['LIDRADAR_RESTORE_TARGET'])))
PY
    )"
    ;;
  *) echo 'unknown LIDRADAR_BACKUP_MODE' >&2; exit 2 ;;
esac
[ -f "${SQL_DIR}/bootstrap-roles.sql" ] && [ -f "${SQL_DIR}/verify-roles.sql" ] || { echo 'SQL files are missing next to the script' >&2; exit 2; }

# SQL читается psql со стандартного ввода; параметры сеанса идут перед файлом.
# Имя пользователя приложения проверено выше и не содержит кавычек.
session_sql() {
  printf 'SET client_min_messages = warning;\n'
  if [ -n "${APP_USER}" ]; then
    printf "SET lidradar.app_user = '%s';\n" "${APP_USER}"
  fi
  cat "$1"
}

run_psql() {
  case "${MODE}" in
    compose) docker compose exec -T postgres psql -X -v ON_ERROR_STOP=1 -U "${DB_USER}" -d "${TARGET_DB}" "$@" ;;
    container) docker exec -i "${LIDRADAR_BACKUP_CONTAINER:?LIDRADAR_BACKUP_CONTAINER is required}" psql -X -v ON_ERROR_STOP=1 -U "${DB_USER}" -d "${TARGET_DB}" "$@" ;;
    local) psql -X -v ON_ERROR_STOP=1 "$@" "${TARGET_URL}" ;;
  esac
}

if [ "${VERIFY_ONLY}" -eq 0 ]; then
  session_sql "${SQL_DIR}/bootstrap-roles.sql" | run_psql -q -1 -f -
  echo "roles and grants applied to ${TARGET_DB}"
fi

PROBLEMS="$(session_sql "${SQL_DIR}/verify-roles.sql" | run_psql -q -At -f -)"
if [ -n "${PROBLEMS}" ]; then
  echo "roles and grants verification FAILED for ${TARGET_DB}:" >&2
  echo "${PROBLEMS}" >&2
  exit 1
fi
echo "roles and grants verified for ${TARGET_DB}"
