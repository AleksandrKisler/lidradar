#!/bin/sh

set -eu

fail() {
	printf '%s\n' "$1" >&2
	exit 1
}

dotenv_token() {
	file=${LIDRADAR_ENV_FILE:-.env}
	[ -r "$file" ] || return 0
	awk -v key="$1" '
		$0 ~ ("^[[:space:]]*" key "[[:space:]]*=") {
			value = $0
			sub(/^[^=]*=/, "", value)
			sub(/\r$/, "", value)
			gsub(/^[[:space:]]+|[[:space:]]+$/, "", value)
			if ((substr(value, 1, 1) == "\"" && substr(value, length(value), 1) == "\"") ||
			    (substr(value, 1, 1) == "\047" && substr(value, length(value), 1) == "\047")) {
				value = substr(value, 2, length(value) - 2)
			}
			print value
			exit
		}
	' "$file"
}

command -v curl >/dev/null 2>&1 || fail "Для подключения требуется curl"
command -v openssl >/dev/null 2>&1 || fail "Для подключения требуется openssl"

token=${LIDRADAR_TELEGRAM_TOKEN:-${LIDAR_TELEGRAM_TOKEN:-}}
[ -n "$token" ] || token=$(dotenv_token LIDRADAR_TELEGRAM_TOKEN)
[ -n "$token" ] || token=$(dotenv_token LIDAR_TELEGRAM_TOKEN)
[ -n "$token" ] || fail "LIDRADAR_TELEGRAM_TOKEN не найден в окружении или .env"

tenant_id=${LIDRADAR_TENANT_ID:-}
session=${LIDRADAR_SESSION:-}
api_base=${LIDRADAR_API_BASE_URL:-http://127.0.0.1:8080}
location_id=${LIDRADAR_TELEGRAM_LOCATION_ID:-}
webhook_secret=${LIDRADAR_TELEGRAM_WEBHOOK_SECRET:-}

[ -n "$tenant_id" ] || fail "LIDRADAR_TENANT_ID обязателен"
[ -n "$session" ] || fail "LIDRADAR_SESSION обязателен"
[ -n "$webhook_secret" ] || webhook_secret=$(openssl rand -hex 32)

case "$tenant_id" in *[!0-9A-Fa-f-]*|'') fail "LIDRADAR_TENANT_ID имеет неверный формат" ;; esac
case "$location_id" in *[!0-9A-Fa-f-]*) fail "LIDRADAR_TELEGRAM_LOCATION_ID имеет неверный формат" ;; esac
case "$session" in *[!A-Za-z0-9_-]*|'') fail "LIDRADAR_SESSION имеет неверный формат" ;; esac
case "$token" in *[!A-Za-z0-9_:-]*|'') fail "LIDRADAR_TELEGRAM_TOKEN имеет неверный формат" ;; esac
case "$webhook_secret" in *[!A-Za-z0-9_-]*|'') fail "Секрет webhook имеет неверный формат" ;; esac
case "$api_base" in http://*|https://*) ;; *) fail "LIDRADAR_API_BASE_URL должен быть HTTP(S)-адресом" ;; esac
case "$api_base" in *'"'*|*'\'*|*[[:space:]]*) fail "LIDRADAR_API_BASE_URL содержит недопустимые символы" ;; esac

api_base=${api_base%/}
umask 077
curl_config=$(mktemp "${TMPDIR:-/tmp}/lidradar-telegram-curl.XXXXXX")
cleanup() {
	rm -f "$curl_config"
	unset token session webhook_secret
}
trap cleanup EXIT HUP INT TERM

{
	printf 'url = "%s/api/v1/integrations/CONNECTED_BUSINESS_BOT/connect"\n' "$api_base"
	printf 'request = "POST"\n'
	printf 'header = "Content-Type: application/json"\n'
	printf 'header = "X-Tenant-ID: %s"\n' "$tenant_id"
	printf 'header = "Cookie: lidradar_session=%s"\n' "$session"
	printf 'fail-with-body\n'
	printf 'silent\n'
	printf 'show-error\n'
} >"$curl_config"

if [ -n "$location_id" ]; then
	printf '{"name":"Telegram разработки","locationId":"%s","webhookSecret":"%s","botToken":"%s"}' \
		"$location_id" "$webhook_secret" "$token"
else
	printf '{"name":"Telegram разработки","webhookSecret":"%s","botToken":"%s"}' \
		"$webhook_secret" "$token"
fi | curl --config "$curl_config" --data-binary @-
printf '\n'
