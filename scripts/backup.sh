#!/usr/bin/env bash
# Логическая резервная копия PostgreSQL LidRadar в формате pg_dump -Fc с
# ротацией. По умолчанию выполняется внутри контейнера compose `postgres`,
# чтобы версия pg_dump совпадала с сервером; LIDRADAR_BACKUP_MODE=local
# использует pg_dump из PATH и LIDRADAR_DATABASE_URL.
set -euo pipefail

BACKUP_DIR="${LIDRADAR_BACKUP_DIR:-backups}"
KEEP="${LIDRADAR_BACKUP_KEEP:-14}"
MODE="${LIDRADAR_BACKUP_MODE:-compose}"
DB_NAME="${LIDRADAR_BACKUP_DB:-lidradar}"
DB_USER="${LIDRADAR_BACKUP_USER:-lidradar}"
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
TARGET="${BACKUP_DIR}/lidradar-${STAMP}.dump"

mkdir -p "${BACKUP_DIR}"
case "${MODE}" in
  compose)
    docker compose exec -T postgres pg_dump -U "${DB_USER}" -Fc --no-owner --no-privileges "${DB_NAME}" > "${TARGET}"
    ;;
  container)
    docker exec "${LIDRADAR_BACKUP_CONTAINER:?LIDRADAR_BACKUP_CONTAINER is required}" pg_dump -U "${DB_USER}" -Fc --no-owner --no-privileges "${DB_NAME}" > "${TARGET}"
    ;;
  local)
    pg_dump --dbname="${LIDRADAR_DATABASE_URL:?LIDRADAR_DATABASE_URL is required}" -Fc --no-owner --no-privileges > "${TARGET}"
    ;;
  *)
    echo "неизвестный LIDRADAR_BACKUP_MODE: ${MODE}" >&2
    exit 2
    ;;
esac

SIZE="$(wc -c < "${TARGET}" | tr -d ' ')"
if [ "${SIZE}" -lt 1024 ]; then
  echo "резервная копия подозрительно мала: ${SIZE} байт" >&2
  exit 1
fi
echo "backup written: ${TARGET} (${SIZE} bytes)"

# Ротация: оставить KEEP последних копий.
ls -1t "${BACKUP_DIR}"/lidradar-*.dump 2>/dev/null | tail -n +"$((KEEP + 1))" | while read -r old; do
  rm -f -- "${old}"
  echo "rotated: ${old}"
done
