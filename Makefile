.PHONY: ai-benchmark-context ai-benchmark-dev ai-benchmark-golden ai-benchmark-v2-dev ai-dataset ai-dataset-audit ai-selection-verify build check clean fmt test test-db vet

BIN_DIR ?= bin
COMMANDS := api worker scheduler ai-agent ai-node-register ai-node-manage migrate dev-data
BUILD_VERSION ?= development
BUILD_REVISION ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)
BUILD_LDFLAGS := -X lidradar/backend/platform/buildinfo.Version=$(BUILD_VERSION) -X lidradar/backend/platform/buildinfo.Revision=$(BUILD_REVISION)
AI_BENCHMARK_ENDPOINT ?= http://127.0.0.1:8080/v1/chat/completions
AI_BENCHMARK_PROMPT_VERSION ?= analyze-conversation.prompt.v9
AI_BENCHMARK_SCHEMA_VERSION ?= analyze-conversation.v2
AI_BENCHMARK_TRACE_DIR ?= runtime/ai-benchmark
AI_BENCHMARK_TIMEOUT ?= 2h
AI_SELECTION ?= models/reports/lidradar-main-v1-prompt-v9-selection.json
AI_BENCHMARK_GATES := -minimum-precision 0.90 \
	-minimum-fact-precision 0.85 \
	-minimum-fact-recall 0.85 \
	-minimum-recall 0.90 \
	-minimum-f1 0.90 \
	-minimum-exact-rate 0.85 \
	-minimum-valid-rate 0.99 \
	-minimum-evidence-exact-rate 0.90 \
	-maximum-p95-ms 8000
AI_BENCHMARK := go run ./backend/cmd/ai-benchmark -endpoint $(AI_BENCHMARK_ENDPOINT) \
	-schema-version $(AI_BENCHMARK_SCHEMA_VERSION) -prompt-version $(AI_BENCHMARK_PROMPT_VERSION) -timeout $(AI_BENCHMARK_TIMEOUT) $(AI_BENCHMARK_GATES)

build:
	@mkdir -p $(BIN_DIR)
	@for command in $(COMMANDS); do \
		go build -ldflags "$(BUILD_LDFLAGS)" -o "$(BIN_DIR)/lidradar-$$command" "./backend/cmd/$$command" || exit; \
	done

test:
	go test ./...

# test-db намеренно завершается ошибкой без PostgreSQL: эта цель используется,
# когда пропуск интеграционных проверок недопустим.
test-db:
	@test -n "$$LIDRADAR_DATABASE_URL" || { \
		echo "LIDRADAR_DATABASE_URL обязателен для make test-db" >&2; \
		exit 1; \
	}
	go run ./backend/tools/testgate -output runtime/test-db -- $(GO_TEST_FLAGS)

# Набор создаётся воспроизводимо только из синтетических шаблонов. Команда
# перезаписывает выборки GOLDEN (400) и DEV (100) и контрольную сумму golden-файла.
ai-dataset:
	go run ./backend/cmd/ai-dataset-generate

ai-dataset-audit:
	go run ./backend/cmd/ai-dataset-audit

# Цели печатают в stdout только отчёт JSON (служебные сообщения идут в stderr), поэтому
# `make ai-benchmark-dev > отчёт.json` даёт готовый файл. Выборку DEV можно запускать
# многократно при настройке инструкции. Версии
# контракта и инструкции подменяются в памяти (файлы наборов остаются прежними);
# измерения каждого обращения к модели пишутся в $(AI_BENCHMARK_TRACE_DIR).
# Исторический замер v6: AI_BENCHMARK_SCHEMA_VERSION=analyze-conversation.v1
# AI_BENCHMARK_PROMPT_VERSION=analyze-conversation.prompt.v6.
ai-benchmark-dev:
	@mkdir -p $(AI_BENCHMARK_TRACE_DIR)
	@$(AI_BENCHMARK) -dataset models/datasets/dev_v1.jsonl -checksum '' -trace $(AI_BENCHMARK_TRACE_DIR)/dev.jsonl

# Наборы смысловых ожиданий v2: намерения, договорённости, независимые обещания. Каждый набор
# пишет отчёт в $(AI_BENCHMARK_TRACE_DIR)/<имя>.json; цель запускает все три и возвращает
# ошибку, если порог не прошёл хотя бы в одном.
ai-benchmark-v2-dev:
	@mkdir -p $(AI_BENCHMARK_TRACE_DIR)
	@failed=0; for pair in intent_regression_v2:intents agreements_dev_v2:agreements agreements_independent_v2:independent; do \
		file=$${pair%%:*}; name=$${pair##*:}; \
		$(AI_BENCHMARK) -dataset models/datasets/$$file.jsonl -checksum '' -trace $(AI_BENCHMARK_TRACE_DIR)/$$name.jsonl > $(AI_BENCHMARK_TRACE_DIR)/$$name.json || failed=1; \
		echo "$$name: отчёт $(AI_BENCHMARK_TRACE_DIR)/$$name.json" >&2; \
	done; exit $$failed

# Зонд длинного контекста (scripts/ai-context-probe.py): самая длинная переписка, которую
# собирает продукт. Метки пусты, поэтому код выхода 2 («пороги качества не пройдены») здесь
# ожидаем; решает отчёт об оборудовании по полю performance (отказы сервера, обрыв по длине).
ai-benchmark-context:
	@mkdir -p $(AI_BENCHMARK_TRACE_DIR)
	@$(AI_BENCHMARK) -dataset models/datasets/context_probe_v1.jsonl -checksum '' -trace $(AI_BENCHMARK_TRACE_DIR)/context.jsonl; status=$$?; [ $$status -eq 0 ] || [ $$status -eq 2 ]

# Контрольная выборка защищена суммой и открывается только для окончательного
# решения о фиксации модели. Цель отказывается работать, если файлы tuple
# изменились после записи выбора (scripts/ai-selection.py): GOLDEN нельзя
# запускать, пока инструкция ещё настраивается.
ai-benchmark-golden:
	@python3 scripts/ai-selection.py verify $(AI_SELECTION) >&2
	@mkdir -p $(AI_BENCHMARK_TRACE_DIR)
	@$(AI_BENCHMARK) -dataset models/datasets/golden_v1.jsonl -checksum models/datasets/golden_v1.sha256 -trace $(AI_BENCHMARK_TRACE_DIR)/golden.jsonl

ai-selection-verify:
	@python3 scripts/ai-selection.py verify $(AI_SELECTION)

fmt:
	gofmt -w backend

vet:
	go vet ./...

check: vet test ai-dataset-audit
	go run ./backend/tools/archcheck -root backend

clean:
	rm -rf "$(BIN_DIR)"

# Отдельный стенд фронтенда: .env и основная база никогда не используются.
FRONTEND_COMPOSE := docker compose --env-file /dev/null -f compose.frontend.yaml
.PHONY: frontend-up frontend-stop frontend-data-up frontend-data-down frontend-data-status frontend-background

frontend-up:
	@mkdir -p runtime/frontend
	$(FRONTEND_COMPOSE) up -d --wait postgres
	$(FRONTEND_COMPOSE) run --rm --build migrate
	$(FRONTEND_COMPOSE) run --rm --build --user "$$(id -u):$$(id -g)" dev-data up -password-file /run/lidradar-dev/password.txt
	$(FRONTEND_COMPOSE) up -d --build --wait api

frontend-data-up:
	@mkdir -p runtime/frontend
	$(FRONTEND_COMPOSE) run --rm --build --user "$$(id -u):$$(id -g)" dev-data up -password-file /run/lidradar-dev/password.txt

frontend-data-down:
	$(FRONTEND_COMPOSE) stop api worker scheduler
	$(FRONTEND_COMPOSE) run --rm --build dev-data down -confirm frontend-v1

frontend-data-status:
	$(FRONTEND_COMPOSE) run --rm --build dev-data status

frontend-background:
	$(FRONTEND_COMPOSE) --profile background up -d --build worker scheduler

frontend-stop:
	$(FRONTEND_COMPOSE) --profile background stop
