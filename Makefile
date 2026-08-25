.PHONY: build check clean fmt test test-db vet

BIN_DIR ?= bin
COMMANDS := api worker scheduler ai-agent migrate
BUILD_VERSION ?= development
BUILD_REVISION ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)
BUILD_LDFLAGS := -X lidradar/backend/platform/buildinfo.Version=$(BUILD_VERSION) -X lidradar/backend/platform/buildinfo.Revision=$(BUILD_REVISION)

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

fmt:
	gofmt -w backend

vet:
	go vet ./...

check: vet test
	go run ./backend/tools/archcheck -root backend

clean:
	rm -rf "$(BIN_DIR)"
