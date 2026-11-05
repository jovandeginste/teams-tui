.PHONY: build install test lint deps run-teams

build:
	CGO_ENABLED=0 go build ./cmd/teams-tui

install:
	CGO_ENABLED=0 go install ./cmd/teams-tui

test:
	go test ./...

lint:
	golangci-lint run --allow-parallel-runners

deps:
	go mod tidy
	go mod vendor

run-teams:
	@WRAPPER="$$(command -v teams-for-linux)"; \
	if [ -z "$$WRAPPER" ]; then \
		echo "teams-for-linux not found in PATH" >&2; \
		exit 1; \
	fi; \
	if pgrep -x teams-for-linux >/dev/null 2>&1; then \
		echo "Closing existing teams-for-linux instance first..."; \
		pkill -x teams-for-linux || true; \
		sleep 1; \
	fi; \
	EXTRACTED="$$(sed -n 's/.*appimage-run -w \([^ ]*\).*/\1/p' "$$WRAPPER" | head -n1)"; \
	NODE_BIN="$$(sed -n 's/.*TEAMS_LOGGER_NODE="\([^"]*\)".*/\1/p' "$$WRAPPER" | head -n1)"; \
	if [ -z "$$EXTRACTED" ] || [ ! -d "$$EXTRACTED" ]; then \
		echo "Could not locate extracted AppDir in $$WRAPPER" >&2; \
		exit 1; \
	fi; \
	export TEAMS_LOGGER_NODE="$${NODE_BIN:-$$(command -v node)}"; \
	export TEAMS_LOGGER_SCRIPT="$(CURDIR)/message-logger.js"; \
	echo "Starting teams-for-linux with live $(CURDIR)/message-logger.js ..."; \
	sed "s|--bind /nix /nix|--bind /nix /nix --ro-bind \"$(CURDIR)/message-logger.js\" \"$$EXTRACTED/resources/app/app/browser/message-logger.js\"|" "$$(command -v appimage-run)" | bash -s -- -w "$$EXTRACTED" -- $(TEAMS_ARGS)
