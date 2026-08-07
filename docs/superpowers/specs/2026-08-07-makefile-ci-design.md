# Makefile + GitHub Actions CI — Design

Date: 2026-08-07

## Goal

Add a basic `Makefile` (build, test, lint including gofmt) and a GitHub Actions
workflow that builds and lints on push/PR (no test yet). Fix any issues so CI
passes green. Deliver via a PR to `main`, merged once checks pass.

## Context

- Standard Go project: `github.com/sklarsa/kanedias`, Go 1.26.5, cobra CLI.
- No existing `Makefile` and no `.github/` directory.
- `golangci-lint` v2.12.2 installed locally; `gofmt`, `go vet`, and `go build`
  are all currently clean.
- Main package builds to a single binary from the repo root (`main.go`).

## Decisions

- **Linter**: `golangci-lint` + `gofmt` (matches local setup, most thorough).
- **CI Go version**: read from `go.mod` via `go-version-file` (low maintenance).

## Components

### Makefile

`.PHONY` targets; `help` is the default target.

- `help` — list available targets (default).
- `build` — `go build -o bin/kanedias .`
- `test` — `go test ./...`
- `fmt` — `gofmt -w` on the tree (convenience helper to fix formatting).
- `lint` — fails if any file is unformatted (`gofmt -l`), then `golangci-lint run ./...`.

The `gofmt` check fails the target when files need formatting:

```make
lint:
	@files=$$(gofmt -l .); if [ -n "$$files" ]; then \
		echo "The following files need gofmt:"; echo "$$files"; exit 1; fi
	golangci-lint run ./...
```

### .gitignore

Add `bin/` so build output is not committed.

### GitHub Actions — `.github/workflows/ci.yml`

- Triggers: `push` and `pull_request`.
- Single job `build-lint` on `ubuntu-latest`:
  1. `actions/checkout@v4`
  2. `actions/setup-go@v5` with `go-version-file: go.mod`
  3. `make build`
  4. gofmt check step (fails on unformatted files)
  5. `golangci-lint/golangci-lint-action@v7` (v7+ is required for
     golangci-lint v2), version pinned to `v2.12.2` to match local behavior.
- No test step yet (per request).

## Error Handling

- `lint` exits non-zero if unformatted files exist or golangci-lint reports issues.
- CI job fails if any step fails (build, gofmt, or lint).

## Testing / Verification

- Run `make build`, `make test`, `make lint` locally; all must pass.
- Fix any `golangci-lint` findings so `make lint` is green.
- Confirm the CI workflow succeeds on the PR before merge.

## Scope Notes

- The unrelated uncommitted change to `internal/image/install.sh` is kept out of
  this PR.
- No `test` step in CI yet (explicitly deferred).
