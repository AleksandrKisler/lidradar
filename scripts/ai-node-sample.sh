#!/usr/bin/env bash
# Снимает видеопамять и состояние контейнера llama.cpp на AI-узле (ADR 0053).
#
#   scripts/ai-node-sample.sh ФАЙЛ.csv [КОНТЕЙНЕР]        остановка — Ctrl+C
#
# Запускается НА УЗЛЕ, пока на другом компьютере идёт прогон (make ai-benchmark-*).
# Раз в секунду пишет строку: время, видеокарта, версия драйвера, занятая и полная видеопамять,
# число перезапусков контейнера, признак остановки по нехватке памяти (OOM), «запущен» и ID образа
# контейнера (привязка к сборке llama.cpp, которую по HTTP не видно).
# Видеопамять читается изнутри контейнера: библиотека NVML хоста может не совпадать с
# драйвером. Из CSV отчёт об оборудовании собирает scripts/ai-hardware-report.py.
# Контейнер по умолчанию — llama-server проекта lidradar-ai-node (docker-compose.ai.yml).
set -u

OUT="${1:-}"
CONTAINER="${2:-lidradar-ai-node-llama-server-1}"
INTERVAL="${LIDRADAR_SAMPLE_INTERVAL:-1}"
[ -n "${OUT}" ] || { echo 'использование: ai-node-sample.sh ФАЙЛ.csv [КОНТЕЙНЕР]' >&2; exit 2; }

echo 'epoch,gpu,driverVersion,memoryUsedMiB,memoryTotalMiB,restartCount,oomKilled,running,imageId' >"${OUT}" || exit 1
trap 'exit 0' INT TERM

while :; do
  gpu="$(docker exec "${CONTAINER}" nvidia-smi --query-gpu=name,driver_version,memory.used,memory.total --format=csv,noheader,nounits 2>/dev/null | head -n 1)"
  state="$(docker inspect -f '{{.RestartCount}},{{.State.OOMKilled}},{{.State.Running}},{{.Image}}' "${CONTAINER}" 2>/dev/null)"
  # "NVIDIA GeForce RTX 4060, 595.91.07, 5360, 8188" → имя, драйвер, занято, всего (пусто, если nvidia-smi недоступен).
  name="$(printf '%s' "${gpu}" | awk -F', *' '{print $1}')"
  driver="$(printf '%s' "${gpu}" | awk -F', *' '{print $2}')"
  used="$(printf '%s' "${gpu}" | awk -F', *' '{print $3}')"
  total="$(printf '%s' "${gpu}" | awk -F', *' '{print $4}')"
  printf '%s,%s,%s,%s,%s,%s\n' "$(date +%s)" "${name}" "${driver}" "${used}" "${total}" "${state:-,,,}" >>"${OUT}"
  sleep "${INTERVAL}"
done
