#!/usr/bin/env bash
# Сквозная проверка образа копий вне хоста на настоящих инструментах (ADR 0051):
# PostgreSQL 18, S3-хранилище (rclone serve s3), age, pg_dump, pg_restore. Нужен Docker и
# собранный образ; внешняя сеть — только чтобы скачать postgres:18-alpine.
#
#   docker build -f deploy/backup/Dockerfile -t lidradar-backup:ci .
#   scripts/tests/e2e-offhost-backup.sh lidradar-backup:ci
#
# Код 0 — все проверки прошли. Контейнеры, сеть и тома удаляются при любом выходе.
set -euo pipefail

IMAGE="${1:?использование: e2e-offhost-backup.sh ОБРАЗ}"
RUN="lrbk-e2e-$$"
SPOOL_VOLUME="${RUN}-spool"
WORK="$(mktemp -d)"
FAILURES=0

cleanup() {
  docker rm -f "${RUN}-pg" "${RUN}-s3" >/dev/null 2>&1 || true
  docker network rm "${RUN}" >/dev/null 2>&1 || true
  docker volume rm "${SPOOL_VOLUME}" >/dev/null 2>&1 || true
  rm -rf "${WORK}"
}
trap cleanup EXIT

ok() { echo "ok    $1"; }
bad() { echo "FAIL  $1" >&2; FAILURES=$((FAILURES + 1)); }
check() { # check ОПИСАНИЕ КОМАНДА...: команда обязана завершиться успешно
  local description="$1"
  shift
  if "$@" >/dev/null 2>&1; then ok "${description}"; else bad "${description}"; fi
}
check_fails() { # check_fails ОПИСАНИЕ КОМАНДА...: команда обязана завершиться ошибкой
  local description="$1"
  shift
  if "$@" >/dev/null 2>&1; then bad "${description}"; else ok "${description}"; fi
}

docker network create "${RUN}" >/dev/null
docker volume create "${SPOOL_VOLUME}" >/dev/null

# --- стенд: база с данными, хранилище, ключи age ------------------------------------------
docker run -d --name "${RUN}-pg" --network "${RUN}" -e POSTGRES_PASSWORD=e2e-password -e POSTGRES_USER=lidradar -e POSTGRES_DB=lidradar \
  postgres:18-alpine >/dev/null
# Образ сначала поднимает временный сервер для инициализации: ждём, пока он завершится и
# запустится настоящий, иначе заполнение попадёт в остановку.
for _ in $(seq 1 90); do
  if docker logs "${RUN}-pg" 2>&1 | grep -q 'init process complete' && docker exec "${RUN}-pg" pg_isready -q -U lidradar -d lidradar; then
    break
  fi
  sleep 1
done
docker exec -i "${RUN}-pg" psql -X -q -1 -v ON_ERROR_STOP=1 -U lidradar -d lidradar <<'SQL'
CREATE TABLE schema_migrations (version text PRIMARY KEY);
INSERT INTO schema_migrations VALUES ('000022_membership_invitations'), ('000023_unfinished_agreements');
CREATE TABLE organizations (id serial PRIMARY KEY, name text);
CREATE TABLE messages (id serial PRIMARY KEY, body text);
CREATE TABLE risk_signals (id serial PRIMARY KEY);
CREATE TABLE revenue_events (id serial PRIMARY KEY);
CREATE TABLE audit_log (id serial PRIMARY KEY);
INSERT INTO organizations (name) SELECT 'org ' || g FROM generate_series(1, 3) g;
INSERT INTO messages (body) SELECT md5(g::text) FROM generate_series(1, 5000) g;
INSERT INTO audit_log DEFAULT VALUES;
SQL

docker run -d --name "${RUN}-s3" --network "${RUN}" --entrypoint sh "${IMAGE}" -c \
  'mkdir -p /tmp/s3/lidradar-backups && exec rclone serve s3 --auth-key e2e-access,e2e-secret --addr :9000 /tmp/s3' >/dev/null
sleep 3

docker run --rm --entrypoint age-keygen "${IMAGE}" >"${WORK}/age.key" 2>/dev/null
docker run --rm --entrypoint age-keygen "${IMAGE}" >"${WORK}/other.key" 2>/dev/null
chmod 644 "${WORK}/age.key" "${WORK}/other.key"
RECIPIENT="$(grep 'public key' "${WORK}/age.key" | sed 's/.*: //')"
SECRET=e2e-secret

# backup [аргументы docker run] ... образ-команда: служба копий с настройками стенда.
backup() {
  docker run --rm --network "${RUN}" -v "${SPOOL_VOLUME}:/spool" -v "${WORK}/age.key:/run/age.key:ro" -v "${WORK}/other.key:/run/other.key:ro" \
    -e LIDRADAR_ENV=staging -e LIDRADAR_VERSION=e2e \
    -e "LIDRADAR_BACKUP_DATABASE_URL=postgres://lidradar:e2e-password@${RUN}-pg:5432/lidradar?sslmode=disable" \
    -e LIDRADAR_BACKUP_REMOTE=offhost:lidradar-backups/ci -e LIDRADAR_BACKUP_S3_PROVIDER=Other \
    -e "LIDRADAR_BACKUP_S3_ENDPOINT=http://${RUN}-s3:9000" -e LIDRADAR_BACKUP_S3_ACCESS_KEY_ID=e2e-access \
    -e "LIDRADAR_BACKUP_S3_SECRET_ACCESS_KEY=${SECRET}" -e "LIDRADAR_BACKUP_AGE_RECIPIENTS=${RECIPIENT}" \
    "${IMAGE}" "$@"
}
s3() { docker exec "${RUN}-s3" sh -c "$1"; }

spool_is_clean() { ! docker run --rm -v "${SPOOL_VOLUME}:/spool" --entrypoint sh "${IMAGE}" -c 'ls -A /spool' | grep -q -e '^work-' -e '\.dump'; }
list_shows() { backup list | grep -q "$1"; }
status_shows_failure() { backup status --json | jq -e '.consecutiveFailures >= 1 and .lastAttemptOk == false' >/dev/null; }
drill_confirms() { [ "$(backup drill --identity /run/age.key 2>/dev/null | jq -r .ok)" = true ]; }
scratch_database_gone() { [ "$(docker exec "${RUN}-pg" psql -X -At -U lidradar -d postgres -c "select count(*) from pg_database where datname like 'lidradar_drill_%'")" = 0 ]; }

# --- цикл -------------------------------------------------------------------------------------
if backup once >"${WORK}/once.log" 2>&1; then ok 'once: копия создана, загружена и сверена'; else bad 'once завершился с ошибкой'; cat "${WORK}/once.log" >&2; fi
check 'в ярусе frequent лежат файл копии и манифест' s3 'ls /tmp/s3/lidradar-backups/ci/frequent | grep -q dump.age && ls /tmp/s3/lidradar-backups/ci/frequent | grep -q manifest.json'
check 'в первом цикле суток копия попадает и в ярус daily' s3 'ls /tmp/s3/lidradar-backups/ci/daily | grep -q manifest.json'
check 'в хранилище шифротекст age, а не открытый текст' s3 'head -c 21 /tmp/s3/lidradar-backups/ci/frequent/*.dump.age | grep -q age-encryption.org/v1'
check 'в журнале нет паролей' sh -c "! grep -q -e e2e-password -e e2e-secret '${WORK}/once.log'"
check 'status: точка свежая' backup status
check 'в спуле не осталось выгрузок' spool_is_clean

STAMP="$(s3 'ls /tmp/s3/lidradar-backups/ci/frequent' | grep manifest | head -n 1 | sed 's/\.manifest\.json//')"
check 'list показывает копию' list_shows "${STAMP}"

# --- восстановление ---------------------------------------------------------------------------
check 'drill: скачано, расшифровано, восстановлено, счётчики совпали' drill_confirms
check 'учебная база после drill удалена' scratch_database_gone

# --- отказы -------------------------------------------------------------------------------------
check_fails 'чужой ключ age не расшифровывает копию' backup fetch --identity /run/other.key --output /tmp/o
s3 "echo tampered >> /tmp/s3/lidradar-backups/ci/frequent/${STAMP}.dump.age"
check_fails 'подменённый объект в хранилище обнаруживается по сумме' backup fetch --identity /run/age.key --output /tmp/o
SECRET=wrong-secret
check_fails 'неверный ключ хранилища: цикл завершается ошибкой' backup once
SECRET=e2e-secret
check 'сбой записан в состоянии службы' status_shows_failure

echo
if [ "${FAILURES}" -eq 0 ]; then
  echo 'сквозная проверка копий вне хоста: замечаний нет'
else
  echo "сквозная проверка копий вне хоста: замечаний ${FAILURES}" >&2
  exit 1
fi
