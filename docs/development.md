# Development Guide

This guide covers building, testing, and contributing to the BOSH PVE CPI Go codebase.

## Prerequisites

- Go 1.27 or later (BOSH packaging compiles against the `golang-1.27` blob; see `packages/golang-1.27/`)
- BOSH CLI v2 (optional, for creating BOSH releases): https://bosh.io/docs/cli-v2.html
- PVE 9.x (optional, for end-to-end integration testing; unit tests do not require a live instance)

## Setup

```bash
git clone https://github.com/fivetwenty-io/bosh-pve-cpi
cd bosh-pve-cpi
cd src/pve_cpi && go mod download
```

Go sources live under `src/pve_cpi/` so the layout matches the BOSH release packaging. The repository vendors its dependencies; `go build -mod=vendor` works offline.

The Makefile re-roots every `go` invocation into `src/pve_cpi/` — prefer the `make` targets below.

## Make Targets

| Target          | Purpose                                                  |
|-----------------|----------------------------------------------------------|
| `make build`    | Compile the CPI binary into `./bin/cpi`                  |
| `make test`     | Run unit tests with race detector                        |
| `make coverage` | Generate `coverage.out` and print per-function coverage  |
| `make coverage-check` | Fail if total coverage drops below 80%             |
| `make fmt`      | Run `gofmt -w` on all Go files                           |
| `make vet`      | Run `go vet ./...`                                       |
| `make staticcheck` | Run `staticcheck ./...` when installed                |
| `make check`    | `vet` + `staticcheck` + `test`                           |
| `make security` | `govulncheck` + `gosec`                                  |
| `make tidy`     | Run `go mod tidy`                                        |
| `make clean`    | Remove build artifacts                                   |
| `make release`  | `check` + `security` + `bin/cpi`                         |

## Running a Subset of Tests

```bash
cd src/pve_cpi && go test -run TestHandleCreateVM ./internal/cpi/handlers
```

Use `-race` and `-count=1` to disable the test cache and surface data races.

```bash
cd src/pve_cpi && go test -race -count=1 ./internal/pve
```

## Building a BOSH Release

The repository ships BOSH job and package definitions under `jobs/pve_cpi/`, `packages/pve_cpi/`, and `packages/golang-1.27/`. The Go toolchain ships as a blob — register it before the first release:

```bash
make download-blobs   # downloads + sha256-verifies + bosh add-blob
```

To create a development release after a code change:

```bash
make dev-release      # or: bosh create-release --force
```

A final release pins a version:

```bash
bosh create-release --version=X.Y.Z --tarball=releases/pve-cpi-X.Y.Z.tgz
```

## Logging

The binary writes JSON logs to stderr. The log level is controlled by the `pve.log_level` config property (`debug`, `info`, `warn`, `error`). Each log record includes `request_id` and `method` attributes extracted from the request context.

The BOSH job template wires stdout to the JSON-RPC channel and stderr to `/var/vcap/sys/log/bosh/cpi/pve.log` via the standard BOSH log redirection.

## Integration Testing

To verify the CPI against a live PVE instance:

1. Build the binary with `make bin/cpi`.

2. Write a `cpi.json` config matching the schema in `docs/configuration.md`.

3. Use the BOSH CPI test framework (https://github.com/cloudfoundry/bosh-cpi-certification) or hand-craft JSON-RPC requests piped to stdin.
