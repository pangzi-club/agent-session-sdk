.PHONY: help build build-cli test vet fmt check tidy

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "} {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'

build: ## Build all packages (no cgo)
	CGO_ENABLED=0 go build ./...

build-cli: ## Build the agent-session-sdk CLI binary into ./bin
	CGO_ENABLED=0 go build -o bin/agent-session-sdk ./cmd/agent-session-sdk

test: ## Run tests (no cgo)
	CGO_ENABLED=0 go test ./...

vet: ## Run go vet
	go vet ./...

fmt: ## Format all Go sources
	gofmt -w *.go

check: build vet test ## Run the full pre-commit check

tidy: ## Tidy go.mod dependencies
	go mod tidy