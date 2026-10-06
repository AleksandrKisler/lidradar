#!/usr/bin/env bash
# Логическая резервная копия PostgreSQL LidRadar в формате pg_dump -Fc с
# ротацией. По умолчанию выполняется внутри контейнера compose `postgres`,
# чтобы версия pg_dump совпадала с сервером; LIDRADAR_BACKUP_MODE=local
# использует pg_dump из PATH и LIDRADAR_DATABASE_URL.
set -euo pipefail
umask 077

BACKUP_DIR="${LIDRADAR_BACKUP_DIR:-backups}"
KEEP="${LIDRADAR_BACKUP_KEEP:-14}"
MODE="${LIDRADAR_BACKUP_MODE:-compose}"
DB_NAME="${LIDRADAR_BACKUP_DB:-lidradar}"
DB_USER="${LIDRADAR_BACKUP_USER:-lidradar}"
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
[[ "${KEEP}" =~ ^[1-9][0-9]*$ ]] || { echo 'LIDRADAR_BACKUP_KEEP must be a positive integer' >&2; exit 2; }

mkdir -p "${BACKUP_DIR}"
TEMP="$(mktemp "${BACKUP_DIR}/.lidradar-${STAMP}.XXXXXX")"
TARGET="${BACKUP_DIR}/lidradar-${STAMP}-${TEMP##*.}.dump"
trap 'rm -f -- "${TEMP}"' EXIT
case "${MODE}" in
  compose)
    docker compose exec -T postgres pg_dump -U "${DB_USER}" -Fc --no-owner --no-privileges "${DB_NAME}" > "${TEMP}"
    ;;
  container)
    docker exec "${LIDRADAR_BACKUP_CONTAINER:?LIDRADAR_BACKUP_CONTAINER is required}" pg_dump -U "${DB_USER}" -Fc --no-owner --no-privileges "${DB_NAME}" > "${TEMP}"
    ;;
  local)
    pg_dump --dbname="${LIDRADAR_DATABASE_URL:?LIDRADAR_DATABASE_URL is required}" -Fc --no-owner --no-privileges > "${TEMP}"
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
