.PHONY: build install test lint deps run-teams appimage appimage-nix

V ?= 2.28.0

build:
	@mkdir -p bin
	CGO_ENABLED=0 go build -o bin/teams-tui ./cmd/teams-tui
	CGO_ENABLED=0 go build -o bin/teams-worker ./cmd/teams-worker

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
	./run-teams.sh $(TEAMS_ARGS)

appimage:
	./build-appimage.sh -V "$(V)"

appimage-nix:
	./build-appimage-nix.sh -V "$(V)"
