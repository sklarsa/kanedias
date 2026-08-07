BINARY := bin/kanedias

.DEFAULT_GOAL := help

.PHONY: help build test lint fmt

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'

build: ## Build the binary into bin/
	go build -o $(BINARY) .

test: ## Run all tests
	go test ./...

fmt: ## Format all Go files in place
	gofmt -w .

lint: ## Check formatting (gofmt) and run golangci-lint
	@files=$$(gofmt -l .); if [ -n "$$files" ]; then \
		echo "The following files need gofmt:"; echo "$$files"; exit 1; fi
	golangci-lint run ./...
