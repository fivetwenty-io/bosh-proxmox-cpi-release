# BOSH PVE CPI

Go implementation of the BOSH Cloud Provider Interface (CPI) v2 for Proxmox VE 9.x.

The CPI provisions VMs, persistent disks, networks, snapshots, and stemcells on a PVE cluster on behalf of a BOSH Director. It speaks the BOSH JSON-RPC envelope over stdin/stdout, supports three agent bootstrap modes (`cloudinit`, `registry`, `noagent`), and ships as a single static Go binary packaged into a BOSH release.

- BOSH CPI v2 compliant (`api_version: 2`)
- PVE SDN vnet lifecycle plus Linux bridge fallback for managed networks
- ConfigDrive ISO agent bootstrap with no external registry dependency
- API token or password authentication; TLS verification on by default

## Quickstart

The path below takes a fresh checkout through to a deployed Director on a PVE cluster.

### 1. Clone the repository

```bash
git clone https://github.com/fivetwenty-io/bosh-pve-cpi-release.git
cd bosh-pve-cpi-release
```

### 2. Build the BOSH release tarball

```bash
make download-blobs   # one-time: fetch the Go toolchain blob
make dev-release      # builds dev_releases/bosh-pve-cpi/bosh-pve-cpi-<version>.tgz
RELEASE_TGZ=$(scripts/create-release dev | grep '^RELEASE_TGZ=' | cut -d= -f2)
```

For a versioned final release use `make release VERSION=1.0.0`. Both targets write the tarball under `dev_releases/` or `releases/` — never at the repo root. `make release-hygiene` enforces that invariant.

### 3. Configure your deployment variables

Copy the example and fill in PVE host, credentials, storage pool names, and network bridge:

```bash
cp manifests/bosh/vars.yml.example manifests/bosh/vars.yml
$EDITOR manifests/bosh/vars.yml
```

`vars.yml` and `creds.yml` are gitignored. Treat them as secret material — `scripts/create-release` refuses to commit changes to them unless `ALLOW_SECRET_COMMIT=1` is set.

### 4. Deploy the Director

The deployment composes the upstream [bosh-deployment](https://github.com/cloudfoundry/bosh-deployment) `bosh.yml` base manifest with this repository's `manifests/bosh/cpi.yml` ops file:

```bash
export BOSH_DEPLOYMENT_DIR=~/w/cloudfoundry/bosh-deployment   # path to a bosh-deployment checkout

bosh create-env $BOSH_DEPLOYMENT_DIR/bosh.yml \
  --state manifests/bosh/state.json \
  --vars-store manifests/bosh/creds.yml \
  --vars-file manifests/bosh/vars.yml \
  --var release_artifact_path="$RELEASE_TGZ" \
  -o manifests/bosh/cpi.yml
```

`scripts/bosh create-env` wraps this invocation with the conventional flag set.

### 5. Validate the deployment

```bash
bosh -e $(bosh int manifests/bosh/vars.yml --path /internal_ip) \
     --ca-cert <(bosh int manifests/bosh/creds.yml --path /director_ssl/ca) \
     login

bosh -e <alias> env
bosh -e <alias> task --recent=1 --debug
```

For end-to-end CPI method exercise against a live cluster, see [BOSH CPI Lifecycle Tests](#bosh-cpi-lifecycle-tests) below.

## Properties

The CPI exposes 37 properties under the `pve.*`, `agent.*`, and `registry.*` namespaces. The most operator-relevant subset is summarized below; the [full property reference](docs/configuration.md) documents every field with defaults, types, and validation rules.

| Property | Description | Default |
|---|---|---|
| `pve.host` | PVE host (IP or FQDN) | **required** |
| `pve.user` | PVE username (e.g. `bosh@pve`) | **required** |
| `pve.api_token` | PVE API token — preferred over password in production | `""` |
| `pve.password` | PVE password — mutually exclusive with `api_token` | `""` |
| `pve.node` | Default PVE node for placement | **required** |
| `pve.vm_storage` | Storage pool for VM root disks | **required** |
| `pve.disk_storage` | Storage pool for persistent disks | **required** |
| `pve.stemcell_storage` | Shared file-based storage for qcow2 stemcells | falls back to `vm_storage` |
| `pve.iso_storage` | Storage pool for per-VM ConfigDrive ISOs | `local` |
| `pve.network_bridge` | Default Linux bridge for VM NICs | `vmbr0` |
| `pve.network_mode` | Managed-network mode (`sdn`, `bridge`, `auto`) | `auto` |
| `pve.agent_mode` | Agent bootstrap mode (`cloudinit`, `registry`, `noagent`) | `cloudinit` |

The full table covers the remaining 25 properties including SDN zone management, snapshot guards, hotplug flags, reboot strategy, VMID allocation range, registry TLS pinning, and the `agent.*` mbus fallback. See the [configuration reference](docs/configuration.md).

## Operations

Day-2 operations, log locations, error triage, ConfigDrive ISO storage hardening, persistent-disk guidance, and orphan-resource cleanup live in the [operations runbook](docs/operations.md). Symptom-first failure triage lives in the [troubleshooting guide](docs/troubleshooting.md).

Specific topics:

- [ConfigDrive ISO storage](docs/operations.md#configdrive-iso-storage) — the default `local` value places ISOs on node-local storage where any PVE node user can read agent credentials. Dedicate a separate pool for production.

- [Error message hygiene](docs/operations.md#error-message-hygiene) — what surfaces in BOSH task output versus what stays in CPI logs, and the secrets-redaction contract.

- [Persistent disks](docs/operations.md#persistent-disks-cloud_properties) — `disk_format` cloud-property requirements for LVM/ZFS pools (`raw`) versus directory/network-backed pools (`qcow2`).

- [Release workflow](docs/operations.md) — operator workflow for capturing `RELEASE_TGZ` from `scripts/create-release` and passing it via `--var release_artifact_path=...`.

## Network configuration

The CPI implements `create_network` and `delete_network` for BOSH-managed networks. Two paths are available: PVE SDN vnets (cluster API) and Linux bridges (nodes API). Path selection is controlled by `pve.network_mode` and the `cloud_properties` keys in the network spec.

See [Network configuration](docs/networks.md) for the full `cloud_properties` schema, zone/vnet/subnet semantics, vnet naming rules, the zone auto-manage safety rule, and worked SDN and bridge manifest examples.

## Development

### Requirements

- Go 1.26 or higher (BOSH packaging compiles against the `golang-1.26` blob)
- `golangci-lint` (`make lint` falls back to `go run` at a pinned version when not installed)
- `staticcheck` (optional, `make staticcheck` skips with a notice when missing)
- `govulncheck` and `gosec` (optional, `make security`)

### Make targets

| Target | Description |
|---|---|
| `make build` | Compile `bin/cpi` with version ldflags (alias `make bin/cpi`) |
| `make test` | Run all Go tests with race detection |
| `make coverage` | Generate coverage profile and print a function-level summary |
| `make coverage-html` | Write HTML coverage report to `src/pve_cpi/coverage.html` |
| `make coverage-check` | Fail if total line coverage falls below `COVERAGE_THRESHOLD` |
| `make check` | Run `vet`, `staticcheck`, `lint`, `coverage-check`, and `test` |
| `make fmt` | Format all Go sources with `gofmt` |
| `make lint` | Run `golangci-lint` (pinned version via `go run` fallback) |
| `make security` | Run `govulncheck` and `gosec` |
| `make release-build` | Build the Go CPI binary only (`bin/cpi`); does not produce a BOSH tarball |
| `make dev-release` | Build a dev BOSH release tarball under `dev_releases/bosh-pve-cpi/` |
| `make release VERSION=X.Y.Z` | Build a versioned BOSH release tarball; prints `RELEASE_TGZ=<path>` |
| `make release-hygiene` | Assert no loose `bosh-pve-cpi-*.tgz` exists at the repo root |
| `make release-clean` | Remove `bin/`, coverage artifacts, and release tarballs |
| `make download-blobs` | Download and register the Go toolchain blob (`golang-1.26`) |

`make release-build` and `make release` are intentionally distinct. `release-build` is the Go-binary-only path for local iteration. `make release` (and `make dev-release`) call `scripts/create-release` to assemble the BOSH release tarball under `dev_releases/` or `releases/`. They never produce a loose tarball at the repo root.

### CI gating

CI runs `make check` on every push. The composite target gates `vet`, `staticcheck`, `lint`, `coverage-check`, and `test`, in that order, fast-fail. `COVERAGE_THRESHOLD` is currently `75` and rises to `85` once the test additions land.

Go sources live under `src/pve_cpi/`. Direct `go test` and `go build` invocations must run from there; the `make` targets re-root for you.

See [Development guide](docs/development.md) for the source layout, testing strategy, mock conventions, and stem-cell test fixtures.

## BOSH CPI Lifecycle Tests

A local harness exercises the 14 canonical CPI methods end-to-end against a live PVE cluster:

```bash
export CPI_CONFIG=~/.bosh-pve-cpi/cpi.json
export STEMCELL_PATH=/path/to/bosh-stemcell-*.tgz
./scripts/lifecycle
```

See [CPI certification](docs/bosh-cpi-certification.md) for the full prerequisites list, config schema, supported environment overrides, and the upstream Concourse certification path.

## Troubleshooting

For symptom-first triage of deployment, VM creation, disk attachment, network, and stemcell failures, see the [troubleshooting guide](docs/troubleshooting.md). Common starting points:

- VM creation hangs at agent settle — check [CPI logs](docs/operations.md#cpi-logs) for the import task UPID and the post-import NIC/disk attach sequence.
- Disk attach rejected with snapshot guard — review [snapshot guard on disk operations](docs/cpi_methods.md#snapshot-guard-on-disk-operations).
- Registry endpoint rejected at startup — `registry.endpoint` must be `https://` unless `registry.allow_insecure: true` is set explicitly.

## Reference

- [CPI methods reference](docs/cpi_methods.md) — every BOSH CPI v2 method with args, returns, errors, and notes.
- [Configuration reference](docs/configuration.md) — all 37 properties with defaults and validation rules.
- [Network configuration](docs/networks.md) — SDN and bridge `cloud_properties` schema and manifest examples.
- [Operations runbook](docs/operations.md) — day-2 operations and diagnostics.
- [Troubleshooting](docs/troubleshooting.md) — symptom-first failure triage.
- [ConfigDrive layout](docs/configdrive.md) — ISO 9660 volume layout and SCSI slot reservation map.
- [PVE API permissions](docs/pve-api-permissions.md) — minimum-privilege `bosh@pve` user setup and token creation.
- [PVE settings](docs/pve-settings.md) — cluster-level settings the CPI assumes (storage content types, SDN enablement).

## External links

- [BOSH CPI v2 specification](https://bosh.io/docs/cpi-api-v2/)
- [Proxmox VE documentation](https://pve.proxmox.com/pve-docs/)
- [bosh.io stemcells](https://bosh.io/stemcells/)

## License

MIT
