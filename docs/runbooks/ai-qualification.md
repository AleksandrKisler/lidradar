# Квалификация AI tuple: порядок прогона и фиксации

Порядок для смены инструкции, схемы генерации, валидатора, параметров, модели или
сервера llama.cpp. Решение и пороги — [ADR 0053](../adr/0053-ai-tuple-qualification.md);
наборы и команды — [models/datasets/README.md](../../models/datasets/README.md);
текущее состояние — [манифест](../../models/manifests/lidradar-main-v1.json).

**Tuple** — всё, от чего зависит ответ модели и его проверка: файлы из
`scripts/ai-selection.py` (инструкции, примеры, схема генерации, поставщик, валидаторы,
runner, контракты), параметры генерации, веса, сборка и настройки сервера, наборы данных,
оборудование. Изменение любого элемента — новый tuple: прежний freeze его не подтверждает.

## 1. Правила

1. Инструкцию, примеры и схему генерации настраивают только по DEV. Результаты
   `golden_v1.jsonl` во время настройки не смотрят: файл защищён суммой, а цель
   `make ai-benchmark-golden` не запускается, пока выбор не записан и не совпадает с деревом
   (сам runner без этой цели выборку откроет — обходить цель нельзя).
2. GOLDEN запускают **один раз** для записанного выбора. Первый прогон — результат
   квалификации; повторять его до «зелёного» нельзя. Не прошёл — новая версия инструкции и
   новая контрольная выборка (при просмотре отчёта GOLDEN выборка перестаёт быть слепой).
3. Повторный прогон того же выбора допустим только как доказательство оборудования (п. 6) и
   не заменяет первый результат.
4. Изменение любого файла tuple после записи выбора делает выбор недействительным:
   `make ai-selection-verify` покажет, что изменилось.

## 2. Подготовка

Туннель или адрес llama.cpp, на котором идёт прогон, передаётся как `AI_BENCHMARK_ENDPOINT`.
Сервер запускается с настройками узла (`docker-compose.ai.yml`: контекст 8192 по решению владельца, один слот, все
слои на GPU); отчёты записывают сборку, модель и параметры последнего запроса, и манифест
сверяет их с зафиксированными.

```sh
make ai-dataset-audit        # баланс 400 + 100, нет совпадающих переписок
go test ./backend/internal/ai/...
```

### Доступ к узлу

llama.cpp виден только во внутренней сети Docker узла, поэтому прогон идёт через SSH-туннель.
Вход — по ключу, пароль в скриптах и сессиях не используется. Адрес контейнера в сети узла
находят с самого узла (права на docker не нужны), он меняется при пересоздании контейнера:

```sh
ssh -i ~/.ssh/КЛЮЧ пользователь@узел 'for ip in 172.21.0.2 172.21.0.3 172.21.0.4; do curl -s --max-time 2 http://$ip:8080/health && echo " $ip"; done'
ssh -N -f -L 127.0.0.1:18089:АДРЕС_КОНТЕЙНЕРА:8080 -i ~/.ssh/КЛЮЧ -o IdentitiesOnly=yes -o BatchMode=yes \
  -o ExitOnForwardFailure=yes -o ServerAliveInterval=15 -o ServerAliveCountMax=3 пользователь@узел
curl -s http://127.0.0.1:18089/props     # n_ctx и build_info
```

Пересоздание контейнера с нужным контекстом (нужен sudo на узле, в каталоге с `docker-compose.ai.yml`):
`sudo docker compose -f docker-compose.ai.yml up -d llama-server`. После него проверьте `n_ctx` в `/props`
и пересоздайте туннель: адрес контейнера мог измениться.

## 3. Настройка по DEV

```sh
make ai-benchmark-dev        # dev_v1: 100 случаев, версии подменяются в памяти
make ai-benchmark-v2-dev     # намерения, договорённости, независимые обещания
```

`make ai-benchmark-dev` печатает отчёт JSON в stdout (служебные сообщения идут в stderr, поэтому
`make ai-benchmark-dev > отчёт.json` даёт чистый файл); `make ai-benchmark-v2-dev` пишет отчёты в
`runtime/ai-benchmark/{intents,agreements,independent}.json`. Измерения каждого обращения (токены,
скорость, сырой ответ, причина отказа) цели пишут в `runtime/ai-benchmark/*.jsonl`. Отчёт содержит
доли по типам фактов и список несовпавших случаев с причиной. Код выхода 2 — пороги не пройдены.
Модель недетерминирована в пределах одного-двух случаев, поэтому решение о правке принимают по
повторным прогонам, а не по одному.

## 4. Фиксация выбора до GOLDEN

Когда все наборы DEV проходят пороги на итоговой конфигурации сервера (контекст из `/props` совпадает
с манифестом), сохраните отчёты в `models/reports/` и запишите выбор. Отчёты снимают целями Makefile на
наборах из репозитория: отчёт по преобразованной копии набора манифест не примет.

```sh
make ai-benchmark-dev > models/reports/lidradar-main-v1-prompt-v9-dev.json
make ai-benchmark-v2-dev
for name in intents agreements independent; do cp runtime/ai-benchmark/$name.json models/reports/lidradar-main-v1-prompt-v9-dev-$name.json; done
scripts/ai-selection.py write \
  --out models/reports/lidradar-main-v1-prompt-v9-selection.json \
  --prompt-version analyze-conversation.prompt.v9 --schema-version analyze-conversation.v2 \
  --dev-report models/reports/lidradar-main-v1-prompt-v9-dev.json \
  --dev-report models/reports/lidradar-main-v1-prompt-v9-dev-intents.json \
  --dev-report models/reports/lidradar-main-v1-prompt-v9-dev-agreements.json \
  --dev-report models/reports/lidradar-main-v1-prompt-v9-dev-independent.json
```

Скрипт отказывается записывать выбор по непройденному отчёту DEV. В выбор попадают SHA-256
всех файлов tuple, наборов данных и отчётов DEV.

## 5. GOLDEN

```sh
make ai-benchmark-golden > models/reports/lidradar-main-v1-prompt-v9-golden.json
```

Цель сначала проверяет выбор (`scripts/ai-selection.py verify`) и только потом открывает
выборку. После прогона проверьте выбор ещё раз: файлы tuple не должны были измениться.
Бюджет всего прогона задаёт `AI_BENCHMARK_TIMEOUT` (по умолчанию 2 часа; у самого runner — 30 минут,
а 400 случаев на медленном узле могут не уложиться). Прогон на 400 случаев занимает около 20 минут:
всё это время компьютер с туннелем не должен засыпать, а сэмплер на узле должен работать.
Если узел или туннель перестали отвечать, runner прерывает прогон без отчёта (`ErrModelUnavailable`):
это отказ инфраструктуры, а не результат, и такой запуск можно повторить после восстановления связи.

## 6. Оборудование

Порог оборудования (видеопамять ≤ 7500 МиБ, нет OOM и перезапусков, ≥ 20 токенов/с, ни один ответ
не оборван по длине, запрос с ответом умещается в контекст) нельзя измерить по HTTP: нужны
снимки на узле. На узле (не на компьютере с репозиторием):

```sh
scripts/ai-node-sample.sh /tmp/vram.csv        # оставить работать; контейнер по умолчанию lidradar-ai-node-llama-server-1
```

Скрипт на узле оставляют работать на все прогоны квалификации: DEV (§ 4), зонд длинного контекста и
GOLDEN (§ 5). Зонд — самая длинная переписка, которую разрешает продукт:

```sh
make ai-benchmark-context > models/reports/lidradar-main-v1-prompt-v9-context-probe.json
```

После GOLDEN скрипт на узле останавливают (Ctrl+C), CSV копируют на компьютер в `models/reports/` (это
первоисточник: отчёт записывает имя файла и его SHA-256) и собирают отчёт по всем прогонам:

```sh
scripts/ai-hardware-report.py --samples models/reports/lidradar-main-v1-prompt-v9-vram-samples.csv \
  --benchmark models/reports/lidradar-main-v1-prompt-v9-dev.json \
  --benchmark models/reports/lidradar-main-v1-prompt-v9-dev-intents.json \
  --benchmark models/reports/lidradar-main-v1-prompt-v9-dev-agreements.json \
  --benchmark models/reports/lidradar-main-v1-prompt-v9-dev-independent.json \
  --benchmark models/reports/lidradar-main-v1-prompt-v9-context-probe.json \
  --benchmark models/reports/lidradar-main-v1-prompt-v9-golden.json \
  --out models/reports/lidradar-main-v1-prompt-v9-hardware.json
```

Зонд собирает `scripts/ai-context-probe.py` из пределов `analysis.go`: если запрос самой длинной
переписки не умещается в контекст сервера, отчёт об оборудовании не проходит — это отказ tuple, а не
шум прогона.

Скрипт проверяет, что снимки покрывают время прогонов (допуск 120 с на расхождение часов).
Хэш файла весов пересчитывают на узле и сохраняют отчётом (файл читается только на чтение):

```sh
ssh узел python3 - /srv/lidradar/models/Qwen3-8B-Q4_K_M.gguf < scripts/ai-model-hash.py \
  > models/reports/lidradar-main-v1-prompt-v9-model-hash.json
```

## 7. Манифест

Сначала сохраните действующий манифест как отчёт (скрипт откажется работать без него):
`cp models/manifests/lidradar-main-v1.json models/reports/lidradar-main-v1-prompt-v6-manifest.json`.

```sh
scripts/ai-manifest.py --base models/manifests/lidradar-main-v1.json \
  --previous-report models/reports/lidradar-main-v1-prompt-v6-manifest.json \
  --selection models/reports/lidradar-main-v1-prompt-v9-selection.json \
  --dev models/reports/lidradar-main-v1-prompt-v9-dev.json --golden models/reports/lidradar-main-v1-prompt-v9-golden.json \
  --v2-dev intents=… --v2-dev agreements=… --v2-dev independent=… \
  --hardware models/reports/lidradar-main-v1-prompt-v9-hardware.json \
  --model-hash-report models/reports/lidradar-main-v1-prompt-v9-model-hash.json --context-size 8192 \
  --limitation "известное ограничение по результатам прогона" \
  --out models/manifests/lidradar-main-v1.json
```

Известные ограничения (`--limitation`, можно повторять) формулируют по отчётам: что не совпало на GOLDEN и
почему, что выборка не проверяет, чем ограничен запас по порогам. Числа в них пишут вручную, поэтому их
сверяют с отчётами.

Скрипт собирает числа из отчётов и сверяет версии, сумму GOLDEN, параметры, которые сервер увидел,
размер контекста и число слотов. Числа каждого отчёта (доли, полнота и точность по типам фактов, p95)
он сравнивает с порогами манифеста сам: флаг `passed` в отчёте выставляет runner по тем порогам,
которые передал запускавший, и на веру не принимается. Статус `FROZEN` ставится, только если пройдено всё, включая
оборудование и проверку весов; иначе манифест остаётся `CANDIDATE` со списком недостающего.
При расхождении манифест не записывается.

## 8. Что делать после

1. `make ai-selection-verify` — файлы tuple не менялись с выбора.
2. Обновить документы: `docs/backend/07-ai.md` (§ 9), `docs/engineering/RELEASE_GATES.md` (RG-AI),
   `docs/engineering/EXTERNAL_ARTIFACTS.md`, `docs/spec/BACKEND_SPEC.md`; пересобрать
   `docs/notion` (`python3 docs/notion/build.py`).
3. Тесты AI-пакетов и общий `go test ./...`.

## 9. Диагностика

| Признак | Причина и действие |
|---|---|
| `GOLDEN_DIGEST_MISMATCH` | подменён или изменён `golden_v1.jsonl`; восстановить файл, сумму менять нельзя |
| `make ai-benchmark-golden` останавливается на `verify` | tuple изменился после выбора; вернуть файлы или записать новый выбор и новую выборку |
| `несовместимый входной контракт` | инструкция v7–v9 требует контракта v2, v1–v6 — контракта v1: задайте оба флага |
| `ошибка вызова модели` с `exceeds the available context size` | запрос не умещается в контекст сервера. Поставщик v9 сначала сужает окно (до 4 повторов), но в бенчмарке такой отказ проваливает отчёт об оборудовании: увеличить `--ctx-size` узла (§ 2) и повторить зонд; прежний контекст 4096 не вмещает и половину предела продукта ([отчёт](../../models/reports/lidradar-main-v1-prompt-v9-context-probe-4096.json)) |
| пересоздание контейнера llama падает с `open /run/nvidia-persistenced/socket: no such file or directory`; на узле `nvidia-smi` пишет `Driver/library version mismatch` | apt обновил библиотеки NVIDIA, а модуль ядра остался старым (`cat /proc/driver/nvidia/version` ≠ версия `nvidia-utils`), описание GPU для Docker (`/var/run/cdi/nvidia.yaml`) устарело, а его автообновление (`nvidia-cdi-refresh`) упало. Старый контейнер жил, пока его не перезапускали. Перезагрузить узел (`/var/run/reboot-required`), затем `sudo systemctl restart nvidia-cdi-refresh.service` и `docker compose up -d llama-server`; после обновления драйвера перезагружаться до пересоздания контейнеров |
| `finish_reason = length` | ответ оборван по длине: то же, плюс проверить размер вывода |
| `server.errors` в отчёте | сервер не отдал `/props`, `/v1/models` или `/slots`: привязка неполная, манифест не соберётся |
