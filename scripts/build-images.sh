#!/usr/bin/env bash
# Сборка неизменяемых образов выпуска LidRadar (LR-BE-2601, ADR 0050).
#
#   scripts/build-images.sh [--allow-dirty] [--tag ТЕГ] [--only команда,...] [--web-dir ПУТЬ | --no-web]
#
# Образы lidradar-<команда>:<тег> собираются из текущего коммита для команд api,
# worker, scheduler, migrate, ai-agent, platform-admin, ai-node-register и
# ai-node-manage, lidradar-backup:<тег> (копии вне хоста, deploy/backup/Dockerfile) —
# из того же коммита, а lidradar-web:<тег> — из каталога веб-клиента (--web-dir или
# LIDRADAR_WEB_DIR). Тег по умолчанию — первые 12 символов git sha: по тегу
# всегда видно, какой коммит собран, а /health/ready показывает те же версию и
# ревизию. Образ нельзя собрать из незафиксированного дерева: --allow-dirty
# разрешён только для проб и добавляет к тегу суффикс -dirty.
set -euo pipefail

COMMANDS="api worker scheduler migrate ai-agent platform-admin ai-node-register ai-node-manage backup"
ALLOW_DIRTY=0
TAG=""
WEB_DIR="${LIDRADAR_WEB_DIR:-}"
BUILD_WEB=1

usage() {
  echo 'использование: build-images.sh [--allow-dirty] [--tag ТЕГ] [--only команда,...] [--web-dir ПУТЬ | --no-web]' >&2
  exit 2
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --allow-dirty) ALLOW_DIRTY=1 ;;
    --tag) [ "$#" -ge 2 ] && [ -n "$2" ] || usage; TAG="$2"; shift ;;
    --only) [ "$#" -ge 2 ] || usage; COMMANDS="${2//,/ }"; shift ;;
    --web-dir) [ "$#" -ge 2 ] || usage; WEB_DIR="$2"; shift ;;
    --no-web) BUILD_WEB=0 ;;
    *) usage ;;
  esac
  shift
done

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "${ROOT}"

KNOWN="api worker scheduler migrate ai-agent platform-admin ai-node-register ai-node-manage backup"
for command in ${COMMANDS}; do
  case " ${KNOWN} " in *" ${command} "*) ;; *) echo "неизвестная команда: ${command}" >&2; exit 2 ;; esac
done

REVISION="$(git rev-parse HEAD)"
DIRTY=""
if [ -n "$(git status --porcelain)" ]; then
  [ "${ALLOW_DIRTY}" -eq 1 ] || { echo 'в дереве есть незафиксированные изменения: образ выпуска собирается только из коммита (пробную сборку разрешает --allow-dirty)' >&2; exit 2; }
  DIRTY="-dirty"
fi
TAG="${TAG:-${REVISION:0:12}}${DIRTY}"
[[ "${TAG}" =~ ^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$ ]] || { echo 'недопустимый тег образа' >&2; exit 2; }

WEB_REVISION=""
if [ "${BUILD_WEB}" -eq 1 ]; then
  [ -n "${WEB_DIR}" ] || { echo 'не указан каталог веб-клиента: --web-dir ПУТЬ, LIDRADAR_WEB_DIR или --no-web' >&2; exit 2; }
  [ -f "${WEB_DIR}/Dockerfile" ] || { echo 'в каталоге веб-клиента нет Dockerfile' >&2; exit 2; }
  WEB_REVISION="$(git -C "${WEB_DIR}" rev-parse HEAD)"
  if [ -n "$(git -C "${WEB_DIR}" status --porcelain)" ] && [ "${ALLOW_DIRTY}" -ne 1 ]; then
    echo 'в дереве веб-клиента есть незафиксированные изменения: образ выпуска собирается только из коммита' >&2
    exit 2
  fi
fi

for command in ${COMMANDS}; do
  echo "== lidradar-${command}:${TAG}"
  if [ "${command}" = backup ]; then
    # Образ копий вне хоста: свой Dockerfile, контекст — корень репозитория.
    docker build -f deploy/backup/Dockerfile --build-arg "VERSION=${TAG}" --build-arg "REVISION=${REVISION}" \
      -t "lidradar-backup:${TAG}" .
  else
    docker build --build-arg "COMMAND=${command}" --build-arg "VERSION=${TAG}" --build-arg "REVISION=${REVISION}" \
      -t "lidradar-${command}:${TAG}" .
  fi
done

if [ "${BUILD_WEB}" -eq 1 ]; then
  echo "== lidradar-web:${TAG}"
  docker build --build-arg APP_MODE=production \
    --label "org.opencontainers.image.title=lidradar-web" \
    --label "org.opencontainers.image.version=${TAG}" \
    --label "org.opencontainers.image.revision=${WEB_REVISION}" \
    -t "lidradar-web:${TAG}" "${WEB_DIR}"
fi

echo
echo "выпуск ${TAG}: бэкенд ${REVISION}${WEB_REVISION:+, веб-клиент ${WEB_REVISION}}"
for command in ${COMMANDS}; do
  printf '  lidradar-%s:%s  %s\n' "${command}" "${TAG}" "$(docker image inspect --format '{{.Id}}' "lidradar-${command}:${TAG}")"
done
if [ "${BUILD_WEB}" -eq 1 ]; then
  printf '  lidradar-web:%s  %s\n' "${TAG}" "$(docker image inspect --format '{{.Id}}' "lidradar-web:${TAG}")"
fi
echo "в production-окружении задайте LIDRADAR_VERSION=${TAG}"
