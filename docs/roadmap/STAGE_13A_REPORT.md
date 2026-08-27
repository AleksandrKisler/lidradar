# Отчёт по части 13A — облачный контур домашнего AI-узла

Дата итоговой проверки: 27 августа 2026 года.

## Результат

Часть 13A реализует весь серверный и агентский контур, который не требует
физической RTX 4060. PostgreSQL остаётся источником истины, домашний узел
самостоятельно обращается к Cloud Core, а `llama.cpp` не публикуется наружу.

```text
PostgreSQL
    ↑
внутренний подписанный API
    ↑ исходящий HTTPS
Go AI Agent
    ↓ закрытая Docker-сеть
Fake Provider либо llama.cpp
```

Этап 13 целиком пока не закрыт: натурные проверки Ubuntu reboot, CUDA,
загрузки модели и физического обрыва домашнего интернета выполняются в части
13B после ремонта оборудования.

## Выполненные задачи

- `LR-BE-1301`: `ai_nodes` хранит узлы, digest секрета, heartbeat, модель и
  доступную ёмкость.
- `LR-BE-1302`: `ai_jobs` хранит авторитетную очередь со статусами `PENDING`,
  `LEASED`, `RUNNING`, `SUCCEEDED`, `RETRY`, `DEAD`.
- `LR-BE-1303`: каждая попытка получает долговечный `ai_runs`; потерянная
  аренда закрывает старую попытку безопасным кодом.
- `LR-BE-1304`: job/run хранят revision, последнее проанализированное сообщение,
  версии модели, prompt и схемы.
- `LR-BE-1305`: запрос содержит Node ID, Bearer secret, UTC timestamp,
  уникальный request ID, hash тела и HMAC-подпись. Повтор request ID в пределах
  окна отклоняется. В PostgreSQL находится только SHA-256 digest секрета.
  Отдельная команда безопасно меняет секрет либо отзывает узел.
- `LR-BE-1306`: heartbeat сообщает `READY/OFFLINE`, model version и свободный
  slot; действующая аренда продлевается только её текущему владельцу.
- `LR-BE-1307`: claim использует транзакцию и `FOR UPDATE SKIP LOCKED`,
  учитывает `max_inflight=1`, точное совпадение версии модели и возвращает
  просроченную работу в обработку.
- `LR-BE-1308`: started/complete/failed идемпотентно изменяют job и run;
  свежесть результата повторно проверяется внутри транзакции завершения.
- `LR-BE-1309`: heartbeat продлевает 120-секундную аренду во время долгой
  генерации; прежний владелец не может завершить повторно захваченное задание.
- `LR-BE-1310`: рабочая команда `ai-agent` использует настоящий HTTP-клиент,
  продолжает heartbeat во время inference и не сохраняет prompt/result на диск.
- `LR-BE-1311`: `LlamaProvider` проверяет `/health` и вызывает совместимый с
  OpenAI маршрут `/v1/chat/completions`. Физическая CUDA-проверка отложена до
  части 13B.
- `LR-BE-1312`: добавлен отдельный `docker-compose.ai.yml` с политикой
  `restart: unless-stopped`. Реальный reboot ещё не доказан.
- `LR-BE-1313`: Fake Provider остаётся обязательным и строит результат для
  фактического `analysisThroughMessageId` из задания.
- `LR-BE-1314`: автоматический PostgreSQL-тест имитирует пропадание первого
  узла, истечение lease и захват вторым. Физический обрыв сети остаётся для 13B.

## Подпись машинного запроса

Каноническая строка:

```text
HTTP_METHOD
URL_PATH
UTC_RFC3339_NANO_TIMESTAMP
REQUEST_UUID
SHA256_HEX_OF_EXACT_BODY
```

Подпись:

```text
X-LidRadar-Signature = sha256=<HMAC-SHA256(node_secret, canonical)>
```

Обязательные заголовки:

```text
Authorization: Bearer <node_secret>
X-LidRadar-Node-ID: <uuid>
X-LidRadar-Timestamp: <UTC RFC3339Nano>
X-LidRadar-Request-ID: <uuid>
X-LidRadar-Signature: sha256=<64 hex>
```

Authorization, подписи, prompt, customer message и raw result не журналируются.

## Безопасная регистрация

Команда создаёт новый файл с правами `0600`, не перезаписывает существующий и
не печатает `node_secret`:

```bash
mkdir -p runtime

LIDRADAR_ENV=development \
LIDRADAR_DATABASE_URL='postgres://lidradar:lidradar@127.0.0.1:5432/lidradar?sslmode=disable' \
go run ./backend/cmd/ai-node-register \
  --name AI-NODE-01 \
  --output runtime/ai-node.json
```

Каталог `runtime/` исключён из Git. Для домашнего узла файл переносится в
`/srv/lidradar/config/ai-node.json` по доверенному каналу.

Смена секрета создаёт новый файл и немедленно переводит узел в `OFFLINE`.
Старый секрет перестаёт работать, а незавершённая аренда естественно истекает:

```bash
LIDRADAR_ENV=development \
LIDRADAR_DATABASE_URL='postgres://lidradar:lidradar@127.0.0.1:5432/lidradar?sslmode=disable' \
go run ./backend/cmd/ai-node-manage rotate \
  --node-id '<uuid-узла>' \
  --output runtime/ai-node-rotated.json
```

Безвозвратный отзыв сохраняет историю заданий и попыток, но запрещает узлу все
последующие запросы:

```bash
LIDRADAR_ENV=development \
LIDRADAR_DATABASE_URL='postgres://lidradar:lidradar@127.0.0.1:5432/lidradar?sslmode=disable' \
go run ./backend/cmd/ai-node-manage revoke --node-id '<uuid-узла>'
```

Ни одна из команд не выводит секрет. Новый файл не перезаписывает существующий
и получает права `0600`.

## Локальная проверка без GPU

```bash
docker compose up -d postgres
docker compose run --rm migrate
docker compose up -d api

LIDRADAR_AI_CREDENTIALS_HOST_FILE="$PWD/runtime/ai-node.json" \
docker compose --profile ai up -d ai-agent
```

В этом режиме агент использует Fake Provider. Основной `compose.yaml` не
запускает его без явного профиля `ai`.

## Проверка домашнего узла после ремонта

```bash
export LIDRADAR_AI_CLOUD_URL='https://<устойчивый-домен-cloud-core>'
export LIDRADAR_AI_CREDENTIALS_HOST_FILE='/srv/lidradar/config/ai-node.json'

docker compose -f docker-compose.ai.yml config --quiet
docker compose -f docker-compose.ai.yml pull
docker compose -f docker-compose.ai.yml up -d
docker compose -f docker-compose.ai.yml ps
```

`llama-server` находится только в сети `ai-internal`; секции `ports` нет.
`LIDRADAR_AI_MODEL_VERSION` на Cloud Core и домашнем агенте должен совпадать:
задание другой версии модели узел намеренно не забирает.

## Проверки части 13A

На PostgreSQL 18 успешно выполнены:

```bash
make test-db GO_TEST_FLAGS='-race -count=1'
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@v0.6.1 ./...
go run ./backend/tools/archcheck -root backend
make build
npx --yes @redocly/cli@1.34.5 lint contracts/openapi/openapi.yaml
docker compose config --quiet
docker compose -f docker-compose.ai.yml config --quiet
```

Дымовой запуск API вернул `200 ready` и последнюю миграцию
`000013_home_ai_node`. Отдельные тесты доказывают неверную подпись, повтор
request ID, изменение тела, смену и отзыв секрета, несовпадение модели,
heartbeat во время долгого inference, истечение lease, повторный claim,
запрет позднего ответа прежнего узла и изменение revision в момент завершения.

Испытательная Docker-сборка на этой машине остановилась до компиляции: исходящий
доступ BuildKit к Alpine и Go Proxy вернул `connection refused`. Все локальные
исполняемые файлы собираются; CI повторяет сборку образа в сетевой среде.

## Оставшийся выходной критерий 13B

```text
Ubuntu reboot
→ Docker auto-start
→ llama.cpp model ready
→ AI Agent heartbeat READY
→ claim
→ реальный inference
→ validated result в PostgreSQL
→ физический disconnect
→ lease expiry/reclaim
→ core API и Money Loop продолжают работать
```
