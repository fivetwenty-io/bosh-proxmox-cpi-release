# Architecture

The BOSH PVE CPI is a Go binary that implements the BOSH CPI v2 specification for PVE 9.x. The director invokes the binary once per request, exchanging JSON-RPC envelopes over stdin and stdout. Internally, the code splits into layered packages. Each layer has a single responsibility and a narrow interface, so unit tests replace dependencies with mocks.

This document describes the structure: where code lives, how the packages depend on one another, what happens during a single request, the cross-cutting mechanisms that span packages, and how errors flow back to the director. For method signatures and per-method behavior, see [CPI Methods](cpi_methods.md).

## Source Layout

Go sources live under `src/pve_cpi/` so the directory shape matches the BOSH packaging spec (`packages/pve_cpi/spec` globs `pve_cpi/...`):

- `src/pve_cpi/cmd/cpi` — binary entry point.

- `src/pve_cpi/internal/...` — internal packages described below.

- `src/pve_cpi/go.mod`, `src/pve_cpi/go.sum`, `src/pve_cpi/vendor/` — module metadata and vendored dependencies.

The Go module path is `github.com/fivetwenty-io/bosh-pve-cpi`, and the internal import paths (for example `internal/cpi/handlers`) are unchanged. References to `cmd/cpi` and `internal/...` below name Go packages, not repo-root directories.

## Package Layers

Packages are grouped by role. Each is described in one sentence.

### Entry and transport

- `cmd/cpi` — binary entry point: parses CLI flags, loads config, builds dependencies, resolves configured hooks, and runs the JSON-RPC loop reading one request per line from stdin until EOF or signal.

- `internal/jsonrpc` — encodes and decodes BOSH CPI request and response envelopes, and carries the per-request `Context` type.

### Dispatch and handlers

- `internal/cpi` — the dispatcher routes a method name to its `Handler`, wraps every call in panic recovery and a timeout, gates request tracing, and installs the rollback stack; it also defines the `Hook` interface used by middleware.

- `internal/cpi/handlers` — one file per CPI method, plus cross-cutting helpers (the layered cloud-properties resolver, per-disk performance encoding, the in-flight semaphore, placement glue, storage-tier matching, stemcell provenance, and SDN wiring).

- `internal/cpi/hooks` — the built-in hooks (`audit_log`, `notes_audit`, `lb_register`, `external_command`) and the `Registry` map that config validation checks names against.

### PVE and stemcell transport

- `internal/pve` — wraps `github.com/fivetwenty-io/pve-apiclient-go/v3`: VM, disk, network, and storage CRUD, task UPID polling, retry and backoff, pushback detection, the cluster pool lock, SDN apply, the foreign-disk guard, and SDK-error normalization.

- `internal/pve/stemcell_fetch` — fetches a stemcell tarball (S3 or local), extracts and uploads it, and replicates the resulting template across nodes in parallel.

### Bootstrap and side channels

- `internal/agent` — the `Agent` abstraction (`Configure`, `Remove`, `UpdateDiskHints`) with cloudinit, noagent, and auto factories.

- `internal/configdrive` — builds the ISO 9660 config-drive image the cloudinit agent uses to deliver settings; see [ConfigDrive](configdrive.md).

- `internal/lb` — a HAProxy Data Plane API client used by the `lb_register` hook to add and remove backend servers.

- `internal/exec` — a sandboxed subprocess runner (path allowlist, symlink resolution, process-group kill, environment scrub) used by the `external_command` hook.

- `internal/placement` — collects per-node facts (cluster status, resources, HA tags) and scores nodes for VM placement with AZ-group anti-affinity.

### Shared primitives

- `internal/config` — parses and validates the CPI JSON document (vm_types, disk_types, storage_tiers, hooks, and the rest); it imports `internal/cpi/hooks` only to validate hook names against the registry.

- `internal/errors` — the BOSH CPI error taxonomy (`Cloud`, `RetriableCloud`, `NotSupported`, `NotImplemented`, and the not-found variants) with `OkToRetry` semantics.

- `internal/log` — an slog-backed logger with context helpers (request ID, method, and OTel trace/span correlation), a test observer, and the `RedactSecrets` scrubber.

- `internal/otel` — builds three independently opt-in OpenTelemetry pipelines (traces, logs, metrics), each with its own setup and bounded-timeout shutdown; every pipeline speaks OTLP over HTTP or gRPC per one shared protocol option, metrics use delta rather than cumulative temporality (the CPI is a one-shot process with no cross-invocation counter state), the logs pipeline is beta (its upstream SDK is pre-1.0), and a shared fail-open error handler routes SDK-internal errors to stderr logging. Disabled configuration yields a no-op tracer, a no-op meter, and an unmodified logger, with no network activity.

- `internal/version` — build-time version identifiers populated via `-ldflags`.

## Dependency Tiers

The internal packages form an acyclic graph. Leaf packages depend only on the standard library; each higher tier depends on lower ones, and `cmd/cpi` wires everything together at the top.

One chain looks like a cycle but is not. `config` imports `cpi/hooks` (for the registry name check), `cpi/hooks` imports `cpi` (for the `Hook` interface), and `cpi` does **not** import `config`. The hook config structs live in the `hooks` package precisely to keep this a one-way chain. The build confirms it: there is no import cycle.

```mermaid
graph TD
    subgraph T0[Tier 0 leaf]
        errors
        jsonrpc
        log
        version
        configdrive
    end
    subgraph T1[Tier 1]
        exec
        lb
        placement
    end
    subgraph T2[Tier 2]
        pve
        config
    end
    subgraph T3[Tier 3]
        hooks[cpi/hooks]
        sfetch[pve/stemcell_fetch]
    end
    subgraph T4[Tier 4]
        cpi
        agent
    end
    subgraph T5[Tier 5]
        handlers[cpi/handlers]
    end
    cmd[cmd/cpi]

    exec --> log
    lb --> errors
    placement --> log
    pve --> config
    pve --> log
    config --> hooks
    hooks --> cpi
    hooks --> exec
    hooks --> lb
    sfetch --> pve
    agent --> configdrive
    agent --> pve
    handlers --> agent
    handlers --> placement
    handlers --> sfetch
    handlers --> cpi
    cmd --> handlers
    cmd --> agent
    cmd --> pve
```

The single most important non-obvious edge: `internal/pve` does not import `internal/agent`, and `internal/agent` does not import `internal/cpi`. The PVE client knows nothing about agents, and agents know nothing about dispatch.

## Request Flow

A request enters as one JSON-RPC line on stdin and leaves as one line on stdout. The dispatcher is the fixed point everything routes through.

```mermaid
graph TD
    Director -->|stdin JSON-RPC| Main[cmd/cpi/main]
    Main -->|Decode| RPC[internal/jsonrpc]
    Main -->|Handle| Dispatcher[internal/cpi.Dispatcher]
    Dispatcher -->|Hook chain| Hooks[internal/cpi/hooks]
    Hooks -->|Lookup| Handler[handlers.Handle*]
    Handler -->|Score nodes| Placement[internal/placement]
    Handler -->|VM, disk, task| PVE[internal/pve]
    Handler -->|Bootstrap| Agent[internal/agent]
    PVE -->|HTTPS, retry + pushback| API[PVE API]
    Agent -.->|cloudinit| PVE
    Handler -->|error| Rollback[Rollback stack LIFO]
    Handler -->|Result or Error| Dispatcher
    Dispatcher -->|Encode| RPC
    RPC -->|stdout JSON-RPC| Director
```

The dispatcher decodes the envelope, looks up the handler, and runs it inside a `recover()` block under a timeout. Configured hooks wrap the handler as a chain, running before and after it without per-handler code. The handler decodes its arguments, optionally scores nodes through `internal/placement`, then drives the PVE client and the agent. Each resource the handler acquires registers a cleanup function on the rollback stack. If the handler returns a non-nil error, the dispatcher fires those cleanups in LIFO order; on success it drops them. The result or typed error is encoded back to the director.

## Cross-Cutting Subsystems

These mechanisms span multiple packages. Operational depth lives in the linked sibling docs; what follows is architecture-level only.

### Dispatcher: panic recovery, timeout, and tracing

The dispatcher wraps every handler call in `recover()`. A recovered panic becomes a non-retriable `Cloud` error and a logged stack trace, so the director receives a structured error response instead of a malformed one and the process stays alive for subsequent requests. A configurable per-request timeout bounds each call. Request tracing is gated behind a config flag; when enabled, the dispatcher passes arguments and results through `RedactSecrets` before logging them. Separately, opt-in OpenTelemetry tracing (`pve.otel.*`, see [Configuration](configuration.md)) opens one root span per CPI action and a child span per PVE API call, with span-recorded error text passing through the same URL-credential scrubbing as the logs; spans never touch stdout, and export failure never fails an action. A dedicated `cpi.response_write_failure` span covers the one case the root span cannot record: a panic while writing the JSON-RPC response, after the root span has already ended. Metrics are equally opt-in and equally narrow: the sole instrument, `cpi.action.duration`, is a millisecond histogram tagged with the CPI method and the action's final outcome — `success`, `error` (covering handler errors, timeouts, and recovered panics), or `marshal_error` — recorded once per dispatched action by the dispatcher itself, after every post-handler reclassification has settled — there are no per-PVE-call metrics, since spans already cover that at finer granularity. Logs gain an additive OTLP handler when enabled: the existing stderr stream is unchanged, and log records fan out to both. A CPI failure must never wedge the director or leak a secret into a log — or into a span.

### Rollback stack

`WrapHandler` installs a `rollbackHolder` — a LIFO stack of cleanup functions — into the request context at registration time. After acquiring a resource (a VM, a disk, or an LB backend), a handler or hook calls `RegisterRollback`. `WrapHandler` runs the stack only when the inner handler returns a non-nil error, and a `sync.Once` makes the firing idempotent. Every method gets partial-failure cleanup without per-handler boilerplate; a half-created VM does not leak.

### Hooks middleware

`hooks.Registry` is a `map[string]func(Deps) cpi.Hook`. Config validation rejects any configured hook name absent from the map. At startup, `cmd/cpi` resolves the configured names, builds `Deps` (logger, LB client, and config), and passes the constructed hooks to the dispatcher, which chains them around each handler. Four hooks ship: `audit_log` logs the call and duration, `notes_audit` writes VM notes on create, `lb_register` adds and removes HAProxy backends (with an SSRF guard and a rollback registration so deregistration fires on create failure), and `external_command` runs a sandboxed command through `internal/exec`. With no hooks configured, the overhead is zero. Hook configuration properties are documented in [Configuration](configuration.md).

### Placement and AZ anti-affinity

`GatherNodeFacts` queries `/cluster/status`, `/cluster/resources`, and per-node storage to build each node's memory, CPU, storage, guest count, and HA tags. `ScoreNodes` computes a weighted sum (memory 1.0, storage 0.5, CPU 0.5, guest count 0.3, anti-affinity penalty 5.0×). The `create_vm` handler calls `SelectNode`, feeds the winner to every subsequent clone and attach call, and optionally writes a PVE HA node-affinity rule (`bosh-na-{vmid}`). A sentinel AZ value of `dlb` hands placement to the PVE 9.2 Dynamic Load Balancer instead; see [DLB-Aware Placement](dlb-aware-placement.md). VMs land on nodes that can host them, and anti-affinity groups spread across failure domains.

### Cluster pool lock

PVE resource pools double as a cluster-wide mutex: `POST /pools` is pmxcfs-serialized, so a duplicate poolid returns a 4xx, signaling that the lock is held. `AcquireClusterLock` creates a `bosh-lock-{key}` pool, embedding an owner UUID and an expiry in the pool comment; waiters poll and steal a lock past its expiry. The CPI uses this to serialize the read-modify-write of HA anti-affinity rules across concurrent `create_vm` processes, which would otherwise clobber one another.

### Resource pool lifecycle

Two independent PVE resource pools carry every CPI-managed object: `pve.vm_pool` (default `bosh`) for workload VMs and `pve.stemcell_template_pool` (default `bosh-templates`) for stemcell templates. Config validation keeps the two distinct — pools are the CPI's ACL anchor, and letting a VM pool and a template pool collide would let a `create_vm`-scoped grant also reach shared templates.

`create_vm` resolves the VM's pool name from a small pipeline, highest precedence first: call-level `cloud_properties.pool`, then a `vm_type` profile's `cloud_properties.pool`, then a rendered `pve.vm_pool_template` (four variables: `{prefix}`, `{director}`, `{deployment}`, `{instance_group}`), then the `pve.vm_pool` global default. The pipeline stops at the first non-empty candidate; when every layer is empty, no pool is assigned at all. Every resolved name is validated as a flat PVE poolid — no `/`, no reserved `bosh-lock-` prefix — before use, since the CPI never creates a nested pool.

Both pools are create-if-missing: `EnsurePoolExists` (`internal/pve/pool.go`) creates the resolved pool the first time `create_vm` or `create_stemcell` needs it, tagging it with a `managed by bosh-pve-cpi` provenance comment. A concurrent create from a second CPI process is tolerated the same way the cluster lock above tolerates it — pmxcfs serializes `POST /pools`, and the CPI treats the resulting duplicate-poolid error as success. Ensure-before-assign ordering matters: the pool is created (or confirmed to already exist) before the VM create/clone call that names it, on both the import path and the clone path.

Destroying a VM needs no membership cleanup: PVE removes a destroyed VM's pool membership automatically (confirmed against PVE 9.2.4), so `delete_vm` never has to touch the pool API on the common path. An opt-in reaper, `pve.pool_reap_empty` (default `false`), goes one step further: before destroying the VM, `delete_vm` records its pool membership; after the destroy completes, it checks whether that pool carries the CPI's provenance comment and, if so, attempts to delete it. PVE itself is the backstop against deleting a pool with real members: a pool that still has a VM in it refuses the delete with an HTTP 500 carrying `is not empty` in the error text, rather than the 404/409 an HTTP client might expect for that kind of conflict — a race the reaper recognizes by matching that text and tolerates silently rather than surfacing as an error. A pool without the provenance comment is left alone unconditionally, never deleted, regardless of whether it is empty. `delete_vm` itself never fails because of the reaper.

### Layered cloud properties and storage tiers

`newLayeredResolver` reads `vm_type` and `disk_type` string selectors from the call's cloud properties into the configured profile maps, then resolves each key across the layers call → disk_type → vm_type → global config, with the call winning. `storage_tiers` adds a layer that matches against live PVE storage, and the global `DiskPerformance` config is the final fallback. Per-disk performance options (iothread, cache, discard, ssd, mbps, iops) are carried in the disk CID's envelope payload (base64url-encoded JSON; `pvd-`, or the opt-in gzip-compressed `pvz-` form for CIDs that would exceed 255 characters), so `attach_disk` decodes them and merges with global config without any out-of-band state. See [Persistent Disks](persistent-disks.md) and [Configuration](configuration.md).

### Agent mode selection

Three agent modes exist: `cloudinit` (the default), `noagent`, and `auto`. The `auto` mode always selects configdrive bootstrap — it is equivalent to `cloudinit` for all stemcells. The factory resolves `auto` per call rather than once at startup. The `internal/configdrive` package builds the ISO 9660 image the cloudinit path delivers; see [ConfigDrive](configdrive.md).

### Retry, pushback, and in-flight limiting

`RetryOnTransient` wraps every PVE API call: exponential backoff of 1s × 1.5ⁿ with ±30% jitter, capped at 15s, up to 8 attempts. `IsPVEPushback` detects HTTP 429 and known PVE phrase patterns and injects a longer `PushbackBackoff` (5s base, 60s cap). An optional per-node in-flight semaphore (`max_inflight_per_node`) gates mutating calls before they reach PVE, reducing pushback at the source. Because pvedaemon recycles workers and serializes per-storage operations under burst load, all three layers are necessary; see [PVE Transient Transport Faults](pve-transient-transport.md) and [PVE Storage Locking](pve-storage-locking.md).

### Task awaiting

Every PVE call that returns a UPID is awaited via `AwaitTask` before the handler returns. By default, the wrapper polls at a fixed 2-second interval. When adaptive polling is enabled, the interval varies between 1 and 10 seconds based on the task's reported progress, polling faster as the task nears completion. Standard calls use a 300-second deadline; stemcell upload and VM disk import use a 600-second deadline (`pve.StemcellMaxWait`) to accommodate large qcow2 files and format conversion such as qcow2 → raw on LVM storage.

### Log redaction

`RedactSecrets` deep-walks maps and strings before anything reaches the log. It masks map values whose key contains a sensitive fragment (password, secret, token, mbus, signature, and similar) or matches `user`/`username` exactly, URL userinfo segments (`scheme://user:pass@host`), and URL query parameters matching a sensitive fragment or the exact token `sig`. The dispatcher applies it to traced arguments and results; the same walk masks credentials embedded in URL-shaped strings. Credentials in cloud properties must never appear in plaintext logs.

## Error Mapping

Every SDK error flows through `internal/pve.WrapError`, which classifies HTTP 4xx as a non-retriable `CloudError` and HTTP 5xx and network timeouts as `RetriableCloudError`. A 404 returns a non-retriable `CloudError` that callers upgrade: `WrapNotFoundVM` and `WrapNotFoundDisk` convert it to `VMNotFound` or `DiskNotFound` at the call site, where the resource type is known. The dispatcher serializes the resulting error's `Type()`, `Error()`, and `OkToRetry()` into the JSON-RPC error envelope; the director uses `OkToRetry` to decide whether to re-drive the call.

## Stemcell Model

Each stemcell is backed by a single frozen PVE template VM. `create_vm` clones that template rather than running a qcow2 block-copy per VM. On linked-clone-capable backends, this drops VM creation from roughly four minutes to seconds.

`create_stemcell` uploads the disk image (extracting a gzip+tar tarball first when needed), imports it into a new QEMU VM in the template VMID range (default `[30000, 30999]`), freezes that VM with `MakeTemplate`, and tags it with a short SHA so later calls can find it. The template's own VMID never appears in the returned CID — the returned stemcell CID is the path-identity `:light:<storage>:import/<file>` or `:heavy:<storage>:import/<file>` form (see [Light Stemcells](light-stemcells.md)) that names the uploaded qcow2 itself. Template creation is idempotent: an existing template matched by content-hash tag is reused. For multi-node clusters, `stemcell_storage` must be a shared pool reachable from every node; `create_stemcell` rejects local storage there, while single-node clusters may use it. The `stemcell_fetch` pipeline can fetch a tarball from S3 or local storage and replicate the frozen template across nodes in parallel; templates carry provenance tags that a cross-node sweep uses to garbage-collect orphans on delete.

`create_vm` and `delete_stemcell` both dispatch on the stemcell CID's `:light:`/`:heavy:` kind, decoded via `pve.ParseStemcellPathCID`:

```mermaid
flowchart TD
    CID[stemcell_cid] --> Parse["ParseStemcellPathCID: kind, storage, path"]
    Parse --> Strategy{stemcell_strategy?}
    Strategy -->|template default| Lookup["Look up per-cluster cache\ntemplate by content-hash tag"]
    Strategy -->|import| Slow[Import qcow2 directly into VM root disk]
    Lookup --> Found{Cache hit?}
    Found -->|yes| Clone[Clone cache template, fast path]
    Found -->|no| Slow
    Clone --> Done[VM created]
    Slow --> Done
```

`stemcell_strategy: template` (the default) clones the per-cluster cache template that `create_stemcell` builds eagerly at upload time. If the cache template is missing (deleted by hand, or never built on this cluster), `create_vm` logs a warning and falls back to `import` for that one VM rather than rebuilding the cache inline. `delete_stemcell` destroys the cache template (kind `light` or `heavy`, both use one) once no director's live reference set (`director_refs`) still names it, then applies kind-specific qcow2 lifecycle: `:light:` files are never deleted (operator-managed, shared across clusters); `:heavy:` files are deleted with the last cache template. Clone type follows `pve.clone_mode` (default `auto`): linked copy-on-write for snapshot-capable backends, full clone for thick LVM. New VMs allocate from the VMID range (default `[100, 8999]`). See [Light Stemcells](light-stemcells.md) for the full CID grammar, cache-template lifecycle, and director-UUID reference counting.

For method signatures, arguments, returns, and per-method error handling, see [CPI Methods](cpi_methods.md). For light-stemcell deployment modes and storage requirements, see [Light Stemcells](light-stemcells.md).

## See Also

- [CPI Methods Reference](cpi_methods.md) — per-method signatures, arguments, returns, and errors.

- [Configuration](configuration.md) — all CPI properties and defaults.

- [ConfigDrive](configdrive.md) — agent settings delivery via the config-drive ISO.

- [Networks](networks.md) — network lifecycle and cloud_properties routing.

- [Persistent Disks](persistent-disks.md) — storage and node-selection cloud properties.

- [Light Stemcells](light-stemcells.md) — light-mode deployment guide.

- [PVE Transient Transport Faults](pve-transient-transport.md) — pvedaemon worker recycling and retry handling.

- [PVE Storage Locking](pve-storage-locking.md) — per-storage lockfile serialization.
