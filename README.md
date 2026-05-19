# BOSH PVE CPI

Go implementation of the BOSH CPI v2 specification for PVE 9.x. The CPI communicates with the BOSH Director over stdin/stdout using the BOSH JSON-RPC envelope protocol, provisions VMs and persistent disks on a PVE cluster, and supports three agent bootstrap modes. It consumes the `github.com/fivetwenty-io/pve-apiclient-go/v3` SDK and compiles to a single static binary.

## Quick Start

**Build:**

```bash
make bin/cpi
```

**Run:**

```bash
./bin/cpi --config cpi.json
```

The CPI reads one JSON-RPC request from stdin, writes one JSON-RPC response to stdout, and exits. The BOSH Director invokes it once per CPI call.

## BOSH Release Usage

Add the release to your BOSH deployment manifest:

```yaml
releases:
- name: bosh-pve-cpi
  version: latest
```

Configure the CPI job under `instance_groups`:

```yaml
instance_groups:
- name: bosh
  jobs:
  - name: pve_cpi
    release: bosh-pve-cpi
    properties:
      pve:
        host: pve.example.com
        user: root@pam
        password: ((pve_password))
        node: pve1
        vm_storage: local-lvm
        disk_storage: local-lvm
        network_bridge: vmbr0
        agent_mode: cloudinit
```

## Configuration

All properties live under the `pve.*` namespace in the BOSH manifest and map directly to JSON fields in `cpi.json` at runtime.

| BOSH manifest property | JSON config field | Description | Default |
|---|---|---|---|
| `pve.host` | `host` | PVE host (IP or FQDN) | **required** |
| `pve.port` | `port` | PVE API port | `8006` |
| `pve.user` | `user` | PVE username (e.g. `root@pam`) | **required** |
| `pve.password` | `password` | PVE password — mutually exclusive with `api_token` | `""` |
| `pve.api_token` | `api_token` | PVE API token — mutually exclusive with `password` | `""` |
| `pve.realm` | `realm` | Authentication realm | `pam` |
| `pve.node` | `node` | Default PVE node for placement | `""` |
| `pve.vm_storage` | `vm_storage` | Storage pool for VM root disks | **required** |
| `pve.disk_storage` | `disk_storage` | Storage pool for persistent disks | **required** |
| `pve.stemcell_storage` | `stemcell_storage` | Shared storage pool for stemcell qcow2 images. Must be accessible from all cluster nodes. Defaults to `vm_storage`; in that case `vm_storage` must also be shared. | `""` |
| `pve.network_bridge` | `network_bridge` | Default Linux bridge for VM NICs | `vmbr0` |
| `pve.verify_ssl` | `verify_ssl` | Verify PVE TLS certificate | `true` |
| `pve.agent_mode` | `agent_mode` | Agent bootstrap mode: `cloudinit`, `registry`, or `noagent` | `cloudinit` |
| `pve.vm_disk_format` | `vm_disk_format` | Disk image format: `qcow2`, `raw`, or `vmdk` | `qcow2` |
| `pve.log_level` | `log_level` | Structured log level: `debug`, `info`, `warn`, or `error` | `info` |
| `pve.vmid_range_start` | `vmid_range_start` | Lowest VMID the CPI may allocate (PVE reserves 0–99) | `100` |
| `registry.endpoint` | `registry_endpoint` | BOSH registry URL — required when `agent_mode=registry` | `""` |
| `registry.user` | `registry_user` | Registry HTTP basic-auth user — required when `agent_mode=registry` | `""` |
| `registry.password` | `registry_password` | Registry HTTP basic-auth password — required when `agent_mode=registry` | `""` |

Exactly one of `password` or `api_token` must be set. The config loader rejects configs that set both or neither.

## Agent Modes

The `agent_mode` property controls how the BOSH agent on a new VM receives its initial settings.

`cloudinit` (default)
The CPI attaches an OpenStack ConfigDrive ISO on `scsi30` carrying the BOSH agent settings. No registry required. Compatible with stock bosh.io OpenStack stemcells that have `api_version: 2` in `stemcell.MF`. See [ConfigDrive](docs/configdrive.md) for the volume layout and SCSI slot reservation map.

`registry`
The CPI writes agent settings to a BOSH registry HTTP endpoint and passes the registry URL to the agent via cloud-init metadata. Requires `registry.endpoint`, `registry.user`, and `registry.password`. Use this mode with older stemcells that do not support `api_version: 2`.

`noagent`
The CPI provisions the VM but does not configure any agent bootstrap. Use for bosh-lite or other scenarios where no BOSH agent runs on the VM.

## Development

**Requirements:**

- Go 1.26 or higher (BOSH packaging compiles against the `golang-1.26` blob)
- `golangci-lint` (optional, for `make lint`)
- `staticcheck` (optional, for `make staticcheck`)
- `govulncheck` and `gosec` (optional, for `make security`)

**Make targets:**

| Target | Description |
|---|---|
| `make build` | Build `bin/cpi` with version ldflags |
| `make test` | Run all tests with race detection |
| `make coverage` | Generate coverage profile and print summary |
| `make coverage-html` | Open HTML coverage report |
| `make coverage-check` | Fail if line coverage is below 80% |
| `make check` | Run `vet`, `staticcheck`, and `test` |
| `make fmt` | Format all Go source with `gofmt` |
| `make lint` | Run `golangci-lint` |
| `make security` | Run `govulncheck` and `gosec` |
| `make download-blobs` | Download + register the Go toolchain blob (`packages/golang-1.26/`) |
| `make dev-release` | Build a development BOSH release tarball |
| `make release` | Full binary release build: `check` + `security` + `bin/cpi` |
| `make clean` | Remove `bin/` and coverage files |

Go sources live under `src/pve_cpi/`. Direct `go test` / `go build` invocations must run from there; the `make` targets re-root for you.

The project enforces an 80% line-coverage minimum via `make coverage-check`.

## BOSH CPI Lifecycle Tests

A local lifecycle harness exercises the 14 canonical CPI methods end-to-end against a live PVE cluster:

```bash
export CPI_CONFIG=~/.bosh-pve-cpi/cpi.json
export STEMCELL_PATH=/path/to/bosh-stemcell-*.tgz
./scripts/lifecycle
```

See [`docs/bosh-cpi-certification.md`](docs/bosh-cpi-certification.md) for the full prerequisites list, config schema, supported environment overrides, and the upstream Concourse certification path.

## License

MIT
