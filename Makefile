.PHONY: build install test lint deps

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
