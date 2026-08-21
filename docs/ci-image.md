# CI Image

Our acceptance and integration jobs run inside a purpose-built container image. The canonical Dockerfile lives at `ci/Dockerfile`; the `CI Image` workflow (`.github/workflows/ci-image.yml`) builds it on every change to that file and pushes it to `ghcr.io/fivetwenty-io/bosh-pve-cpi-ci` with the tags `latest` and the git sha.

Two consumers use the image:

- the scheduled acceptance workflow (`.github/workflows/acceptance.yml`), which runs `scripts/certify`, `scripts/bosh`, and `scripts/bats`

- the Concourse integration task (`ci/tasks/integration.yml`), which runs `./scripts/test integration all`

---

## Why a custom image is required

The stock `bosh/bosh-cli` image ships only the BOSH CLI and its runtime dependencies. Our scripts also need:

- **cf** and **credhub**
  Not present in `bosh/bosh-cli`; the CF tier and credential lookups shell out to them.

- **uv**
  Executes the PEP 723 inline-dependency scripts (`#!/usr/bin/env -S uv run --script`) under `scripts/`.

- **go** and **make**
  Compile `bin/cpi` before the lifecycle tier runs. The Go version must match `src/pve_cpi/go.mod`.

- **ruby** and **bundler**
  `scripts/bats` runs the BOSH Acceptance Tests, a Ruby rspec suite whose `bundle install` compiles native gems (hence `build-essential`).

- **gh**
  The acceptance workflow resolves and downloads the latest published release with it.

---

## Tool matrix

`ci/Dockerfile` pins every version; this table records the floor each consumer needs. The `bosh` and `gh` downloads are sha256-verified with the same published checksums `.github/workflows/release.yml` verifies.

| Tool | Minimum version | Source |
|------|----------------|--------|
| `bosh` | 7.10.9 | https://github.com/cloudfoundry/bosh-cli/releases (sha256-pinned) |
| `gh` | 2.97.0 | https://github.com/cli/cli/releases (sha256-pinned) |
| `cf` | 8.9.0 | https://github.com/cloudfoundry/cli/releases |
| `credhub` | 2.9.45 | https://github.com/cloudfoundry-incubator/credhub-cli/releases |
| `uv` | 0.4.30 | https://github.com/astral-sh/uv/releases |
| `go` | 1.26.6 | golang official image (must match `src/pve_cpi/go.mod`) |
| `ruby` + `bundler` | 3.3 | ruby official slim image |
| `python3` + PyYAML | 3.11 | Debian packages (`python3`, `python3-yaml`) |
| `git`, `jq`, `make`, `curl`, `openssh-client`, `coreutils`, `build-essential` | Debian bookworm | System packages |

The Python helper scripts use only the standard library plus PyYAML; `uv` can also download and pin its own interpreter (`UV_PYTHON_INSTALL_DIR=/opt/uv/python`).

---

## Building and publishing

The `CI Image` workflow builds and pushes automatically when `ci/Dockerfile` or the workflow itself changes on `main`, and can be dispatched by hand. Its verify step runs every baked tool's version command before the push, and the push step prints the image digest so `acceptance.yml` can pin its `container.image` reference to `ghcr.io/fivetwenty-io/bosh-pve-cpi-ci@sha256:...`.

To build locally:

```
docker build -f ci/Dockerfile -t bosh-pve-cpi-ci:dev .
docker run --rm bosh-pve-cpi-ci:dev sh -c 'bosh --version && gh --version && cf --version && credhub --version && uv --version && go version && ruby --version && bundle --version'
```

If a tool check fails, confirm the corresponding `COPY --from=downloader` line in `ci/Dockerfile` completed. Rebuild with `--no-cache` if a download was silently skipped because of a layer cache hit on a changed URL.

---

## Concourse pipeline integration

Point the `image_resource` stanza in `ci/tasks/integration.yml` at the published image:

```yaml
image_resource:
  type: registry-image
  source:
    repository: ghcr.io/fivetwenty-io/bosh-pve-cpi-ci
    tag: latest
```

The `registry-image` resource type is preferred over `docker-image` for new pipelines. It pulls OCI manifests directly and does not require a Docker daemon on the worker. If the package is made private, add registry credentials to the Concourse secrets store and reference them with `((double-paren))` interpolation.

The image's default user is root because GitHub Actions job containers rely on it for workspace ownership; Concourse tasks can run it under their own user configuration.
