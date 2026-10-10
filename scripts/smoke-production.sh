#!/usr/bin/env bash
# Приёмочная проверка развёрнутого стека снаружи (LR-BE-2605, ADR 0050).
#
#   scripts/smoke-production.sh https://app.example.com [--insecure]
#       [--closed-ports 8080,5432,2019] [--version ТЕГ --compose-dir deploy/production]
#
# Проверяет то, что видит пользователь и посторонний: страницу и маршрутизацию
# через edge, заголовки безопасности (HSTS ровно один), переход с HTTP на HTTPS,
# недоступность /health/ready снаружи, закрытые порты внутренних сервисов. С
# --version и --compose-dir дополнительно читает готовность api изнутри контейнера
# и сверяет версию сборки и миграции, возраст последней копии вне хоста (служба
# backup, ADR 0051) и роли базы (ADR 0052: владелец и рабочий логин без суперправ,
# приложение подключено рабочим логином). Скрипт ничего не меняет и не создаёт
# данных; секреты не требуются. --insecure принимает сертификат любого центра
# (локальная проба с доменом localhost). Для стека с другим именем проекта или
# файлом окружения задайте COMPOSE_PROJECT_NAME и COMPOSE_ENV_FILES, как для
# docker compose. Закрытые порты проверяются с того компьютера, где запущен
# скрипт: запускайте его снаружи хоста. Код 0 — всё в порядке, 1 — есть замечания.
set -uo pipefail

BASE=""
INSECURE=""
CLOSED_PORTS="8080,5432,2019"
VERSION=""
COMPOSE_DIR=""

usage() {
  echo 'использование: smoke-production.sh https://домен [--insecure] [--closed-ports СПИСОК] [--version ТЕГ --compose-dir КАТАЛОГ]' >&2
  exit 2
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --insecure) INSECURE="-k" ;;
    --closed-ports) [ "$#" -ge 2 ] || usage; CLOSED_PORTS="$2"; shift ;;
    --version) [ "$#" -ge 2 ] || usage; VERSION="$2"; shift ;;
    --compose-dir) [ "$#" -ge 2 ] || usage; COMPOSE_DIR="$2"; shift ;;
    -*) usage ;;
    *) [ -z "${BASE}" ] || usage; BASE="${1%/}" ;;
  esac
  shift
done
[[ "${BASE}" =~ ^https://[A-Za-z0-9.-]+(:[0-9]+)?$ ]] || { echo 'адрес должен быть вида https://домен[:порт] без пути' >&2; exit 2; }
[[ "${CLOSED_PORTS}" =~ ^([0-9]+(,[0-9]+)*)?$ ]] || usage
{ [ -z "${VERSION}" ] && [ -z "${COMPOSE_DIR}" ]; } || { [ -n "${VERSION}" ] && [ -n "${COMPOSE_DIR}" ]; } || usage

HOST="${BASE#https://}"
HOST="${HOST%%:*}"
WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT
FAILURES=0

pass() { echo "ok    $1"; }
fail() { echo "FAIL  $1" >&2; FAILURES=$((FAILURES + 1)); }

# request МЕТОД ПУТЬ [аргументы curl] — заполняет STATUS и файлы заголовков и тела.
request() {
  local method="$1" path="$2"
  shift 2
  STATUS="$(curl -sS ${INSECURE} --max-time 15 -X "${method}" -o "${WORK}/body" -D "${WORK}/headers" \
    -w '%{http_code}' "$@" "${BASE}${path}" 2>"${WORK}/error")" || STATUS="000"
}
# header ИМЯ — значение последнего заголовка с таким именем без учёта регистра.
header() {
  awk -v name="$(echo "$1" | tr 'A-Z' 'a-z')" -F': ' 'tolower($1) == name { sub(/\r$/, "", $2); value = $2 } END { print value }' "${WORK}/headers"
}
header_count() { grep -ci "^$1:" "${WORK}/headers" || true; }
body_has() { grep -q -- "$1" "${WORK}/body"; }

# --- страница и статика -------------------------------------------------------
request GET /
if [ "${STATUS}" = 200 ] && header content-type | grep -qi 'text/html' && body_has 'id="app"'; then
  pass 'GET / отдаёт страницу веб-клиента'
else
  fail "GET / не отдаёт страницу веб-клиента (HTTP ${STATUS}) $(head -c 200 "${WORK}/error")"
fi
[ "$(header_count strict-transport-security)" = 1 ] && pass 'HSTS на странице ровно один' || fail "HSTS на странице: $(header_count strict-transport-security) заголовков, нужен один"
[ -n "$(header content-security-policy)" ] && pass 'у страницы есть Content-Security-Policy' || fail 'у страницы нет Content-Security-Policy'
[ "$(header x-content-type-options)" = nosniff ] && pass 'X-Content-Type-Options: nosniff' || fail 'нет X-Content-Type-Options: nosniff'
[ -z "$(header server)" ] && pass 'заголовок Server не раскрывает ПО' || fail "заголовок Server раскрывает ПО: $(header server)"

request GET /risks/not-a-real-page
if [ "${STATUS}" = 200 ] && body_has 'id="app"'; then pass 'переход SPA по глубокой ссылке работает'; else fail "глубокая ссылка не открывает веб-клиент (HTTP ${STATUS})"; fi

# --- API через edge -----------------------------------------------------------
request GET /api/v1/auth/me
if [ "${STATUS}" = 401 ] && body_has '"code":"UNAUTHENTICATED"' && [ -n "$(header x-request-id)" ]; then
  pass 'API отвечает через edge (401 UNAUTHENTICATED с X-Request-Id)'
else
  fail "GET /api/v1/auth/me через edge: HTTP ${STATUS}, ожидалось 401 UNAUTHENTICATED"
fi
header cache-control | grep -qi 'no-store' && pass 'ответы API не кэшируются' || fail 'ответ API без Cache-Control: no-store'
[ "$(header_count strict-transport-security)" = 1 ] && pass 'HSTS на ответе API ровно один' || fail "HSTS на ответе API: $(header_count strict-transport-security) заголовков, нужен один"

request GET /api/v1/admin/me
[ "${STATUS}" = 401 ] && pass 'API администратора без сессии даёт 401' || fail "GET /api/v1/admin/me без сессии: HTTP ${STATUS}, ожидалось 401"

request POST /internal/v1/ai/nodes/heartbeat -H 'Content-Type: application/json' -d '{}'
case "${STATUS}" in 400 | 401) pass "API AI-узла доступен и не пускает без подписи (HTTP ${STATUS})" ;; *) fail "POST /internal/v1/ai/nodes/heartbeat: HTTP ${STATUS}, ожидалось 400 или 401" ;; esac

request POST /api/v1/webhooks/GENERIC_WEBHOOK/00000000-0000-7000-8000-000000000000/00000000-0000-7000-8000-000000000001 \
  -H 'Content-Type: application/json' -d '{}'
if [ "${STATUS}" -ge 400 ] && [ "${STATUS}" -lt 500 ]; then pass "маршрут вебхуков доступен и отвергает чужой запрос (HTTP ${STATUS})"; else fail "вебхук с неизвестным подключением: HTTP ${STATUS}, ожидался отказ 4xx"; fi

# --- живость и закрытая готовность -------------------------------------------
request GET /health/live
if [ "${STATUS}" = 200 ] && body_has '"status":"ok"'; then pass '/health/live доступен снаружи'; else fail "/health/live: HTTP ${STATUS}"; fi
request GET /health/ready
if [ "${STATUS}" = 404 ] && ! body_has 'revision'; then pass '/health/ready снаружи закрыт и не раскрывает ревизию'; else fail "/health/ready виден снаружи (HTTP ${STATUS}) или раскрывает сборку"; fi

# --- HTTP переходит на HTTPS --------------------------------------------------
if [[ "${BASE}" =~ :[0-9]+$ ]]; then
  echo 'skip  переход с HTTP на HTTPS не проверяется для нестандартного порта'
else
  STATUS="$(curl -sS --max-time 15 -o /dev/null -D "${WORK}/headers" -w '%{http_code}' "http://${HOST}/some/path" 2>/dev/null)" || STATUS="000"
  case "${STATUS}" in
    301 | 302 | 307 | 308)
      header location | grep -q "^https://${HOST}/some/path" && pass "HTTP перенаправляет на HTTPS (${STATUS})" || fail "HTTP перенаправляет не туда: $(header location)" ;;
    *) fail "HTTP не перенаправляет на HTTPS (HTTP ${STATUS})" ;;
  esac
fi

# --- внутренние порты не слушают снаружи ---------------------------------------
for port in ${CLOSED_PORTS//,/ }; do
  if python3 - "${HOST}" "${port}" <<'PY'
import socket, sys
try:
    socket.create_connection((sys.argv[1], int(sys.argv[2])), timeout=3).close()
except OSError:
    sys.exit(1)
PY
  then
    fail "порт ${port} на ${HOST} принимает соединения, он должен быть закрыт"
  else
    pass "порт ${port} на ${HOST} закрыт"
  fi
done

# --- готовность изнутри и версия сборки ----------------------------------------
if [ -n "${COMPOSE_DIR}" ]; then
  READY="$(docker compose --project-directory "${COMPOSE_DIR}" -f "${COMPOSE_DIR}/compose.yaml" exec -T api wget -qO- http://127.0.0.1:8080/health/ready 2>"${WORK}/error")" \
    || READY=""
  if [ -z "${READY}" ]; then
    fail "не удалось прочитать /health/ready изнутри api: $(head -c 200 "${WORK}/error")"
  else
    VERDICT="$(READY="${READY}" VERSION="${VERSION}" python3 - <<'PY'
import json, os
try:
    d = json.loads(os.environ["READY"])
except ValueError:
    print("ответ не JSON")
    raise SystemExit
problems = []
if d.get("status") != "ready":
    problems.append("статус %r" % d.get("status"))
build = d.get("build", {})
if build.get("version") != os.environ["VERSION"]:
    problems.append("версия сборки %r, ожидалась %r" % (build.get("version"), os.environ["VERSION"]))
if build.get("modified"):
    problems.append("сборка из изменённого дерева")
migrations = d.get("migrations", {})
if not migrations.get("latest") or migrations.get("applied") != migrations.get("latest"):
    problems.append("миграции %r из %r" % (migrations.get("applied"), migrations.get("latest")))
print("; ".join(problems) if problems else "OK %s %s" % (build.get("version"), migrations.get("latest")))
PY
)"
    case "${VERDICT}" in
      OK*) pass "api готов изнутри: ${VERDICT#OK }" ;;
      *) fail "api изнутри: ${VERDICT}" ;;
    esac
  fi

  # Копия вне хоста: точка не старше допустимого возраста и нет двух неудачных циклов подряд.
  BACKUP="$(docker compose --project-directory "${COMPOSE_DIR}" -f "${COMPOSE_DIR}/compose.yaml" exec -T backup lidradar-backup status --json 2>"${WORK}/error" || true)"
  if [ -z "${BACKUP}" ]; then
    fail "не удалось прочитать состояние службы копий вне хоста: $(head -c 200 "${WORK}/error")"
  else
    VERDICT="$(BACKUP="${BACKUP}" python3 - <<'PY'
import json, os
try:
    d = json.loads(os.environ["BACKUP"])
except ValueError:
    print("ответ не JSON")
    raise SystemExit
age = d.get("ageSeconds")
if age is None:
    print("подтверждённых копий вне хоста ещё нет")
elif not d.get("ok"):
    print("последняя копия вне хоста старше допустимого возраста: %d мин из %d; ошибка: %s" % (age // 60, d.get("maxAgeSeconds", 0) // 60, d.get("lastError") or "нет"))
elif d.get("consecutiveFailures", 0) >= 2:
    print("копия вне хоста не создаётся: %d цикла подряд неудачны; ошибка: %s" % (d["consecutiveFailures"], d.get("lastError") or "нет"))
else:
    gap = d.get("maxGapSeconds") or 0
    print("OK последняя копия вне хоста %d мин назад, наибольший разрыв между точками %d мин" % (age // 60, gap // 60))
PY
)"
    case "${VERDICT}" in
      OK*) pass "${VERDICT#OK }" ;;
      *) fail "${VERDICT}" ;;
    esac
  fi

  # Роли базы: нет привилегированных логинов, база принадлежит владельцу, к ней не подключён
  # суперпользователь, а процессы приложения входят рабочим логином. Внешняя база (нет сервиса
  # postgres) проверяется вручную: см. docs/runbooks/production-deployment.md.
  DATABASE_ROLES="$(docker compose --project-directory "${COMPOSE_DIR}" -f "${COMPOSE_DIR}/compose.yaml" exec -T postgres psql -X -At -F '|' -U postgres -d lidradar -c "
    SELECT (SELECT count(*) FROM pg_roles WHERE rolname IN ('lidradar', 'lidradar_runtime')),
           (SELECT count(*) FROM pg_roles WHERE rolname IN ('lidradar', 'lidradar_runtime') AND (rolsuper OR rolcreaterole OR rolbypassrls OR rolreplication)),
           (SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = 'lidradar'),
           (SELECT count(*) FROM pg_stat_activity a JOIN pg_roles r ON r.rolname = a.usename WHERE r.rolsuper AND a.backend_type = 'client backend' AND a.pid <> pg_backend_pid()),
           (SELECT count(*) FROM pg_stat_activity WHERE usename = 'lidradar_runtime' AND backend_type = 'client backend')" 2>"${WORK}/error" || true)"
  if [ -z "${DATABASE_ROLES}" ]; then
    echo "skip  роли базы не проверены: сервис postgres стека недоступен (внешняя база?): $(head -c 120 "${WORK}/error")"
  else
    VERDICT="$(DATABASE_ROLES="${DATABASE_ROLES}" python3 - <<'PY'
import os
try:
    present, privileged, owner, superusers, runtime = os.environ["DATABASE_ROLES"].strip().split("|")
    present, privileged, superusers, runtime = int(present), int(privileged), int(superusers), int(runtime)
except ValueError:
    print("ответ не разобран")
    raise SystemExit
problems = []
if present != 2:
    problems.append("нет ролей lidradar и lidradar_runtime (создаёт postgres/10-roles.sql при первой инициализации тома)")
if privileged:
    problems.append("у владельца или рабочего логина есть привилегии суперпользователя, CREATEROLE, BYPASSRLS или репликации")
if owner != "lidradar":
    problems.append("база принадлежит %r, а не lidradar" % owner)
if superusers:
    problems.append("к базе подключён суперпользователь (%d)" % superusers)
if runtime < 1:
    problems.append("процессы приложения не подключены рабочим логином lidradar_runtime")
print("; ".join(problems) if problems else "OK роли базы: владелец и рабочий логин без суперправ, приложение подключено рабочим логином (%d соединений)" % runtime)
PY
)"
    case "${VERDICT}" in
      OK*) pass "${VERDICT#OK }" ;;
      *) fail "роли базы: ${VERDICT}" ;;
    esac
  fi
fi

echo
if [ "${FAILURES}" -eq 0 ]; then
  echo "проверка ${BASE}: замечаний нет"
else
  echo "проверка ${BASE}: замечаний ${FAILURES}" >&2
  exit 1
fi
