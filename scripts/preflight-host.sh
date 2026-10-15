#!/usr/bin/env bash
# Проверка нового хоста перед первым запуском боевого стека (ADR 0050, docs/runbooks/first-release.md).
#
#   scripts/preflight-host.sh --domain app.example.ru [--public-ip АДРЕС] [--smtp ХОСТ:ПОРТ]
#                             [--s3-endpoint URL] [--compose-dir ПУТЬ] [--pull-images]
#
# Запускается на самом хосте до `docker compose up`. Ничего не устанавливает и не меняет (с --pull-images
# только скачивает два сторонних образа). Проверяет: систему и версии Docker, память и диск, часы, свободные
# порты 80 и 443, DNS домена, выход наружу (Docker Hub, Let's Encrypt, Telegram, SMTP, хранилище копий).
#
# Код 0 — блокирующих замечаний нет (предупреждения возможны), 1 — есть блокирующее замечание, 2 — неверные
# аргументы. Скрипт совместим с bash 3.2 и стандартными утилитами Ubuntu/Debian.
set -uo pipefail

DOMAIN=""
PUBLIC_IP=""
SMTP_TARGET="smtp.gmail.com:587"
S3_ENDPOINT=""
COMPOSE_DIR=""
PULL_IMAGES=0

usage() {
  echo 'использование: preflight-host.sh --domain ДОМЕН [--public-ip АДРЕС] [--smtp ХОСТ:ПОРТ] [--s3-endpoint URL] [--compose-dir ПУТЬ] [--pull-images]' >&2
  exit 2
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --domain) [ "$#" -ge 2 ] || usage; DOMAIN="$2"; shift ;;
    --public-ip) [ "$#" -ge 2 ] || usage; PUBLIC_IP="$2"; shift ;;
    --smtp) [ "$#" -ge 2 ] || usage; SMTP_TARGET="$2"; shift ;;
    --s3-endpoint) [ "$#" -ge 2 ] || usage; S3_ENDPOINT="$2"; shift ;;
    --compose-dir) [ "$#" -ge 2 ] || usage; COMPOSE_DIR="$2"; shift ;;
    --pull-images) PULL_IMAGES=1 ;;
    *) usage ;;
  esac
  shift
done

# Шаблоны в переменных: так выражения одинаково работают в bash 3.2 и в новых версиях.
DOMAIN_PATTERN='^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)+$'
IPV4_PATTERN='^[0-9]{1,3}(\.[0-9]{1,3}){3}$'
SMTP_PATTERN='^[A-Za-z0-9.-]+:[0-9]{1,5}$'
S3_PATTERN='^https?://[^[:space:]/]+(/.*)?$'
[[ "${DOMAIN}" =~ ${DOMAIN_PATTERN} ]] || {
  echo 'домен должен быть именем вида app.example.ru' >&2
  usage
}
[ -z "${PUBLIC_IP}" ] || [[ "${PUBLIC_IP}" =~ ${IPV4_PATTERN} ]] || { echo '--public-ip: ожидается адрес IPv4' >&2; usage; }
[[ "${SMTP_TARGET}" =~ ${SMTP_PATTERN} ]] || { echo '--smtp: ожидается ХОСТ:ПОРТ' >&2; usage; }
[ -z "${S3_ENDPOINT}" ] || [[ "${S3_ENDPOINT}" =~ ${S3_PATTERN} ]] || { echo '--s3-endpoint: ожидается адрес http(s)://хост' >&2; usage; }
SMTP_HOST="${SMTP_TARGET%:*}"
SMTP_PORT="${SMTP_TARGET##*:}"

FAILURES=0
WARNINGS=0
ok() { printf 'ok    %s\n' "$1"; }
warn() { printf 'warn  %s\n' "$1"; WARNINGS=$((WARNINGS + 1)); }
bad() { printf 'FAIL  %s\n' "$1"; FAILURES=$((FAILURES + 1)); }
section() { printf '\n== %s ==\n' "$1"; }

# --- хост ------------------------------------------------------------------------------------
section 'Хост'
KERNEL="$(uname -s 2>/dev/null || true)"
MACHINE="$(uname -m 2>/dev/null || true)"
if [ "${KERNEL}" != Linux ]; then
  bad "система ${KERNEL:-неизвестна}: боевой стек рассчитан на Linux"
else
  case "${MACHINE}" in
    x86_64 | amd64) ok 'Linux x86_64: образы выпуска собирайте с DOCKER_DEFAULT_PLATFORM=linux/amd64' ;;
    aarch64 | arm64) ok 'Linux arm64: образы выпуска собирайте с DOCKER_DEFAULT_PLATFORM=linux/arm64' ;;
    *) warn "архитектура ${MACHINE:-неизвестна} не проверялась (ожидались x86_64 или arm64)" ;;
  esac
fi

if ! command -v docker >/dev/null 2>&1; then
  bad 'docker не найден: установите Docker Engine и плагин Compose v2'
else
  DOCKER_SERVER="$(docker version --format '{{.Server.Version}}' 2>/dev/null || true)"
  if [ -z "${DOCKER_SERVER}" ]; then
    bad 'демон Docker не отвечает: он не запущен или у пользователя нет прав (нужна группа docker)'
  else
    ok "Docker ${DOCKER_SERVER}"
  fi
  COMPOSE_VERSION="$(docker compose version --short 2>/dev/null || true)"
  COMPOSE_VERSION="${COMPOSE_VERSION#v}"
  if [ -z "${COMPOSE_VERSION}" ]; then
    bad 'плагин Docker Compose v2 не найден (docker compose version)'
  else
    COMPOSE_MAJOR="${COMPOSE_VERSION%%.*}"
    COMPOSE_MINOR="$(printf '%s' "${COMPOSE_VERSION}" | cut -d. -f2)"
    if ! [[ "${COMPOSE_MAJOR}" =~ ^[0-9]+$ ]] || ! [[ "${COMPOSE_MINOR}" =~ ^[0-9]+$ ]]; then
      warn "версия Compose ${COMPOSE_VERSION} не разобрана"
    elif [ "${COMPOSE_MAJOR}" -lt 2 ] || { [ "${COMPOSE_MAJOR}" -eq 2 ] && [ "${COMPOSE_MINOR}" -lt 20 ]; }; then
      warn "Compose ${COMPOSE_VERSION} старее проверенной 2.35.1: файл стека может не разобраться"
    else
      ok "Compose ${COMPOSE_VERSION}"
    fi
  fi
fi

MEMINFO="${PREFLIGHT_MEMINFO:-/proc/meminfo}"
MEM_KB="$(awk '/^MemTotal:/ {print $2}' "${MEMINFO}" 2>/dev/null || true)"
if ! [[ "${MEM_KB}" =~ ^[0-9]+$ ]]; then
  warn 'объём памяти прочитать не удалось'
else
  MEM_GB="$(awk -v kb="${MEM_KB}" 'BEGIN { printf "%.1f", kb / 1048576 }')"
  if [ "${MEM_KB}" -lt 2097152 ]; then
    bad "памяти ${MEM_GB} ГБ: меньше 2 ГБ, стек не поместится (пределы контейнеров в сумме около 2,8 ГБ)"
  elif [ "${MEM_KB}" -lt 3670016 ]; then
    warn "памяти ${MEM_GB} ГБ: рекомендуется не меньше 4 ГБ"
  else
    ok "памяти ${MEM_GB} ГБ"
  fi
fi

DISK_DIR=/var/lib/docker
[ -d "${DISK_DIR}" ] || DISK_DIR=/
DISK_KB="$(df -Pk "${DISK_DIR}" 2>/dev/null | awk 'NR == 2 {print $4}' || true)"
if ! [[ "${DISK_KB}" =~ ^[0-9]+$ ]]; then
  warn "свободное место на ${DISK_DIR} прочитать не удалось"
else
  DISK_GB="$(awk -v kb="${DISK_KB}" 'BEGIN { printf "%.0f", kb / 1048576 }')"
  if [ "${DISK_KB}" -lt 10485760 ]; then
    bad "на ${DISK_DIR} свободно ${DISK_GB} ГБ: меньше 10 ГБ"
  elif [ "${DISK_KB}" -lt 15728640 ]; then
    warn "на ${DISK_DIR} свободно ${DISK_GB} ГБ: рекомендуется не меньше 15 ГБ"
  else
    ok "на ${DISK_DIR} свободно ${DISK_GB} ГБ"
  fi
fi

if command -v timedatectl >/dev/null 2>&1; then
  SYNCED="$(timedatectl show -p NTPSynchronized --value 2>/dev/null || true)"
  case "${SYNCED}" in
    yes) ok 'часы синхронизированы с NTP' ;;
    no) warn 'часы не синхронизированы (NTPSynchronized=no): сертификат и сроки токенов зависят от времени, включите chrony или systemd-timesyncd' ;;
    *) warn 'синхронизацию часов проверить не удалось' ;;
  esac
else
  warn 'timedatectl не найден: синхронизацию часов проверить не удалось'
fi

# --- порты -----------------------------------------------------------------------------------
section 'Порты'
port_busy() { # port_busy ПОРТ: печатает первую строку слушателя, если порт занят
  if command -v ss >/dev/null 2>&1; then
    ss -ltnH "sport = :$1" 2>/dev/null | head -n 1
  else
    if timeout 3 bash -c "exec 3<>/dev/tcp/127.0.0.1/$1" 2>/dev/null; then echo 'порт принимает соединения'; fi
  fi
}
for port in 80 443; do
  LISTENER="$(port_busy "${port}")"
  if [ -n "${LISTENER}" ]; then
    bad "порт ${port} занят: ${LISTENER} (нужен Caddy для сертификата и HTTPS)"
  else
    ok "порт ${port} свободен"
  fi
done

# --- DNS -------------------------------------------------------------------------------------
section "DNS ${DOMAIN}"
A_RECORDS="$(getent ahostsv4 "${DOMAIN}" 2>/dev/null | awk '{print $1}' | sort -u || true)"
if [ -z "${PUBLIC_IP}" ]; then
  for service in https://api.ipify.org https://ifconfig.me https://icanhazip.com; do
    CANDIDATE="$(curl -4 -fsS -m 8 "${service}" 2>/dev/null | tr -d '[:space:]' || true)"
    if [[ "${CANDIDATE}" =~ ${IPV4_PATTERN} ]]; then PUBLIC_IP="${CANDIDATE}"; break; fi
  done
fi
if [ -z "${A_RECORDS}" ]; then
  bad "A-запись ${DOMAIN} не найдена: создайте её у регистратора и дождитесь распространения"
elif [ -z "${PUBLIC_IP}" ]; then
  warn "внешний адрес хоста определить не удалось (задайте --public-ip); A-записи: $(printf '%s' "${A_RECORDS}" | tr '\n' ' ')"
elif printf '%s\n' "${A_RECORDS}" | grep -qx "${PUBLIC_IP}"; then
  ok "${DOMAIN} указывает на ${PUBLIC_IP}"
  if [ "$(printf '%s\n' "${A_RECORDS}" | wc -l | tr -d ' ')" -gt 1 ]; then
    warn "у ${DOMAIN} несколько A-записей ($(printf '%s' "${A_RECORDS}" | tr '\n' ' ')): лишние уберите, иначе часть клиентов уйдёт мимо хоста"
  fi
else
  bad "${DOMAIN} указывает на $(printf '%s' "${A_RECORDS}" | tr '\n' ' '), а внешний адрес хоста ${PUBLIC_IP}"
fi
AAAA_RECORDS="$(getent ahostsv6 "${DOMAIN}" 2>/dev/null | awk '{print $1}' | grep -vi '^::ffff:' | sort -u || true)"
if [ -n "${AAAA_RECORDS}" ]; then
  warn "у ${DOMAIN} есть AAAA-запись ($(printf '%s' "${AAAA_RECORDS}" | tr '\n' ' ')): адрес клиента по IPv6 за опубликованными портами Docker не проверен, на первом выпуске её лучше не создавать"
else
  ok 'AAAA-записи нет (для первого выпуска так и нужно)'
fi

# --- выход наружу ----------------------------------------------------------------------------
section 'Выход наружу'
http_code() { curl -sS -m 12 -o /dev/null -w '%{http_code}' "$1" 2>/dev/null || true; }

CODE="$(http_code https://registry-1.docker.io/v2/)"
case "${CODE}" in
  200 | 401) ok 'Docker Hub (registry-1.docker.io) отвечает' ;;
  *) warn "Docker Hub недоступен с хоста (HTTP ${CODE:-000}): образы caddy и postgres доставьте через docker save, как образы выпуска" ;;
esac

CODE="$(http_code https://acme-v02.api.letsencrypt.org/directory)"
if [ "${CODE}" = 200 ]; then ok "Let's Encrypt (acme-v02.api.letsencrypt.org) отвечает"; else bad "Let's Encrypt недоступен (HTTP ${CODE:-000}): сертификат не выпустится"; fi

CODE="$(http_code https://api.telegram.org/)"
case "${CODE}" in
  000 | '') bad 'Telegram Bot API (api.telegram.org) недоступен с хоста: уведомления и подключение ботов не заработают' ;;
  *) ok "Telegram Bot API (api.telegram.org) отвечает (HTTP ${CODE})" ;;
esac

BANNER="$(timeout 12 bash -c "exec 3<>/dev/tcp/${SMTP_HOST}/${SMTP_PORT} && read -r -t 10 line <&3 && printf '%s' \"\${line}\"" 2>/dev/null || true)"
case "${BANNER}" in
  220*) ok "SMTP ${SMTP_HOST}:${SMTP_PORT} отвечает баннером 220" ;;
  '') bad "SMTP ${SMTP_HOST}:${SMTP_PORT} недоступен: хостер, вероятно, закрыл исходящий порт; письма сброса пароля не уйдут (worker без почты не стартует). Спросите поддержку или возьмите другой порт/сервис" ;;
  *) warn "SMTP ${SMTP_HOST}:${SMTP_PORT} отвечает не баннером 220: ${BANNER}" ;;
esac

if [ -n "${S3_ENDPOINT}" ]; then
  CODE="$(http_code "${S3_ENDPOINT}")"
  case "${CODE}" in
    000 | '') bad "хранилище копий ${S3_ENDPOINT} недоступно с хоста: служба backup не сможет загружать копии" ;;
    *) ok "хранилище копий ${S3_ENDPOINT} отвечает (HTTP ${CODE})" ;;
  esac
else
  warn 'адрес хранилища копий не задан (--s3-endpoint): доступность не проверена'
fi

# --- сторонние образы ------------------------------------------------------------------------
if [ "${PULL_IMAGES}" -eq 1 ]; then
  section 'Сторонние образы стека'
  COMPOSE_FILE="${COMPOSE_DIR:-deploy/production}/compose.yaml"
  if [ ! -f "${COMPOSE_FILE}" ]; then
    bad "не найден ${COMPOSE_FILE}: укажите --compose-dir"
  else
    IMAGES="$(grep -E '^    image: (caddy|postgres):' "${COMPOSE_FILE}" | awk '{print $2}')"
    if [ -z "${IMAGES}" ]; then
      bad "в ${COMPOSE_FILE} не найдены образы caddy и postgres"
    else
      for image in ${IMAGES}; do
        if docker pull --quiet "${image}" >/dev/null 2>&1; then ok "образ ${image} скачан"; else bad "образ ${image} не скачался: доставьте его через docker save"; fi
      done
    fi
  fi
fi

printf '\n'
if [ "${FAILURES}" -gt 0 ]; then
  printf 'Итог: блокирующих замечаний %s, предупреждений %s\n' "${FAILURES}" "${WARNINGS}" >&2
  exit 1
fi
printf 'Итог: блокирующих замечаний нет, предупреждений %s\n' "${WARNINGS}"
