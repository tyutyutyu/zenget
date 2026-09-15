# zenget public Go repository

`zenget` is a Cobra-based Go CLI that installs GitHub release binaries and supports local registry, manifests, recipe resolution, and integrity checks. This repository contains public code and documentation; planning data lives in the sibling `../zenget-backlog/.backlog/` repository.

## Working here

When working in `zenget-workspace`, run `backlog instructions overview` from `../zenget-backlog/` for every request, then follow the matching Backlog task guide when the request involves task lifecycle work. Use the Backlog CLI to change task metadata and content. A standalone clone can be built and tested without the sibling planning repository. Keep `.backlog/` out of this public repository.

Use Go modules. Source is in `main.go`, `cmd/`, and `internal/`; tests are adjacent `*_test.go` files. `docs/`, `schemas/`, `renovate/`, `scripts/`, and `tools/attestation-harness/` provide documentation, schemas, automation, and an offline attestation spike. User-facing behavior is documented in `README.md`.

Build with `go build ./...`. Run `go test ./...`, `go vet ./...`, and `scripts/check-coverage.sh` before finishing Go changes; the coverage gate requires at least 80% total coverage. Format Go files with `gofmt`. Exported identifiers need accurate doc comments. Use `go mod tidy` after dependency changes. Tests should isolate files with `t.TempDir()` and environment changes with `t.Setenv`; use `httptest` for GitHub API behavior.

The CLI stores runtime state under `$XDG_CONFIG_HOME/zenget` or `~/.config/zenget`. Do not commit credentials, `.env` files, tokens, or private keys. Check `README.md` for CLI behavior and the local pilot against sibling `../zenget-registry`.
