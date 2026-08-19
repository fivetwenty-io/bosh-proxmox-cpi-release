# CI Image Requirements

The integration task defined in `ci/tasks/integration.yml` requires a Docker image that includes several CLIs not present in the stock `bosh/bosh-cli` image. This document describes what the image must contain, provides an example Dockerfile, and shows how to wire it into the pipeline.

---

## Why a custom image is required

The `bosh/bosh-cli` image ships only the BOSH CLI and its runtime dependencies. The integration task runs `./scripts/test integration all`, which calls `scripts/bosh`, `scripts/cf`, `scripts/lifecycle`, and several Python helper scripts, which require:

- **cf** — not present in `bosh/bosh-cli`
- **credhub** — not present in `bosh/bosh-cli`
- **uv** — not present in any standard BOSH image; required to execute PEP 723 inline-dependency scripts (`#!/usr/bin/env -S uv run --script`)
- **go** and **make** — to compile `bin/cpi` before the lifecycle tier runs

A custom image that bundles all of these tools is required for the integration job to succeed.

---

## Required CLIs

| Tool | Minimum version | Install reference |
|------|----------------|-------------------|
| `bosh` | 7.0.0 | https://github.com/cloudfoundry/bosh-cli/releases |
| `cf` | 8.0.0 | https://github.com/cloudfoundry/cli/releases |
| `credhub` | 2.9.0 | https://github.com/cloudfoundry-incubator/credhub-cli/releases |
| `uv` | 0.4.0 | https://github.com/astral-sh/uv/releases (or `curl -LsSf https://astral.sh/uv/install.sh`) |
| `go` | 1.26.6 | https://go.dev/dl/ (must match `src/pve_cpi/go.mod`) |
| `make` | 4.3 | System package (`apt-get install make`) |
| `jq` | 1.6 | System package (`apt-get install jq`) |
| `curl` | 7.88 | System package (`apt-get install curl`) |
| `base64` | (coreutils) | System package (`apt-get install coreutils`) |
| `ssh` | (openssh-client) | System package (`apt-get install openssh-client`) |
| `git` | 2.39 | System package (`apt-get install git`) |
| `python3` | 3.11 | Required by `uv` runtime; `uv` downloads and pins a matching interpreter automatically |

The Python scripts use only the standard library. `uv` manages the Python runtime; no extra packages need to be pre-installed.

---

## Example Dockerfile

This multi-stage Dockerfile pins every binary version so CI runs are reproducible. Adjust version numbers on upgrade.

```dockerfile
# syntax=docker/dockerfile:1

ARG GO_VERSION=1.26.6
ARG BOSH_CLI_VERSION=7.9.6
ARG CF_CLI_VERSION=8.9.0
ARG CREDHUB_CLI_VERSION=2.9.45
ARG UV_VERSION=0.4.30

# ── Stage 1: download binaries ─────────────────────────────────────────────
FROM debian:bookworm-slim AS downloader

ARG BOSH_CLI_VERSION
ARG CF_CLI_VERSION
ARG CREDHUB_CLI_VERSION
ARG UV_VERSION

RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /downloads

# bosh CLI
RUN curl -fsSL \
    "https://github.com/cloudfoundry/bosh-cli/releases/download/v${BOSH_CLI_VERSION}/bosh-cli-${BOSH_CLI_VERSION}-linux-amd64" \
    -o bosh && chmod +x bosh

# cf CLI (v8 tarball ships a single binary named cf8 → rename to cf)
RUN curl -fsSL \
    "https://packages.cloudfoundry.org/stable?release=linux64-binary&version=${CF_CLI_VERSION}&source=github" \
    -o cf-cli.tgz && tar xzf cf-cli.tgz cf8 && mv cf8 cf && chmod +x cf

# credhub CLI
RUN curl -fsSL \
    "https://github.com/cloudfoundry-incubator/credhub-cli/releases/download/${CREDHUB_CLI_VERSION}/credhub-linux-amd64-${CREDHUB_CLI_VERSION}.tgz" \
    -o credhub.tgz && tar xzf credhub.tgz && chmod +x credhub

# uv
RUN curl -fsSL \
    "https://github.com/astral-sh/uv/releases/download/${UV_VERSION}/uv-x86_64-unknown-linux-musl.tar.gz" \
    -o uv.tgz && tar xzf uv.tgz --strip-components=1 "uv-x86_64-unknown-linux-musl/uv" && chmod +x uv

# ── Stage 2: Go toolchain ──────────────────────────────────────────────────
FROM golang:${GO_VERSION}-bookworm AS gobase

# ── Stage 3: final image ───────────────────────────────────────────────────
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        coreutils \
        git \
        jq \
        make \
        openssh-client \
    && rm -rf /var/lib/apt/lists/*

# Go toolchain from official image
COPY --from=gobase /usr/local/go /usr/local/go
ENV PATH="/usr/local/go/bin:${PATH}"

# Downloaded CLIs
COPY --from=downloader /downloads/bosh     /usr/local/bin/bosh
COPY --from=downloader /downloads/cf       /usr/local/bin/cf
COPY --from=downloader /downloads/credhub  /usr/local/bin/credhub
COPY --from=downloader /downloads/uv       /usr/local/bin/uv

# uv manages its own Python runtimes in a cache directory; point it somewhere writable.
ENV UV_PYTHON_INSTALL_DIR=/opt/uv/python

RUN useradd -m -u 1000 ci
USER ci
WORKDIR /home/ci
```

Build and push the image:

```
docker build \
  --build-arg GO_VERSION=1.26.6 \
  --build-arg BOSH_CLI_VERSION=7.9.6 \
  --build-arg CF_CLI_VERSION=8.9.0 \
  --build-arg CREDHUB_CLI_VERSION=2.9.45 \
  --build-arg UV_VERSION=0.4.30 \
  -t registry.example.com/bosh-pve-cpi-ci:latest \
  -f ci/Dockerfile .

docker push registry.example.com/bosh-pve-cpi-ci:latest
```

Store the Dockerfile at `ci/Dockerfile` in the repository so version bumps are tracked in git.


---

## Pipeline integration

Replace the `image_resource` stanza in `ci/tasks/integration.yml` after the custom image is built and pushed:

```yaml
image_resource:
  type: registry-image
  source:
    repository: registry.example.com/bosh-pve-cpi-ci
    tag: latest
    # If the registry requires authentication:
    # username: ((registry_username))
    # password: ((registry_password))
```

The `registry-image` resource type is preferred over `docker-image` for new pipelines — it pulls OCI manifests directly and does not require a Docker daemon on the worker.

If the registry is private, add the credentials to your Concourse secrets store and reference them with `((double-paren))` interpolation as shown above.


---

## Verification

After building the image, run the following one-liner to confirm every required tool is present and meets the minimum version.


```
docker run --rm registry.example.com/bosh-pve-cpi-ci:latest \
  sh -c 'bosh --version && cf --version && credhub --version && uv --version && go version && make --version'
```

Expected output (versions will vary based on build args):

```
BOSH CLI version 7.9.6-...
cf version 8.9.0+...
credhub 2.9.45
uv 0.4.30 (...)
go version go1.26.6 linux/amd64
GNU Make 4.3
```

If any command fails, check that the corresponding `COPY --from=downloader` line in the Dockerfile completed successfully. Rebuild with `--no-cache` if a download was silently skipped because of a layer cache hit on a changed URL.
