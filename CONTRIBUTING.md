# Contributing to bosh-pve-cpi-release

Thank you for helping improve the BOSH PVE CPI. We welcome bug reports, documentation fixes, and code contributions. This guide explains how to report a problem, set up a development environment, and submit a change.

## Reporting issues

Open an issue on GitHub when you find a bug or want to propose a feature. A good bug report lets us reproduce the problem without guessing. Please include:

- The Proxmox VE version and the BOSH Director version you are running.

- The CPI release version, or the commit hash if you built from source.

- The BOSH task output and the relevant CPI log lines. The CPI redacts credentials from its logs, but please double check before you paste.

- The steps that trigger the problem, as precisely as you can.

For feature requests, describe the problem you want to solve rather than only the change you have in mind. Knowing the goal helps us weigh alternatives.

## Before you start a large change

For small fixes, a pull request is enough. For anything larger, such as a new CPI method, a new configuration property, or a change in default behavior, please open an issue first and describe your plan. This avoids wasted work when a design needs discussion, and it gives us a place to record the decision.

## Development

### Prerequisites

- Go 1.26 or higher. The BOSH packaging compiles against the `golang-1.26` blob, so local builds should match.

- `golangci-lint`. When it is not installed, `make lint` falls back to `go run` at a pinned version.

- `staticcheck`, `govulncheck`, and `gosec` are optional. The corresponding make targets skip with a notice when a tool is missing.

Go sources live under `src/pve_cpi/`. Direct `go test` and `go build` invocations must run from that directory. The `make` targets re-root automatically, so you can run them from the repository root.

### Running unit tests

```bash
make test
```

This runs all Go tests with race detection. Every code change should come with tests that cover it.

### Running the full check suite

```bash
make check
```

This runs `fmt-check`, `vet`, `staticcheck`, `lint`, `coverage-check`, and `test` in order, stopping at the first failure. CI runs the same target on every push, so a green `make check` locally means CI should pass too. The coverage gate is 80 percent.

### Running security scans

```bash
make security
```

This runs `govulncheck`, `gosec`, and `trivy`. We run these scans before every release, and CI runs them as well.

### Running lifecycle tests

This step is optional. Green unit tests are enough for a pull request. If you want to validate a change against a real cluster, a local harness exercises the canonical CPI methods end to end:

```bash
export CPI_CONFIG=~/.bosh-pve-cpi/cpi.json
export STEMCELL_PATH=/path/to/bosh-stemcell-*.tgz
./scripts/lifecycle
```

The harness needs a live Proxmox VE cluster and will create and destroy real VMs and disks on it. See [CPI certification](docs/bosh-cpi-certification.md) for the prerequisites and the config schema.

## Submitting a pull request

1. Fork the repository and create a branch for your change.

2. Make the change, with tests.

3. Run `make check` and make sure it passes.

4. Open a pull request against `main`. Describe what the change does and why. Link the related issue if one exists.

Keep each pull request focused on one change. A small, focused pull request is easier to review and lands faster than a large one that mixes concerns.

### Commit messages

Write commit messages that describe the code change, not the process that produced it. This repository follows the Conventional Commits style: a type prefix such as `fix:`, `feat:`, `docs:`, or `ci:`, followed by a short summary in the imperative mood. Look at `git log` for examples.

## License

This project is licensed under the [Apache License, Version 2.0](LICENSE). By submitting a contribution, you agree that it will be licensed under the same terms.
