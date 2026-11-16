# zenget public Go repository

`zenget` is a Cobra-based Go CLI that installs GitHub release binaries and supports local registry, manifests, recipe resolution, and integrity checks. This repository contains public code and documentation.

## Working here

Use Go modules. Source is in `main.go`, `cmd/`, and `internal/`; tests are adjacent `*_test.go` files. `docs/`, `schemas/`, `renovate/`, `scripts/`, and `tools/attestation-harness/` provide documentation, schemas, automation, and an offline attestation spike. User-facing behavior is documented in `README.md`.

Build with `go build ./...` (`task cli:build` writes the `./zenget` binary). Run `task dev:verify` before finishing Go changes; it mirrors the CI test job: gofmt check, `go mod verify` and `go mod tidy -diff`, `go vet ./...`, `scripts/check-coverage.sh` (which runs `go test ./...`), `go test -race ./...` and `go build ./...`. Tasks live in `.tasks/` (`Taskfile.yml` is only the index); the coverage gate requires at least 80% total coverage. Format Go files with `gofmt`. Exported identifiers need accurate doc comments. Use `go mod tidy` after dependency changes. Tests should isolate files with `t.TempDir()` and environment changes with `t.Setenv`; use `httptest` for GitHub API behavior.

The CLI stores runtime state under `$XDG_CONFIG_HOME/zenget` or `~/.config/zenget`. Do not commit credentials, `.env` files, tokens, or private keys. Check `README.md` for CLI behavior and the local pilot against sibling `../zenget-registry`.
