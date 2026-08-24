.PHONY: build check clean fmt test vet

BIN_DIR ?= bin
COMMANDS := api worker scheduler ai-agent migrate

build:
	@mkdir -p $(BIN_DIR)
	@for command in $(COMMANDS); do \
		go build -o "$(BIN_DIR)/lidradar-$$command" "./backend/cmd/$$command" || exit; \
	done

test:
	go test ./...

fmt:
	gofmt -w backend

vet:
	go vet ./...

check: vet test
	go run ./backend/tools/archcheck -root backend

clean:
	rm -rf "$(BIN_DIR)"
