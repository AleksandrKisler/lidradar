.PHONY: build clean test

BIN_DIR ?= bin
COMMANDS := api worker scheduler ai-agent migrate

build:
	@mkdir -p $(BIN_DIR)
	@for command in $(COMMANDS); do \
		go build -o "$(BIN_DIR)/lidradar-$$command" "./backend/cmd/$$command" || exit; \
	done

test:
	go test ./...

clean:
	rm -rf "$(BIN_DIR)"
