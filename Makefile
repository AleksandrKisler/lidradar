.PHONY: ai-benchmark-commitments ai-benchmark-commitments-independent ai-benchmark-commitments-independent-v2 ai-benchmark-context ai-benchmark-dev ai-benchmark-golden ai-benchmark-holdout ai-benchmark-v2-dev ai-dataset ai-dataset-audit ai-lexicon-audit ai-scan-probe ai-selection-verify build check clean fmt test test-db vet

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
AI_SELECTION ?= models/reports/lidradar-main-v1-prompt-v9r4-selection.json
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

# Набор обязательств компании (ADR 0054) измеряет один тип факта, BUSINESS_COMMITMENT. Общие пороги здесь не решают:
# отрицательные факты других типов в строгой разметке считаются ложными, поэтому код выхода 2 допустим, а решение
# принимает scripts/ai-manifest.py по полноте и точности именно этого типа. Отчёт — в stdout.
ai-benchmark-commitments:
	@mkdir -p $(AI_BENCHMARK_TRACE_DIR)
	@$(AI_BENCHMARK) -dataset models/datasets/commitment_dev_v1.jsonl -checksum '' -trace $(AI_BENCHMARK_TRACE_DIR)/commitments.jsonl; status=$$?; [ $$status -eq 0 ] || [ $$status -eq 2 ]

# Слепая выборка обязательств запускается один раз для записанного выбора, как GOLDEN: цель сначала проверяет выбор,
# а сумма выборки утверждена в commitment_holdout_v3.sha256 до первого прогона. Тексты выборки не читают.
# Ревизии 4 (ADR 0055) принадлежит commitment_holdout_v3; commitment_holdout_v1 (ревизия 2) и commitment_holdout_v2 (ревизия 3) прочитаны и теперь наборы настройки.
AI_HOLDOUT ?= commitment_holdout_v3
ai-benchmark-holdout:
	@python3 scripts/ai-selection.py verify $(AI_SELECTION) >&2
	@mkdir -p $(AI_BENCHMARK_TRACE_DIR)
	@$(AI_BENCHMARK) -dataset models/datasets/$(AI_HOLDOUT).jsonl -checksum models/datasets/$(AI_HOLDOUT).sha256 -trace $(AI_BENCHMARK_TRACE_DIR)/holdout.jsonl; status=$$?; [ $$status -eq 0 ] || [ $$status -eq 2 ]

# Бывшая слепая выборка v1 (188 переписок): после слепой проверки ревизии 2 она прочитана и служит независимым набором настройки.
# Отчёт — в stdout; результаты по ней не доказывают обобщение: настройка шла и по её текстам.
ai-benchmark-commitments-independent:
	@mkdir -p $(AI_BENCHMARK_TRACE_DIR)
	@$(AI_BENCHMARK) -dataset models/datasets/commitment_holdout_v1.jsonl -checksum models/datasets/commitment_holdout_v1.sha256 -trace $(AI_BENCHMARK_TRACE_DIR)/commitments-independent.jsonl; status=$$?; [ $$status -eq 0 ] || [ $$status -eq 2 ]

# Слепая выборка v2 ревизии 3 (248 переписок): единственный прогон ревизии 3 сделан 2026-10-11, для ревизии 4 выборка прочитана и служит набором настройки.
ai-benchmark-commitments-independent-v2:
	@mkdir -p $(AI_BENCHMARK_TRACE_DIR)
	@$(AI_BENCHMARK) -dataset models/datasets/commitment_holdout_v2.jsonl -checksum models/datasets/commitment_holdout_v2.sha256 -trace $(AI_BENCHMARK_TRACE_DIR)/commitments-independent-v2.jsonl; status=$$?; [ $$status -eq 0 ] || [ $$status -eq 2 ]

# Только проход по обещаниям (ADR 0055): вероятность «обещание» по каждому исходящему сообщению набора и состояние словаря, без текстов.
# Нужен для настройки; качество ревизии измеряет ai-benchmark. Пример: make ai-scan-probe AI_SCAN_DATASET=models/datasets/commitment_dev_v1.jsonl
AI_SCAN_DATASET ?= models/datasets/commitment_dev_v1.jsonl
ai-scan-probe:
	@go run ./backend/cmd/ai-scan-probe -dataset $(AI_SCAN_DATASET) -endpoint $(AI_BENCHMARK_ENDPOINT)

# Словарь обещаний без модели: покрытие обещаний и ложные допуски на наборе DEV. Выборки HOLDOUT так не читают.
ai-lexicon-audit:
	@go run ./backend/cmd/ai-lexicon-audit -dataset models/datasets/commitment_dev_v1.jsonl -summary

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
