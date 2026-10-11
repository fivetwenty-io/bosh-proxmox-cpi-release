# syntax=docker/dockerfile:1
#
# Canonical CI image for bosh-proxmox-cpi-release.
#
# Built by .github/workflows/ci-image.yml and pushed to
# ghcr.io/fivetwenty-io/bosh-proxmox-cpi-ci. Used by:
#   - the scheduled certification workflow (.github/workflows/certification.yml),
#     which runs scripts/certify, scripts/bosh, and scripts/bats
#   - the Concourse integration task (ci/tasks/integration.yml), which runs
#     ./scripts/test integration all
# See docs/ci-image.md for the tool matrix.
#
# bosh and gh are sha256-pinned with the same published checksums
# .github/workflows/release.yml verifies; the cf, credhub, and uv downloads
# are version-pinned over TLS from their vendors' release channels.

ARG GO_VERSION=1.27.2
ARG RUBY_VERSION=3.3
ARG BOSH_CLI_VERSION=7.11.0
# sha256 of bosh-cli-7.11.0-linux-amd64 from cloudfoundry/bosh-cli releases
ARG BOSH_CLI_SHA256=3ad84c3967672070b7f70f91f8da76abcbb604d588787b4f98457338cc9fc8d2
ARG GH_CLI_VERSION=2.102.0
# sha256 of gh_2.102.0_linux_amd64.tar.gz from cli/cli releases
ARG GH_CLI_SHA256=bb766f710eef8ede859c18578c72c327597cd4c8a85b06001b1f3843c6019386
ARG CF_CLI_VERSION=8.19.0
ARG CREDHUB_CLI_VERSION=2.9.62
ARG UV_VERSION=0.13.0

# -- Stage 1: download CLI binaries -----------------------------------------
FROM debian:bookworm-slim AS downloader

ARG BOSH_CLI_VERSION
ARG BOSH_CLI_SHA256
ARG GH_CLI_VERSION
ARG GH_CLI_SHA256
ARG CF_CLI_VERSION
ARG CREDHUB_CLI_VERSION
ARG UV_VERSION

RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /downloads

# bosh CLI (sha256-verified)
RUN curl -fsSL \
        "https://github.com/cloudfoundry/bosh-cli/releases/download/v${BOSH_CLI_VERSION}/bosh-cli-${BOSH_CLI_VERSION}-linux-amd64" \
        -o bosh \
    && echo "${BOSH_CLI_SHA256}  bosh" | sha256sum -c - \
    && chmod +x bosh

# gh CLI (sha256-verified)
RUN curl -fsSL \
        "https://github.com/cli/cli/releases/download/v${GH_CLI_VERSION}/gh_${GH_CLI_VERSION}_linux_amd64.tar.gz" \
        -o gh.tar.gz \
    && echo "${GH_CLI_SHA256}  gh.tar.gz" | sha256sum -c - \
    && tar -xzf gh.tar.gz --strip-components=2 "gh_${GH_CLI_VERSION}_linux_amd64/bin/gh" \
    && chmod +x gh

# cf CLI (v8 tarball ships a single binary named cf8 -> rename to cf)
RUN curl -fsSL \
        "https://packages.cloudfoundry.org/stable?release=linux64-binary&version=${CF_CLI_VERSION}&source=github" \
        -o cf-cli.tgz \
    && tar xzf cf-cli.tgz cf8 && mv cf8 cf && chmod +x cf

# credhub CLI
RUN curl -fsSL \
        "https://github.com/cloudfoundry-incubator/credhub-cli/releases/download/${CREDHUB_CLI_VERSION}/credhub-linux-amd64-${CREDHUB_CLI_VERSION}.tgz" \
        -o credhub.tgz \
    && tar xzf credhub.tgz && chmod +x credhub

# uv (runs the PEP 723 inline-dependency scripts under scripts/)
RUN curl -fsSL \
        "https://github.com/astral-sh/uv/releases/download/${UV_VERSION}/uv-x86_64-unknown-linux-musl.tar.gz" \
        -o uv.tgz \
    && tar xzf uv.tgz --strip-components=1 "uv-x86_64-unknown-linux-musl/uv" \
    && chmod +x uv

# -- Stage 2: Go toolchain (must match src/pve_cpi/go.mod) ------------------
FROM golang:${GO_VERSION}-bookworm AS gobase

# -- Stage 3: final image ---------------------------------------------------
# ruby:<ver>-slim is Debian bookworm with ruby + bundler in /usr/local; BATS
# (bosh-acceptance-tests) needs ruby 3.3+ with bundler, and its bundle
# install compiles native gems, hence build-essential. certify derives the
# light-stemcell qcow2 with qemu-img, hence qemu-utils.
FROM ruby:${RUBY_VERSION}-slim-bookworm

# Links the ghcr package to this repository so the workflows' GITHUB_TOKEN
# can push it and repo collaborators inherit access.
LABEL org.opencontainers.image.source="https://github.com/fivetwenty-io/bosh-proxmox-cpi-release"
LABEL org.opencontainers.image.description="CI image for bosh-proxmox-cpi-release certification and integration jobs"

RUN apt-get update && apt-get install -y --no-install-recommends \
        build-essential \
        ca-certificates \
        coreutils \
        curl \
        git \
        jq \
        libssl-dev \
        make \
        openssh-client \
        pkg-config \
        python3 \
        python3-yaml \
        qemu-utils \
    && rm -rf /var/lib/apt/lists/*

# Go toolchain from the official image
COPY --from=gobase /usr/local/go /usr/local/go
ENV GOPATH=/go
ENV PATH="/usr/local/go/bin:/go/bin:${PATH}"

# Downloaded CLIs
COPY --from=downloader /downloads/bosh    /usr/local/bin/bosh
COPY --from=downloader /downloads/gh      /usr/local/bin/gh
COPY --from=downloader /downloads/cf      /usr/local/bin/cf
COPY --from=downloader /downloads/credhub /usr/local/bin/credhub
COPY --from=downloader /downloads/uv      /usr/local/bin/uv

# uv manages its own Python runtimes in a cache directory; point it somewhere
# writable and stable (the system python3.11 already satisfies the scripts'
# requires-python >= 3.11, so this is only used if uv decides to download).
ENV UV_PYTHON_INSTALL_DIR=/opt/uv/python

# No USER directive: GitHub Actions job containers rely on the image default
# (root) so the runner-mounted workspace and /github/home stay writable.
# Concourse can still run this image under its own user configuration.
