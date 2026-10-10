#!/usr/bin/env bash
# Логическая резервная копия PostgreSQL LidRadar в формате pg_dump -Fc с
# ротацией. По умолчанию выполняется внутри контейнера compose `postgres`,
# чтобы версия pg_dump совпадала с сервером; LIDRADAR_BACKUP_MODE=local
# использует pg_dump и psql из PATH и LIDRADAR_DATABASE_URL.
#
# Таблицы с tenant_id защищены FORCE ROW LEVEL SECURITY (миграция 000020).
# pg_dump по умолчанию отключает политики, и от обычного владельца схемы
# выгрузка падает; с --enable-row-security она проходит, но роль без права
# обхода получила бы только строки, видимые политике, то есть тихо неполную
# копию. Поэтому до выгрузки проверяется, что роль видит все строки:
# суперпользователь, BYPASSRLS либо член lidradar_platform (его пропускает
# политика). Роли lidradar_platform нет только в базе без миграции 000020.
set -euo pipefail
umask 077

BACKUP_DIR="${LIDRADAR_BACKUP_DIR:-backups}"
KEEP="${LIDRADAR_BACKUP_KEEP:-14}"
MODE="${LIDRADAR_BACKUP_MODE:-compose}"
DB_NAME="${LIDRADAR_BACKUP_DB:-lidradar}"
DB_USER="${LIDRADAR_BACKUP_USER:-lidradar}"
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
[[ "${KEEP}" =~ ^[1-9][0-9]*$ ]] || { echo 'LIDRADAR_BACKUP_KEEP must be a positive integer' >&2; exit 2; }
case "${MODE}" in
  compose | container | local) ;;
  *) echo "неизвестный LIDRADAR_BACKUP_MODE: ${MODE}" >&2; exit 2 ;;
esac

SEES_ALL_ROWS_SQL="SELECT to_regrole('lidradar_platform') IS NULL OR r.rolsuper OR r.rolbypassrls OR pg_has_role(current_user, to_regrole('lidradar_platform'), 'MEMBER') FROM pg_roles AS r WHERE r.rolname = current_user"
role_sees_all_rows() {
  case "${MODE}" in
    compose) docker compose exec -T postgres psql -X -At -v ON_ERROR_STOP=1 -U "${DB_USER}" -d "${DB_NAME}" -c "${SEES_ALL_ROWS_SQL}" ;;
    container) docker exec "${LIDRADAR_BACKUP_CONTAINER:?LIDRADAR_BACKUP_CONTAINER is required}" psql -X -At -v ON_ERROR_STOP=1 -U "${DB_USER}" -d "${DB_NAME}" -c "${SEES_ALL_ROWS_SQL}" ;;
    local) psql -X -At -v ON_ERROR_STOP=1 "${LIDRADAR_DATABASE_URL:?LIDRADAR_DATABASE_URL is required}" -c "${SEES_ALL_ROWS_SQL}" ;;
  esac
}
# Сбой подключения останавливает скрипт собственной ошибкой psql; отказ роли
# отличается от него сообщением ниже. Файлов на этом этапе ещё нет.
SEES_ALL_ROWS="$(role_sees_all_rows)"
if [ "${SEES_ALL_ROWS}" != "t" ]; then
  echo 'роль копии не видит все строки таблиц с RLS: нужен суперпользователь, BYPASSRLS или членство в lidradar_platform; иначе копия окажется неполной' >&2
  exit 1
fi

mkdir -p "${BACKUP_DIR}"
TEMP="$(mktemp "${BACKUP_DIR}/.lidradar-${STAMP}.XXXXXX")"
TARGET="${BACKUP_DIR}/lidradar-${STAMP}-${TEMP##*.}.dump"
trap 'rm -f -- "${TEMP}"' EXIT
case "${MODE}" in
  compose)
    docker compose exec -T postgres pg_dump -U "${DB_USER}" -Fc --no-owner --no-privileges --enable-row-security "${DB_NAME}" > "${TEMP}"
    ;;
  container)
    docker exec "${LIDRADAR_BACKUP_CONTAINER:?LIDRADAR_BACKUP_CONTAINER is required}" pg_dump -U "${DB_USER}" -Fc --no-owner --no-privileges --enable-row-security "${DB_NAME}" > "${TEMP}"
    ;;
  local)
    pg_dump --dbname="${LIDRADAR_DATABASE_URL:?LIDRADAR_DATABASE_URL is required}" -Fc --no-owner --no-privileges --enable-row-security > "${TEMP}"
    ;;
  *)
    echo "неизвестный LIDRADAR_BACKUP_MODE: ${MODE}" >&2
    exit 2
    ;;
esac

SIZE="$(wc -c < "${TEMP}" | tr -d ' ')"
if [ "${SIZE}" -lt 1024 ]; then
  echo "резервная копия подозрительно мала: ${SIZE} байт" >&2
  exit 1
fi
# Publish only completed archives; failed dumps never participate in rotation.
mv -- "${TEMP}" "${TARGET}"
echo "backup written: ${TARGET} (${SIZE} bytes)"

# Ротация: оставить KEEP последних копий.
ls -1t "${BACKUP_DIR}"/lidradar-*.dump 2>/dev/null | tail -n +"$((KEEP + 1))" | while read -r old; do
  rm -f -- "${old}"
  echo "rotated: ${old}"
done
