.PHONY: ai-benchmark-dev ai-benchmark-golden ai-dataset ai-dataset-audit build check clean fmt test test-db vet

BIN_DIR ?= bin
COMMANDS := api worker scheduler ai-agent ai-node-register ai-node-manage migrate
BUILD_VERSION ?= development
BUILD_REVISION ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)
BUILD_LDFLAGS := -X lidradar/backend/platform/buildinfo.Version=$(BUILD_VERSION) -X lidradar/backend/platform/buildinfo.Revision=$(BUILD_REVISION)
AI_BENCHMARK_ENDPOINT ?= http://127.0.0.1:8080/v1/chat/completions
AI_BENCHMARK_GATES := -minimum-precision 0.90 \
	-minimum-fact-precision 0.85 \
	-minimum-recall 0.90 \
	-minimum-f1 0.90 \
	-minimum-exact-rate 0.85 \
	-minimum-valid-rate 0.99 \
	-minimum-evidence-exact-rate 0.90 \
	-maximum-p95-ms 8000

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
	go test $(GO_TEST_FLAGS) ./...

# Набор создаётся воспроизводимо только из синтетических шаблонов. Команда
# перезаписывает выборки GOLDEN (400) и DEV (100) и контрольную сумму golden-файла.
ai-dataset:
	go run ./backend/cmd/ai-dataset-generate

ai-dataset-audit:
	go run ./backend/cmd/ai-dataset-audit

# Выборку DEV можно запускать многократно при настройке инструкции.
ai-benchmark-dev:
	go run ./backend/cmd/ai-benchmark \
		-dataset models/datasets/dev_v1.jsonl \
		-checksum '' \
		-endpoint $(AI_BENCHMARK_ENDPOINT) \
		$(AI_BENCHMARK_GATES)

# Контрольная выборка защищена суммой и открывается только для окончательного
# решения о фиксации модели.
ai-benchmark-golden:
	go run ./backend/cmd/ai-benchmark \
		-dataset models/datasets/golden_v1.jsonl \
		-checksum models/datasets/golden_v1.sha256 \
		-endpoint $(AI_BENCHMARK_ENDPOINT) \
		$(AI_BENCHMARK_GATES)

fmt:
	gofmt -w backend

vet:
	go vet ./...

check: vet test ai-dataset-audit
	go run ./backend/tools/archcheck -root backend

clean:
	rm -rf "$(BIN_DIR)"
