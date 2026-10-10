#!/usr/bin/env bash
# Восстановление из копии вне хоста (ADR 0051, docs/runbooks/offhost-backup.md).
#
#   cd deploy/production
#   ../../scripts/restore-offhost.sh --identity ПУТЬ/к/age.key [--tier frequent|daily] [--stamp ШТАМП]
#                                    [--target-db lidradar_restored] [--replace-database]
#
# Порядок: контейнер backup скачивает копию, сверяет суммы и расшифровывает её (остальной
# стек не запускается) → scripts/restore.sh восстанавливает в НОВУЮ базу → scripts/
# bootstrap-roles.sh выдаёт роли и права. Текущую базу скрипт не трогает, пока не задан
# --replace-database: тогда при остановленных api, worker, scheduler и backup она
# переименовывается в <база>_replaced_<время>, а восстановленная становится рабочей.
# Удаления нет: прежнюю базу убирают вручную после проверки.
#
# Ключ доступа к хранилищу для чтения и перечня можно задать окружением команды
# (LIDRADAR_RESTORE_S3_ACCESS_KEY_ID, LIDRADAR_RESTORE_S3_SECRET_ACCESS_KEY): он заменяет
# ключ записи из .env только на время этого запуска. Приватный ключ age — только файлом.
set -euo pipefail
umask 077

TIER=frequent
STAMP=''
IDENTITY=''
TARGET_DB=lidradar_restored
REPLACE=0
LIVE_DB="${LIDRADAR_BACKUP_DB:-lidradar}"
DB_USER="${LIDRADAR_BACKUP_USER:-lidradar}"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

usage() {
  echo 'использование: restore-offhost.sh --identity ФАЙЛ_КЛЮЧА [--tier frequent|daily] [--stamp ШТАМП] [--target-db ИМЯ] [--replace-database]' >&2
  exit 2
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --replace-database) REPLACE=1; shift ;;
    --identity | --tier | --stamp | --target-db)
      [ "$#" -ge 2 ] || usage
      case "$1" in
        --identity) IDENTITY="$2" ;;
        --tier) TIER="$2" ;;
        --stamp) STAMP="$2" ;;
        --target-db) TARGET_DB="$2" ;;
      esac
      shift 2
      ;;
    *) usage ;;
  esac
done
[ -n "${IDENTITY}" ] || usage
[ -r "${IDENTITY}" ] || { echo "файл ключа age не читается: ${IDENTITY}" >&2; exit 2; }
case "${TIER}" in frequent | daily) ;; *) usage ;; esac
[[ -z "${STAMP}" || "${STAMP}" =~ ^[0-9]{8}T[0-9]{6}Z$ ]] || { echo 'штамп копии должен иметь вид 20261010T080000Z' >&2; exit 2; }
[[ "${TARGET_DB}" =~ ^[a-zA-Z_][a-zA-Z0-9_]{0,62}$ ]] || { echo 'недопустимое имя целевой базы' >&2; exit 2; }
case "${TARGET_DB}" in postgres | template0 | template1 | "${LIVE_DB}") echo 'целевая база должна быть новой и не совпадать с рабочей' >&2; exit 2 ;; esac
IDENTITY="$(cd "$(dirname "${IDENTITY}")" && pwd)/$(basename "${IDENTITY}")"

# По TCP: пока том инициализируется, временный сервер образа слушает только сокет и уже
# принимает подключения, а роли и пароли ещё не созданы.
docker compose exec -T postgres pg_isready -q -h 127.0.0.1 -U "${DB_USER}" -d postgres \
  || { echo 'PostgreSQL стека не запущен или ещё инициализируется: docker compose up -d --wait postgres' >&2; exit 1; }

if [ "${REPLACE}" -eq 1 ]; then
  RUNNING="$(docker compose ps --status running --services | grep -E '^(api|worker|scheduler|backup)$' || true)"
  if [ -n "${RUNNING}" ]; then
    echo "заменять базу можно только при остановленных сервисах: $(echo "${RUNNING}" | tr '\n' ' ')(docker compose stop api worker scheduler backup)" >&2
    exit 1
  fi
fi

WORK="$(mktemp -d)"
# Расшифрованная выгрузка — открытый текст всех данных: каталог удаляется при любом выходе.
trap 'rm -rf "${WORK}"' EXIT

# Ключ хранилища для восстановления передаётся именем переменной, а не значением в командной строке.
EXTRA_ENV=()
if [ -n "${LIDRADAR_RESTORE_S3_ACCESS_KEY_ID:-}" ]; then
  [ -n "${LIDRADAR_RESTORE_S3_SECRET_ACCESS_KEY:-}" ] \
    || { echo 'вместе с LIDRADAR_RESTORE_S3_ACCESS_KEY_ID задайте LIDRADAR_RESTORE_S3_SECRET_ACCESS_KEY' >&2; exit 2; }
  export LIDRADAR_BACKUP_S3_ACCESS_KEY_ID="${LIDRADAR_RESTORE_S3_ACCESS_KEY_ID}"
  export LIDRADAR_BACKUP_S3_SECRET_ACCESS_KEY="${LIDRADAR_RESTORE_S3_SECRET_ACCESS_KEY}"
  EXTRA_ENV=(-e LIDRADAR_BACKUP_S3_ACCESS_KEY_ID -e LIDRADAR_BACKUP_S3_SECRET_ACCESS_KEY)
fi
FETCH_ARGS=(fetch --identity /run/identity --tier "${TIER}" --output /restore)
[ -z "${STAMP}" ] || FETCH_ARGS+=(--stamp "${STAMP}")

echo "== 1. копия скачивается, сверяется и расшифровывается (контейнер backup)"
docker compose run --rm -T --no-deps --user "$(id -u):$(id -g)" \
  -v "${WORK}:/restore" -v "${IDENTITY}:/run/identity:ro" -e LIDRADAR_BACKUP_SPOOL=/restore/.spool \
  ${EXTRA_ENV[@]+"${EXTRA_ENV[@]}"} backup "${FETCH_ARGS[@]}" >"${WORK}/fetch.json"
DUMP="$(ls -1 "${WORK}"/lidradar-*.dump 2>/dev/null | head -n 1 || true)"
[ -n "${DUMP}" ] || { echo 'контейнер не оставил расшифрованную выгрузку' >&2; exit 1; }
cat "${WORK}/fetch.json"

echo "== 2. восстановление в новую базу ${TARGET_DB}"
"${SCRIPT_DIR}/restore.sh" "${DUMP}" "${TARGET_DB}"
echo "== 3. роли и права"
"${SCRIPT_DIR}/bootstrap-roles.sh" "${TARGET_DB}"

REPLACED="${LIVE_DB}_replaced_$(date -u +%Y%m%d%H%M%S)"
if [ "${REPLACE}" -eq 1 ]; then
  echo "== 4. ${LIVE_DB} становится ${REPLACED}, ${TARGET_DB} становится ${LIVE_DB}"
  docker compose exec -T postgres psql -v ON_ERROR_STOP=1 -U "${DB_USER}" -d postgres <<SQL
ALTER DATABASE "${LIVE_DB}" RENAME TO "${REPLACED}";
ALTER DATABASE "${TARGET_DB}" RENAME TO "${LIVE_DB}";
SQL
  echo "готово: рабочая база восстановлена, прежняя сохранена как ${REPLACED}"
  echo 'дальше: docker compose up -d, приёмка (scripts/smoke-production.sh), удалить приватный ключ age с хоста'
else
  echo "готово: копия восстановлена в ${TARGET_DB}, рабочая база ${LIVE_DB} не тронута"
  echo "чтобы сделать её рабочей при остановленных сервисах: docker compose stop api worker scheduler backup; затем"
  echo "  ALTER DATABASE \"${LIVE_DB}\" RENAME TO \"${REPLACED}\"; ALTER DATABASE \"${TARGET_DB}\" RENAME TO \"${LIVE_DB}\";"
  echo "или повторите команду с --replace-database"
fi
