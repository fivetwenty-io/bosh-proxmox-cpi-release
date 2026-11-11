# Architecture

The BOSH Proxmox CPI is a Go binary that implements the BOSH CPI v2 specification for PVE 9.x. The director invokes the binary once per request, exchanging JSON-RPC envelopes over stdin and stdout. Internally, the code splits into layered packages. Each layer has a single responsibility and a narrow interface, so unit tests replace dependencies with mocks.

This document describes the structure: where code lives, how the packages depend on one another, what happens during a single request, the cross-cutting mechanisms that span packages, and how errors flow back to the director. For method signatures and per-method behavior, see [CPI Methods](cpi_methods.md).

This is the structural view. For the reasoning behind it — why the design takes this shape, derived problem first and feature last — read [An Architecture, From First Principles](architecture/index.md), the thirteen-chapter narrative, which also ships as a [Slidev deck](presentations/architecture/README.md).

## Source Layout

Go sources live under `src/pve_cpi/` so the directory shape matches the BOSH packaging spec (`packages/pve_cpi/spec` globs `pve_cpi/...`):

- `src/pve_cpi/cmd/cpi` — binary entry point.

- `src/pve_cpi/internal/...` — internal packages described below.

- `src/pve_cpi/go.mod`, `src/pve_cpi/go.sum`, `src/pve_cpi/vendor/` — module metadata and vendored dependencies.

The Go module path is `github.com/fivetwenty-io/bosh-proxmox-cpi`, and the internal import paths (for example `internal/cpi/handlers`) are unchanged. References to `cmd/cpi` and `internal/...` below name Go packages, not repo-root directories.

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

- `internal/pve` — wraps `github.com/fivetwenty-io/proxmox-apiclient-go/v3`: VM, disk, network, and storage CRUD, task UPID polling, retry and backoff, pushback detection, the cluster pool lock, SDN apply, the foreign-disk guard, and SDK-error normalization.

- `internal/pve/endpointpool` — the ordered set of API endpoints for one cluster and the transport that walks it. It owns the active endpoint, the in-process demotions, the tethers on mutations, the send-by deadline, and the cold-start candidates from `placement.az_map`.

- `internal/endpointcache` — the private cache directory and the files in it. It chooses the directory in layers, checks that the CPI's user owns it and that no one else can read it, and signs and verifies each file. The endpoint cache, the HA profile cache, and the HA recovery episode record all live there.

- `internal/pve/stemcell_fetch` — resolves and fetches a stemcell image from `https://`, `s3://`, `bosh+blobstore:`, or `oci://`, then extracts and uploads it. Per-node template replication lives in `internal/cpi/handlers` (`create_stemcell`), not here.

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

`GatherNodeFacts` queries `/cluster/status`, `/cluster/resources`, and per-node storage to build each node's memory, CPU, storage, guest count, and HA tags. `ScoreNodes` computes a weighted sum (memory 1.0, storage 0.5, CPU 0.5, guest count 0.3, anti-affinity penalty 5.0×). The `create_vm` handler calls `SelectNode`, feeds the winner to every subsequent clone and attach call, and, when `pve.ha.mode` is `auto` or `managed`, registers each VM that passes the HA decision with PVE HA, and writes that VM's node-affinity rule (`bosh-na-{vmid}`) and any anti-affinity rule. A sentinel AZ value of `dlb` hands placement to the PVE 9.2 Dynamic Load Balancer instead; see [DLB-Aware Placement](dlb-aware-placement.md) and [HA and Resurrection](ha-and-resurrection.md). VMs land on nodes that can host them, and anti-affinity groups spread across failure domains.

### Cluster pool lock

PVE resource pools double as a cluster-wide mutex: `POST /pools` is pmxcfs-serialized, so a duplicate poolid returns a 4xx, signaling that the lock is held. `AcquireClusterLock` creates a `bosh-lock-{key}` pool, embedding an owner UUID and an expiry in the pool comment; waiters poll and steal a lock past its expiry. The CPI uses this to serialize the read-modify-write of HA anti-affinity rules across concurrent `create_vm` processes, which would otherwise clobber one another.

### Resource pool lifecycle

Workload VMs land in a pool per deployment by default: `pve.vm_pool_template` (default `bosh-{director}-{deployment}`) renders each deployment's pool, `pve.vm_pool` (default `bosh`) is the static fallback beneath it, and `pve.stemcell_template_pool` (default `bosh-templates`) holds stemcell templates. Config validation keeps the VM-side and template-side pools distinct — pools are the CPI's ACL anchor, and letting a VM pool and a template pool collide would let a `create_vm`-scoped grant also reach shared templates — and the same collision rule is re-checked against every resolved name at `create_vm` time, since a template render can produce any string.

`create_vm` resolves the VM's pool name from a small pipeline, highest precedence first: call-level `cloud_properties.pool`, then a `vm_type` profile's `cloud_properties.pool`, then the rendered `pve.vm_pool_template` (four variables: `{prefix}`, `{director}`, `{deployment}`, `{instance_group}`), then the `pve.vm_pool` static fallback. The pipeline stops at the first non-empty candidate; when every layer is empty, no pool is assigned at all. Every resolved name is validated as a flat PVE poolid — no `/`, no reserved `bosh-lock-` prefix, not the stemcell template pool — before use, since the CPI never creates a nested pool. The winning layer and the template tokens are persisted on the VM as a `bosh_pool` record in the description sentinel; `set_vm_metadata` later re-renders the template from those persisted inputs and moves a `template`-layer VM whose pool has drifted (a changed template, or a legacy VM adopted out of the static pool), never touching call-level, `vm_type`, or static placements.

Both pools are create-if-missing: `EnsurePoolExists` (`internal/pve/pool.go`) creates the resolved pool the first time `create_vm` or `create_stemcell` needs it, tagging it with a `managed by bosh-proxmox-cpi` provenance comment. A concurrent create from a second CPI process is tolerated the same way the cluster lock above tolerates it — pmxcfs serializes `POST /pools`, and the CPI treats the resulting duplicate-poolid error as success. Ensure-before-assign ordering matters: the pool is created (or confirmed to already exist) before the VM create/clone call that names it, on both the import path and the clone path.

Destroying a VM needs no membership cleanup: PVE removes a destroyed VM's pool membership automatically (confirmed against PVE 9.2.4), so `delete_vm` never has to touch the pool API on the common path. A reaper, `pve.pool_reap_empty` (default `true`, so per-deployment pools disappear with their deployments), goes one step further: before destroying the VM, `delete_vm` records its pool membership; after the destroy completes, it checks whether that pool carries the CPI's provenance comment and, if so, attempts to delete it, refusing the static `pve.vm_pool` and `pve.stemcell_template_pool` by name regardless of comment. PVE itself is the backstop against deleting a pool with real members: a pool that still has a VM in it refuses the delete with an HTTP 500 carrying `is not empty` in the error text, rather than the 404/409 an HTTP client might expect for that kind of conflict — a race the reaper recognizes by matching that text and tolerates silently rather than surfacing as an error. A pool without the provenance comment is left alone unconditionally, never deleted, regardless of whether it is empty. `delete_vm` itself never fails because of the reaper.

### Layered cloud properties and storage tiers

`newLayeredResolver` reads `vm_type` and `disk_type` string selectors from the call's cloud properties into the configured profile maps, then resolves each key across the layers call → disk_type → vm_type → global config, with the call winning. `storage_tiers` adds a layer that matches against live PVE storage, and the global `DiskPerformance` config is the final fallback. Per-disk performance options (iothread, cache, discard, ssd, mbps, iops) are carried in the disk CID's envelope payload (base64url-encoded JSON; `pvd-`, or the opt-in gzip-compressed `pvz-` form for CIDs that would exceed 255 characters), so `attach_disk` decodes them and merges with global config without any out-of-band state. See [Persistent Disks](persistent-disks.md) and [Configuration](configuration.md).

### Agent mode selection

Three agent modes exist: `cloudinit` (the default), `noagent`, and `auto`. The `auto` mode always selects configdrive bootstrap — it is equivalent to `cloudinit` for all stemcells. The factory resolves `auto` per call rather than once at startup. The `internal/configdrive` package builds the ISO 9660 image the cloudinit path delivers; see [ConfigDrive](configdrive.md).

### Endpoint pool, failover walk, and tethers

A cluster entry can name more than one API endpoint, through `pve.endpoints`, through the nodes that discovery adds, or through the cold-start candidates that `placement.az_map` supplies. The PVE client hands every HTTP request to `internal/pve/endpointpool`, which installs itself as the transport of the one HTTP client the SDK builds. Because the login goes through the same client, every byte the CPI sends passes through the pool. A pool of one endpoint leaves the transport as it was.

The pool keeps one active endpoint, and a request starts there. When the request fails before a connection exists, the pool tries the next endpoint, which is a walk. A walk ends when a request gets an answer, after four hops, or when every eligible endpoint has been tried. Before it starts each hop after the first, it also checks two budgets. It starts no new hop once it has spent 30 seconds in connect attempts, or once 90 seconds have passed since the walk began.

```mermaid
flowchart TD
    A[Request arrives at the pool] --> B{Past its send-by deadline?}
    B -- yes --> X[Refuse with ErrSendByPassed, send nothing]
    B -- no --> C{Mutation with a tether?}
    C -- yes --> T[Send only to the tethered endpoint, never walk]
    C -- no --> D[Start at the active endpoint]
    D --> E{Connect phase failed?}
    E -- yes --> F[Demote the endpoint, try the next one]
    F --> G{Hop cap or budget spent?}
    G -- no --> E
    G -- yes --> H[Return an error that lists each endpoint]
    E -- no --> I{Request sent and not answered?}
    I -- no, answered --> J[Return the response, keep this endpoint active]
    I -- yes --> K{Read, ticket login, or mutation?}
    K -- read or login --> F
    K -- mutation --> L[Fix a tether on this endpoint, return the error]
```

The picture rests on one proof. Every endpoint gets its own clones of the HTTP transport, and each clone tags a failure that happens while it is dialing, resolving a name, or doing the TLS handshake. Go writes no request byte before it has a connection, so a tagged failure proves that nothing was sent. Only then can any request move to another node. Any other failure, such as a reset or a status code, leaves a mutation maybe sent, and a status code never counts as proof, whatever it says.

Tethers carry that rule across retries. A mutation that may have reached an endpoint is tethered to it for the rest of its call, so a replay from the SDK or from the CPI's own retry helpers goes back to that endpoint and never to a second node that could run it twice. A tether is keyed by the method and path, except that every mutation under `/cluster/ha`, `/cluster/replication`, and `/pools` shares a key per family, because the writers in those areas re-plan after an unknown outcome and can change the method or the path. Reads and the ticket login are never tethered, and they hop freely.

Two more mechanisms bound the cost. Idempotent requests that are not on a walk's final hop carry a 60-second response-header deadline, so a node that accepts a connection and then goes quiet moves the walk along without holding it for the 30-minute client timeout. The cluster lock's steal puts a send-by deadline on its pool delete, and the pool refuses to hand that request to a connection once the deadline has passed, because a late delete could break the lock's mutual exclusion. [Endpoint Failover](endpoint-failover.md) covers the behavior from the operator's side, and [Troubleshooting](troubleshooting.md#endpoint-failover) lists the log lines.

The choice of cache directory is separate. [HA and Resurrection](ha-and-resurrection.md#the-profile-cache-and-when-it-is-off) explains which files the HA code puts in it and when the discovery mode keeps them out.

### Retry, pushback, and in-flight limiting

`RetryOnTransient` wraps every PVE API call: exponential backoff of 1s × 1.5ⁿ with ±30% jitter, capped at 15s, up to 8 attempts. `IsPVEPushback` detects HTTP 429 and known PVE phrase patterns and injects a longer `PushbackBackoff` (5s base, 60s cap). An optional per-node in-flight semaphore (`max_inflight_per_node`) gates mutating calls before they reach PVE, reducing pushback at the source. Because pvedaemon recycles workers and serializes per-storage operations under burst load, all three layers are necessary; see [PVE Transient Transport Faults](pve-transient-transport.md) and [PVE Storage Locking](pve-storage-locking.md).

### Task awaiting

Every PVE call that returns a UPID is awaited via `AwaitTask` before the handler returns. By default, the wrapper polls at a fixed 2-second interval. When adaptive polling is enabled, the interval varies between 1 and 10 seconds based on the task's reported progress, polling faster as the task nears completion. Standard calls use a 300-second deadline; stemcell upload and VM disk import use a 600-second deadline (`pve.StemcellMaxWait`) to accommodate large qcow2 files and format conversion such as qcow2 → raw on LVM storage.

### Log redaction

`RedactSecrets` deep-walks maps and strings before anything reaches the log. It masks map values whose key contains a sensitive fragment (password, secret, token, mbus, signature, and similar) or matches `user`/`username` exactly, URL userinfo segments (`scheme://user:pass@host`), and URL query parameters matching a sensitive fragment or the exact token `sig`. The dispatcher applies it to traced arguments and results; the same walk masks credentials embedded in URL-shaped strings. Credentials in cloud properties must never appear in plaintext logs.

## Error Mapping

Every SDK error flows through `internal/pve.WrapError`, which classifies HTTP 4xx as a non-retriable `CloudError` and HTTP 5xx and network timeouts as `RetriableCloudError`. A 404 returns a non-retriable `CloudError` that callers upgrade: `WrapNotFoundVM` and `WrapNotFoundDisk` convert it to `VMNotFound` or `DiskNotFound` at the call site, where the resource type is known. The dispatcher serializes the resulting error into the JSON-RPC error envelope, translating the internal taxonomy to the classes the director's allow-list recognizes: a retriable error escaping `create_vm` crosses the wire as `VMCreationFailed` with `ok_to_retry: true`, which the director's create step retries up to `max_vm_create_tries`, while retriable errors from every other method cross as `CloudError` with the flag preserved, since no other method has a director-side retry loop. `RetriableCloudError` itself never crosses the wire: the director knows it only as an abstract base class and rejects it as an unknown CPI error.

## Stemcell Model

Each stemcell is backed by a single frozen PVE template VM. `create_vm` clones that template rather than running a qcow2 block-copy per VM. On linked-clone-capable backends, this drops VM creation from roughly four minutes to seconds.

`create_stemcell` uploads the disk image (extracting a gzip+tar tarball first when needed), imports it into a new QEMU VM in the template VMID range (default `[30000, 30999]`), freezes that VM with `MakeTemplate`, and tags it with a short SHA so later calls can find it. The template's own VMID never appears in the returned CID — the returned stemcell CID is the path-identity `:light:<storage>:import/<file>` or `:heavy:<storage>:import/<file>` form (see [Light Stemcells](light-stemcells.md)) that names the uploaded qcow2 itself. Template creation is idempotent: an existing template matched by content-hash tag is reused. For multi-node clusters, `stemcell_storage` must be a shared pool reachable from every node, unless `vm_storage` is shared and the strategy is `template`: in that case a node-local staging pool is accepted because the single template's disk clones cross-node; in every other multi-node configuration `create_stemcell` rejects local storage, while single-node clusters may use it. The `stemcell_fetch` pipeline can fetch the image from `https://`, `s3://`, `bosh+blobstore:`, or `oci://`. Separately, when `pve.stemcell_replicate_local` is set and the `vm_storage` pool — where every cache template's root disk lands — classifies as node-local, `create_stemcell` replicates the frozen template to the remaining cluster nodes in parallel, best-effort. The classification deliberately keys on `vm_storage`, not the stemcell pool. Templates carry provenance tags that a cross-node sweep uses to garbage-collect orphans on delete.

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

`stemcell_strategy: template` (the default) clones the per-cluster cache template that `create_stemcell` builds eagerly at upload time. If the cache template is missing (deleted by hand, or never built on this cluster), `create_vm` logs a warning and falls back to `import` for that one VM rather than rebuilding the cache inline. The fallback needs the stemcell qcow2 readable from the VM's own node: with a node-local `stemcell_storage` staging pool (the single-shared-template topology), only the staging node can import, and `create_vm` on any other node fails with a retriable error directing a retry or a `bosh upload-stemcell --fix` to rebuild the cache template. `delete_stemcell` destroys the cache template (kind `light` or `heavy`, both use one) once no director's live reference set (`director_refs`) still names it, then applies kind-specific qcow2 lifecycle: `:light:` files are never deleted (operator-managed, shared across clusters); `:heavy:` files are deleted with the last cache template. Clone type follows `pve.clone_mode` (default `auto`): linked copy-on-write for snapshot-capable backends, full clone for thick LVM. New VMs allocate from the VMID range (default `[100, 8999]`). See [Light Stemcells](light-stemcells.md) for the full CID grammar, cache-template lifecycle, and director-UUID reference counting.

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
