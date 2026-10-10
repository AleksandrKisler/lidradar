#!/usr/bin/env bash
# Общие функции копий вне хоста (ADR 0051). Файл подключают lidradar-backup и
# lidradar-backup-cycle; сам он ничего не выполняет. Код совместим с bash 3.2,
# чтобы тесты шли и на macOS.

SERVICE='lidradar-backup'
SPOOL="${LIDRADAR_BACKUP_SPOOL:-/spool}"
STATUS_FILE="${SPOOL}/status.json"
LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Обычный scripts/backup.sh в режиме local: он же проверяет, что роль видит все строки.
BACKUP_SH="${LIDRADAR_BACKUP_SCRIPT:-${LIB_DIR}/backup.sh}"

# Счётчики таблиц и последняя миграция: те же значения сверяет restore-drill.sh.
COUNTS_SQL="SELECT (SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public') || '|' || (SELECT coalesce(max(version), '') FROM schema_migrations) || '|' || (SELECT count(*) FROM organizations) || '|' || (SELECT count(*) FROM messages) || '|' || (SELECT count(*) FROM risk_signals) || '|' || (SELECT count(*) FROM revenue_events) || '|' || (SELECT count(*) FROM audit_log)"

now() { date -u +%s; }
# fmt_epoch ЭПОХА ФОРМАТ: GNU и busybox date понимают -d @N, BSD (macOS) — -r N.
fmt_epoch() { date -u -d "@$1" "+$2" 2>/dev/null || date -u -r "$1" "+$2"; }
iso_now() { fmt_epoch "$(now)" %Y-%m-%dT%H:%M:%SZ; }

# log УРОВЕНЬ СОБЫТИЕ СООБЩЕНИЕ [ключ=значение ...] — строка JSON в stderr, как у
# приложения. Секреты в сообщения не попадают: вызывающий передаёт только имена,
# размеры, времена и тексты ошибок инструментов.
log() {
  local level="$1" event="$2" message="$3" pair key
  shift 3
  local filter='{time:$time,level:$level,msg:$msg,service:$service,environment:$environment,event:$event}'
  local -a args=(--arg time "$(iso_now)" --arg level "${level}" --arg msg "${message}" --arg event "${event}" --arg service "${SERVICE}" --arg environment "${LIDRADAR_ENV:-}")
  for pair in "$@"; do
    key="${pair%%=*}"
    args+=(--arg "v_${key}" "${pair#*=}")
    filter="${filter} + {${key}: \$v_${key}}"
  done
  jq -nc "${args[@]}" "${filter}" >&2
}

# die СОБЫТИЕ СООБЩЕНИЕ [КОД]: ошибка настройки или запуска.
die() {
  log ERROR "$1" "$2"
  exit "${3:-2}"
}

# duration_seconds 10m -> 600 (допустимы 90, 90s, 10m, 2h).
duration_seconds() {
  local value="$1" number unit
  [[ "${value}" =~ ^([0-9]+)([smh]?)$ ]] || return 1
  number="${BASH_REMATCH[1]}"
  unit="${BASH_REMATCH[2]}"
  case "${unit}" in
    m) echo $((number * 60)) ;;
    h) echo $((number * 3600)) ;;
    *) echo "${number}" ;;
  esac
}

# seconds_until_next ИНТЕРВАЛ СЕЙЧАС: секунды до ближайшего момента, кратного
# интервалу по часам UTC. Расписание не плывёт и пропускает упущенные такты.
seconds_until_next() { echo $(($1 - ($2 % $1))); }

# human_duration СЕКУНДЫ -> 1h02m03s / 7m05s / 42s
human_duration() {
  local s="$1"
  if [ "${s}" -ge 3600 ]; then
    printf '%dh%02dm%02ds' $((s / 3600)) $((s % 3600 / 60)) $((s % 60))
  elif [ "${s}" -ge 60 ]; then
    printf '%dm%02ds' $((s / 60)) $((s % 60))
  else
    printf '%ds' "${s}"
  fi
}

sha256_of() { sha256sum "$1" | cut -d' ' -f1; }

# --- настройка --------------------------------------------------------------

# require_remote: удалённое хранилище rclone задано и имеет допустимый вид.
require_remote() {
  REMOTE="${LIDRADAR_BACKUP_REMOTE:-}"
  [ -n "${REMOTE}" ] || die backup.configuration_invalid 'не задан LIDRADAR_BACKUP_REMOTE (например offhost:бакет/префикс)'
  [[ "${REMOTE}" =~ ^(:[a-z0-9]+:|[A-Za-z0-9_][A-Za-z0-9_.-]*:)[A-Za-z0-9._/-]+$ ]] \
    || die backup.configuration_invalid 'LIDRADAR_BACKUP_REMOTE должен иметь вид имя:путь или :тип:путь без пробелов'
  REMOTE="${REMOTE%/}"
  mkdir -p "${SPOOL}"
  chmod 700 "${SPOOL}" 2>/dev/null || true
  configure_rclone
}

# configure_rclone: параметры rclone берутся из окружения, файла настроек нет.
configure_rclone() {
  export HOME="${HOME:-/tmp}"
  export RCLONE_CONFIG="${RCLONE_CONFIG:-/tmp/rclone.conf}"
  [ -e "${RCLONE_CONFIG}" ] || : >"${RCLONE_CONFIG}"
  export RCLONE_RETRIES="${RCLONE_RETRIES:-3}" RCLONE_LOW_LEVEL_RETRIES="${RCLONE_LOW_LEVEL_RETRIES:-5}"
  export RCLONE_CONTIMEOUT="${RCLONE_CONTIMEOUT:-20s}" RCLONE_TIMEOUT="${RCLONE_TIMEOUT:-2m}"
  # Загрузка не проверяет назначение перечнем и копией: ключу хватает записи и чтения объекта.
  export RCLONE_NO_CHECK_DEST=true
  [ -n "${LIDRADAR_BACKUP_S3_ACCESS_KEY_ID:-}" ] || return 0
  [ "${REMOTE%%:*}" = offhost ] \
    || die backup.configuration_invalid 'переменные LIDRADAR_BACKUP_S3_* описывают хранилище с именем offhost: LIDRADAR_BACKUP_REMOTE=offhost:бакет/префикс'
  [ -n "${LIDRADAR_BACKUP_S3_SECRET_ACCESS_KEY:-}" ] || die backup.configuration_invalid 'не задан LIDRADAR_BACKUP_S3_SECRET_ACCESS_KEY'
  export RCLONE_CONFIG_OFFHOST_TYPE=s3
  export RCLONE_CONFIG_OFFHOST_PROVIDER="${LIDRADAR_BACKUP_S3_PROVIDER:-Other}"
  export RCLONE_CONFIG_OFFHOST_ENDPOINT="${LIDRADAR_BACKUP_S3_ENDPOINT:-}"
  export RCLONE_CONFIG_OFFHOST_REGION="${LIDRADAR_BACKUP_S3_REGION:-}"
  export RCLONE_CONFIG_OFFHOST_ACCESS_KEY_ID="${LIDRADAR_BACKUP_S3_ACCESS_KEY_ID}"
  export RCLONE_CONFIG_OFFHOST_SECRET_ACCESS_KEY="${LIDRADAR_BACKUP_S3_SECRET_ACCESS_KEY}"
  export RCLONE_CONFIG_OFFHOST_NO_CHECK_BUCKET=true RCLONE_CONFIG_OFFHOST_NO_HEAD=true
}

# require_database: адрес базы для выгрузки задан.
require_database() {
  DB_URL="${LIDRADAR_BACKUP_DATABASE_URL:-}"
  [ -n "${DB_URL}" ] || die backup.configuration_invalid 'не задан LIDRADAR_BACKUP_DATABASE_URL'
}

# require_recipients: получатели age заданы и принимаются самим age. Приватных
# ключей у процесса нет: расшифровать копию может только владелец ключа.
require_recipients() {
  local list="${LIDRADAR_BACKUP_AGE_RECIPIENTS:-}" item
  [ -n "${list}" ] || die backup.configuration_invalid 'не задан LIDRADAR_BACKUP_AGE_RECIPIENTS (публичные ключи age1…, через запятую)'
  AGE_ARGS=()
  local IFS=','
  for item in ${list}; do
    item="${item// /}"
    [ -n "${item}" ] || continue
    printf '' | age -r "${item}" >/dev/null 2>&1 \
      || die backup.configuration_invalid 'LIDRADAR_BACKUP_AGE_RECIPIENTS содержит значение, которое age не принимает как публичный ключ'
    AGE_ARGS+=(-r "${item}")
  done
  [ "${#AGE_ARGS[@]}" -gt 0 ] || die backup.configuration_invalid 'LIDRADAR_BACKUP_AGE_RECIPIENTS пуст'
}

# remote_path ЯРУС ИМЯ -> имя:путь/ярус/имя
remote_path() { echo "${REMOTE}/$1/$2"; }

# --- работа с хранилищем ------------------------------------------------------

# tool_error ФАЙЛ: последняя непустая строка вывода инструмента (итог его ошибки), последние
# 260 символов: начало строки обычно служебное, причина стоит в конце.
tool_error() {
  grep -v '^[[:space:]]*$' "$1" 2>/dev/null | tail -n 1 | tr -d '\r' | rev | cut -c1-260 | rev
}

# upload_object ФАЙЛ УДАЛЁННЫЙ_ПУТЬ: ошибка rclone остаётся в $UPLOAD_ERROR.
upload_object() {
  UPLOAD_ERROR=''
  rclone copyto "$1" "$2" 2>"${SPOOL}/.rclone.err" && return 0
  UPLOAD_ERROR="$(tool_error "${SPOOL}/.rclone.err")"
  return 1
}

# verify_object ФАЙЛ УДАЛЁННЫЙ_ПУТЬ: объект скачивается обратно и сверяется с файлом
# по SHA-256. Скачивание через copyto требует от ключа только чтения объекта; перечень
# и rclone cat/hashsum, которым нужен листинг, здесь намеренно не используются.
verify_object() {
  local back="${SPOOL}/.verify.$$"
  rm -f "${back}"
  if ! rclone copyto "$2" "${back}" 2>"${SPOOL}/.rclone.err"; then
    UPLOAD_ERROR="объект не прочитан из хранилища после загрузки: $(tool_error "${SPOOL}/.rclone.err")"
    rm -f "${back}"
    return 1
  fi
  if [ "$(sha256_of "${back}")" = "$(sha256_of "$1")" ]; then
    rm -f "${back}"
    return 0
  fi
  rm -f "${back}"
  UPLOAD_ERROR='объект, прочитанный из хранилища, не совпал с загруженным файлом'
  return 1
}

# --- состояние ----------------------------------------------------------------

# status_read: повреждённый или пустой файл состояния читается как пустой объект.
status_read() {
  if [ -s "${STATUS_FILE}" ] && jq -e 'type == "object"' "${STATUS_FILE}" >/dev/null 2>&1; then
    cat "${STATUS_FILE}"
  else
    echo '{}'
  fi
}

# status_write: JSON со стандартного входа записывается атомарно.
status_write() {
  local tmp="${STATUS_FILE}.tmp.$$"
  cat >"${tmp}" && mv -f "${tmp}" "${STATUS_FILE}"
}

# ping URL — необязательный сигнал внешнему мониторингу. Адрес может содержать
# секрет, поэтому в журнал он не попадает; сбой сигнала итог копии не меняет.
ping() {
  [ -n "${1:-}" ] || return 0
  curl -fsS -m 10 --retry 2 --retry-delay 2 -o /dev/null "$1" 2>/dev/null \
    || log WARN backup.ping_failed 'не удалось отправить сигнал внешнему мониторингу'
}

# sql ЗАПРОС: одно значение из базы выгрузки.
sql() { psql -X -At -v ON_ERROR_STOP=1 "${DB_URL}" -c "$1"; }

# url_with_database АДРЕС ИМЯ_БАЗЫ: тот же адрес подключения к другой базе.
url_with_database() {
  local base="${1%%\?*}" rest
  rest="${1#"${base}"}"
  echo "${base%/*}/$2${rest}"
}

record_failure() { # record_failure ШАГ СООБЩЕНИЕ ЭПОХА
  status_read | jq --argjson at "$3" --arg step "$1" --arg message "$2" \
    '. + {version: 1, lastAttemptEpoch: $at, lastAttemptOk: false, lastError: ($step + ": " + ($message | .[-260:])), consecutiveFailures: ((.consecutiveFailures // 0) + 1)}' | status_write
}

counts_json() { # counts_json СТРОКА_ЧЕРЕЗ_| — разбор ответа COUNTS_SQL
  jq -n --arg row "$1" '$row | split("|") | {tables: (.[0] | tonumber), migration: .[1], organizations: (.[2] | tonumber), messages: (.[3] | tonumber), risk_signals: (.[4] | tonumber), revenue_events: (.[5] | tonumber), audit_log: (.[6] | tonumber)}'
}
