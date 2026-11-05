.PHONY: build install test lint deps run-teams

build:
	CGO_ENABLED=0 go build ./cmd/teams-tui
	CGO_ENABLED=0 go build ./cmd/teams-worker

install:
	CGO_ENABLED=0 go install ./cmd/teams-tui
	CGO_ENABLED=0 go install ./cmd/teams-worker

test:
	go test ./...

lint:
	golangci-lint run --allow-parallel-runners

deps:
	go mod tidy
	go mod vendor

run-teams: build
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
	if [ -z "$$EXTRACTED" ] || [ ! -d "$$EXTRACTED" ]; then \
		echo "Could not locate extracted AppDir in $$WRAPPER" >&2; \
		exit 1; \
	fi; \
	export TEAMS_LOGGER_WORKER="$(CURDIR)/teams-worker"; \
	export TEAMS_LOGGER_SCRIPT="$(CURDIR)/message-logger.js"; \
	echo "Starting teams-for-linux with live $(CURDIR)/message-logger.js and $(CURDIR)/teams-worker ..."; \
	sed "s|--bind /nix /nix|--bind /nix /nix --ro-bind \"$(CURDIR)/message-logger.js\" \"$$EXTRACTED/resources/app/app/browser/message-logger.js\"|" "$$(command -v appimage-run)" | bash -s -- -w "$$EXTRACTED" -- $(TEAMS_ARGS)
