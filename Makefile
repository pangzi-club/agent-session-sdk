.PHONY: help build build-cli test test-race vet fmt check tidy

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "} {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'

build: ## Build all packages (no cgo)
	CGO_ENABLED=0 go build ./...

build-cli: ## Build the agent-session-sdk CLI binary into ./bin
	CGO_ENABLED=0 go build -o bin/agent-session-sdk ./cmd/agent-session-sdk

test: ## Run tests (no cgo)
	CGO_ENABLED=0 go test ./...

test-race: ## Run tests with the race detector (as CI does on Linux)
	go test -race ./...

vet: ## Run go vet
	go vet ./...

fmt: ## Format all Go sources
	gofmt -w ./*.go ./cmd/agent-session-sdk/*.go

check: build vet test test-race ## Run the full pre-commit check (mirrors CI)

tidy: ## Tidy go.mod dependencies
	go mod tidy
