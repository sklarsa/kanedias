BINARY := bin/kanedias

.DEFAULT_GOAL := help

# Load .env (if present) so live/E2E targets pick up local secrets and
# repository pointers. Committed targets that do not need it are unaffected.
ifneq (,$(wildcard .env))
include .env
export
endif

.PHONY: help build test test-live lint fmt

help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

build: ## Build the binary into bin/
	go build -o $(BINARY) .

test: ## Run the hermetic test suite (no Incus, no network)
	go test ./...

test-live: ## Run live Incus/E2E tests (needs Incus + .env; see .env.example)
	@if [ ! -f .env ]; then \
		echo "test-live requires a .env file; copy .env.example and fill it in."; exit 1; fi
	# -count=1 disables Go's test result cache: live tests are destructive and
	# must actually run every time rather than replaying a stale cached pass.
	go test -count=1 -tags incus ./...

fmt: ## Format all Go files in place
	gofmt -w .

lint: ## Check formatting (gofmt) and run golangci-lint
	@files=$$(gofmt -l .); if [ -n "$$files" ]; then \
		echo "The following files need gofmt:"; echo "$$files"; exit 1; fi
	golangci-lint run ./...
