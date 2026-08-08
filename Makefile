BINARY := bin/kanedias
# Config the CLI/server/proxy commands load. Override with CONFIG=/path/to/config.toml.
CONFIG ?= config.toml

.DEFAULT_GOAL := help

# Load .env (if present) so live/E2E targets pick up local secrets and
# repository pointers. Committed targets that do not need it are unaffected.
ifneq (,$(wildcard .env))
include .env
export
endif

.PHONY: help build test test-live lint fmt run server proxy

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

run: build ## Run the egress proxy (only if not already up) + web server on 127.0.0.1:8080; Ctrl-C stops the server (and the proxy only if we started it)
	@if ss -ltn 2>/dev/null | grep -q ':3128 '; then \
		echo "Egress proxy (10.76.111.1:3128) already running; reusing it."; \
	else \
		echo "Starting egress proxy (10.76.111.1:3128) in the background..."; \
		$(BINARY) --config $(CONFIG) proxy run & \
		proxy_pid=$$!; \
		trap 'echo "stopping proxy..."; kill $$proxy_pid 2>/dev/null || true' EXIT INT TERM; \
	fi
	$(BINARY) --config $(CONFIG) server --listen 127.0.0.1:8080

server: build ## Run the web server on 127.0.0.1:8080 (sessions also need the proxy; see `proxy` or `run`)
	$(BINARY) --config $(CONFIG) server --listen 127.0.0.1:8080

proxy: build ## Run the egress credential proxy (needed for sandboxed sessions to reach the network/model)
	$(BINARY) --config $(CONFIG) proxy run