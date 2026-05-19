# Architecture

The BOSH PVE CPI is a Go binary that implements the BOSH CPI v2 specification for PVE 9.x. The director invokes the binary per request, exchanging JSON-RPC envelopes over stdin and stdout. Internally the code is split into layered packages; each layer has a single responsibility and a narrow interface so unit tests can replace dependencies with mocks.

## Source Layout

Go sources live under `src/pve_cpi/` so the directory shape matches the BOSH packaging spec (`packages/pve_cpi/spec` globs `pve_cpi/...`). Concretely:

- `src/pve_cpi/cmd/cpi` — binary entry point
- `src/pve_cpi/internal/...` — internal packages described below
- `src/pve_cpi/go.mod`, `src/pve_cpi/go.sum`, `src/pve_cpi/vendor/`

The Go module path is `github.com/fivetwenty-io/bosh-pve-cpi` and the internal package import paths (e.g. `internal/cpi/handlers`) are unchanged. References to `cmd/cpi` and `internal/...` below name Go packages, not repo-root directories.

## Layers

- `cmd/cpi`

  Binary entry point. Parses CLI flags, loads config, builds dependencies, dispatches one request per line from stdin until EOF or signal.

- `internal/jsonrpc`

  Encodes and decodes BOSH CPI request and response envelopes. Owns the wire format.

- `internal/cpi`

  Dispatcher and handler registry. Routes a method name to its `Handler`, translates handler errors to typed CPI error responses.

- `internal/cpi/handlers`

  One file per CPI method. Each handler decodes arguments, calls the PVE wrapper and agent strategy, then returns either a result or a typed CPI error.

- `internal/agent`

  Agent bootstrap strategies. `ConfigDrive`, `RegistryAgent`, and `NoAgent` implement the `Agent` interface (`Configure` / `Remove` / `UpdateDiskHints`). `ConfigDrive` is selected by `pve.agent_mode: cloudinit` (the default); see [ConfigDrive](configdrive.md).

- `internal/pve`

  Thin wrapper over `github.com/fivetwenty-io/pve-apiclient-go/v3`. Builds the SDK client from `internal/config`, polls task UPIDs, allocates VMIDs race-safely, parses disk CIDs, and normalises SDK errors to BOSH CPI error types.

- `internal/registry`

  Minimal HTTP client for the optional BOSH registry. Used only when `agent_mode = registry`.

- `internal/config`, `internal/errors`, `internal/log`, `internal/version`

  Shared primitives. Config parses and validates the CPI JSON document. Errors defines the BOSH CPI error taxonomy with `OkToRetry` semantics. Log wraps `log/slog` with context-carrying helpers and a test observer. Version exposes build-time identifiers populated via `-ldflags`.

## Request Flow

```mermaid
graph TD
    Director -->|stdin JSON-RPC| Main[cmd/cpi/main]
    Main -->|Decode| RPC[internal/jsonrpc]
    Main -->|Handle| Dispatcher[internal/cpi.Dispatcher]
    Dispatcher -->|Lookup| Handler[internal/cpi/handlers.Handle*]
    Handler -->|VM, disk, task| PVE[internal/pve]
    Handler -->|Bootstrap| Agent[internal/agent]
    PVE -->|HTTPS| API[PVE API]
    Agent -.->|cloudinit| PVE
    Agent -.->|registry| Registry[internal/registry]
    Handler -->|Result or Error| Dispatcher
    Dispatcher -->|Encode| RPC
    RPC -->|stdout JSON-RPC| Director
```

## Dependency Direction

```text
cmd/cpi
   ↓
internal/cpi → internal/cpi/handlers
                       ↓
                 internal/agent, internal/pve
                       ↓
internal/registry, internal/config, internal/errors, internal/log, internal/version, internal/jsonrpc
                       ↓
                   stdlib + SDK
```

No cycles. `internal/pve` does not import `internal/agent`; `internal/agent` does not import `internal/cpi`.

## Error Mapping

Every SDK error flows through `internal/pve.WrapError`, which classifies HTTP 4xx as `CloudError`, HTTP 404 as a flag the caller upgrades to `VMNotFound` or `DiskNotFound`, HTTP 5xx and network timeouts as `RetriableCloudError`. The dispatcher serialises the resulting error's `Type()`, `Error()`, and `OkToRetry()` into the JSON-RPC error envelope.

## Task Awaiting

Every PVE API call that returns a UPID is awaited via `internal/pve.AwaitTask` before the handler returns. The wrapper polls with a default 2-second interval and a configurable deadline. Standard calls use a 300-second deadline; stemcell upload and VM disk import use a 600-second deadline (`pve.StemcellMaxWait`) to accommodate large qcow2 files and PVE format conversion (e.g., qcow2 → raw on LVM storage).

## Stemcell Model

Stemcells are stored as raw qcow2 files on a shared PVE storage pool. No template VMs are created.

### create_stemcell

The handler uploads the disk image (or extracts it from a gzip+tar tarball first) to the configured `stemcell_storage` pool under content type `import`. The storage volume is addressed as `<storage>:import/<filename>`.

Filename format:

```
bosh-stemcell-<sanitized-name>-<sanitized-version>-<sha8>.qcow2
```

where `sha8` is the first 8 hex characters of the SHA-256 hash of the disk image. This makes the filename a content-addressed key, enabling deduplication: if a volume with the same filename already exists, `create_stemcell` returns the existing CID without re-uploading.

A companion sidecar file (`*.json`) is uploaded alongside the qcow2 for operator audit. The CPI never reads the sidecar; if the sidecar upload fails, `create_stemcell` returns an error and attempts to delete the already-uploaded qcow2.

The returned **stemcell CID** is the full PVE volume identifier:

```
<storage>:import/<filename>
```

For example: `nfs-stemcells:import/bosh-stemcell-ubuntu-jammy-1.438-a1b2c3d4.qcow2`

### create_vm

The handler passes the stemcell CID directly to the PVE `POST /nodes/<node>/qemu` call as the `import-from=` parameter of `scsi0`:

```
scsi0: <vm_storage>:0,import-from=<stemcell_cid>,format=<vm_disk_format>,size=<N>G
```

PVE copies the qcow2 data into the VM's root disk at create time in a single API operation. No clone step occurs. Once the import task completes, the VM has no live dependency on the stemcell volume — stemcells can be deleted at any time without affecting running VMs.

VMID allocation uses the range `[vmid_range_start, 5999]`. Stemcells no longer occupy a reserved VMID range.

### delete_stemcell

The handler parses the CID to extract the storage name and volume path, then calls `DeleteVolumeIfExists` for both the qcow2 and the sidecar. Missing volumes are logged at WARN and treated as success (idempotent).

### Shared-storage requirement

`stemcell_storage` must be a shared PVE storage pool accessible from every cluster node (NFS, CIFS, CephFS, GlusterFS, or any other pool with `shared=1` in the PVE storage config). The CPI enforces this at `create_stemcell` time: local storage is rejected with a descriptive error when the cluster has more than one node. Single-node clusters may use local storage.
