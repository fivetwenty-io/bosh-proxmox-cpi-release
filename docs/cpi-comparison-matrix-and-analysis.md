# Proxmox VE CPI — Cross-CPI Comparison Matrix and Improvement Analysis

A re-analysis of the Proxmox VE (PVE) BOSH CPI against six upstream CPI implementations,
measured against the canonical BOSH CPI API v2 contract. It mines proven patterns, confirms
which earlier recommendations have shipped, and ranks the capabilities still worth adding —
each with a PVE-specific rationale and a concrete build approach.

## 1. Reference Set and Method

Six reference CPIs were inventoried in depth from real handler source, not just
READMEs, then compared against the current PVE CPI through six cross-cutting capability
themes.

| CPI | Language | API/SDK surface |
|-----|----------|-----------------|
| AWS | Ruby | EC2/EBS/ELB/ALB, v1/v2/v3 API versions |
| vSphere | Ruby | vSphere SDK + NSX-T Manager/Policy, cpi_plugins |
| OpenStack (Go) | Go | Gophercloud — Nova/Cinder/Neutron/Octavia |
| Google | Go | GCE/Compute, target pools + backend services |
| Azure | Ruby | ARM — managed disks, Compute Gallery, LB/App Gateway |
| Alicloud | Go | ECS/SLB/NLB, legacy SDK + Tea OpenAPI SDK |
| **Proxmox (PVE)** | **Go** | **`pve-apiclient-go` v3 — this repository** |

The OpenStack reference ships **two** CPI implementations. `bosh_openstack_cpi/` is the
Ruby CPI wired into the `openstack_cpi` BOSH job — the production path that operators
deploy today. `openstack_cpi_golang/` is a parallel Go port (175 non-vendor files), a
rewrite still in progress while the Ruby CPI continues to ship. This report's
"OpenStack-Go" rows and citations target the Go port. By implementation language, AWS,
vSphere, and Azure are Ruby; the OpenStack Go port, Google, and Alicloud are Go, as is
PVE.

Each reference CPI received a dedicated deep-read producing a method-by-method status
inventory plus a standout-feature list. The PVE CPI received the same treatment, with
explicit cross-checks of which prior-roadmap items are now implemented. Six thematic
analyses — placement, networking, storage, stemcell/agent, resiliency/observability,
and extensibility/ops-UX — then compared the reference behaviors against the current PVE
code and classified each capability as already done or as a Tier 1–4 (or
not-recommended) gap. Every Tier-1 headline below was re-verified directly against PVE
source.

## 2. Executive Summary

Every gap §7.1–§7.41 below is shipped and source-verified — §7.5 only partially, since its
host-side ARP probe is structurally blocked on Proxmox. A feature-group-by-feature-group
re-read of `src/pve_cpi` confirmed each "Shipped." claim against the real control flow,
with file-and-function citations folded into the "In this codebase" and "Shipped"
blocks. Going deeper changed two things. First, the verification surfaced honest caveats
the original prose had glossed — fail-open windows, method-class-global (not per-call)
timeouts, text-pattern pushback fragility, post-import (not pre-commit) checksum, and
reactive-only orphan GC — now recorded as "Limits" under each feature. Second, the wider
reference re-read turned up ten new gaps (§7.26–§7.35), none of which appeared in the
prior report, all now shipped as extensions of the existing work: they enforce the
invariants §7.9 records, monitor the resizes §7.24 sizes, and make the polling §7.25
fixes adaptive.

A deeper read of all six references against the now-shipped
§7.1–§7.35 surfaced six more genuinely new gaps (§7.36–§7.41, all since shipped),
clustered on a theme the prior rounds under-weighted: cross-process and
in-flight-operation safety. They cover an unguarded HA-rule read-modify-write under
parallel deploys (§7.36), a racing concurrent template clone (§7.37), an unguarded
`delete_disk` against a locked volume (§7.38), and three operability and hardening items
— SDN eventual-consistency (§7.39), an ephemeral ≥2×RAM invariant (§7.40), and
dispatcher log redaction (§7.41).

The §3 matrix gained one correction: Azure `update_disk` is a full method, now `Y`, with
a third documented refusal (account-type conversion). The §6 standout list was corrected
against source in several places. Azure's per-backend-pool LB and Application-Gateway
assignment is restored — it is real, in `vm_manager_network.rb`. The §7.30
transport-tuning model is re-attributed to Alicloud; the OpenStack Go CPI parses
`ConnectionOptions` but never applies it to an `http.Transport`. The §7.29 agent-checksum
model lives in the Ruby OpenStack CPI, not the Go one in this reference set. The
earlier finding that process-level panic recovery (§7.4) is not unique to OpenStack-Go —
Google has it too — still holds. A new §9 steps back from the method-by-method view to
compare all seven CPIs along six cross-cutting dimensions: concurrency and cluster-config
safety, observability, test strategy, configuration validation, a consolidated failure-mode
taxonomy, and the performance envelope — the last being the one dimension still argued from
mechanism rather than measured numbers. The remainder of this summary is
the original framing that motivated §7.1–§7.25, retained for provenance.

## 3. Method Implementation Matrix

Measured against the CPI v2 canonical method set. `Y` = real handler logic;
`~` = partial (registered but stubbed, raises `NotImplemented`, or no-op); `N` = absent.

| Method | AWS | vSphere | OpenStack-Go | Google | Azure | Alicloud | Proxmox |
|--------|-----|---------|--------------|--------|-------|----------|---------|
| `info` | Y | ~ (pong) | Y | Y | Y | Y | Y |
| `create_stemcell` | Y | Y | Y | Y | Y | Y | Y |
| `delete_stemcell` | Y | Y | Y | Y | Y | Y | Y |
| `create_vm` | Y | Y | Y | Y | Y | Y | Y |
| `delete_vm` | Y | Y | Y | Y | Y | Y | Y |
| `has_vm` | Y | Y | Y | Y | Y | Y | Y |
| `reboot_vm` | Y | Y | Y | Y | Y | Y | Y |
| `set_vm_metadata` | Y | Y | Y | Y | Y | Y | Y |
| `calculate_vm_cloud_properties` | Y | Y | Y | Y | Y | ~ (empty) | Y |
| `create_disk` | Y | Y | Y | Y | Y | Y | Y |
| `delete_disk` | Y | Y | Y | Y | Y | Y | Y |
| `has_disk` | Y | Y | Y | Y | Y | Y | Y |
| `attach_disk` | Y | Y | Y | Y | Y | Y | Y |
| `detach_disk` | Y | Y | Y | Y | Y | Y | Y |
| `get_disks` | Y | N | Y | Y | Y | Y | Y |
| `resize_disk` | Y | N (raises) | Y | Y | Y | Y | Y |
| `set_disk_metadata` | Y | ~ (no-op) | Y | N | ~ (raises) | Y | Y |
| `snapshot_disk` | Y | N | Y | Y | Y | Y | Y |
| `delete_snapshot` | Y | N | Y | Y | Y | Y | Y |
| `create_network` | N | Y (NSX-T) | N | N | N | N | Y (SDN/bridge) |
| `delete_network` | N | Y (NSX-T) | N | N | N | N | Y (SDN/bridge) |
| `update_disk` (extension) | N | N | N | N | Y¹ | N | Y |

¹ Azure `update_disk` was previously marked `~` (partial). A source re-read shows a full
implementation (`cloud.rb:460`, `disk_manager2.rb:49`) for managed disks — size grow,
IOPS, and MBPS — with three deliberate, documented refusals: caching mode is
creation-time-only, disk shrink is rejected, and account-type/tier conversion raises
`NotSupported` (`cloud.rb:494-500`). It is a complete method with constraints, not a
stub, so it is now `Y`.

Two other re-reads held against source. AWS `calculate_vm_cloud_properties` is real
(`cloud_v1.rb:419`, mapping `cpu`/`ram`/`ephemeral_disk_size` → `instance_type` +
`ephemeral_disk`), so it stays `Y`. Alicloud `calculate_vm_cloud_properties` is an empty
stub (`action/calculate_vm_properties.go` returns `NewVMCloudPropsFromMap(nil)`), so it
stays `~ (empty)`. OpenStack-Go `get_disks` was confirmed a real handler
(`cpi/methods/get_disks.go:26`). Google `set_disk_metadata` and `update_disk` were
confirmed absent in source.

The takeaway, reconfirmed across six references and re-verified against source: **surface
coverage is a settled strength.** PVE implements all 22 canonical methods with real logic
plus the `update_disk` extension, and is one of only two CPIs (with vSphere) to implement
network lifecycle. Every remaining improvement is depth within methods, not new method
stubs.

## 4. What the PVE CPI Already Does Well

Stated explicitly so the roadmap does not regress these. Items marked **(new since prior
report)** were recommendations in the previous roadmap that have since shipped.

- **Live node scoring at `create_vm` (new).** A weighted filter-and-scorer over live
  cluster facts — free-memory fraction, free-storage fraction, CPU headroom, and inverse
  guest density — with repeatable-random tie-break and AZ filtering. It is the direct
  analogue of the vSphere placement pipeline and richer than any cloud CPI's flavor/AZ
  selection.

- **Dual anti-affinity (new).** A soft scoring penalty plus cluster-enforced PVE HA
  negative resource-affinity rules keyed on the BOSH instance group, self-cleaning on
  delete, with an optional strict mode — the analogue of vSphere DRS anti-affinity.

- **AZ-to-node mapping (new).** BOSH `availability_zone` maps to a restricted candidate
  node set; a missing AZ is a hard error, matching AWS/Google/Azure AZ selection.

- **DLB / CRS integration (new).** Opt-in PVE 9.2+ Dynamic Load Balancer registration
  with version, cluster-size, and shared-storage guards — a continuous-rebalance
  equivalent of vSphere DRS that no cloud CPI has, correctly delegated to the platform.

- **Pre-create IP-conflict detection (new, partial coverage).** A parallel cluster scan
  for duplicate static `ipconfig` IPs on the target bridge. It covers CPI-managed static
  IPs; see §6 for the DHCP and foreign-device half still open.

- **Per-VM and per-NIC firewall and security groups (new).** Cluster-level firewall group
  references attached to VMs via the PVE firewall API, with per-NIC override.

- **Dispatcher hook middleware (new).** A zero-cost-when-unused `Before`/`After`
  middleware chain with a static registry and config validation — the vSphere cpi_plugin
  analogue, currently one built-in hook: audit logging.

- **Full v2 contract**, including `info` with `api_version=2`, `disk_hint` return on
  `attach_disk`, and `network_info` return on `create_vm`.

- **Three agent-delivery modes** — cloudinit/configdrive, registry, and noagent —
  matching AWS's registry-optional design and exceeding vSphere (env ISO only).

- **Three stemcell modes** — heavy tarball, light pre-uploaded, and light fetch over
  HTTPS/S3/BOSH-blobstore/OCI — with magic-byte format detection and `os.Root` extraction
  sandboxing; a broader fetch surface than AWS light AMIs.

- **Mature, differentiated retry infrastructure.** Three distinct backoff curves
  (transient, storage-lock, combined) with exponential growth, ±30% jitter, caps, and
  `ctx.Done()` short-circuit; VMID-conflict allocate-retry; and async UPID task polling
  with retriable poll-fault handling. At parity with Azure and beyond OpenStack-Go.

- **PVE-aware fault classification.** The error mapper classifies SDK 4xx/5xx, net
  timeouts, and — uniquely — Perl `die()` strings inside UPID task bodies: storage-lock
  timeout, pmxcfs race, LVM timeout, VMID conflict, clone-source-missing,
  snapshot-blocked, and volume-missing. No reference CPI is this platform-aware because
  PVE leaks failure detail as strings in task bodies rather than HTTP codes.

- **Richest error taxonomy in the set.** `TypeCloud`/`TypeRetriableCloud` plus 14
  specialized typed errors — richer than AWS, Google, or OpenStack-Go.

- **`create_vm` rollback.** A rollback defer (stop + purge, idempotent on 404,
  transient-retry-wrapped, surviving caller cancel via context-without-cancel) — matching
  Azure parallel cleanup, vSphere delete-on-NSX-failure, and OpenStack delete-ports.

- **Per-call `request_id` propagation** threaded through context into every structured
  log line and the dispatcher — matching vSphere and Azure.

- **Clone-mode intelligence.** Auto linked-vs-full clone by storage-backend capability,
  with cross-node shared-storage handling and per-disk format negotiation
  (qcow2/raw/vmdk with block-storage auto-omit) — exceeding the reference set's clone
  handling.

- **Snapshot-aware disk guards.** Attach, detach, and resize pre-flight checks for active
  snapshots, which neither reference cloud CPI needs but Proxmox's snapshot model does.

## 5. Prior-Roadmap Status

| Prior item | Prior tier | Status |
|------------|-----------|--------|
| Availability-aware placement (live scoring) | Tier 1 | **Done** |
| Anti-affinity via PVE HA rules | Tier 1 | **Done** |
| AZ → node-group mapping | Tier 1 | **Done** |
| Pre-create IP-conflict detection | Tier 1 | **Done (static IPs); DHCP/foreign-device half open** |
| Firewall / security groups (PVE firewall API) | Tier 3 | **Done** |
| Plugin / lifecycle-hook middleware | Tier 3 | **Done (framework + 1 hook); catalog + rollback contract open** |
| Disk-CID storage-pattern stickiness | Tier 2 | **Open** |
| Storage selection priority chain | Tier 2 | **Open** |
| Director retryability-signal audit | Tier 3 | **Open** |
| NIC grouping | Tier 4 | **Open (deferred, no use case)** |
| Per-disk encryption | Not recommended | Reaffirmed not recommended |
| VIP / elastic-IP / LB registration | Not recommended (core) | Reaffirmed; deliver via VIP ipfilter + external hook |

## 6. Standout Features Worth Mining (by reference CPI)

The capabilities below are where the reference CPIs are genuinely ahead and the pattern
transfers to a PVE primitive. They feed the gap analysis in §7.

- **vSphere** — `DirectorDiskCID` encoding (`<uuid>.<base64url-json>`, the exact §7.7
  pattern), state-free placement stickiness; on-demand DRS anti-affinity
  (`AntiAffinityRuleSpec`) and VM-host affinity (`VmHostRuleInfo`) rule automation;
  storage-policy (PBM) compatible-datastore discovery; **`cpi_plugins` pre/post hooks on
  every method with plugin rollback on post-hook failure** (the model §7.19 adopted);
  IP-conflict pre-detection; **adaptive, progress-aware task polling** — interval
  `= (elapsed·100/progress − elapsed)/5` clamped 1–10s, so a slow clone is polled less
  often (the model for §7.28); **HA/vSAN-aware delete delay** (a 15s sleep before removal
  to avoid quorum alarms); primary-plus-fallback disk placement with retry on
  `GenericVmConfigFault` (the model for §7.31); multi-cluster placement with datastore
  fallback; and a vCenter custom-field created as a distributed mutex (platform-native
  locking). Every VIM SOAP call routes through a **`RetryableStubAdapter` whose 18-entry
  `NON_RETRYABLE_CRITERIA` table**, keyed on `(entity_class, method_name, fault_class)`
  triples, decides retry: network errors retry up to 8× with `2^i` backoff capped at 32s,
  while `DuplicateName`, `FileAlreadyExists`, and delete-side `FileFault` never retry —
  a far finer-grained classifier than PVE's curve-based model. At `create_stemcell` the
  CPI also **registers itself as a vCenter Extension** (`sddc.cpi.extension`) and stamps
  `config.managed_by` on every CPI-managed VM and template, so vCenter Solution Manager
  groups CPI resources by owner (`cpi_extension.rb:1-70`) — the resource-ownership model
  behind §7.13.

- **AWS** — three-way AZ consensus (volume + subnet + vm_type), applied piecemeal per
  operation (`create_disk` picks an AZ from the instance, `create_vm` re-checks via
  `common_availability_zone`) rather than as a single monolithic gate; per-disk
  `iops`/`throughput`/type and KMS encryption;
  **`VMCreationFailed.new(retryable)` signaling that gates fallback decisions** — a
  bidding failure is non-retriable (fall back to on-demand), an instance failure is
  retriable (retry); ELB and ALB target-group registration **at create only, with no
  delete-side deregistration** (contrast §7.19's rollback contract); `wait_until_running`
  is a **waiter that raises `VMCreationFailed(retryable)` on timeout**, not Azure-style
  serial-console capture; an `AbruptlyTerminated` retry loop (up to 2×) for launch races
  (the model for §7.31); **`fast_path_delete`** that tags and returns without polling for
  terminated state (the model for §7.32); and a volume-modification wait loop
  (`ResourceWait.for_volume_modification`, the model for §7.27). AWS also groups manual
  networks that share a subnet into a **`NicGroup`** — one ENI per group, with VIP
  networks referencing a group name to bind an EIP to the right device index — the
  multi-homing model behind the deferred NIC-grouping roadmap item; and it remaps device
  paths per instance family, exposing EBS volumes on **NVMe instance families** as
  `/dev/disk/by-id/nvme-Amazon_Elastic_Block_Store_<volid>` rather than the `xvd*` alias.

- **Azure** — managed-disk caching/tier baked into the disk CID; **Compute Gallery
  stemcell replication that is automatic at the platform layer** — the CPI calls
  `ensure_gallery_image_in_target_location` only to *validate* replicas exist, not to
  orchestrate the push (the "validate, don't orchestrate" lesson §7.2 follows); the
  gallery path computes a streaming SHA-256, stores it as an `image_sha256` tag, and
  hard-errors on a same-version content mismatch (`compute_gallery_manager.rb:129-169`) —
  a version-collision guard §7.13's provenance work parallels; three-level Gallery
  namespacing (gallery/image-definition/version) with stemcell reference-counting tags for
  idempotent re-upload; `keep_failed_vms` forensic mode (the §7.20 model); parallel
  rollback on create failure; **native `update_disk` (size grow / IOPS / MBPS) that
  explicitly rejects caching-mode changes, disk shrink, and account-type/tier conversion
  as `NotSupported`** (the creation-time-invariant model behind §7.26); optional Disk
  Encryption Set (BYOK) per managed disk; and **per-backend-pool load-balancer and
  Application-Gateway assignment at create** (`vms/vm_manager_network.rb:91-147` —
  `_get_load_balancers` maps each vm_type LB config to a single LB backend pool and joins
  it to the NIC), with **no deregistration on delete** (`vm_manager.rb:288-387` deletes
  the NIC and IPs but never detaches the pool) — the create-only LB posture §7.19's
  rollback contract is the on-prem answer to. Notably, the Azure CPI **hand-rolls its REST
  client** rather than using an SDK; it takes **OS `flock` cross-process locks** under
  `/tmp/azure_cpi/` for availability-set, copy-stemcell, and per-gallery-image-version
  mutations (`utils/helpers.rb:130-138,564-575`) — the only Ruby reference with explicit
  cross-process locking, the same shape as PVE's §7.36 cluster mutex; it stamps a fresh
  `x-ms-client-request-id` (a `SecureRandom.uuid`) on every request and logs the echoed
  correlation IDs; and it ships **opt-in per-operation telemetry** that records operation
  name, duration, and success to a forked handler at most once per 60s
  (`telemetry/telemetry_manager.rb:1-103`) — the lone reference that emits metrics.

- **Google** — LB target-pool/backend-service registration at create; cross-project (XPN)
  networking; **operator-controlled local-SSD opt-in (`ephemeral_disk_type: local-ssd`)
  plus CPU-aware custom machine types** (there is no automatic SSD auto-scaling by machine
  series); remote-tarball SHA-1 verification before import (the verify-before-write model
  §7.6's caveat aspires to); an operation waiter with exponential backoff capped at
  **~782s (≈13 min: `maxTries=100`, `maxSleepExponent=3`)**; multi-zone disk-set conflict
  rejection; **process-level panic recovery** (`main/main.go:29`,
  `defer logger.HandlePanic`, see §7.4); and VM `cloud_properties` that override
  network-level defaults (the cross-property-override model for §7.34). Google's
  observability is distinctive: a **`MultiLogger` folds the full DEBUG trace into every
  CPI response under a `log` field** (`api/multilogger.go`), so the director captures the
  complete per-call trace with no separate aggregation, and secrets are scrubbed at the
  dispatcher and metadata-write boundaries (`redactor/redactor.go:7-9`). Every
  metadata/label mutation reads the current **fingerprint and resubmits it as a CAS
  token**, so GCE rejects a stale concurrent write with a 409 (`metadata_client.go:147`)
  — the read-before-write model behind §7.44 — and every image is created with
  **immutable `GuestOsFeatures` (`MULTI_IP_SUBNET`, `UEFI_COMPATIBLE`, `GVNIC`)** to
  enforce modern NIC and boot capabilities unconditionally.

- **OpenStack-Go** — `allowed_address_pairs` + VRRP port check for in-deployment VIPs
  (the §7.14 model), re-applied idempotently on re-attach (a refinement §7.14 does not yet
  cover); LB pool membership automation with metadata tracking and cleanup; shuffled
  multi-AZ with per-failure next-AZ fallback (§7.10); stale Neutron-port cleanup on retry
  (a cloud-specific network-orphan pattern with no direct PVE analogue, since PVE networks
  are cluster-wide); process-level panic handler (§7.4 — *not* unique to OpenStack-Go);
  and **a single global `state_timeout`** on async operations, coarser than PVE's
  per-method-class envelope (§7.15) — a PVE strength to claim. Two structural traits stand
  out as cautionary. The CPI splits its transport into a plain `ServiceClient` and a
  `RetryableServiceClient`, but it **re-authenticates to Keystone on each service build**,
  costing four-plus auth round-trips per `create_vm` (the cost of stateless purity §7.30
  avoids). And it carries **six parse-but-dead config fields** (`ConnectionOptions`,
  `DefaultVolumeType`, `WaitResourcePollInterval`, `EphemeralDisk`, `SchedulerHints`,
  `UseNovaNetworking`) — the config-surface drift of a young Go port and the case for the
  opt-in unknown-key rejection §7.17 ships. Two earlier attributions to this CPI are
  withdrawn after a source re-read: it does **not** implement transport/connection-pool
  tuning (`ConnectionOptions` is parsed at `config.go:51` but never consumed by any
  `http.Transport`), so §7.30's model is Alicloud; and it injects **no** agent checksum
  into user-data — that capability lives in the Ruby OpenStack CPI, outside this Go
  reference set — so §7.29 is a PVE-originated mechanism.

- **Alicloud** — **`ClientToken` that is *regenerated* on `IdempotentFailed`** (a new
  token on collision, `instance_manager.go` build path), distinct from AWS/Azure
  same-token retry — the two-response idempotency model feeding §7.33's contrast; classic
  SLB + modern NLB dual integration (both registered at create, both deregistered at
  delete) with backward-compatible structured and flat manifest forms; KMS image-copy
  encryption; capacity-reservation tag mapping from `env.Bosh.Group`; proactive ENI
  cleanup on delete; **an `Invoker`/`Catcher` abstraction giving per-operation retry
  budgets** (15–60 retries × 5–15s, each `Catcher` matching an error-string reason,
  `invoker.go:13-58`) rather than a few global curves — the finer-grained take on the
  shipped §7.25; callback-based state-machine polling that embeds per-operation recovery
  (auto-stop, cleanup) inside the poll loop; and explicit SDK transport tuning — a shared
  `http.Transport` with `MaxIdleConns: 500` and an env-tunable `TLSHandshakeTimeout`
  (`alicloud/common.go:99-110`, `config.go:278`), the actual model for §7.30. Image import
  is **OSS-staged**: `CreateFromTarball` creates a private ephemeral OSS bucket,
  multipart-uploads `root.img` in 5 MB chunks across five goroutines (`oss.Routines(5)`),
  then calls `ImportImage` and defers deletion of both the object and the bucket — avoiding
  a direct large-image stream to the API (the model for §7.35).

## 7. Gap Analysis

Gaps are deduplicated across themes and grouped by tier. Each states what the reference
CPIs do, why it matters specifically on Proxmox (the documented lab incidents are cited
where they apply), and a concrete build approach on PVE primitives. All additions follow
the established additive-optional convention: validate only when set, omit from VM config
when empty, zero behavior change for existing manifests.

### Tier 1 — Correctness and latent-bug closure (do first)

These six are not enhancements; they are bugs or missing safety primitives. The first
three exist *because* placement intelligence shipped: each subsystem still assumes the
create-time node is fixed and reachable.

#### 7.1 DONE — Maintenance / unhealthy-node exclusion in the scorer

*References: vSphere, Google.* The scorer's `Filter()` rejects a node only when it is
offline or outside the candidate set; node facts never read HA or maintenance state.
Worse, the free-memory weight makes the scorer **actively prefer a node being drained**
— a draining node is emptying, so it has the most free memory and scores highest. An
operator drains node N for a PVE/kernel upgrade, the scorer lands a CF diego-cell or
HAProxy on N, N reboots, the VM dies, and BOSH resurrects it back onto N. This is the
same "placed a workload where it must not live" class as the documented NATS incident.

**Build:** add per-node `Maintenance`/`Healthy` to the node facts, sourced from
`GET /cluster/ha/status/current` and/or an operator node tag; add a hard rejection in
`Filter()` behind `placement.exclude_maintenance_nodes` (default on). Fail-open per node
on a status-fetch error, exactly like the existing storage-facts gathering, so a
diagnostics gap never fails `create_vm`.

**In this codebase.** `GatherNodeFacts` (`internal/placement/facts.go:161-191`) reads
`GET /cluster/ha/status/current` and a configurable per-node operator tag list
(`MaintenanceNodeTags`), marking a node `InMaintenance` when its HA state is
`maintenance`, `error`, `fence`, or `recovery`, or when it carries a maintenance tag.
`Filter()` (`internal/placement/scorer.go:158-160`) then hard-rejects those nodes before
scoring whenever `Request.ExcludeMaintenanceNodes` is set. A status-fetch error fails open
per node (`InMaintenance` stays false). **Why it matters here.** This removes the
draining-node foot-gun directly: an operator can put a node into PVE HA maintenance — or
just tag it — for a kernel/PVE upgrade, and the scorer stops landing diego-cells or
HAProxy there instead of preferring it for its transiently high free memory. The dual
HA-state plus operator-tag path gives both an automatic and a manual lever, which matters
in a lab where maintenance windows are frequent. The asymmetry with vSphere is structural:
vSphere DRS continuously rebalances VMs off a host entering maintenance and the CPI merely
declares rules, whereas PVE HA rules are advisory and never re-enforced by a distributed
scheduler (`lib/cloud/vsphere/drs_rules/drs_lock.rb`), so the CPI must do the exclusion
itself at placement time. **Limits.** Fail-open means a flapping HA API can momentarily
leave a draining node eligible; the tag list is maintained separately from PVE HA state
and has no auto-cleanup when a tag is removed.

#### 7.2 DONE — Multi-node template reachability (per-node stemcell replication)

*References: vSphere, Azure, AWS, Alicloud.* The stemcell template is created on a single
node, and `create_vm` clones from it. The clone path sets a cross-node `Target` only
when storage is **shared**; on node-local storage (dir/LVM/LVM-thin/local-ZFS — the lab
runs per-node local ZFS) a template that lives only on node A cannot be cloned onto
node B. The shipped live scorer and DLB will pick a best-fit node that may not hold the
template, turning the new placement intelligence into a clone failure.

**Build:** read the storage `shared` attribute (the backend resolver already reads
storage types). If shared, no-op. If node-local, after `MakeTemplate` replicate per
candidate node via `qm clone --target` (full) or ZFS storage replication; identity via
the existing sha8 content tag; resolve the template VMID on the chosen node at
`create_vm`. Make it lazy and best-effort (replicate on first `create_vm` targeting a
node that lacks it), behind `stemcell.replicate_local` (default off). `delete_stemcell`
removes all per-node replicas (mirror vSphere parallel replica deletion).

**In this codebase.** Replicas are indexed by PVE VM tags rather than a side table: a
replica carries both `bosh-stemcell-sha-<sha8>` (content identity) and
`bosh-stemcell-node-<node>` (replica marker). `ResolveTemplateVMIDForNode`
(`internal/pve/template.go:425-511`) scans a node's guests and returns the replica VMID
when both tags are present, or the primary VMID when the node tag is absent;
`resolveTargetNode` (`internal/cpi/handlers/create_vm.go:673`) consults this during
placement. The tag format is deterministic (`dnsSafeStemcellPart` sanitization) so
replication and lookup agree across CPI processes. **Why it matters here.** A best-fit node
chosen by the scorer or DLB no longer fails its clone just because the template happens to
live on a different node; on the lab's per-node local ZFS this converts a hard clone
failure into a local, fast clone, and on slow inter-node links a local replica cuts clone
time substantially. The reference models split here: Azure treats replication as
platform-driven — its Compute Gallery replicates an image definition to multiple regions on
its own and the CPI only consumes the result (`compute_gallery_manager`) — while vSphere's
linked clone shares VMDK blocks with the parent snapshot and consumes only delta storage.
PVE has neither; `qm clone` is a full copy unless the template sits on shared storage, which
is exactly why a node-local replica is the right primitive. **Limits.** Use is opportunistic
— the CPI consumes a replica if one exists but does not itself create it on every node; there
is no replica lifecycle (re-sync after a stemcell change, periodic GC) beyond the sha-tag
sweep on delete.

**Operator impact.** Indexing replicas by PVE guest tags rather than a CPI-side table is what
makes this safe under parallel deploys: any CPI process can discover an existing replica purely
from cluster state via `ResolveTemplateVMIDForNode` (`internal/pve/template.go:441-511`, a single
`ListQemu` per node), with no shared registry to corrupt or lock. Because identity is the
deterministic `bosh-stemcell-sha-<sha8>` + `bosh-stemcell-node-<node>` tag pair, two concurrent
`create_vm` calls targeting the same node converge on the same replica VMID (lowest-VMID-wins
tiebreak) instead of racing to clone duplicates.

#### 7.3 DONE — VM-disk fault-domain co-location

*References: AWS, Google, Azure, OpenStack-Go.* `create_disk` resolves only a storage
pool name with no node/AZ awareness, and `attach_disk` does not validate that a disk's
backing storage is reachable from the VM's landed node. On node-local storage a disk
created on node A becomes **un-attachable** once anti-affinity or DLB later places the
VM on node B — failing late and opaquely at attach time. Every reference CPI treats
VM-disk fault-domain co-location as a hard invariant, and each enforces it before the
VM exists rather than discovering the conflict at attach. AWS intersects the disk, vm_type,
and subnet AZ constraints and errors unless all are identical — there is no scorer and no
fallback, because EBS volumes are AZ-scoped and cross-AZ attach is impossible by API.
OpenStack's Cinder volumes are likewise zoned, so the disk AZ must match the VM AZ; Google
rejects a multi-zone disk set outright; Azure migrates a regional disk into the VM's zone.

**Build:** detect node-local backends via the existing resolver; record the disk's home
node (carried in the CID encoding of §7.7). When `create_vm` receives existing disk CIDs
in its `disks` argument, constrain candidate nodes to those that can reach the disks
(all online for shared, home-node only for local). Pre-flight `attach_disk`: if backing
storage is local and absent on the VM's node, return a clear error instead of an opaque
PVE failure. Extend the DLB shared-storage requirement to cover the local-disk +
anti-affinity combination.

**In this codebase.** `deriveDiskFaultConstraints`
(`internal/cpi/handlers/create_vm.go:1210-1298`) reads the `Node`/`AZ` labels carried in
each existing disk CID (via `ParseEncodedDiskCID`, the §7.7 codec) and classifies the
backend through the storage resolver: local-backed disks impose a single hard node pin
(all local disks must share one node, else a non-retriable `CloudError`), shared-backed
disks with AZ labels add to a required-AZ set. Those constraints feed `resolveTargetNode`
*before* AZ selection (`create_vm.go:820-824`), so a VM can only score onto a node that can
actually reach its disks. **Why it matters here.** This is the fix that makes placement and
node-local storage coexist: once anti-affinity or DLB can move a VM, a persistent disk
created on node A would otherwise become un-attachable when the VM lands on node B, failing
late and opaquely at attach. It closes the orphan/unreachable-disk path and matches the
hard fault-domain invariant every cloud CPI enforces — PVE differs only in that shared
storage is cluster-global, so a shared-backed disk imposes no node pin at all, while the
local-disk path mirrors the AWS-style hard intersection rather than a soft score.
**Limits.** Bare legacy CIDs carry no metadata and therefore impose no constraint
(backward-compatible, but unprotected); backend classification is best-effort (a
`/cluster/storage` fetch error fails open, dropping the AZ constraint); and there is no
runtime check that the disk volume still physically exists on the chosen node.

#### 7.4 DONE — Process-level panic recovery in the dispatch path

*References: OpenStack-Go, Google.* There is no `recover()` anywhere in `internal/` or
`cmd/cpi/main.go` (verified). A nil-deref or index panic in any of the 22 handlers —
config decode, ipconfig parsing, and placement scoring all do pointer/slice/map work on
PVE API responses and cloud_properties — crashes the process; the director then receives
empty stdout and a non-zero exit with no typed `CloudError`, no `request_id`, and no
method context. This is the clearest structural-safety gap versus the reference set.

**Build:** add a deferred `recover()` in the dispatcher around the handler call plus a
backstop in `main.go`; convert the recovered value to `Cloud("panic in %s: %v", method, r)`,
log the stack at error level with `request_id` attached. Mirrors the OpenStack-Go panic
handler. Zero new dependencies.

**In this codebase.** Recovery is two-layered. The dispatcher's `Handle`
(`internal/cpi/dispatcher.go:165-175`) captures `method` and `request_id` *before* calling
the handler, wraps the call in `defer recover()`, logs `runtime/debug.Stack()` at error
level, and converts any panic into a non-retriable `CloudError` carrying the method name.
A second backstop in `main.go`'s `dispatchOne` (`cmd/cpi/main.go:416-434`) catches panics
outside the dispatcher (e.g. in response writing), resets the buffered writer to discard
corrupted partial output, and emits a clean error response. **Why it matters here.** A
nil-deref or slice/map panic in any of the 22 handlers — config decode, ipconfig parsing,
and placement scoring all do pointer/slice/map work over untrusted PVE responses and
`cloud_properties` — no longer kills the process and hands the director an empty stdout with
no typed error or context; the CPI stays alive to serve the rest of the deploy.
**Comparative note.** PVE is *not* the only-or-second CPI with this: Google installs
process-level recovery in its entry point (`main/main.go:29`, `defer logger.HandlePanic`),
and OpenStack-Go does the same in its `main.go:22`. PVE's placement at the *handler*
boundary is architecturally stronger — the process survives and the director can retry
*other* methods — but the recovered value is returned non-retriable, so panic recovery
prevents process death; it does not make the failed operation succeed.

#### 7.5 PARTIAL — Active IP-conflict probe (ARP / guest-agent) for DHCP and foreign devices

*References: vSphere, OpenStack-Go, AWS, Alicloud.* The shipped detector scans static
`ipconfig{N}` entries only; its source notes it cannot detect DHCP-assigned addresses
and does not see physical hosts, containers, or non-PVE devices. The documented CF
NATS-churn incident was caused by exactly that uncovered half — a BOSH VM IP that also
answered ARP from a physical device on the shared LAN. The detector covers the easy half
and leaves the half that bit the deployment uncovered. OpenStack goes further by
self-healing: on an IP conflict at NIC pre-create it auto-deletes any DOWN or unowned
stale Neutron port holding that address and retries once
(`networking/...`); Alicloud retries `InvalidPrivateIpAddress.Duplicated` up to 30 times
on a backoff rather than failing immediately.

**Build:** add an opt-in active-probe mode under the existing `ensure_no_ip_conflicts`
flag (e.g. `ip_conflict_probe: arp`). Before boot, for each target static IP on the
bridge, run `arping -c2 -w1 -I <bridge> <ip>` via node exec (or read `/proc/net/arp`
after a ping sweep) to catch any responder, physical or virtual. Optionally fan out the
QEMU guest-agent `network-get-interfaces` across running guests to catch DHCP-assigned
addresses not in `ipconfig`. Reuse the existing conflict error path. Keep the cheap
static scan as default; gate the active probe opt-in since it needs node exec.

**Status:** the guest-agent half shipped — opt-in `ip_conflict_probe: agent` fans out
`network-get-interfaces` across running guests to catch DHCP-assigned addresses absent
from `ipconfig`, fail-open, reusing the conflict error path. The host-level ARP half is
NOT shipped: the CPI client is PVE-API-only and PVE exposes no arbitrary host shell
(`/nodes/{node}/execute` is a bulk API-call runner, not a shell), so `arping` on the
bridge is not reachable without adding node SSH. Detecting physical/non-PVE responders
therefore remains open; the config is an enum so an `arp` mode can be added if node SSH
is introduced. The physical-device vector from the NATS-churn incident is separately
mitigated by the isolated-SDN migration. A PVE-native analogue of OpenStack's stale-port
self-heal — clearing a stale `ipconfig` entry from a dead VM holding the target IP before
provisioning — is also unbuilt and would close the same class without needing node SSH.

**In this codebase.** Phase 1 (`internal/cpi/handlers/create_vm_ipconflict.go`,
`detectIPConflict`) is always-on when `ensure_no_ip_conflicts` is set: it lists cluster
VMs, fetches each config in parallel (bounded at 16 workers), parses `ipconfig{N}` for
static IPs on the target bridge, and cancels the remaining workers the instant a conflict
is found. Phase 2 (`create_vm_ipconflict_agent.go`, `probeGuestAgentIPConflict`, opt-in
`ip_conflict_probe: agent`) fans `network-get-interfaces` across running guests (also 16
parallel, fail-open per guest) to catch DHCP-assigned addresses absent from `ipconfig`.
Both share a `context.WithCancel` early-abort. **Why it matters here.** The static scan
covers CPI-managed IPs; the guest-agent phase closes the DHCP half that the original
detector's own comments admitted it could not see — the exact uncovered half implicated in
the CF NATS-churn duplicate-IP incident. **Limits.** Neither phase can see physical hosts
or non-PVE devices on a shared LAN: the client is PVE-API-only and PVE exposes no arbitrary
host shell, so the `arping`-on-bridge path remains unbuilt (the config is an enum so an
`arp` mode can be added if node SSH is ever introduced). The physical-device vector is
mitigated structurally instead, by the isolated test SDN.

#### 7.6 DONE — Stemcell image checksum verification

*References: Google, Alicloud, OpenStack-Go.* PVE validates magic bytes and a size cap
and computes a sha256 **after** import for the identity tag, but never compares against
an expected digest. The light-fetch surface (HTTPS, S3, BOSH blobstore, OCI) is the
broadest and most failure-prone, and the documented Tailscale LAN-route and
Cloudflare-tunnel hazards make truncated reads plausible. Because the sha8 tag is
computed from whatever bytes arrived, a corrupt fetch is then permanently reused via the
dedup fast-path. Verifying against an expected digest converts silent corruption into a
clean retriable error.

**Build:** accept an expected digest from `cloud_properties.sha1`/`sha256` or the
stemcell manifest; wrap the download reader in a hash and compare on completion. On
mismatch raise a checksum-mismatch error — retriable for network sources, non-retriable
for local heavy tarballs. With no expected digest supplied, keep compute-only behavior
but warn that integrity was unverified.

**Shipped.** `verifyExpectedDigest` (`internal/cpi/handlers/create_stemcell.go:2202-2268`)
compares an operator-supplied `expected_sha256`/`expected_sha1` against the hash computed
in-flight: the light-fetch path streams the remote body through an `io.MultiWriter`+
`TeeReader` into a temp file while hashing (`create_stemcell.go:1445-1451`), and the heavy
path hashes the inner `.img` during tar extraction. A mismatch is non-retriable for local
sources and retriable for network sources, with SHA-256 taking precedence over SHA-1; an
empty expected digest logs an integrity-unverified warning and proceeds. **Why it matters
here.** The documented Tailscale LAN-route and Cloudflare-tunnel hazards make truncated
fetches plausible, and because the dedup fast-path keys on the post-import sha8 tag, a
corrupt fetch would otherwise be cached and reused forever; an expected digest turns that
silent corruption into a clean, retriable failure. Two references verify earlier in the
pipeline than PVE can: Google passes `source_url` and `raw_disk_sha1` straight to the GCE
image-import API for server-side integrity verification with no local download, and Azure
stores each gallery image's `image_sha256` in version tags and hard-errors on a collision
whose digests disagree rather than silently overwriting
(`compute_gallery_manager`). **Limits (verified this round).** Verification is client-side
and *after* download/extraction, so a deterministically corrupt source re-verifies to the
same bad hash on retry (unlike Google's server-side verify-before-write); and the retriable
network-mismatch path has no internal exponential backoff — it relies entirely on
director-level retry. This is the open half that new gap §7.29 (boot-path agent integrity)
and the verify-before-commit lesson address.

### Tier 2 — Operability

#### 7.7 DONE — CID-encoded placement and storage stickiness

*References: vSphere, Azure.* The disk CID is a bare `<storage>:<volid>`; the chosen
pool, tier, and home node are forgotten on detach. In tiered clusters (ceph-nvme hot
vs local-zfs bulk) a re-attach after node failure or migration can silently relocate a
fast-pool disk to a slow pool, and there is no durable carrier for the disk home node
that §7.3 needs. This is the exact vSphere base64-JSON disk-CID pattern.

**Build:** extend the CID codec with an optional, backward-compatible base64-JSON suffix
carrying `{pool, node, az}`; bare legacy CIDs continue to parse unchanged (the
delete path already tolerates `light:`-prefixed and bare CIDs). `create_disk` records
pool + home node; `attach_disk` and scoring read it. Ship this before §7.3, which
depends on it.

**Shipped.** `EncodeDiskCID` (`internal/pve/disk.go:127-138`) appends a base64url-JSON
suffix to the bare CID behind a pipe separator — `<storage>:<volid>|<base64url>` — carrying
`{Pool, Node, AZ, Opts}` (`DiskCIDMeta`, `disk.go:86-107`). `create_disk` writes it
(`create_disk.go:451-456`); `attach_disk`, `update_disk`, and placement decode via
`ParseEncodedDiskCID`. Pool is always recorded, Node for local backends, AZ when
`cloud_properties.availability_zone` is supplied. **Why it matters here.** This is the
durable carrier the §7.3 fault-domain fix reads, and it makes re-attach deterministic — in a
tiered cluster (ceph-nvme hot vs local-zfs bulk) a fast-pool disk can no longer silently
relocate to a slow pool after a node failure or migration. The two reference encodings bracket
PVE's: vSphere's `DirectorDiskCID` is the same `<id>.<base64url-json>` shape, while Azure
takes the opposite tack with structured `key:value` strings (`caching:[C];disk_name:...;
resource_group_name:[RG]`) carrying the same intent in a human-readable form. PVE follows
vSphere because a PVE volid is opaque and a base64 suffix never collides with it. **Limits.**
Legacy bare CIDs still parse, carry no metadata, and therefore impose no §7.3 constraint; the
metadata is advisory — PVE API calls always use the bare volid extracted from the encoded form.

**Operator impact.** The upgrade requires no disk migration: `EncodeDiskCID` returns the bare
CID unchanged when all metadata fields are zero (`internal/pve/disk.go:127-130`), and the `|`
separator is chosen precisely because a PVE volid (`<storage>:vm-<int>-disk-<int>`) can never
contain a pipe (`disk.go:109-113`), so old and new CIDs round-trip through the same parser. An
operator can deploy this CPI over an existing director with persistent disks in flight and
observe zero CID churn — only disks created *after* the upgrade gain the placement suffix, and
even those degrade gracefully to bare volids for every PVE API call.

#### 7.8 DONE — Layered cloud_properties resolution (vm_type → disk_type → global)

*References: vSphere, AWS, Azure, Alicloud.* PVE config is single-level: only per-call
cloud_properties override global config. Storage resolves
`cloud_properties.storage_pool → storage alias → config.disk_storage` with no
vm_type/disk_type tier between, so operators cannot express "all database VMs persist on
the fast pool" without stamping cloud_properties on every disk in every manifest. This is
the keystone operator-UX gap and the open prior-roadmap storage-chain item. Four of six
reference CPIs ship the chain. AWS sets cluster-wide defaults (for example
`aws.encrypted` and `aws.kms_key_arn`) that each resource's cloud_properties may override
independently; Alicloud reads a per-disk `*bool` and falls back to the global flag when it
is nil; OpenStack-Go's Go port carries the equivalent at the config level (`boot_from_volume`
as a global with per-call effect). vSphere expresses the same intent declaratively through
its storage policy engine (PBM).

**Build:** a pure config/resolution change, no new PVE API. Generalize the existing
storage-resolver precedence into a typed resolver over an ordered slice:
`disk_type.cloud_properties → vm_type.cloud_properties → global`. Apply it to attributes
that already have a single home — storage pool, disk format, clone mode, bridge/vnet/zone,
security groups, firewall, placement weights/AZ. Optionally match by PVE storage
attributes from `/cluster/storage` (type, shared) so a `storage_tier: fast` resolves by
attribute, mirroring vSphere PBM.

**Shipped.** A generic layered resolver now applies the precedence `per-call
cloud_properties → disk_type profile → vm_type profile → global config` to every attribute
named above. Because the BOSH director merges vm_type/disk_type cloud_properties into one
flat dict before the CPI call (the CPI never receives the type name), profiles are defined
in CPI config (`vm_types`, `disk_types`) and selected per deployment via the
`cloud_properties.vm_type` / `cloud_properties.disk_type` keys — mirroring vSphere
storage-policy-by-name. `create_vm` now honors a profile- or call-selected root/ephemeral
storage pool (previously only `config.vm_storage`, the keystone gap). `storage_tier`
attribute matching is implemented: `cloud_properties.storage_tier` resolves against live
`/cluster/storage` using operator-defined `storage_tiers` criteria (allowed types and a
shared/local predicate), returning the first matching pool; an unknown tier or no match is
a non-retriable error rather than a silent fallback. A global default `security_groups`
list and a per-profile firewall toggle are also resolved through the chain. Every property
is opt-in: with no profiles, selectors, or tiers configured, resolution is byte-identical
to prior releases.

**Operator impact.** This collapses the keystone UX gap from the intro: an operator declares
`disk_types: {fast: {cloud_properties: {storage_pool: nvme}}}` once in CPI config and selects it
per deployment with `cloud_properties.disk_type: fast`, instead of stamping the pool on every
disk in every manifest. Because the director pre-merges the profile into a flat cloud_properties
dict (the CPI never sees the type name, see `resolveProfileLayer` in
`internal/cpi/handlers/cloudprops_resolver.go:71`), the selector-by-name model mirrors vSphere
storage-policy-by-name while keeping PVE's flat single-level config. The `storage_tier` path goes
further by matching live `/cluster/storage` attributes (`storage_tier.go:81`), so a tier survives
operators renaming the underlying pool.

#### 7.9 DONE — Per-disk performance options (iothread / cache / discard / ssd / IO limits)

*References: AWS, Azure, Alicloud.* The PVE-native analogue of AWS `iops`/`throughput`
and Azure `iops`/`mbps`. QEMU disks accept per-disk options governing performance and
data safety: `iothread=1` (recommended with `virtio-scsi-single`), `cache`, `discard=on`
(TRIM/UNMAP — important for thin pools to release freed space), `ssd=1`, and
`mbps_*`/`iops_*` throttles. None are set today: `create_vm` hard-codes
`scsihw=virtio-scsi-pci` with no options on the root disk, and `attach_disk` never
populates the SDK's `AttachOpts.Extra`. This is the cheapest high-impact storage item
because the SDK already merges arbitrary disk params via `AttachOpts.Extra` — it is one
struct field away on attach.

**Build:** add optional disk cloud_properties (`iothread`, `cache`, `discard`, `ssd`,
optional `mbps_rd/wr` + `iops_rd/wr`) plus vm_type/global defaults resolved through §7.8.
On `attach_disk` marshal enabled options into `AttachOpts.Extra`. On `create_vm` set the
same on the root disk and switch the controller to `virtio-scsi-single` when `iothread`
is requested. Validate-only-when-set, omit-when-empty.

**Shipped.** All eight options (`iothread`, `cache`, `discard`, `ssd`, `mbps_rd`,
`mbps_wr`, `iops_rd`, `iops_wr`) plus an opt-in `virtio_scsi_single` controller toggle now
resolve through the §7.8 layered resolver (per-call cloud_properties → `disk_type` profile →
`vm_type` profile → a global `disk_performance` config block). Options are baked into the
PVE disk value string (`scsi1: store:vol,iothread=1,cache=writeback,…`), reusing the disk
option codec already proven in `update_disk` — not `AttachOpts.Extra`, which the SDK merges
as top-level VM config keys rather than per-disk options. Because `attach_disk` receives no
cloud_properties (CPI v2 passes only `vm_cid` and `disk_cid`), `create_disk` resolves the
options and encodes them into the disk CID metadata; `attach_disk` decodes them and merges
over any global defaults, with the per-disk values winning. On `create_vm` the resolved
options are applied to the `virtio0` root disk on both the import and clone paths, and
`scsihw` switches to `virtio-scsi-single` only when explicitly opted in (default stays
`virtio-scsi-pci`). Resolution is bus-aware: `ssd` is dropped on the virtio-blk root disk
(invalid there) but kept for scsi persistent disks. Cache mode and non-negative throttles
are validated at config-load and call time, surfacing a non-retriable error on bad input
rather than a late PVE rejection. Every option is opt-in: with none set, the encoded CID,
the attach call, and the create parameters are byte-identical to prior releases.

**Benefit.** Each knob maps to a concrete Proxmox storage win: `discard=on` issues TRIM/UNMAP so
a thin LVM or Ceph pool actually reclaims space the guest frees (without it, a thin pool fills
permanently as files are deleted); `iothread=1` gives the disk a dedicated QEMU I/O thread,
lifting the single-threaded bottleneck on busy DB VMs; `mbps_*`/`iops_*` cap a noisy-neighbor VM
on a shared pool — the direct PVE equivalent of AWS per-volume `iops`/`throughput`. Azure makes
the same bet differently, folding disk `caching` into its structured CID. The CID-carried encoding
(`disk_performance.go` resolves at create, attach decodes) is the load-bearing design choice:
`attach_disk` receives only `vm_cid`/`disk_cid` under
CPI v2, so options that must survive a detach/re-attach cycle have nowhere else to live but the
disk CID metadata.

#### 7.10 DONE — Multi-AZ candidate spread with next-AZ fallback

*References: OpenStack-Go, AWS, vSphere.* `availability_zone` is a single string yielding
one candidate set; if that AZ is full, all in maintenance (§7.1), or yields no viable
candidate, `create_vm` fails rather than spilling to a sibling AZ. Small on-prem clusters
(three nodes modeled as three single-node AZs) are exactly where one AZ being full or
drained is routine. OpenStack accepts an `availability_zones` array, shuffles it, and tries
each in order on failure — but it must set `ignore_server_availability_zone` because Cinder
disk-AZ coherence cannot be guaranteed across the shuffle. AWS sits at the strict end: it
intersects AZ constraints with no fallback at all, since cross-AZ EBS attach is impossible
by API.

**Build:** accept `cloud_properties.availability_zones` (plural) or
`placement.az_fallback_order`; build candidate sets per AZ in operator/shuffled order,
run the existing filter+score per set, advance to the next AZ on empty-after-filter.
Reuse the scorer unchanged — only the candidate-set loop is new. Default to current
single-AZ strict behavior (opt-in, since it relaxes the AZ guarantee).

**Shipped.** `buildAZOrder` (`create_vm.go:1120-1180`) turns a singular `availability_zone`
into a one-element list, passes a plural `availability_zones` through as-is, and appends
`placement.az_fallback_order` after an optional shuffle. `resolveTargetNodeWithRNG`
(`create_vm.go:870-1040`) gathers node facts once and runs filter→score→pick per AZ,
advancing to the next AZ on an empty-after-filter pass and falling back to `config.node`
only when all AZs are exhausted. **Why it matters here.** On a three-node lab modeled as
three single-node AZs, one AZ being full or drained (§7.1) is routine; the waterfall spills
to a sibling AZ instead of failing the deploy, with no sequential per-AZ retry delay. PVE's
model is structurally the OpenStack one but operates over node names, and it avoids
OpenStack's `ignore_server_availability_zone` caveat because §7.3 already pins disk-bearing
VMs to a fault domain rather than trusting the shuffle to preserve disk-AZ coherence.
**Limits.** The legacy singular form yields no AZ fallback (only the ultimate `config.node`
fallback); the `config.node` fallback is silent (debug log) and steps outside the AZ
topology; the transient-vs-permanent decision on an exhausted run is the §7.23 heuristic.

#### 7.11 DONE — Retryability-flag boundary audit across all 22 handlers

*References: AWS, Google, Alicloud.* The taxonomy and the error mapper exist, but not
every error return is confirmed to set the retriable bit on the correct boundary
(prior-roadmap item, still open). Mis-signaling is deploy-cost-bearing: a transient
fault mis-classified as permanent fails a whole CF deploy; a permanent fault
mis-classified as retriable burns the director's retry budget. Directly relevant given
the documented NATS-churn, wedged-task, and orphan-VM fragility. The references carry the
retriable bit as a typed, per-raise-site decision: AWS's `VMCreationFailed(retryable: bool)`
sets it at each raise site (true on a wait-for-running timeout, false on a bad network ID),
and Google's `RetryableError` interface exposes `CanRetry()` on typed error structs.
OpenStack-Go is the cautionary counter-example: its `VMCreationFailed` carries the bit but
all other operations raise a flat `CloudError` with no retryable bit at all.

**Build:** a mechanical pass over all 22 handlers ensuring every error return routes
through the wrap functions so the classifier sets the retriable bit; add table-driven
boundary tests asserting `IsRetriable()` for representative SDK error shapes per handler.

**Shipped.** Classification lives in `WrapError` (`internal/pve/error_map.go:29-97`):
404→non-retriable, 5xx/429→retriable, other 4xx→non-retriable, transport faults
(`ConnectionError`/`TimeoutError`/`net.Timeout`) retriable, and task-body strings
(storage-lock, pmxcfs race, pushback phrases) matched to typed predicates
(`error_map.go:65-96`). The boolean rides `*cpierrors.Error.retriable`, and `dispatchError`
(`dispatcher.go:386-401`) projects it into the JSON-RPC `ok_to_retry` the director reads.
**Why it matters here.** A momentary network blip on a single-node lab is retried instead of
failing a CF deploy; a real 404 fails fast instead of burning the director's retry budget —
the exact boundary the NATS-churn, wedged-task, and orphan-VM incidents are sensitive to.
Unlike OpenStack-Go, PVE classifies *every* operation through one central mapper rather than
leaving non-create paths as untyped `CloudError`, so the retriable bit is set uniformly
across all 22 handlers. **Limits (verified this round).** Retryability is a single binary
flag: an intra-CPI retry loop (`RetryOnTransient`, `AllocateWithRetry`) that exhausts its
budget surfaces the *last* attempt's classification — which may have flipped from retriable
to non-retriable — and there is no coordination between the CPI's own retry budget and the
director's.

#### 7.12 DONE — Post-boot guest-agent / VM health verification

*References: AWS, Azure.* `create_vm` returns once the start task completes; it never
verifies the VM booted or that the QEMU/BOSH agent is reachable. The documented emptyvm
pre-start NATS hazard (agent dead from a long synchronous apt wedging the pre-start
canary) and the orphan-VM duplicate-IP incident are exactly the class a post-boot health
gate would surface earlier with actionable diagnostics. AWS babysits with
`wait_until_running`; Azure captures boot diagnostics/serial console. vSphere adds a related
signal worth noting: its `VMPowerOnError` exposes an `unplaceable?` predicate (true for a DRS
rule violation or generic DRS fault) so the creator can tell a wrong-placement failure from a
transient one — a distinction PVE's health gate could eventually borrow.

**Build:** after the start-task await, config-gated poll
`nodes/{node}/qemu/{vmid}/agent/ping` until ready or deadline; on failure scrape
`qm status` and the last serial lines via the API and fold them into the error before
rollback. Reuse the task-waiter poll-fault classification.

**Shipped.** Opt-in via `health_check.enabled`: after the start task, `waitUntilAgentReady`
(`create_vm.go` ~2300-2380) polls `CreateQemuAgentPing` on an interval of
`max(config, 1s)` until `health_check.timeout_sec`, retrying transient ping errors and
failing fast on permanent ones (4xx/auth); both the deadline and parent-context cancel are
checked each iteration. On timeout it calls `gatherHealthDiagnostics`
(`ListQemuStatusCurrent`) and folds the result into the error before the standard
`create_vm` rollback. **Why it matters here.** It stops `create_vm` from returning an IP and
a "ready" signal for a VM whose agent is wedged from a long synchronous apt — the emptyvm
pre-start NATS hazard — surfacing it with diagnostics at create time instead of as a later
canary failure. **Limits (verified this round).** Diagnostics are gathered only on the
failure path (no success-path boot metrics), and the 1s poll-interval floor (`health_seam.go`)
is hard-coded with no operator override.

#### 7.13 DONE — Stemcell provenance metadata and orphaned-template GC

*References: vSphere, AWS, Azure.* PVE encodes name + version in the template name and a
sha8 content tag, but does not store full BOSH stemcell metadata (version, os_type,
source, owning director) as queryable PVE notes/tags, and there is no sweep for
templates orphaned by an interrupted `create_stemcell` or for stale per-node replicas
(after §7.2). As cluster density grows — multiple directors, light + heavy + per-node
replicas — provenance and cruft become operability problems; templates also consume the
9000–29999 VMID range. The documented orphan-VM and wedged-task incidents show this
cluster accumulates cruft from interrupted operations. Two references model the ends of
this design. Azure tracks a comma-separated `stemcell_references` tag — multiple BOSH CIDs
share one gallery image, `delete_stemcell` removes only the named reference, and the image
is deleted only when the CSV empties (ref-counting). vSphere instead registers a vCenter
Extension and stamps every CPI-managed VM and template with `managed_by`, so the platform's
own UI can group and filter CPI-owned resources
(`lib/cloud/vsphere/cpi_extension.rb:1-70`).

**Build:** at template finalization, set the notes field to a JSON provenance block and
add stable `bosh-stemcell*` tags (reusing the tag-sanitization path). In
`delete_stemcell`, best-effort remove all sha-tag matches across nodes (covers §7.2
replicas); add an opt-in prune of `bosh-stemcell`-tagged templates with no referencing
clones (cross-check `/cluster/resources`). Best-effort, config-gated, warn-never-fail.

**Shipped.** At template finalization, when `pve.stemcell.provenance` is enabled the CPI
stamps the template Notes field with a JSON provenance block containing name, version,
os_type, disk_format, sha8, source, director_id, and created timestamp, and adds stable
tags: a bare `bosh-stemcell` marker plus `bosh-stemcell-name-<v>`,
`bosh-stemcell-version-<v>`, and `director--<id>` (sanitized), alongside the existing
`bosh-stemcell-sha-<sha8>` content tag. Both the primary template and per-node replica
templates (§7.2) receive the same stamps. The feature is off by default; with it unset,
template config is byte-identical to prior releases. The `bosh-stemcell` marker plus
`director--<id>` tag give PVE the resource-ownership grouping vSphere gets from its
managed-by extension, expressed in PVE-native tags rather than a platform extension object.

`delete_stemcell` now always performs a best-effort cross-node sweep: it resolves the
stemcell sha8 from the primary template, then deletes every template across the cluster
carrying `bosh-stemcell-sha-<sha8>` (discovered via `/cluster/resources`), covering
replicas created by §7.2. Errors are warned, never fatal. PVE keys this sweep on content
sha rather than Azure's per-CID reference count: identical-content stemcells already
collapse to one template by the sha8 tag, so the cluster never holds the duplicate
references that Azure's CSV ref-counting is built to track.

Opt-in orphan pruning is available via `pve.stemcell.prune_orphans` (with
`pve.stemcell.prune_dry_run` for a preview pass). The prune runs as a tail of
`delete_stemcell` and is director-scoped: it requires `pve.stemcell.director_id`,
enumerates all `bosh-stemcell`-tagged templates owned by that director, and attempts
deletion for each. Rather than pre-scanning linked clones, it relies on Proxmox to
atomically refuse removal of a base volume still referenced by a linked clone — such
templates are skipped with a warning. Best-effort, config-gated, warn-never-fail.

**Limits (verified this round).** The orphan prune is *reactive only* — it fires as a tail
of `delete_stemcell` (`delete_stemcell.go:260-340`), never on a schedule, so an interrupted
`create_stemcell` leaves a template until the next delete of a same-content stemcell. Owner
matching is by the `director--<id>` tag, but the tag sanitizer silently strips special
characters (`stemcell_provenance.go`), so a `director_id` containing colons or semicolons is
mangled and the prune assumes the sanitized form — keep director IDs to tag-safe characters.

#### 7.14 DONE — Allowed-address-pairs / VIP ipfilter for in-deployment floating VIPs

*References: OpenStack-Go, AWS, Google, Azure.* Now that the per-VM firewall has shipped,
its default anti-spoof `ipfilter` can silently drop traffic for a floated VIP that is not
the NIC's primary IP — so the firewall feature itself becomes a foot-gun for the classic
on-prem HA-LB pattern. This lab fronts CF with HAProxy and a floating frontend, which is
precisely a VRRP/keepalived VIP case. OpenStack exposes `allowed_address_pairs` for
exactly this: it sets the pairs on the Neutron port at NIC pre-create and re-applies them
idempotently on every re-attach, so a re-bound NIC keeps permitting its VIPs without
operator action.

**Build:** add `cloud_properties.allowed_address_pairs` (or network-level
`vip_addresses`). After the firewall-enable step in `create_vm`, populate the per-NIC
`ipfilter-net{N}` ipset (the ipset API is already exercised by the firewall work) with
the declared VIPs so the anti-spoof filter permits them. Additive, best-effort,
omit-when-empty.

**Shipped.** A per-NIC network cloud property `allowed_address_pairs` (a list of IP or
CIDR strings) now declares the floating VIPs a NIC may source traffic from. When a
firewalled NIC carries this property, `create_vm` seeds the PVE `ipfilter-net{N}` ipset
for every firewalled NIC with that NIC's own primary IP as a `/32` plus the declared VIP
entries, then enables the VM-level firewall and `ipfilter` option in a single call so the
anti-spoof allowlist is actually enforced. Because the PVE `ipfilter` option is VM-wide
and the QEMU ipset is the complete source allowlist (the primary IP is not auto-added),
every firewalled NIC is seeded with its own IP first — so turning on the filter never
locks a NIC out of its own network. Bare IPs are normalized to `/32`; a CIDR entry permits
that whole range as a source, so prefer host `/32` entries unless a subnet is intended.

The feature is opt-in by the presence of the property: with no `allowed_address_pairs` on
any network, `create_vm` makes no firewall-ipset calls and leaves `ipfilter` off, so
behavior is byte-identical to prior releases. Application is best-effort and ordered for
safety: all ipset entries are written before `ipfilter` is enabled, and any PVE API
failure leaves `ipfilter` off and the VM working (logged as a warning) rather than risk an
incomplete allowlist. Two configurations are skipped with a warning instead of enabling a
filter that would break connectivity: a firewalled NIC using DHCP/dynamic addressing (its
runtime IP is unknown at create time) and a firewalled NIC whose static IP does not parse.
Malformed entries are rejected before the VM is created, so a typo fails the deploy fast
rather than silently leaving an unprotected VIP. Unlike OpenStack, which re-applies the
pairs to the Neutron port on every re-attach, PVE writes the ipset at `create_vm` only — the
property is a create-time NIC declaration rather than a per-attach reconciliation.

Operator note for VRRP/keepalived: the PVE per-NIC `macfilter` (on by default) blocks the
RFC-3768 virtual MAC, so run keepalived without `use_vmac` — ARP and VRRP advertisements
then use the NIC's real MAC and pass the filter, while `allowed_address_pairs` permits the
floated IP at layer 3.

**Operator impact.** Net effect: an operator can leave the per-VM anti-spoof firewall enabled on
a HAProxy/keepalived tier and still float a VIP, instead of disabling `ipfilter` cluster-wide to
avoid silently dropping VIP traffic. The fail-open ordering in `applyVIPAllowedAddressPairs`
(`create_vm_vip.go:153`) is what makes this safe to adopt: every firewalled NIC is seeded with
its own `/32` before `ipfilter` is turned on, and any PVE API failure leaves `ipfilter` off with
the VM still reachable — so enabling the allowlist can degrade to no-filter but never to lockout.

### Tier 3 — Integration and hardening

#### 7.15 DONE — Per-operation deadline / timeout envelope

*References: OpenStack-Go, Google.* No `context.WithTimeout`/`WithDeadline` wraps handler
execution (verified). Retry budgets bound individual SDK calls, but a pathological
storage-lock-retries × task-await-polls combination has no single ceiling — this is how
the documented wedged-task incident (an un-cancellable poll holding a director queue slot
forever) escapes. OpenStack-Go has `state_timeout`; Google's waiter caps at ~782s.

**Build:** wrap handler dispatch in `context.WithTimeout` with a per-method-class budget
from config (create 30m, delete 15m, has/get 2m defaults). The existing retry loops
already honor `ctx.Done()`, so this composes cleanly and converts the wedged-poll hang
into a retriable timeout the director can act on.

**Shipped.** Opt-in via the `operation_timeout` block: `WithMethodTimeouts`
(`dispatcher.go:219-225`, resolver `:356-377`, wired at `cmd/cpi/main.go:207-222`)
classifies each method into create/delete/query/default budget classes and wraps the
handler context in `context.WithTimeout`; when the deadline fires *and* the handler returned
an error, the dispatcher converts it to a retriable `Timeout` `CloudError`. Parent-context
cancellation (process shutdown) is explicitly excluded so a clean shutdown is not reported
as a per-op overrun. **Why it matters here.** A pathological storage-lock-retries ×
task-await-polls combination — the wedged-task incident — gets a single ceiling and becomes
a retriable timeout instead of a poll holding a director queue slot forever; in a
draining-node scenario the `create_vm` timeout fires and the director retries elsewhere.
PVE's per-method-class budget (create 30m / delete 15m / has-get 2m defaults) is
finer-grained than OpenStack-Go's single global `state_timeout`, which applies one coarse
ceiling to every operation regardless of cost. **Limits (verified this round).** The budget
is method-class *global* (all `create_vm` share one timeout, no per-stemcell override); inner
retry loops (`AllocateWithRetry`) are not deadline-aware; and a nil-error success is returned
even if the deadline just fired — the envelope only acts on a non-nil error.

#### 7.16 DONE — PVE pushback handling (429 / lock-storm / bounded in-flight)

*References: Azure, AWS, Alicloud.* PVE classifies 5xx as retriable but has no explicit
handling of PVE's own pushback — pvedaemon/pveproxy worker saturation, `/cluster/tasks`
queue pressure, pmxcfs lock contention. Parallel BOSH deploys can saturate the small
fixed per-node worker pool, and the CPI itself becomes the cause of the storm.

**Build:** classify HTTP 429 and PVE "worker busy" / "lock-acquire timeout" strings as a
distinct retriable subtype with longer backoff than generic 5xx; add a client-side
bounded semaphore over outstanding mutating calls per node (reuse the bounded-goroutine
pattern from the IP-conflict scan); bound the poll duration and surface a retriable error
rather than holding a queue slot (reinforces §7.15).

**Shipped.** HTTP 429 and the conservative pushback phrase set (worker busy, lock-acquire
timeout, too many requests) are classified as a distinct retriable subtype that backs off
on a longer curve (5s base, 60s cap) than the generic transient (1s/15s) and storage-lock
(2s/30s) curves. The curve is folded into the central retry helpers, so every mutating call
site inherits it, and is tunable through `pve.retry.pushback.{base_ms,cap_ms}`. An opt-in
`pve.max_inflight_per_node` caps concurrent mutating operations per node through a
per-node semaphore across create_vm, delete_vm, create_disk, attach_disk, and
create_stemcell; zero (the default) means unlimited and is byte-identical to prior
releases. A semaphore-wait cancellation returns a retriable error rather than a fatal one.
The task poller's empty-exit-on-timeout path now returns a retriable error so a wedged
await re-queues instead of holding a Director slot. The only always-on change is the 429
reclassification (previously a fatal 4xx); everything else is opt-in.

**Limits (verified this round).** Detection is text-pattern plus HTTP-429 only
(`error_map.go:439-474`, `IsPVEPushback`): a 429 returned for a non-rate-limit reason (a
custom middleware, say) is still treated as pushback and backed off, and the plain-text
phrase set is matched against task output, so a PVE error-wording change requires updating
the phrase list. The in-flight cap (`inflight.go`) gates at placement-decision time via a
per-node semaphore, *not* at every API call — so the placement scan itself (§7.1 scorer +
§7.5 IP probe) runs outside the cap and is the first thing to saturate the cluster under a
large parallel deploy (see new gaps §7.28 and §7.30).

**Benefit.** PVE runs a small, fixed per-node pveproxy/pvedaemon worker pool, so a wide parallel
BOSH deploy can make the CPI itself the source of the storm — saturating workers, backing up
`/cluster/tasks`, and contending on pmxcfs locks until calls fail. The opt-in
`max_inflight_per_node` semaphore (`inflight.go`) lets an operator cap concurrent mutating calls
per node so the CPI throttles itself rather than overrunning the cluster, and the 5s/60s pushback
curve (longer than the 1s/15s transient curve) spaces retries out instead of hammering an
already-saturated node. The reference CPIs read a server-directed signal PVE does not emit: Azure
paces its async polling by the response's `Retry-After` header, and Alicloud assigns each
operation class its own retry budget (`ServiceUnavailable`, `OperationConflict`, and
`InternalError` each retry 60× at 5s). PVE has no equivalent header, so
the 5s/60s curve plus the semaphore are the substitute. A limit of 0 is unlimited and
byte-identical, so the throttle is purely additive insurance for high-fan-out deploys.

#### 7.17 DONE — Fail-fast config validation (schema strictness)

*References: vSphere, OpenStack-Go, Azure.* `ApplyDefaults` is permissive — no rejection
of unknown keys, contradictory combinations, or out-of-range values. A typo'd
`storage_pool` or invalid `network_mode` surfaces only mid-deploy at the first PVE API
call. The CPI now has many interacting optional blocks (placement, anti-affinity, DLB,
hooks, SDN, firewall, registry TLS) with cross-field constraints enforced ad hoc.

**Build:** add a `Validate()` phase after `ApplyDefaults`: reject unknown top-level keys,
enforce enums, enforce documented cross-field rules (HA anti-affinity requires
anti-affinity enabled; DLB shared-storage requirement only with DLB enabled; SDN mode
requires a zone), and validate ranges (non-overlapping VMID ranges, weights ≥ 0).

**Shipped.** Enum validation and range validation (non-overlapping VMID bands, weights ≥ 0,
backoff bounds) were already enforced unconditionally. The remaining strictness is gated
behind an opt-in `pve.strict_config_validation` flag so existing manifests keep loading
unchanged by default. When enabled, the validator rejects unknown top-level keys and three
documented cross-field contradictions: `use_ha_rules` without anti-affinity enabled,
`network_mode: sdn` without a zone or `sdn_auto_manage_zone`, and a DLB
`require_shared_storage` setting while the dynamic load balancer is disabled. Each rejection
names the offending key or combination so the failure is actionable at start-up rather than
mid-deploy. Unknown-key checking is top-level only, matching the build scope.

**Why it matters here.** The CPI now has many interacting optional blocks (placement,
anti-affinity, DLB, hooks, SDN, firewall, registry TLS) whose contradictions previously
surfaced only at the first PVE API call mid-deploy; failing at start-up with an
accumulated, semicolon-delimited list of every violation (`config.go:1744-1767`) cuts the
MTTR for a misconfigured cluster. PVE's unknown-key rejection is the strictest in the
reference set: vSphere alone ships a declarative schema (Membrane's `SchemaParser` DSL), yet
even Membrane does not reject unknown top-level keys — it passes them through as
`dict(String, any)` — and OpenStack-Go silently drops unrecognized keys rather than failing.
None of the six references rejects an unknown key; PVE's opt-in strict mode does. **Limits.**
Strict mode is opt-in (default off for backward compatibility); unknown-key detection is
top-level only, because `cloud_properties` are intentionally free-form maps the CPI does not
prescribe.

#### 7.18 DONE — Static-IP-in-range validation and gateway/DNS propagation audit

*References: AWS, vSphere, Google, OpenStack-Go.* The network spec's
range/gateway/netmask are captured but not consumed; a BOSH static IP outside the
declared range surfaces only as a non-booting VM. Reference CPIs fail fast on such
inconsistencies. Google also injects DNS via metadata; PVE parses DNS but it is unconfirmed
that it reaches the guest.

**Build:** when a static IP is present, parse range + netmask into a CIDR and verify
containment; return an error listing the offending IP + range if outside. Separately
audit ipconfig assembly to confirm gateway + DNS from the network spec reach the guest;
add if missing. Additive, no behavior change for already-correct manifests.

**Shipped.** The network spec now decodes its `range`, and create_vm validates containment
before any VM is allocated: a manual network whose static IP falls outside its declared
range CIDR fails with a non-retriable error naming the network, IP, and range, so a bad
manifest fails fast instead of producing a non-booting VM. Validation is skipped when the
range is absent, the network is dynamic, or no IP is set, so correct manifests are
unaffected. The ipconfig audit confirmed gateway and DNS nameservers already reach the
guest; the one gap — `searchdomain` was never set — is closed by propagating a search
domain from the network's `search_domain`/`dns_search`/`domain` cloud property when present
(omitted otherwise, byte-identical). A manual network with a static IP but no gateway is
logged as a warning rather than silently accepted.

**Why it matters here.** A BOSH static IP outside its declared subnet otherwise produces a
silently non-booting VM discovered only at the canary; `validateNetworkContainment`
(`create_vm.go:3385-3418`) turns that into an actionable pre-allocation error naming the
network, IP, and range, and the search-domain injection closes the DNS path for agents that
reference hosts by domain rather than bare IP. AWS performs the analogous check at a coarser
grain — its `NicGroup` validates that every NIC in a group resolves to subnets in the same
AZ before launch — whereas PVE's containment check works per static IP against its declared
range. **Limits.** Containment is an `IP ∈ CIDR` check only — it does not confirm the netmask
and gateway are mutually consistent with the range — and is skipped entirely when no range is
declared (dynamic networks).

#### 7.19 DONE — External LB registration hook and expanded hook catalog with rollback contract

*References: vSphere, AWS, OpenStack-Go, Google, Azure, Alicloud.* All six reference CPIs
register VMs into an LB at create and deregister at delete. PVE has no native LB
primitive, so a verbatim port is wrong-layer — but the hook middleware was built for
exactly this and ships with only audit logging. The lab literally fronts Cloud Foundry
with HAProxy; today adding or removing a CF router instance requires editing HAProxy out
of band. Separately, the hook framework has no post-hook failure recovery (hooks observe
only), and vSphere deletes the VM and re-raises on a post-create hook failure to prevent
orphans — and the documented orphan-VM-duplicate-IP incident shows orphans are a real
class here.

**Build:** (1) add a rollback contract — when a `create_vm` post-hook errors, the
dispatcher invokes the existing delete cleanup before propagating (middleware-level, not
per-handler); (2) add useful built-in hooks on PVE primitives — an `lb_register` hook
calling the HAProxy Data Plane API (REST add/remove-server, no config-file editing,
best-effort), a notes-audit hook writing deploy/job/index into guest Notes for PVE-UI
visibility, and an allowlisted `external_command` hook (the right vehicle for the
not-recommended VIP/LB/IPAM items); (3) keep the static registry + config-name validation.

**Shipped.** The dispatch middleware now carries a rollback contract: a `create_vm`
post-hook that turns success into a failure triggers cleanup of the just-created VM before
the error propagates, closing the orphan window the audit-only hook framework left open.
Cleanup is a stack — each participant registers its own undo — so a post-hook failure
unwinds both the VM teardown and any LB registration. The rollback path is panic-safe and
fires at most once, never double-cleaning with the handler's own failure defer. Three
built-in hooks ship: `notes_audit` writes the BOSH deploy identity into the guest Notes for
PVE-UI visibility; `lb_register` registers and deregisters the VM in an HAProxy backend via
the Data Plane API (best-effort, with a dial-time private-IP guard and redirect blocking,
and a deregister-on-rollback so a rolled-back create never leaves a stale backend entry);
and `external_command` runs an operator-allowlisted host command with no shell, an
absolute-path allowlist resolved through symlinks, a scrubbed environment, a timeout, and
process-group kill. Each hook's config is validated at start-up (an active `lb_register`
requires an endpoint and backend; an active `external_command` requires a non-empty
absolute-path allowlist containing the command). All hooks are opt-in and omitted from the
rendered config when unset, so an existing manifest is byte-identical.

**Why it matters here.** The lab fronts Cloud Foundry with HAProxy, so adding or removing a
router instance previously meant editing HAProxy out of band; the `lb_register` hook does it
transparently through the Data Plane API, and the rollback contract
(`middleware.go:62-92`) closes the orphan-VM window the audit-only framework left open by
unwinding the VM and any LB registration (LIFO) when a post-`create_vm` hook turns success
into failure. The references span the full LB-lifecycle spectrum: AWS and Azure register at
create but never deregister at delete; Alicloud deregisters from both SLB and NLB server
groups on `delete_vm`; OpenStack records LBaaS pool membership as VM metadata so a later
`delete_vm` can deregister durably even without a prior rollback; and vSphere runs
declarative pre/post `cpi_plugins`. PVE's hook framework with its rollback contract sits
closest to the vSphere model. **Limits.** Every hook except the rollback contract is
best-effort (failures are logged, not propagated); `external_command` is synchronous with no
shell or stdin (quick actions only); and the catalog is static — hooks are registered from
config names at start-up, with no dynamic discovery. Like AWS and Azure, the `lb_register`
hook keeps no durable record of registrations outside the rollback path, so a `delete_vm`
that was never preceded by a rollback relies on the operator's own LB cleanup.

#### 7.20 DONE — Keep-failed-VM diagnostic mode

*Reference: Azure.* `create_vm` always rolls back, correct for production but destructive
of the evidence operators need to debug the wedged-pre-start and dead-agent failures this
project repeatedly hits. Azure's `keep_failed_vms` leaves the VM intact with a
resource-listing error for post-mortem.

**Build:** gate the rollback cleanup on `debug.keep_failed_vms`; when set, tag the VM
(`bosh-create-failed` plus director/deployment/job via the existing tag sync) and return
an error naming VMID + node instead of destroying it.

**Shipped.** An opt-in `debug.keep_failed_vms` flag gates the post-clone rollback. When set,
a VM that fails mid-creation is preserved instead of destroyed: it is tagged
`bosh-create-failed` plus the deployment and instance group derived from the BOSH env
(merged into the VM's existing tags rather than overwriting them), and `create_vm` returns a
non-retriable error naming the VMID and node — non-retriable so the director does not
re-create and strand a second failed VM. Only the final post-clone rollback is gated; the
per-attempt cleanups inside the VMID-allocation retry loop still destroy their throwaway
attempts, so the mode preserves exactly one VM. The gate also honors the post-hook rollback
and panic paths, and it composes with the LB hook (a preserved-but-failed VM is still pulled
from the load balancer so it stops receiving traffic). Default off — byte-identical to prior
releases.

**Why it matters here.** The agent-dead and pre-start-wedged failures this project hits
repeatedly are exactly the ones whose evidence the default rollback destroys; preserving the
VM (`create_vm.go:3176-3226`) lets an operator read its serial console, `qm status`, and
logs post-mortem, and the non-retriable error stops the director from re-creating and
stranding a second failed VM. PVE matches Azure's named `keep_failed_vms` flag directly;
Alicloud reaches the same forensic goal from the disk side with a per-disk
`delete_with_instance: false` that retains an ephemeral volume for inspection after the VM is
gone. **Limits.** It preserves only the final post-clone attempt (VMID-allocation retries
still clean up their throwaways), tagging is best-effort, and there is no automated reaper —
preserved VMs accumulate until an operator deletes them.

#### 7.21 DONE — Positive node-affinity HA pin (durable AZ guarantee)

*References: vSphere, Google.* Only negative resource-affinity is built; the AZ map pins
placement at create time only and does not survive HA failover or DLB rebalance — a VM
scored into an AZ at birth can later migrate out. PVE 9.x HA also offers node-affinity
(pin to a node set), unused by the CPI. Relevant for licensed-host or storage-tier
locality deployments needing a durable AZ guarantee.

**Build:** when the AZ map is set and `placement.pin_az_via_ha_rules` is enabled, after
scoring create a PVE HA node-affinity rule binding the VM to the AZ node set, reusing the
existing HA-rule plumbing; self-clean on delete; reject the node-affinity + DLB-sentinel-AZ
combination at config-validate time (DLB intentionally un-pins).

**Shipped.** When `placement.pin_az_via_ha_rules` is enabled and an AZ map is set, create_vm
writes a per-VM PVE HA node-affinity rule (`bosh-na-<vmid>`, distinct from the anti-affinity
`bosh-aa-` rules) binding the VM to its AZ's node set after scoring, so the AZ placement
survives HA failover and DLB rebalance. The pinned AZ is derived from the node the scorer
actually chose, so it works for both the singular `availability_zone` and the plural
`availability_zones` forms. Strictness is operator-selectable via `placement.pin_az_strict`
(default true — a hard AZ guarantee that keeps the VM on its node set even under total
node-set failure; set false for a preferred pin that lets HA relocate off-AZ). The rule and
its HA resource self-clean on delete and on any create_vm rollback. Config validation
rejects the pin without an AZ map and the pin combined with the DLB sentinel AZ (DLB
intentionally un-pins), and the runtime additionally skips pinning for the sentinel AZ.
Best-effort and non-fatal: a pin failure is logged, never failing create_vm, since the VM is
already on a correct AZ node from scoring. Default off — byte-identical when unset. Live PVE
9 validation of the `node-affinity` rule type is pending.

**Limits (verified this round).** Because the pin is best-effort and fail-open
(`placement_nodeaffinity.go:80-118`), a transient HA-API outage at create time leaves the VM
*unpinned* — placement still put it on a correct AZ node, but the durable guarantee is
silently absent until the next create that re-asserts it. If the AZ map changes between the
scoring read and the pin write (rare, dynamic config), the rule's node set may not match the
node actually chosen.

**Operator impact.** Without the pin, the AZ map fixes placement only at create time, so a VM
scored into an AZ at birth can later migrate out under HA failover or a DLB rebalance — breaking
deployments that depend on durable AZ locality, such as a licensed-host pool whose vendor terms
bind software to specific nodes, or a storage-tier affinity where a guest must stay on nodes
wired to its NVMe pool. The node-affinity rule (`placement_nodeaffinity.go:86-118`, written via
PVE 9.x HA `node-affinity`) makes that AZ binding survive HA and DLB, and `pin_az_strict` lets
the operator choose a hard guarantee (the VM stays on its node set even under total node-set
failure) versus a preferred pin (HA may relocate off-AZ to keep the VM running). vSphere reaches
the same locality goal through a different door: `vm_type.disable_drs` acquires a
`DISABLE_DRS_LOCK`, clears the cluster rule's `enabled` flag, and pins the VM to whatever host it
booted on, complemented by explicit VM/host-affinity rules. PVE's node-affinity rule expresses
the binding directly rather than disabling rebalancing wholesale. Because it reuses the existing
HA-rule plumbing and is default-off, the guarantee is opt-in and byte-identical when unset.

#### 7.22 DONE — Agent registry-less auto-selection and settings-completeness assertion

*References: AWS, OpenStack-Go, Google, Alicloud.* All three agent modes ship and exceed
vSphere, but mode selection is config-driven, not auto-negotiated from the stemcell's
advertised `api_version` (the reference Go CPIs gate registry-less on
`stemcell_api_version ≥ 2`), and there is no assertion that a v2 stemcell's configdrive
payload carries every setting the agent needs. With configdrive now the default for all
deployments, a mis-paired mode or a missing field silently mis-bootstraps — the same
agent-dead/pre-start-NATS failure class already documented.

**Shipped.** A new `agent_mode: auto` reads the stemcell `api_version` from the create_vm
request context (previously discarded) and selects registry-less configdrive for v2+ and
registry for v1. At boot the configdrive agent is always built, and a registry agent is
built too when `registry_endpoint` is set; a v1 stemcell with no registry agent fails
early with a clear error rather than mis-bootstrapping. A missing `api_version` fails open
to configdrive, the modern default. The api_version parse accepts float, integer, and
numeric-string forms so a v1 stemcell is never silently misread. A settings-completeness
assertion now guards the registry-less path: a configdrive boot with an empty mbus or
agent_id, or no networks, fails `create_vm` immediately instead of booting a
half-configured agent. The registry and noagent paths are unchanged, and the explicit
`cloudinit`, `registry`, and `noagent` modes keep their existing behavior. Covered by a
v1/v2 × mode test matrix.

**Why it matters here.** With configdrive now the default for every deployment, a
mis-paired mode or a missing settings field silently mis-bootstraps the agent — the same
agent-dead / pre-start-NATS failure class documented elsewhere; auto-selection plus the
completeness assertion (`internal/agent/settings.go:40-88`) surface that as a clean
`create_vm` failure at create time rather than as a task-join hang. The references converge
on the same registry-less default but reach it through different side-channels: AWS gates
registry calls in its CloudV2 path on `stemcell_api_version < 2`, Google stores
`bosh_settings` in GCE instance metadata, Alicloud writes the full settings JSON into ECS
`UserData`, and OpenStack-Go gates registry-less behind its dual-API-version check. PVE's
auto-selection reads the same `api_version` signal those CPIs use. **Limits.** The MBus
assertion is non-retriable (a present blobstore with an empty mbus fails the create
outright), and MBus derivation inspects only the blobstore endpoint host, so a blobstore
whose credentials live elsewhere can still trip it.

#### 7.23 DONE — Placement-failure retryability signaling

*References: vSphere, AWS, Google.* A hard placement failure (no viable candidate after
filter) surfaces as a non-retriable error. With strict HA anti-affinity on a small
cluster, no node may momentarily satisfy all constraints — but that impossibility is
usually transient (a node frees up, a drain completes). Signaling it non-retryable makes
the director give up instead of backing off, harming the small-cluster CF deploys this
project runs.

**Build:** when the filter returns empty due to a transiently-clearable cause
(maintenance, capacity, or strict anti-affinity) return a retriable error with the
rejection map in the message; keep a permanent error for permanent causes (e.g. a
misconfigured AZ name). This is the placement slice of §7.11.

**Shipped.** `classifyFilterResult` (`create_vm.go:1361-1393`) inspects the scorer's
rejection reasons; `isTransientRejectionReason` whitelists self-healing causes (node
offline, node in maintenance, insufficient CPU/memory, not in candidate set). When every
node is rejected and all reasons are transient, an exhausted run returns `cpierrors.Retriable`
(`create_vm.go:1031-1044`); any permanent reason (e.g. a misconfigured AZ name) returns
`cpierrors.Cloud`. **Why it matters here.** On a small cluster with strict HA anti-affinity,
no node may *momentarily* satisfy all constraints even though the situation clears in
seconds as a node frees up or a drain completes; signaling that retriable keeps the director
backing off rather than giving up and failing the small-cluster CF deploys this project
runs. The two AWS references that motivate this fail in the opposite direction from each
other: a spot-bid placement that cannot be satisfied falls back to on-demand
(`spot_ondemand_fallback`), while a mid-flight `AbruptlyTerminated` triggers a fixed
two-attempt create retry. PVE classifies the *cause* of the empty-candidate set instead of
counting attempts. **Limits (verified this round).** Classification is heuristic — a
rejection string not in the whitelist is conservatively treated as permanent; there is no
internal timeout, so retries continue for as long as the director (or the §7.15 envelope)
allows.

#### 7.24 DONE — Stemcell-driven root/ephemeral disk sizing and dedicated ephemeral pool

*References: Azure, OpenStack-Go, Google, AWS, Alicloud.* PVE clones the template disk
as-is and the agent carves an ephemeral partition from the grown root — functional, but
operators cannot request a larger root or place ephemeral on a separate (e.g.
fast-volatile local-NVMe) pool to keep churn off Ceph. Reference CPIs size root/ephemeral
from stemcell + cloud_properties and offer a separate ephemeral disk.

**Shipped.** The root-disk resize now reads the actual template disk size from the VM
config after clone and grows by the correct delta, fixing a latent bug where the delta was
computed against a hardcoded 5 GiB assumption (wrong for any other template size). The
requested size comes from a new `root_disk_size` cloud_property (the existing `disk` key
still works); a request smaller than the template is rejected with a clear error instead
of being silently ignored. A transient failure to read the template size surfaces as a
retriable error rather than fabricating a wrong delta. Separately, setting
`ephemeral_disk_size_mb` creates a dedicated ephemeral disk on a `ephemeral_storage_pool`
resolved through the layered cloud_properties resolver (falling back to the VM storage),
attaches it on the next free scsi slot (never scsi0, which would collide with the virtio
root), and surfaces its stable by-id device path to the agent as `disks.ephemeral`. A
created volume is cleaned up if the follow-on attach fails. Both knobs are opt-in: with
neither set, the proven grow-root-then-agent-carves-ephemeral path is byte-identical to
before.

**Operator impact.** On a Ceph-backed cluster the single shared root disk forces all ephemeral
churn (logs, compilation scratch, blobstore spill) onto the same replicated RBD pool, competing
with persistent I/O and amplifying write traffic ~3x across the cluster network. Pointing
`ephemeral_storage_pool` at a local-NVMe `dir`/`lvm` storage keeps that volatile churn node-local
and off Ceph, while the corrected delta math (reading the post-clone size from `QEMU().Config`)
means a template that is not 5 GiB no longer gets mis-grown. The references derive ephemeral
sizing from an abstract resource spec the operator never hand-sizes: AWS's `InstanceTypeMapper`
returns an `ephemeral_disk: {size}` alongside the chosen instance type, and Google derives the
local-SSD *count* from the machine type's vCPU count against GCE-mandated thresholds (for the n2
series, 1/2/4/8/16 SSDs at 1/12/22/42/82 vCPUs). PVE takes an explicit `ephemeral_disk_size_mb`
and pool instead. **Limits.** The dedicated ephemeral disk is local to its node, so it is not part
of any fault-domain or live-migration set — a guest with a local-NVMe ephemeral disk cannot be
migrated off its node without dropping that disk.

#### 7.25 DONE — Configurable per-method retry/backoff policy

*References: OpenStack-Go, Alicloud, Azure, Google.* Two hard-coded backoff classes
exist; only the VMID-alloc attempt count is tunable. PVE serializes storage imports under
cluster locks, and lock-timeout windows differ wildly between a three-node lab and a
large Ceph cluster — the documented cold-e2e and CF-deploy work shows storage-lock
serialization is a real lever. Operator-tunable retry lets ops widen the import budget on
slow Ceph without recompiling.

**Build:** add a retry config block (`retry.{default,storage_import,vmid_alloc,task_poll}.{max_attempts,base_ms,cap_ms,jitter_pct}`)
wired into the existing backoff seam (the curves already exist; this parameterizes them).
Keep current values as defaults — additive and behavior-preserving when unset.

**Shipped.** `EffectiveRetryPolicy` (`internal/config/config.go:1543-1603`) exposes
`RetryStorageImport`, `RetryVMIDAlloc`, `RetryTaskPoll`, and `RetryPushback`, each reading a
`retry.<method>` block (`base_ms`/`cap_ms`/`max_attempts`/`jitter_pct`) and falling back to
the shipped defaults. They drive three curves in `internal/pve/retry.go` —
`StorageLockBackoff` (2s×1.5^n, ±30% jitter, 30s cap), `TransientBackoff` (1s×1.5^n, ±30%,
15s cap), and `PushbackBackoff` (5s/60s default) — selected by the `RetryOn*` helpers.
**Why it matters here.** Storage-lock serialization is a measured lever in the cold-e2e and
CF-deploy work; an operator can widen the import budget for a slow Ceph cluster, or tighten
it for a fast lab, without recompiling. The references split between fixed and operator-facing
policy: Alicloud bakes a per-error-class budget into the binary (10 retries at 15s for
`IncorrectDiskStatus.Initializing` on disk delete, 60 at 5s for `OperationConflict`), while
OpenStack-Go exposes the curve as a `retry_config` map the operator can tune. PVE follows the
operator-facing model. **Limits.** Config is read once at startup (no live re-tune);
`max_attempts ≤ 0` silently reverts to the built-in default; curves are pure and
deterministic except for seeded jitter.

### Tier 4 — Optional / deployment-driven

- **Multi-host PVE API endpoint failover** *(Azure, Alicloud, AWS)* — the PVE endpoint
  *is* one of your hypervisors, so a single-host config makes one node a SPOF for all
  BOSH ops even though the cluster API answers on every node. Accept `hosts: [...]` with
  round-robin/failover, optionally discovering peers from `/cluster/status`. Tier 4 only
  because the lab runs on one host today; this rises in priority the moment a second node
  carries production load.

- **Richer scheduling/perf cloud_properties** *(Azure, Google, AWS)* — per-VM CPU
  type/flags, NUMA topology, ballooning, `cpuunits`/`cpulimit`. On a shared cluster these
  are the real defenses against a noisy neighbor. Resolve through §7.8; PVE supports all
  natively; omit-when-unset.

- **Metadata-encoded snapshot description** *(AWS, OpenStack-Go, Google, Azure)* — set
  the PVE snapshot description from BOSH deployment/job/index for forensic traceability.
  Cosmetic; the CID already round-trips.

- **Multi-NIC grouping** *(AWS, vSphere)* — multiple logical networks onto one vNIC.
  Deferred again: no concrete use case (CF and the director use one network per NIC).

- **Per-create_vm root-disk storage-tier override** *(OpenStack-Go, Azure)* — pass
  `--storage` to `qm clone` from `cloud_properties.root_storage_pool`. Niche; clone-to-same
  works for the common case.

- **Out-of-band telemetry hook** *(Azure)* — a metrics hook on the existing middleware
  chain emitting method + duration + outcome. Low value: the audit-log hook already
  carries the signal for a log pipeline.

### Newly identified gaps (this round)

These ten did not appear in the prior report. They surfaced from the deeper reference
re-read and the source-level verification of the shipped features above, and **all ten
(§7.26–§7.35) have since been shipped** (the six gaps surfaced this round, §7.36–§7.41 below,
have since been shipped as well).
Each follows the same additive-optional convention the shipped work established: validate only
when set, omit from VM config when empty, zero behavior change for existing manifests. They are
ordered roughly by
effort-to-value, not severity.

#### 7.26 DONE — Enforce creation-time disk-performance invariants on re-attach

*References: Azure, AWS.* §7.9 bakes `cache`/`iothread`/`ssd`/bus into the disk-CID
metadata and merges global `disk_performance` defaults at `attach_disk` time — but nothing
*rejected* drift. If the global config changed between create and a later re-attach, the disk
silently came back with a different cache mode than its create-time CID records, so its
runtime profile diverged from its recorded one. Recording an invariant is worthless if no
code path enforces it. Azure makes this explicit: `update_disk` rejects a caching-mode, size-shrink,
or storage-tier change as `NotSupported` (these are creation-time-only), and AWS waits out a
volume modification before treating it as applied.

**Shipped.** `enforceDiskPerfInvariants` (`internal/cpi/handlers/attach_disk.go`) runs in
`HandleAttachDisk` after the §7.9 option merge and before any mutating PVE call, so a reject
never orphans. The pure `diskPerfInvariantViolations` (`disk_performance.go`) compares the
structural options `{cache, iothread, ssd}` recorded in the CID against the effective merged
options; throttle knobs (`mbps_*`, `iops_*`) and `discard` are never enforced. Because the
§7.9 merge already pins CID-recorded values (per-disk wins over global), the only divergence
that fires in practice is global config introducing a structural option the disk lacked at
creation. The `disk_perf_invariant_mode` knob governs the response: `enforce` (default)
rejects with a non-retriable `CloudError`, `warn` logs and proceeds, `off` skips. A disk
whose CID carries no performance options is skipped entirely, so behavior is unchanged for
those disks regardless of mode.

**Operator impact.** This catches a silent runtime-profile drift PVE itself never flags: changing
the global `disk_performance` cache mode (e.g. `none` → `writeback`) and then redeploying would
otherwise re-attach an existing persistent disk with write-back host caching its create-time CID
never recorded, changing crash-consistency semantics under the operator's feet. The `enforce`
default surfaces that as a non-retriable create-time failure naming the diverging keys, so the
operator aligns config or consciously opts out via `warn`/`off` rather than discovering it after a
power-loss event. Azure encodes the same rule on the platform side — caching is part of the
`DiskId` CID, and `update_disk` refuses to change it as `NotSupported` — so PVE's enforce-on-attach
check is the analogue for a platform that has no such server-side guard. **Limits.** Only the three
structural keys (`diskPerfInvariantKeys`, `disk_performance.go:194`) are enforced — throttle and
discard drift is deliberately allowed since PVE can change those on a live device — and because the
§7.9 merge pins CID-recorded values, the only divergence that ever fires in practice is a global
config newly introducing a structural option the disk lacked at creation.

#### 7.27 DONE — Disk-resize completion monitoring

*Reference: AWS.* `resize_disk` issued the PVE resize and returned immediately. On
asynchronous backends (Ceph RBD, LVM-thin) the agent could read the *old* size, or a
subsequent operation could race the still-in-flight resize. §7.24 fixed the sizing *math*
(delta against the real template size) but not post-resize *convergence*. AWS's
`ResourceWait.for_volume_modification` waits for the EBS modification to reach
`completed`/`optimizing` before returning.

**Shipped.** `waitForResizeConvergence` (`internal/cpi/handlers/resize_disk.go`) polls
`QEMU().Config` and re-parses the disk size (`parseDiskSizeGiB`) after the resize task
completes, until the reported size reaches the target. It is **best-effort**: the helper is
void and never returns an error, so a slow or non-converging backend cannot fail the
resize — on timeout it logs a warning and the handler returns success. The whole step is
opt-in via `resize_wait_for_convergence` (default off → zero extra calls, byte-identical) and
bounded by `resize_convergence_timeout_sec` (default 120s), an independent budget so the poll
is bounded even when the §7.15 operation-timeout envelope is disabled.

**Operator impact.** On PVE's async storage backends (Ceph RBD, LVM-thin) the `qm resize` task
can return OK before the new size is visible to the guest; without this wait a follow-on
`resize_disk` or an agent that reads the old size can race the still-in-flight grow. Enabling the
wait makes `resize_disk` return only once `QEMU().Config` reports the disk at or above target, so
a subsequent BOSH disk operation observes a settled size. AWS solves the same race on the modify
side, polling `for_volume_modification` to a terminal state; the OpenStack-Go CPI takes the
complementary stance of rejecting a shrink outright rather than waiting for one to converge. PVE
grows only and waits for the grow to settle. **Limits.** It is strictly best-effort and
non-failing by design — the helper is void and on timeout merely logs a warning and returns
success, so a backend that genuinely never converges within `resize_convergence_timeout_sec` is
indistinguishable from one that converged late; this guard tightens the common case but cannot
turn a stuck backend into a hard error.

#### 7.28 DONE — Progress-aware adaptive task-poll interval

*References: vSphere, Alicloud.* §7.25 uses fixed per-method backoff curves. PVE UPID tasks
expose a `progress` field for long operations (clone, move-disk), which the poller ignored.
A fixed curve polls too often early — adding to the §7.16 pushback pressure that §11 flags
as the unquantified scale risk — and too slowly late. vSphere derives its interval from the
ETA: `(elapsed·100/progress − elapsed)/5`, clamped 1–10s.

**Shipped.** When `task_poll_adaptive` is enabled, `AwaitTask` (`internal/pve/task.go`)
routes to the CPI-owned `awaitTaskAdaptive` loop, so no call site changes. `adaptiveTaskInterval`
applies the vSphere estimator — projected remaining time over five — clamped to 1–10s, and
falls back to the fixed §7.25 cadence when `progress` is absent or non-positive (so
progress-less short tasks poll exactly as before). The loop reads progress through a new
single-shot `tasks.GetStatus` added to the vendored `pve-apiclient-go` SDK alongside a
`Status.Progress` field (both additive), and mirrors `AwaitTask`'s terminal/error
classification via `classifyTaskExit`. Disabled (default) the SDK's fixed-interval `Wait` is
used, byte-identical to prior releases. A warning header in the vendored file and a
compile-time assertion in `internal/pve/task.go` guard against a `go mod vendor` refresh
silently dropping the addition.

**Operator impact.** On a busy cluster the fixed curve hammers
`/nodes/{node}/tasks/{upid}/status` during the slow early phase of a long clone or move-disk,
adding to the per-node API pressure §7.16 pushback and §11 flag as the unquantified scale risk;
the adaptive interval (`adaptiveTaskInterval`, `task.go`) stretches polling toward 10s while a
clone is barely progressing and tightens it as the task nears completion, cutting wasted API
round-trips on parallel deploys without lengthening tail latency. PVE adopts the vSphere
ETA-proportional formula directly (remaining time projected from elapsed-over-progress, divided
by five and clamped); Azure reaches the same end by reading the server's `Retry-After` header on
each async-operation poll. PVE computes the interval locally because the PVE API emits no such
header. **Limits.** The benefit only materializes for PVE operations that actually populate the
UPID `progress` field (clone, move-disk) — short or progress-less tasks fall back to the exact
§7.25 fixed cadence — and the feature depends on a vendored-SDK addition (`Status.Progress` +
single-shot `GetStatus`), guarded only by a compile-time assertion that breaks the build, rather
than silently degrading, if a vendor refresh drops it.

#### 7.29 DONE — Boot-path agent integrity / checksum verification

*Reference: the Ruby OpenStack CPI (mechanism PVE-originated).* §7.12 pings the guest agent
and §7.6 verifies the *stemcell* digest (post-import, per its own caveat). Neither verified
that the BOSH **agent binary** inside the booted guest is the expected one — a tampered or
partially-written agent passed both checks. The Ruby OpenStack CPI injects an expected agent
checksum into the configdrive for boot-time self-verification (the OpenStack **Go** CPI in this
reference set injects no agent checksum into its user-data, so PVE's approach below is its own).

**Shipped.** PVE exposes a guest-agent exec API, so the CPI verifies directly rather than
relying on an agent-side self-check. When `health_check.expected_agent_sha256` is set (and the
health gate is enabled), `assertAgentChecksum` (`internal/cpi/handlers/create_vm_agent_checksum.go`)
runs `sha256sum /var/vcap/bosh/bin/bosh-agent` via `CreateQemuAgentExec` after the §7.12 ping
succeeds and compares the digest. A **confirmed mismatch** fails `create_vm` with a
non-retriable `CloudError`, triggering the existing rollback that destroys the VM. Every other
outcome — guest-agent exec error, non-zero `sha256sum` exit, unparseable output, or an exec
that does not finish within the bound — is **fail-open** (warns and proceeds), so the check
never blocks provisioning when it cannot positively confirm tampering. The command is a fixed
argument vector (no shell), and the assertion is skipped when the digest is unset, so behavior
is unchanged for existing deployments.

**Benefit.** Because PVE gives the CPI a guest-agent exec channel (`CreateQemuAgentExec`,
`internal/cpi/handlers/create_vm_agent_checksum.go:87`), the integrity check runs from the
*control plane* against the *running guest* — it does not trust an agent-side self-report. This
closes the window the §7.12 ping leaves open: an agent that answers the ping is reachable but may
still be the wrong binary (a partial qcow2 write, a tampered template, a stemcell rebuilt with a
different agent). A confirmed mismatch destroys the VM before BOSH hands it real work, so a
compromised or corrupt agent never enters a deployment. The reference set verifies integrity
elsewhere in the pipeline: Google passes a `raw_disk_sha1` to its image-import API for server-side
verification of the *stemcell* image, but the OpenStack-Go CPI injects no agent checksum into
user-data at all — so PVE's control-plane exec-and-compare is its own mechanism, not a port. The
pin is a single SHA-256 string the operator updates whenever the expected agent changes, and it is
a no-op until set.

#### 7.30 DONE — PVE API client connection-pool / keepalive tuning

*Reference: Alicloud.* §11 flags the placement fan-out (the scorer plus the IP scan,
which run on *every* `create_vm`) as unmeasured and interacting with §7.16 pushback. The
SDK's `http.Transport` tuning was previously unspecified, so under a parallel deploy each
scan call could be a fresh TLS handshake, amplifying both load and latency. Five transport
fields are now configurable via CPI config and wired into the SDK `Options` struct at
`NewClient` time: `pve_api_dial_timeout_sec` (TCP dial timeout), `pve_api_tls_handshake_timeout_sec`
(TLS handshake timeout), `pve_api_max_idle_conns_per_host` (idle connection pool cap),
`pve_api_idle_conn_timeout_sec` (idle connection eviction), and `pve_api_tcp_keepalive_sec`
(TCP keepalive probe interval). All five default to 0, which is the SDK no-op sentinel —
behavior is byte-identical to prior releases when unset. Requires upstream SDK v3.2.7, which
adds the five `Options` fields. Pairs with §7.28 (adaptive task polling) as the two
pre-timing-pass scale mitigations. Validated ≥ 0 at config load; negative values are
rejected. Spec properties added under `pve.api_*`; ERB emits each key only when non-zero.

**Operator impact.** The five knobs (`pve/client.go:125-129`) matter most on a large multi-node
cluster where the §7.11 scorer plus the static-IP scan fire on every `create_vm`: without a warm
idle pool each scan re-runs the full TCP+TLS handshake against the PVE API, and a slow or
saturated `pveproxy` then shows up as create latency rather than a clear failure. Raising
`pve_api_max_idle_conns_per_host` lets parallel deploys reuse keep-alive connections instead of
stampeding fresh handshakes, while the dial and handshake timeouts convert a wedged API endpoint
into a bounded, retriable error instead of a long hang. The two reference points bracket the
design space. Alicloud is the positive model: it sets a large `MaxIdleConns` (500) plus an
explicit `TLSHandshakeTimeout` on its transport, exactly the pooling PVE now exposes. OpenStack-Go
is the cautionary one: it parses a `ConnectionOptions` block that is wired nowhere
(`config.go:51`, dead), and it re-authenticates to Keystone on every service build — four-plus
auth round-trips per `create_vm` — so its connection cost is structural rather than tunable. PVE's
ticket reuse plus this pool sidesteps both failure modes. Defaults are the SDK no-op, so a
single-node lab needs none of this — it is a scale-out tuning surface, not a required setting.

#### 7.31 DONE — Post-selection fallback placement on transient create/start failure

*References: vSphere, AWS.* §7.10 ships multi-AZ candidate fallback at *placement-decision*
time (pre-selection). §7.31 is the complementary *post-selection* analogue: once a node is
chosen and the clone or start fails transiently (ephemeral storage briefly full, a node
momentarily wedged), the CPI retries on the next-ranked candidate rather than surfacing the
failure immediately.

**Shipped:** opt-in `pve.placement.fallback_max` integer (default 0, byte-identical to prior
releases). When set to a positive value (valid range 1–5; recommended 2):

- On a transient clone or VM start failure, the partial VM is cleaned up — purged from PVE
  and its VMID freed — before the next attempt begins.
- The next-ranked candidate is drawn from the same constrained scored set that produced the
  original winner: AZ node restriction and disk fault-domain co-location constraints are
  preserved across all attempts.
- The transient/permanent classification reuses the shared error classifier: a clone failure
  falls back when it is `IsTransientTransport` or `IsStorageLockTimeout`; a start failure falls
  back when it is `IsTransientTransport`. VMID conflicts are not a fallback trigger — they are
  resolved on the same node by the allocate-with-retry loop, which regenerates the VMID (see
  §7.33), so they never surface to the candidate loop. Permanent errors (missing clone source,
  non-retriable PVE errors) fail immediately without consuming alternates.
- At most `1 + fallback_max` total attempts are made; 0 means the feature is fully off.

This is the post-selection analogue of the §7.10 pre-selection multi-AZ fallback, mirroring two
references that retry placement after the chosen target fails: vSphere pre-computes fallback
datastores during selection and walks to the next one on a `GenericVmConfigFault` without
re-running its full placement pipeline, and AWS retries the create loop up to twice on
`AbruptlyTerminated` (and falls back from spot to on-demand capacity). Alicloud takes the same
persistence to the delete side with a 10-retry delete loop after a post-create failure. PVE walks
its own scored candidate list, preserving every placement constraint across attempts.

#### 7.32 DONE — Fast-path delete (tag-and-return without terminal-state poll)

*Reference: AWS.* `delete_vm`/`delete_disk` always wait for the resource to disappear. The
documented wedged-task incident — a `get_task` poll that never returns, holding a director
queue slot — is the cost of synchronously waiting on a pathological task. AWS's
`fast_path_delete` tags the resource and returns immediately.

**Shipped:** opt-in `pve.fast_path_delete` boolean (default false, nil-absent → byte-identical
to prior releases). When enabled:

- `delete_vm` stamps the PVE tag `bosh-deleting` on the VM before issuing the destroy
  (best-effort, fail-open: a tag write failure is logged and never blocks the destroy). It then
  issues the stop fire-and-forget (the stop UPID is discarded — no await, so a wedged stop task
  cannot hold a director queue slot) and issues the destroy (`DeleteQemu` with `purge=true`,
  `destroy_unreferenced_disks=true`, `skiplock=true` so a still-running or locked VM is removed),
  returning immediately without calling `AwaitTaskWithLogger`. Existing BOSH-managed tags are
  preserved via `mergeTagList`.
- `delete_disk` issues `DeleteVolumeAsync` and returns without awaiting the imgdel UPID. Disk
  volumes cannot carry PVE tags, so no `bosh-deleting` marker is applied; the operator relies
  on the volume eventually disappearing from storage.
- Idempotency is preserved in both modes: a 404 on the destroy or delete call returns success
  without error.

**Eventual-consistency tradeoff:** a subsequent `has_vm` or `has_disk` call may briefly still
see the resource until PVE's async destroy completes. A stalled async destroy is reaped by
`sweepFastDeleteStragglers`, which runs at the start of every fast-path `delete_vm`: it scans the
cluster for VMs still carrying `bosh-deleting` and re-issues a `skiplock`+`purge` destroy for
each (best-effort, fire-and-forget, idempotent on 404). The `bosh-deleting` tag therefore acts as
a self-draining work queue, so leftover VMs do not accumulate across deployments. (This is a
fast-delete-specific sweep, distinct from the §7.13 orphan-GC, which keys on stemcell sha and
director-scoped base volumes rather than this tag.) The fast path bypasses the §7.15
operation-timeout envelope naturally: the handler returns before any task-poll loop begins, so
the two are complementary fixes — bound the wait (§7.15) and skip the wait (§7.32) cover
different operations. AWS pairs the same `fast_path_delete` tag-and-return behavior with eventual
cleanup; Google exposes the equivalent as a `CPI_ASYNC_DELETE` toggle that fires the delete and
returns without polling. PVE adds the `bosh-deleting` sweep so the skipped waits still converge.

#### 7.33 DONE — Articulate the idempotency-collision model

*References: Alicloud (regenerate identity), AWS/Azure (retry same identity).* PVE's
VMID collision model is **regenerate-identity**: when `AllocateWithRetry` receives a
conflict error ("VMID already in use"), it discards the conflicted VMID and calls
`NextVMID` again to obtain a fresh one. The conflicted VMID is never presented to a
second create attempt. This is correct because a VMID conflict on PVE means the numeric
identity is already occupied by a live VM — retrying the same VMID would loop forever.

This contrasts with same-token-retry clouds: AWS `ClientToken` and Azure
`x-ms-client-request-id` signal "this request is in flight", not "this identity is taken".
Those clouds must retry the same token; generating a new token would create a duplicate
resource. Even the regenerate camp splits on detail: Alicloud regenerates its `ClientToken`
only on a specific `IdempotentFailed` response, whereas AWS and Azure retry the same token
unconditionally. Google sits in a third position — it carries no creation token at all, using
a metadata fingerprint as a compare-and-swap (409 on a stale fingerprint) for label and
settings writes. The classification rule is: regenerate when the collision means *taken*;
retry the same identity when it means *in flight*. PVE's model falls firmly in the first
category, and PVE's API offers no server-side dedup token of its own, so the CPI must
implement the regenerate loop itself.

The §7.23 error classifier already treats "vm already exists" / HTTP 409 as a conflict
that routes to `AllocateWithRetry`, not to a same-VMID retry path. This model is stated
explicitly in the `AllocateWithRetry` doc comment and locked by
`TestAllocateWithRetry_RegeneratesDistinctVMID`, which asserts that every VMID across
all conflict-retry attempts is distinct.

**Benefit.** The regenerate model is what makes `create_vm` safe under PVE's specific identity
scheme: a VMID is a cluster-wide *numeric* identity assigned at create time, not a client-supplied
idempotency token, so a 409 ("VMID already in use") always means that integer is now occupied — by
a concurrent deploy, a manually created VM, or a prior partial create. Retrying the *same* VMID
would 409 forever and wedge the deploy; regenerating via `NextVMID` lets the allocate loop step
past the taken integer and converge. For the operator this means parallel `bosh deploy` runs
against one PVE cluster do not deadlock on VMID contention — each loser simply picks the next free
number — which is the property `TestAllocateWithRetry_RegeneratesDistinctVMID` locks in.

#### 7.34 DONE — Network-property override precedence (VM props override network defaults)

*Reference: Google.* Shipped in `create_vm.go` (`createVMCloudProps.NetworkDefaults`,
`configureNICs`). A VM `cloud_properties.network_defaults` map is the final override layer
for per-NIC attributes. Precedence (highest first):

```
network_defaults[key]  >  per-NIC spec.cloud_properties[key]  >  resolver default
```

Supported keys: `bridge` (string), `model` (string), `firewall` (bool). Unknown keys are
ignored gracefully — cloud_properties are loosely typed and not subject to
`strict_config_validation`. Absent map or absent key leaves behavior byte-identical to
pre-feature state. Extends the §7.8 layered resolver model to network attributes without
changing any resolver, config, spec, or ERB file — `network_defaults` is a call-time
cloud_property, not CPI config.

**Operator impact.** The override layer earns its keep when the BOSH network's per-NIC defaults do
not match the target PVE host — e.g. a deployment whose cloud-config names one `bridge`, but a
particular instance group must land on a different Linux bridge or SDN vnet, or must flip the PVE
per-NIC `firewall` flag on without re-templating the whole network. Setting `network_defaults` on
the VM's `cloud_properties` retargets just those NICs at deploy time, with no edit to shared
cloud-config, CPI config, or the spec/ERB. Because it is the highest-precedence layer, it is also
the escape hatch for one-off placement onto a bridge the resolver would not otherwise pick. Google
establishes the same VM-props-win-over-network precedence; OpenStack-Go applies the equivalent
ordering to security groups, resolving VM over network over global. PVE's resolver chain follows
that established precedence.

#### 7.35 DONE — Bounded-concurrency parallel stemcell replication across cluster nodes

*Reference: Alicloud (parallel-upload pattern).* The original analysis framed this as
multipart S3 upload; on closer inspection the stemcell upload path targets the PVE
single-POST storage endpoint — there is no S3 stemcell path and multipart chunking is not
applicable. The real performance long-pole is the serial per-node replication loop in
`replicateStemcellToNodes`: when `stemcell_replicate_local` is true, the CPI uploads the
qcow2 and builds a template VM on each cluster node one at a time.

**Shipped:** `stemcell_replication_concurrency` (default 0 → serial, up to 64). The serial
`for` loop is refactored into a bounded worker pool (`sem chan struct{}` + `sync.WaitGroup`):
up to `stemcell_replication_concurrency` goroutines run concurrently, each owning one node's
full upload+ensureTemplate sequence. With the default (0/absent → 1), behavior is
byte-identical to prior releases. With N > 1, total replication time on a K-node cluster
scales to approximately `ceil(K/N)` upload round-trips instead of K.

**Concurrency safety:** `uploadStemcellImage` opens its own file handle per goroutine (no
shared `*os.File`); `deps.Logger.With(...)` returns a new zap logger per node (zap is
concurrency-safe); `inflightSems.acquire` is guarded by `sync.Mutex` internally; VMID
allocation uses `AllocateWithRetry` (regenerates on conflict) so parallel goroutines on
different nodes do not collide. Per-node best-effort semantics (idempotent skip, non-fatal
failure) are preserved identically in both serial and parallel modes. `go test -race` is
clean. The pattern mirrors Alicloud's bounded parallel upload, which streams a stemcell image
to its intermediate OSS bucket in 5 MB parts across five goroutines (`oss.Routines(5)`); PVE
applies the same bounded-worker discipline at the per-node granularity its single-POST
endpoint dictates rather than at the chunk granularity.

### Newly identified gaps (latest round)

These six gaps did not appear in the prior report; they surfaced from a deeper read of all six
reference CPIs against the now-shipped §7.1–§7.35 work. None duplicates an existing item; each maps
to a real PVE primitive. They cluster around one theme the earlier rounds under-weighted —
*cross-process and in-flight-operation safety* — which §8 names directly. Only the platforms
that expose *non-atomic shared configuration* need an explicit cross-process lock. vSphere (DRS rules),
Azure (availability sets and shared image galleries), and PVE (pmxcfs-replicated HA rules) all sit in
that camp and all reach for a mutex. The four public-cloud CPIs — AWS, Google, OpenStack-Go, and
Alicloud — do not; they lean on provider-atomic create, compare-and-swap fingerprints, or idempotency
tokens instead. PVE resembles vSphere and Azure here, not the hyperscalers, and the gaps below reflect
that. Each follows the same additive-optional convention: validate only when set, omit from VM config
when empty, and change nothing for existing manifests. Ordered by value, not severity.

#### 7.36 DONE — Cross-process cluster mutex for HA-rule and anti-affinity mutation

*References: vSphere, Azure.* vSphere serializes every DRS-group and anti-affinity reconfiguration
behind a platform-native distributed mutex. `DrsLock#with_drs_lock` creates a vCenter custom field by
name and relies on vCenter raising `DuplicateName` if it already exists — a create-as-compare-and-swap
on the names `drs_lock`, `host_vm_group`, and `DISABLE_DRS_LOCK` — polling every 0.5 s for up to 600 s
and releasing by deleting the field (`drs_rules/drs_lock.rb:16-58`). Azure takes the same posture with
an OS advisory `flock` on `/tmp/azure_cpi/<lock>` (EX, SH, or NB) around availability-set
get-or-create, gallery-image create and update, and user-image create (`helpers.rb:564-575`,
`vm_manager_availability_set.rb:35,65`, `compute_gallery_manager.rb:149,262,270,340`). Both treat
shared cluster-config edits as a critical section across concurrent CPI processes.

The shipped PVE HA work (§7.21 node-affinity pin, dual anti-affinity rules) mutates cluster-wide
`/etc/pve` HA state with an unguarded read-modify-write: `placement_antiaffinity.go` lists HA rules,
deletes the group rule, then re-registers it, and `placement_nodeaffinity.go` likewise lists then
rewrites the pin. No cross-process lock guards this path. The only mutexes in the tree
(`dispatcher.mu`, `rollback.mu`, `inflightSems`) are in-process, and `max_inflight_per_node` (§7.16)
is a per-process semaphore. Under a parallel deploy the BOSH director runs many
`create_vm`/`delete_vm` invocations as **separate processes**, each racing the same `bosh-aa-<group>`
rule: process A reads the member set, process B reads the same stale set, both delete and recreate, and
the last writer wins — silently dropping a VM from its negative-affinity rule. pmxcfs is a replicated
filesystem with last-write-wins semantics, so this is a real data race on safety-critical placement
state, not a theoretical one. The lab notes already record a pmxcfs race and a duplicate-stemcell-template
class of bug.

**Build:** add an opt-in `pve.cluster_lock_mode` (`off` default → byte-identical; `flock` to enable).
When enabled, wrap HA-rule and anti-affinity mutation (`ensureAntiAffinityRule`, `ensureNodeAffinityPin`,
and their delete-side cleanup) in a cluster-wide critical section keyed on a stable lock name (for
example `bosh-cpi-ha`). The natural PVE primitive is a create-or-fail sentinel under `/cluster`: take a
config object whose creation PVE rejects with a conflict if it already exists (mirroring vCenter's
`DuplicateName`), poll-acquire with jittered backoff up to a bounded `cluster_lock_timeout_sec`
(default ~60 s), and release on a `defer`. Because pmxcfs serializes config writes cluster-wide, an
alternative is a lock file under `/etc/pve` taken via the API with a TTL-stamped owner token so a
crashed holder self-expires. Keep the unguarded path as the default so existing deployments are
unchanged.

**Shipped.** Both mechanisms landed, both opt-in and byte-identical when unset. A pool-sentinel cluster
lock (`internal/pve/cluster_lock.go`: `AcquireClusterLock`/`Release` over POST/DELETE/GET `/pools`,
poolid `bosh-lock-<name>`, comment `owner=<token> exp=<unix>`) wraps the per-group anti-affinity
read-modify-write. Create-or-fail is the test-and-set; a duplicate whose recorded expiry has passed is
stolen (delete then recreate) with a post-steal owner re-read that refuses a displaced handle; release
is deferred so it fires even on a mid-RMW error. The matcher is **fail-closed** — an error that cannot
be positively classified as a duplicate maps to a retriable acquire failure rather than a wrong
assumption that the lock is held. A read-after-write **verify** re-lists the rule and asserts the VMID
is present; an absent member returns `TypeRetriableCloud`. Both the anti-affinity and node-affinity
create_vm call sites propagate that retriable class to the director — selectively, since a generic
HA-API blip stays fail-open per §7.21 — so the spread or pin guarantee is re-driven rather than
silently lost. Knobs: `pve.cluster_lock_mode` (`off`|`pool`), `pve.cluster_lock_timeout_sec`,
`pve.antiaffinity_verify`. The node-affinity pin is per-VMID (no cross-group RMW), so it takes the
verify but intentionally skips the coarse lock. *Live-validation caveat:* the exact PVE status and text
for a duplicate poolid and the comment round-trip are inferred from the API shape and pmxcfs
serialization; unit tests assert the contract against a fake `PoolService`, and a true multi-process
race must be validated on a live cluster.

#### 7.37 DONE — Adopt-and-wait on a racing concurrent template clone (clone-target-exists)

*References: vSphere, Azure.* vSphere's `Stemcell#replicate` clones a per-datastore replica and, when a
parallel CPI is already replicating the same stemcell, catches the resulting `DuplicateName`, then
resolves the in-progress replica by `find_by_inventory_path` and blocks until its snapshot property
appears rather than erroring (`stemcell.rb:90-100`). This is the exact adopt-and-wait model: catch the
collision, find the winner's artifact, wait for it to become usable. Azure's `_get_user_image` takes an
EX `flock` around **both** the existence GET and the create specifically so process 2 never VM-creates
against an image process 1 is still building (`stemcell_manager2.rb:138-169`). Both convert a
concurrent-create collision into a safe wait on the winner's artifact.

This is distinct from the shipped §7.35, which parallelizes one CPI's own replication loop — §7.35
governs intra-process concurrency, not the cross-process race. On PVE, when two independent `create_vm`
invocations target a node lacking the stemcell replica, both consult the per-node replica tag
(`ResolveTemplateVMIDForNode`, `template.go:441`), both find it absent, and both issue a clone for the
same template onto the same node. The second clone hits a VMID conflict, and today the create_vm
allocation loop treats VMID conflicts as retriable jitter — it allocates a *fresh* VMID and clones
again, producing a duplicate, half-built replica template instead of waiting for the winner. The
orphan-template and duplicate-stemcell-template hazards in the lab notes are this failure mode exactly.

**Build:** in the replica-ensure path (`ensureTemplate` and the per-node replica build consumed by the
scorer), when a clone or template-create returns a target-exists conflict for the *replica name or tag
we were about to create* — as opposed to an unrelated guest VMID collision — branch to adopt-and-wait:
poll for the per-node replica tag (`bosh-stemcell-node-<node>` plus `bosh-stemcell-sha-<sha8>`) to
appear and the template to leave clone-in-progress state, bounded by `replica_adopt_timeout_sec`
(default ~300 s), then return the adopted replica. Distinguish a replica-name collision (adopt) from a
guest VMID collision (the existing retry-jitter path) so create_vm's allocation loop is unchanged. With
a single CPI process the conflict never fires, so behavior is byte-identical; the new code only changes
the multi-process race outcome from "duplicate orphan template" to "wait for the winner."

**Shipped.** PVE allocates a fresh VMID for every clone, so a duplicate-replica collision is invisible
at the VMID-allocation layer — two losers pick different VMIDs and both succeed — and the
Azure/vSphere catch-`DuplicateName` hook has no PVE analogue. Instead the in-flight winner is observed
directly. A replica VM carries its identity tags (`bosh-stemcell-sha-<sha8>` plus
`bosh-stemcell-node-<node>`) from creation, but `Template` flips true only after the freeze, and the
guest config `lock` reads `clone` or `create` while the build is in flight. A new primitive
`pve.AdoptReplicaTemplate` (`internal/pve/replica_adopt.go`) scans for that mid-build VM via
`findReplicaCandidate` — which, unlike the settled-only `ResolveTemplateVMIDForNode`, does *not* require
the `Template` flag — and polls it to a settled template (frozen and unlocked), bounded by
`pve.replica_adopt_timeout_sec`. The scan prefers a settled candidate over any lower-VMID unsettled
orphan, so a crashed-mid-build remnant cannot shadow a genuine adoptable template. The probe is wired
into the per-node replica build (`replicateOneNode`, `create_stemcell.go`) **before** the qcow2 upload:
on adoption the node skips upload and clone entirely (no duplicate, no orphaned upload); a winner that
never settles within the bound yields a `TypeRetriableCloud` that the best-effort replication loop logs
and skips (re-driven next deploy). Distinguishing a replica collision from a guest-VMID collision is
structural — the adopt probe keys on the replica tag set, while the create_vm allocation loop's
VMID-conflict jitter is untouched. The knob defaults to 0 (disabled): the probe call site is skipped
entirely, so single-process and pre-existing behavior is byte-identical. The residual sub-second TOCTOU
window between a not-found probe and the caller's own clone is shrunk but not eliminated — no
cross-process lock is taken on this path, though the optional `cluster_lock_mode=pool` primitive from
§7.36 could close it.

*Live-validation caveat:* the `clone`/`create` lock strings and the list endpoint's per-VM `lock` and
`tags` fields are exercised against a fake PVE client asserting the contract; a true multi-process
replica race and a stuck-lock winner (which requires operator `qm unlock` recovery, noted in the spec)
need a live cluster to validate.

#### 7.38 DONE — Pre-delete lock/status guard on `delete_disk` against in-flight volume operations

*References: Google, OpenStack.* Google's disk delete refuses when `disk.Status` is neither `READY` nor
`FAILED`, returning an error rather than racing a `CREATING` or `RESTORING` disk
(`google/disk/google_disk_service_delete.go:19-21`). OpenStack's `delete_disk` rejects unless the
volume status is `available`, 404-skips an already-gone volume, then waits for terminal `deleted`
(`delete_disk.go:42-66`). AWS takes the same precaution asymmetrically: `delete_ebs_volume` applies a
linear backoff (1, 6, 11, 15, 15 s) on `VolumeInUse` to ride out post-detach consistency lag before
freeing the volume. All three gate destruction on the resource being quiescent.

PVE's `delete_disk` has no such precondition: `HandleDeleteDisk` goes straight to the `imgdel` call
wrapped only in `RetryOnTransientOrLock` (`delete_disk.go:94`). Retry-on-lock recovers from a
*transient* storage lock, but it does not guard against freeing a volume whose owning VM is
mid-operation — a `qm clone`, a `qm disk move` or `storage migrate`, a backup, or a snapshot rollback
that holds the VM `lock` config field. Freeing the backing image out from under such an operation can
corrupt the in-flight task or leave storage inconsistent. PVE exposes the signal directly: the owning
guest's config carries a `lock` field (`backup`, `clone`, `migrate`, `snapshot`, `rollback`).

**Build:** add an opt-in `pve.disk_delete_state_guard` (`off` default → byte-identical; `on` to
enable). When enabled and the volume resolves to an owning VM, read that VM's config `lock` field
before freeing; if it is set to a destructive or in-flight value, treat it as *retriable* so the
director re-drives the delete after the lock clears (mirroring §7.27's convergence posture), or
fail-fast with a clear non-retriable error. Keep the existing 404-idempotent skip. With the guard off,
current behavior is preserved exactly.

**Shipped.** Opt-in `pve.disk_delete_state_guard` (`off` default → byte-identical; `on` to enable).
When enabled, `HandleDeleteDisk` runs `pve.GuardDiskDeleteState` between node resolution and the
`imgdel` call. The critical design point: the VMID baked into a managed volid name
(`<storage>:vm-<VMID>-disk-<N>`) is only the allocation-time placeholder this CPI assigns at
`create_disk` — BOSH attaches the volume to a different guest without renaming it, so the guard resolves
the *currently-attached* VM by scanning VM configs for the volid (`FindVMByDiskVolid`), **not** by
parsing the name. It then reads that VM's `lock` config field; a destructive or in-flight value
(`backup`, `clone`, `migrate`, `snapshot`, `rollback`, `create`) yields a `TypeRetriableCloud` error so
the director re-drives the delete after the lock clears (mirroring §7.27's convergence posture). The
guard is best-effort and fails open on every uncertainty: a disk attached to no VM (the normal
pre-delete state), an attachment-resolution failure, a config-read error, or a 404 all pass straight
through, so an enabled guard can never convert a hiccup into a delete failure. The existing
404-idempotent skip and the `RetryOnTransientOrLock`-wrapped `imgdel` are unchanged; with the guard off
(the default) no attachment lookup runs and behavior is byte-identical. Residual: the check-then-delete
window is inherently best-effort (a lock taken between the guard read and the `imgdel` is not caught),
and if the attached VM is left with a stuck config lock the delete defers until an operator runs
`qm unlock <vmid>` — the deferral log names the VM, node, and lock. Live-validation caveat: exercised
against fakes asserting the resolution and lock-classification contract; the real
attached-VM-mid-operation race needs a live PVE cluster.

#### 7.39 DONE — Eventual-consistency retry resolving a freshly-created SDN vnet/bridge

*Reference: vSphere.* This gap has no public-cloud analogue. Cloud networks are synchronous and
strongly consistent — a created subnet or security group is queryable everywhere the moment the API
returns — so the cloud CPIs need no convergence wait. vSphere is the one reference that does, because a
vCenter portgroup propagates asynchronously: `find_network_retryably` wraps network lookup in
`Bosh::Retryable` with 62 tries (~10 min) on `NetworkNotFoundError`, explicitly to tolerate the lag
between a portgroup being created and becoming queryable cluster-wide (`vcenter_client.rb:225-239`). The
CPI assumes a freshly-created network is not immediately resolvable on every host and polls until it
converges.

PVE's SDN has the same eventual-consistency property and no corresponding wait. `create_network` stages
a zone, vnet, and subnet and calls `applySDN` to push the config to the data plane
(`create_network.go:266,378`), but returns as soon as the apply task is accepted — it does **not** poll
for the vnet to materialize as a usable bridge on the node where the next `create_vm` lands. On the
consume side, `create_vm` takes the bridge name purely from config or cloud_properties and writes it
into `netN=` with no existence check. SDN apply is asynchronous and per-node (`ifupdown2` reload plus
pmxcfs propagation), so a `create_vm` immediately following a `create_network` on a different node can
attach a NIC to a bridge that does not yet exist there — the VM boots with a dead NIC, or `qm start`
fails, surfacing as a flaky, deploy-order-dependent failure that disappears on retry. This is the lab
reality the SDN-network gap note flags.

**Build:** add an opt-in `pve.network_resolve_retries` (default 0 → byte-identical) and a companion
`network_resolve_timeout_sec`. When set, after `applySDN` succeeds in `create_network`, poll
`/cluster/sdn` (status `available`, no `pending`) and/or `/nodes/{node}/network` until the target vnet
or bridge is resolvable, bounded by the timeout. On the consume side, optionally gate `configureNICs` in
`create_vm` with the same bounded retry resolving the per-NIC bridge before writing `netN=`, classifying
a not-yet-present bridge as retriable so the director re-drives rather than booting a NIC into the void.
With retries at 0 the apply-and-return behavior is unchanged.

**Shipped.** Two opt-in knobs, `network_resolve_retries` (default 0 → byte-identical) and the companion
`network_resolve_timeout_sec` (0 → 60 s), gate both sides via `pve.WaitForSDNVnetConverged` and
`pve.ResolveNodeBridgeOnNode` (`internal/pve/network_resolve.go`), each a bounded poll (retry count plus
absolute timeout) over the shared `lockClock` seam. On the produce side, after `applySDN` succeeds,
`createNetworkSDN` polls the **running** (`pending=false`) cluster SDN vnet list until the new vnet is
committed, returning a retriable error on exhaustion so the director re-drives — the vnet is left in
place (the gate runs after the apply/rollback block, so a convergence wait never tears down a committed
vnet). This confirms cluster **commit**, not per-node realization. On the consume side, `configureNICs`
collects every NIC bridge during the build loop and, before the single `UpdateQemuConfig` write,
resolves each on the **target node** via `nodes.ListNetwork` — so no `netN=` is written for any NIC
until all bridges resolve (no partial config). Only SDN-managed vnets are gated: a bridge that is not a
known SDN vnet (external or static, for example `vmbr0`) passes straight through, and the per-node check
is the authoritative realization signal. The gate is best-effort where it must be — an SDN-membership
lookup failure fails open (never blocks a deploy on the guard's own blip), while a transient
node-network read counts as "not yet present" and keeps polling, ultimately surfacing a retriable error
rather than aborting. A bridge still converging past the budget yields `TypeRetriableCloud`, preserved
through the handler `cpierrors.Wrap`. Validation rejects negative counts; the ERB emits each key only
when set. Live multi-node SDN convergence timing remains to be validated against a real cluster.

#### 7.40 DONE — Ephemeral-disk minimum-size invariant (≥ 2× RAM) on `create_vm`

*Reference: OpenStack.* OpenStack's flavor resolver rejects an `instance_type` whose
`flavor.Ephemeral > 0` but is smaller than `(RAM/1024)*2`, enforcing that the ephemeral disk has
headroom for agent swap plus `/var/vcap/data` (`flavor_resolver.go:78-86`). The invariant encodes a
hard truth: the BOSH agent places swap (sized to RAM) and the data partition on the ephemeral disk, so
an ephemeral disk smaller than ~2× RAM cannot satisfy the agent's own layout. The nearest concept among
the other references is Azure's ephemeral-OS-disk placement, which routes the OS disk onto the
resource/cache disk and so is constrained by that disk's capacity — a related sizing dependency, though
not the same 2× rule.

PVE's §7.24 ephemeral path (`resolveEphemeralShape`, `create_vm.go`) sizes the dedicated ephemeral disk
straight from `ephemeral_disk_size_mb` (rounded up to GiB) and resolves the pool, but asserts
**nothing** about the size relative to VM RAM. An operator who configures a 2 GiB ephemeral disk on an
8 GiB-RAM job gets a VM whose agent cannot lay down its 8 GiB swap file — the agent's ephemeral-disk
setup fails at boot, or swap silently does not activate, producing the same ephemeral-space boot failure
§7.24's root-resize logic already guards against on the *root* disk but not on the *ephemeral* disk.
This is a cheap, high-signal pre-flight invariant the shipped code omits.

**Build:** add an opt-in `pve.ephemeral_disk_min_ratio` (default 0 → no check, byte-identical;
conventional value 2). When set and an ephemeral disk is being created, compute the resolved ephemeral
GiB against the VM's configured memory (already available in the create_vm shape) and, if
`ephemeral_gib < ratio * (memory_mb/1024)`, either fail-fast with a clear non-retriable cloud error
naming the deficit, or warn — operator's choice via an `enforce|warn` knob mirroring §7.26's
`disk_perf_invariant_mode`. This reuses the §7.26 enforce/warn pattern verbatim. With the ratio at 0
nothing changes.

**Shipped.** `create_vm` gained an opt-in `pve.ephemeral_disk_min_ratio` (float, default `0` → no check,
byte-identical) with a companion `pve.ephemeral_disk_min_mode` (`enforce` default | `warn`), mirroring
the §7.26 `disk_perf_invariant_mode` pattern. When the ratio is set and a *dedicated* ephemeral disk is
being provisioned (`ephemeral_disk_size_mb` > 0), `resolveEphemeralShape`'s resolved ephemeral GiB is
checked against the VM's configured RAM as `ephemeral_GiB >= ratio × (memory_MiB / 1024)` — both sides
in binary GiB so the comparison is unit-consistent with the agent's own swap-plus-`/var/vcap/data`
layout, with a `1e-9` epsilon so an exact-boundary disk is never falsely rejected by floating-point
drift. The check is wired into *both* shape builders (`resolveVMShape` and the placement-fallback
`resolveVMShapeWithAlternates`), so the fallback path is gated identically. On violation, `enforce`
returns a non-retriable cloud error naming the deficit (a configuration error, not a transient — it is
never classified retriable, so the director does not re-drive a deploy that can only fail again), while
`warn` logs the deficit and proceeds. The check is skipped entirely when no dedicated ephemeral disk is
requested (the agent then carves ephemeral space from the grown root disk, unchanged). With the ratio at
`0` the create_vm path is byte-identical to prior releases.

#### 7.41 DONE — Secret redaction over the dispatcher request/response log path

*References: AWS, Azure, Google; contrast OpenStack-Go, Alicloud.* Redaction maturity in the reference set is
bimodal, and it tracks team origin rather than language. The hyperscaler-maintained CPIs scrub: AWS
clones and redacts instance params and spot specs (`user_data`, access keys) before logging through
`Bosh::Cpi::Redactor` (`instance_manager.rb:36-41`, `spot_manager.rb:28`); Azure walks the argument
tree against a `CREDENTIAL_KEYWORD_LIST` and suppresses `/listKeys` responses outright; Google wraps
every request, response, and error byte stream in `redactor.RedactSecrets` — a regex over
`account_key`, `json_key`, `password`, and `private_key` — before debug logging at the dispatch boundary
(`api/dispatcher/json.go:70,112,142,161,180`). The two community Go ports redact **nothing**:
OpenStack-Go has no redaction primitive anywhere, and Alicloud logs `AccessKeySecret` and the mbus
password verbatim. PVE's 7.41 places it firmly in the mature tier with AWS, Azure, and Google rather
than with the community ports.

The PVE CPI handles the same sensitive payloads. `create_vm` receives the agent env (an mbus URL with
embedded NATS credentials, blobstore secrets, and a registry endpoint), and the configdrive/cloud-init
user-data is assembled from it. Today the dispatcher logs only `method`, `request_id`, and `duration_ms`
(`dispatcher.go`), not the argument tree, so nothing leaks at the default level — but there is **no
redaction primitive anywhere in the `log`/dispatcher layer**. The CPI is therefore one debug-level `log`
statement (or one well-meaning "log the request to triage a stuck deploy" change) away from writing the
mbus password and blobstore credentials to a log BOSH ships to syslog and the director's debug bundle.
This is a latent hazard, not a live leak — which is exactly when a cheap guardrail is worth adding ahead
of need.

**Build:** add a structured redaction helper in the `log` package and call it at the dispatcher boundary
for any request/response payload logging, gated by an opt-in `pve.redact_logs` (default off →
byte-identical; recommend on). The scrubber masks known-sensitive keys and paths in the CPI argument
tree — `mbus`, `blobstore.options.secret_access_key`/`password`, `registry` credentials, and any
`env`/`agent` settings blob — replacing values with `<redacted>` while preserving structure. No PVE
primitive is required; this is pure log hygiene. Validate-only-when-set, omit from the ERB when empty.

**Shipped.** `internal/log/redact.go` adds `RedactSecrets(tree any) any` — a config-free, deep-copying
scrubber that returns a new tree with every value under a sensitive key replaced by `<redacted>` while
preserving map and slice structure (the input is never mutated, so the live argument tree the handler
goes on to use is untouched). A key is sensitive by case-insensitive substring match (`password`,
`secret`, `token`, `credential`, `mbus`, `private_key`, `access_key`, `api_key`, `authorization` —
catching prefixed variants such as `nats_password` and `client_secret`) or by exact match on
`user`/`username` (exact, so the diagnostic `user_data`/`user_agent` keys are not collateral-masked).
String values are additionally URL-scrubbed: credentials in a `scheme://user:pass@host` userinfo segment
**and** in a sensitive query parameter (`?password=`, `?access_token=` — the query vocabulary is built
from the same fragment list) are masked, including when the URL is whitespace-prefixed or embedded
mid-string. `RedactSecrets` is idempotent. The dispatcher (`internal/cpi/dispatcher.go`) gains a
`WithRequestTrace(bool)` option and, when enabled, emits a debug-level `cpi request` record (the
redacted argument tree, before the handler runs) and a `cpi response` record (the redacted result,
round-tripped through JSON so a typed struct normalizes to the same tree shape) — a malformed argument
or unserializable result logs an opaque placeholder, never raw bytes. The dispatcher takes a plain bool,
so the `log` package does not import `config` and the dispatcher gains no config dependency:
`cmd/cpi/main.go` translates the opt-in `pve.redact_logs` (pointer-typed `*bool`, default off) into
`WithRequestTrace`. With the knob off, both trace helpers early-return before any allocation — logging
is byte-identical to prior releases. Emitted from the ERB only when explicitly true.

### Newly identified gaps (2026-06-05 round)

These fifteen entries record capabilities the reference CPIs ship, drawn from a fresh
six-CPI survey. All but 7.42 (later found already implemented) are OPEN: candidates for
future work, not commitments. The order follows descending PVE relevance, and each
carries a one-line tier hint.

#### 7.42 SHIPPED — Human-readable VM naming

*References: vSphere, OpenStack-Go, Ruby-OpenStack.* The vSphere CPI offers an opt-in
`enable_human_readable_name` that derives a VM name from the BOSH environment — instance
group, deployment, and a UUID suffix, trimmed to a length limit and proportionally
truncated, falling back to the bare UUID when metadata is absent or non-ASCII. On a PVE
cluster the VM identity is a numeric VMID, and the operator-facing `name` field is purely
cosmetic, so without a descriptive `name` a BOSH-managed lab shows rows of
indistinguishable VMIDs in the web UI.

**Status.** This capability predates the 2026-06-05 survey round; the original OPEN
classification was an error. `set_vm_metadata`
(`internal/cpi/handlers/set_vm_metadata.go`, `buildVMName`) stamps the QEMU `name` field
as `<prefix>-<deployment>-<job>-<index>` (e.g. `cpi-cf-api-0`), where the prefix is the
optional `pve.vm_prefix` job property; empty prefix or deployment segments are dropped.
When job or index is missing it falls back to sanitizing the full BOSH instance name
(`<job>/<id>`), and when no source yields a usable DNS label the existing PVE name is
left untouched. Names are sanitized to PVE's DNS-label character set and length cap.

**Deltas vs the vSphere reference.** Naming is always-on rather than gated behind an
opt-in flag (the fallback chain makes it safe by construction), and it lands at
`set_vm_metadata` time rather than `create_vm` time, so a VM briefly shows its bare VMID
between creation and the director's first metadata sync. The name is display-only and
carries no addressing semantics; no CPI lookup or correlation logic reads it. Tier:
operability.

#### 7.43 SHIPPED — Capability-based VM sizing (`vm_resources` / `calculate_vm_cloud_properties`)

*References: AWS, Azure, OpenStack-Go, Google, Alicloud.* Five of the six reference CPIs
implement `calculate_vm_cloud_properties`, the BOSH surface that turns an abstract
`{cpu, ram, ephemeral_disk_size}` request into a concrete machine specification. The AWS
mapper walks a static ordered table of roughly twenty-three instance types and returns the
first that satisfies the request (`lib/cloud/aws/instance_type_mapper.rb:4-46`,
`lib/cloud/aws/cloud_v1.rb:419-434`); Azure queries the live Resource SKU API, caches the
result on disk for twenty-four hours, and selects the smallest satisfying size scored by
series and generation (`lib/cloud/azure/vms/instance_type_mapper.rb:23-61`). This is the
single universal feature PVE lacks: BOSH operators can express sizing in portable terms
everywhere except here, where they must hand-author a `vm_type` per workload.

**Build:** implement `calculate_vm_cloud_properties` by reading per-node capacity from
`GET /nodes/{node}/status` (cores and memory) and producing a `cloud_properties` block with
explicit `cpu`, `memory`, and ephemeral disk values that satisfy the requested minimum. A
static tier table (small, medium, large) keyed off the request, resolved through the §7.8
layered cloud-property resolver, is the lower-risk first cut; a live capacity query is the
richer variant.

**Limits.** PVE has no fixed instance-type catalog, so the CPI must synthesize the mapping
rather than look it up. A live capacity query races against cluster scheduling and is best
treated as advisory, not a reservation. Tier: operability.

**Shipped:** `calculate_vm_cloud_properties` now returns `ephemeral_disk_size_mb` in the
`cloud_properties` response, closing the BOSH `vm_resources` contract. The key name matches
`createVMCloudProps.EphemeralDiskSizeMB` in `create_vm` so that `resolveEphemeralShape`
receives the requested minimum and creates the ephemeral disk; a zero input is omitted,
preserving byte-identical behavior for callers that do not set the field.

#### 7.44 SHIPPED — Concurrency-safe metadata and notes writes

*References: Google.* The Google CPI guards every metadata and label mutation with a
fingerprint read immediately before the write: GCE rejects the write with a 409 if a
concurrent change moved the fingerprint, and the CPI surfaces that as a retriable failure
rather than silently overwriting (`metadata_client.go:147`). PVE offers no such
compare-and-swap. The CPI writes VM tags and a notes-JSON blob (provenance under §7.13,
metadata under `set_vm_metadata`); two processes that read, modify, and write the same notes
field concurrently will lose one of the two updates. This is a genuine data-loss race, and
it also closes the concurrency dimension catalogued in §9.

**Build:** serialize notes and tag mutations per VM. Read the current notes immediately
before writing, merge the new keys into the parsed JSON, and write the merged result under a
per-VM mutex; on a multi-process worker pool, back that mutex with the cross-process cluster
lock already shipped in §7.36 (pmxcfs-backed), keyed by VMID. PVE serializes tasks per VMID,
which narrows but does not close the read-modify-write window, so the explicit
read-before-write merge is still required.

**Shipped:** `withVMIDLock` helper in `handlers/vmid_lock.go` acquires the pmxcfs-backed
cluster lock keyed `bosh-lock-vm-<vmid>` (TTL 30s, timeout 10s) and wraps the tag and
description read-modify-write at four call sites:

- `set_vm_metadata` — tag RMW extracted into `setVMMetadataRMW`; lock failure → retriable error.
- `set_disk_metadata` — both `persistMetadata` and `applyCustomTagsToVM` share a single lock
  acquisition over the case-1 branch so the two writes are serialized together; lock failure → retriable error.
- `tagFailedVM` (in `create_vm`) — lock failure → warn + proceed unlocked (best-effort: failure tag must never be silently dropped).
- `stampDeletingTag` (in `delete_vm`) — same best-effort fallback as `tagFailedVM`.

Two sites are intentionally not locked: `vm_annotator.AnnotateNotes` is a description-only
overwrite with no merged state — a concurrent `set_vm_metadata` can transiently stomp it
under a parallel apply, but no previously-set field is lost because the annotator carries no
prior value to merge. Stemcell provenance is a single-shot write at template creation before
any other process can reference the template VMID.

**Limits.** PVE's task model gives no fingerprint or CAS token, so the guarantee is only as
strong as the CPI's own locking discipline; a write that bypasses the merge path still
clobbers. Tier: correctness.

#### 7.45 SHIPPED — Generic allowlisted VM config passthrough

*References: vSphere.* The vSphere CPI merges a global and per-`vm_type` `vmx_options` hash
into the VM's extra-config, letting operators inject arbitrary low-level settings — for
example `disk.enableUUID=1` for consistent volume UUIDs under multiple SCSI controllers —
without a bespoke CPI change. PVE exposes comparable knobs (machine type, BIOS/firmware,
NUMA topology, raw `args`), but no current path lets an operator set them per VM type.

**Build:** add a `pve_config` (or equivalently named) map to `vm_type` cloud_properties and,
at `create_vm`, apply each key through `pvesh set /nodes/{node}/qemu/{vmid}/config`.
Constrain the accepted keys to a safe allowlist — `machine`, `bios`, `numa`, and a vetted
subset of `args` — to keep the escape hatch from becoming a foot-gun.

**Limits.** A passthrough surface is only as safe as its allowlist; an over-broad list lets
operators write configurations the CPI cannot reason about or roll back. Raw `args` in
particular can break migration and snapshot assumptions. Tier: deployment.

**Shipped:** `pve_config map[string]string` in `vm_type` cloud_properties applies allowlisted
PVE keys (`machine`, `bios`, `cpu`) post-clone in one `UpdateQemuConfig` call. Validation
runs at argument-parse time (before any VM is created): invalid input produces a non-retriable
error with no orphan VM. CPI-managed keys (`cores`, `memory`, `sockets`, `netN`, `scsiN`,
`ideN`, `virtioN`, `boot`, `name`, `tags`, `hotplug`, `numa`, `smbios1`, `agent`, `onboot`,
`tablet`, `vmgenid`, `description`, `ostype`) and `args` (execution surface) are rejected at
that point. Note: `numa` is owned by the hotplug/NUMA resolver and is therefore CPI-managed.
Empty values and values containing shell metacharacters (`;&|$\`<>`) are also rejected
pre-clone. If the post-clone API call fails (transient PVE fault), the candidate VM is
destroyed before the error propagates, matching the cleanup contract of sibling error paths
in the same function. Nil or empty map is byte-identical to prior behavior.

#### 7.46 SHIPPED — CPU and RAM hotplug capability flag

*References: vSphere.* The vSphere CPI wires `cpu_hot_add_enabled` and
`memory_hot_add_enabled` from `vm_type` into the create-time config, allowing online CPU and
memory increases without a reboot. PVE QEMU supports the same through `-hotplug cpu,memory`,
but the CPI never set it, so scaling a CF VM up required a stop-and-restart.

**Build:** add `cpu_hotplug` and `memory_hotplug` booleans to `vm_type` cloud_properties and,
at `create_vm`, pass `hotplug=cpu,memory` (or the requested subset) through the standard VM
config endpoint. No new PVE API is needed; the existing config path carries the field.

**Limits.** Hotplug is a guest-cooperative operation: the guest kernel must support memory
and CPU hot-add and online the new resources, and not every stemcell does. The flag enables
the capability; it does not itself perform a resize. Tier: deployment.

**Shipped:** `cpu_hotplug *bool` and `memory_hotplug *bool` in `vm_type` cloud_properties
merge into the PVE hotplug token string at `create_vm` time via `mergeHotplugToken` in
`create_vm_hotplug.go`. Setting `cpu_hotplug: true` ensures the `cpu` token is present;
`cpu_hotplug: false` removes it. Setting `memory_hotplug: true` ensures the `memory` token
and forces `numa=1` (PVE requires NUMA for memory hotplug to allocate DIMM slots);
`memory_hotplug: false` removes the `memory` token. When `memory_hotplug: true` conflicts
with an explicit `numa: false` in the same cloud_properties, memory hotplug wins and NUMA
is enabled — the CPI documents this override. Unset flags (`nil`) produce output
byte-identical to the pre-feature behavior. No spec or ERB keys are added; these are
`vm_type` cloud_properties only.

#### 7.47 SHIPPED — PCI passthrough and vGPU as cloud_properties

*References: vSphere.* The vSphere CPI accepts `vgpus`, `pci_passthroughs`, and
`device_groups` in `vm_type`, then iterates healthy hosts to find one carrying the requested
device — preferring the placed host — and configures the passthrough devices after the
clone. PVE supports PCI passthrough (`qm set {vmid} -hostpci0 {pci_id}`) and NVIDIA vGPU on
GRID-capable cards, and publishes per-node device inventories at
`/nodes/{node}/hardware/pci`, but the CPI exposes none of this, so GPU and accelerator
workloads cannot be expressed in a manifest.

**Shipped:** `pci_passthroughs: [{address: "0000:01:00.0"}]` in `vm_type` cloud_properties
filters placement to nodes advertising the requested PCI devices via
`/nodes/{node}/hardware/pci`; after clone, `hostpci0..N` are set on the VM via
`UpdateQemuConfig`; a strict single-node HA pin (`bosh-na-<vmid>`) is applied automatically
to block live migration. Address format (DDDD:BB:SS.F) is validated pre-clone so no orphan
VM is produced on bad input. Every node-resolution path — including the static paths that
bypass the placement filter (`target_node`, local-disk pin, `config.node` fallback,
placement disabled) — re-verifies the chosen node's device inventory before any clone, so a
PCI VM is never created on a node lacking the device: device absent is a non-retriable
error, an inventory API fault is retriable. Hardware ids reported in domain-less short form
(`01:00.0`) are normalized before comparison. The pin is removed unconditionally on create
rollback and on `delete_vm` (sync and fast path) — removal is not gated by
`placement.pin_az_via_ha_rules`, since the PCI pin is written regardless of that flag.
Empty list is byte-identical (no filter, no device check, no hostpci config, no pin).
`placement.Request` gains `RequiredPCIAddresses` and `PCIChecker` fields; the PCI filter
pass in `placement.Filter` is fail-safe (API error → node rejected).

**Limits.** IOMMU group resolution is not performed by the CPI — operator responsibility to
ensure IOMMU is enabled on the host and that the device is not shared across IOMMU groups
unless group isolation is confirmed. Live migration is blocked by design when PCI passthrough
is configured. Pin removal at delete/rollback adds two idempotent, not-found-tolerant HA API
calls per VM for all deployments. Live-PVE validated (2026-06-12): `/nodes/{node}/hardware/pci`
returns full-form `DDDD:BB:SS.F` ids; PVE accepts short-form `hostpci` values and stores them
as-is without canonicalizing, so the CPI's `0000:` normalization is required for the
verify-against-hardware-list comparison to match.

#### 7.48 SHIPPED — Multi-reference stemcell deduplication

*References: Azure.* The Azure CPI lets multiple BOSH stemcell CIDs share one gallery image
version: a `stemcell_references` CSV tag tracks every CID, `create_stemcell` updates the tag
rather than re-uploading when the SHA256 matches, and `delete_stemcell` removes only the one
reference, destroying the image only when the count reaches zero
(`lib/cloud/azure/stemcell/compute_gallery_manager.rb:76-111`, `129-138`, `171-188`). PVE
already replicates a template per node (§7.2) but creates a second template when the same
image is uploaded under a different CID, wasting disk.

**Build:** extend the template notes-JSON of §7.13 with a `stemcell_references` CSV. On
`create_stemcell`, if the computed SHA256 matches an existing template, append the new CID to
the list instead of cloning a new template; on `delete_stemcell`, remove the CID and destroy
the template only when the list is empty. The read-modify-write of that CSV must use the
concurrency-safe path of §7.44.

**Limits.** Reference counting introduces a shared mutable field across CPI processes, so it
depends squarely on the §7.44 locking work to avoid a lost decrement that strands or
prematurely deletes a template. Tier: operability.

**Shipped:** `stemcell_refs` CSV field added to `stemcellProvenance` struct in template notes
JSON. `create_stemcell` SHA-match and name-match reuse paths append the new CID (idempotent)
under a per-VMID cluster lock (`withVMIDLock`) via `registerStemcellRef`. New templates write
the initial CID in `stemcell_refs` at creation time, always — regardless of
`stemcell_provenance_enabled`. `delete_stemcell` decrements via
`gatedDeregisterAndDestroyRef`, which holds the per-VMID cluster lock through the destroy
itself, so a concurrent `registerStemcellRef` cannot append a CID between the last-ref
decrement and the `DeleteQemu` call; the template is destroyed only when refs become empty.
Missing, empty, or unparseable refs are treated conservatively (template preserved) to avoid
premature deletion of pre-refs templates; legacy templates created before refs existed are
therefore never destroyed by `delete_stemcell` and are reclaimed instead via the opt-in
director-scoped orphan prune of §7.13 (they carry the `bosh-stemcell` and `director--<id>`
tags when provenance was enabled) or audited manually with `scripts/disk-audit`. The
cross-node SHA-tag sweep destroys replicas without consulting their own `stemcell_refs`:
replica refs are intentionally not maintained, and the sweep runs only after the primary
template has passed the ref gate and been destroyed.
`handleDeleteTemplateStemcellCID` extracted from `HandleDeleteStemcell` to keep cognitive
complexity within the lint threshold.

#### 7.49 SHIPPED — Disk-encryption config surface

*References: AWS, Azure, Alicloud.* Three reference clouds expose at-rest encryption as a
config surface: AWS sets cluster-wide `encrypted` and `kms_key_arn` defaults that stemcell,
persistent-disk, and ephemeral-disk cloud_properties each override
(`lib/cloud/aws/cloud_v1.rb` config path); Azure offers disk-encryption sets with BYOK; and
Alicloud reads `Encrypted` as a `*bool` that inherits from a global flag. PVE has no
encryption surface at all, which blocks compliance-driven deployments.

**Build:** add a global `encrypted: true` plus per-disk `*bool` inheritance (per-disk
overrides global, which overrides off). Map an encryption request to an operator-preconfigured
encrypted storage pool — a ZFS dataset with native encryption or LUKS over LVM-thin —
selected through the §7.8 storage-tier resolver. The CPI selects the pool; it does not manage
keys.

**Limits.** Encryption on PVE is a backend-storage property, not a per-volume API toggle, so
the work reduces to pool selection and presupposes the operator has built encrypted pools and
manages their keys outside the CPI. This overlaps the "not recommended" entry below; it is
listed here as the bounded, delegation-style form that is defensible. Tier: deployment.

**Shipped:** `encrypted *bool` (per-call > global > off) restricts persistent and ephemeral
disk placement to storage tiers marked `encrypted: true` in CPI config `storage_tiers`.
Enforcement rules:

- Explicit `storage_pool` or `ephemeral_storage_pool` with `encrypted=true` → non-retriable
  CloudError (CPI cannot verify an arbitrary named pool is encrypted).
- Named `storage_tier` or `ephemeral_storage_tier` not marked encrypted → non-retriable
  CloudError (contradiction before the live storage query).
- No tier and no pool with `encrypted=true` → auto-select: the lex-first tier in
  `storage_tiers` with `encrypted: true` is run through the normal resolver (Types/Shared
  predicates + live cluster query). No encrypted tier in config → non-retriable CloudError.
- `ephemeral_storage_tier` is the cloud_properties key for ephemeral disk tier selection
  (mirrors `storage_tier` for persistent disks).
- A warning is logged on every encrypted-tier selection; marking a tier encrypted is operator
  responsibility — the CPI cannot verify pool encryption.
- `encrypted` unset at all levels → byte-identical to prior releases.

Residual scope: parked-disk parker-VM reuse and stemcell template replication choose their
storage independently of the encrypted filter (out of scope for this feature).

#### 7.50 SHIPPED — Stemcell creation from URL with checksum

*References: Google.* The Google CPI accepts a `source_url` plus `raw_disk_sha1` in
`create_stemcell` and hands both to the image-import API for server-side integrity
verification, with no local download. PVE 7.2 and later expose a download-URL storage API
that does the same: it streams an image directly into a pool and reports a checksum.
Publishing stemcells as HTTP artifacts rather than transferring tarballs through BOSH cuts
create-env time on slow links.

**Shipped:** `create_stemcell` cloud_properties gain `source_url` (string) and `sha256`
(string). When `source_url` is set, `handleStemcellDownloadURL` is dispatched: the CPI
calls `POST /nodes/{node}/storage/{storage}/download-url` via
`Nodes().CreateStorageDownloadUrl` with `Content="import"` and the canonical qcow2
filename derived from `name`, `version`, and `sha256`. When `sha256` is also set, the
`Checksum` and `ChecksumAlgorithm="sha256"` params are forwarded so PVE verifies the image
server-side; a task failure (including checksum mismatch) is returned as a non-retriable
cloud error. The returned UPID is awaited via the existing `AwaitTask` plumbing. After
the task succeeds the downloaded volume is located by exact filename lookup with a prefix
scan fallback (to handle PVE filename normalization). All downstream steps — template VM
creation via `ensureTemplateVM`, freeze, provenance notes and tags, pool assignment, and
replica distribution — run identically to the existing upload paths; the returned CID is
always `"template:<vmid>"`. When `source_url` is absent the existing flow is byte-identical.

**Limits.** The URL must be reachable from the PVE node, not from the CPI host; operators
must ensure network access from the PVE node to the image host. Requires PVE 7.2+. Task
output exposes no checksum field (live-PVE validated 2026-06-12: storage content list and
`GetStorageContent` return volid/format/size/path only), so deriving the non-retriable error
from task failure status is the only option; a checksum mismatch fails the task with
"checksum mismatch: got '<actual>' != expect '<expected>'" and leaves the partial file on
storage, which the best-effort `import/<filename>` cleanup removes. The requested filename
is preserved verbatim (no normalization observed). Tier: deployment.

#### 7.51 SHIPPED — Per-operation timing metrics

*References: Azure.* The Azure CPI ships opt-in telemetry: it forks a background process that
reports each operation's duration and success or failure to the platform fabric. No other
reference emits metrics, and PVE emits none, so operators have no service-level-indicator
data on CPI operations and the observability dimension in §9 stays partly open.

**Build:** wrap the dispatcher boundary — the same seam that already carries `request_id` and
the redacted trace (§7.41) — to record wall-clock duration and outcome per RPC. Write
the samples to a metrics file or a Prometheus pushgateway, gated behind an opt-in flag so the
default path allocates nothing.

**Limits.** PVE offers no fabric telemetry endpoint analogous to Azure's wireserver, so the
sink must be operator-provided. A single-shot per-RPC process cannot aggregate; it can only
emit one sample per invocation, leaving rollup to the collector. Tier: hardening.

**Shipped:** opt-in `MetricsHook` controlled by `pve.metrics.enabled` and `pve.metrics.file_path`. When enabled, the hook appends one JSON-line sample per CPI RPC — fields `ts` (RFC3339Nano), `method`, `duration_ms`, `outcome` (ok|error), `request_id` — via open-append-close atomic writes. `duration_ms` measures handler execution; post-call work by other configured hooks is excluded. Write failures are logged at Warn level and never fail the RPC. When disabled (the default), the hook is not registered and adds zero dispatch-path overhead; config output is byte-identical.

#### 7.52 SHIPPED — Configurable User-Agent and operator tag on PVE API calls

*References: Google, Azure.* Google prepends a configurable `user_agent_prefix` to its
`bosh-google-cpi/<version>` User-Agent for billing attribution and log filtering; Azure
injects a fixed-by-default, operator-overridable ISV tracking GUID into every request's
User-Agent (`BOSH-AZURE-CPI/<version> pid-<guid>`). PVE API calls from this CPI carry no
distinguishing User-Agent, so operators cannot attribute API load or throttling to BOSH in
PVE access logs.

**Build:** set a `BOSH-PVE-CPI/<version>` User-Agent header on the pve-apiclient-go HTTP
client through its transport wrapper, and add an optional `operator_id` config key appended to
the header for per-operator attribution.

**Limits.** PVE does not bill on API usage as the public clouds do, so the value is confined
to audit, throttle attribution, and log filtering rather than cost accounting. Tier:
hardening.

**Shipped:** `BOSH-PVE-CPI/<version>` User-Agent is set on all PVE API calls via
`raw.SetHeader` at client construction in `internal/pve/client.go`. The version token uses
`version.Short()` (ldflags-settable; defaults to `dev`). The optional `pve.operator_id`
config key appends `pid-<value>` to the header for log attribution when multiple BOSH
directors share a PVE cluster. When `operator_id` is unset, the header contains no trailing
space — byte-identical to prior releases. Both the regular and upload request paths in the
SDK call `applyCustomHeaders`, so one `SetHeader` call covers all API traffic.

#### 7.53 SHIPPED — Resource-ownership tagging

*References: vSphere.* On `create_stemcell` the vSphere CPI registers a vCenter extension and
sets `config.managed_by` on every CPI-created VM and template, so vCenter's Solution Manager
groups all BOSH-managed resources under one owner. PVE mixes BOSH VMs and templates with
user-created guests in the same node view, with no managed-by marker; §7.13 tags templates
for provenance but VMs carry no general ownership signal.

**Shipped:** the fixed `bosh-cpi` tag is stamped on every CPI-created VM (`create_vm`) and
stemcell template (`create_stemcell`, primary and replicas) at creation time via
`mergeTagList`/`UpdateQemuConfig`. The constant `ownershipTag = "bosh-cpi"` lives in
`tags.go`; it is NOT in `reservedBoshTagPrefixes`, so `set_vm_metadata` preserves it across
every metadata update. Operators can filter the PVE UI and `disk-audit` output by this tag to
scope views to CPI-managed guests only. No new spec key or config toggle is needed — the tag
is always-on and additive, writing one additional semicolon-delimited token alongside any
operator-supplied tags.

**Limits.** PVE tags are flat free-text labels, not a structured ownership object, so the tag
is a convention the CPI must apply consistently rather than an enforced relationship. A
manually retagged VM silently breaks it. Tier: operability.

#### 7.54 SHIPPED — Per-disk retain-on-delete (forensic ephemeral)

*References: Alicloud.* The Alicloud CPI reads `delete_with_instance` as a `*bool` on
ephemeral disks: when false, the disk survives the instance's deletion. PVE deletes every
attached disk with the VM, so an operator who wants to preserve an ephemeral disk for
post-mortem analysis after a failed VM has no in-band option. This complements the
`debug.keep_failed_vms` capability of §7.20.

**Shipped:** Two independent retention surfaces, both unset by default (byte-identical to
prior behavior).

*Persistent-disk retain flag.* `create_disk` cloud_properties accepts `retain_on_delete:
true` (`*bool`). When set, the string `"retain_on_delete":"1"` is encoded into
`DiskCIDMeta.Opts` inside the disk CID. Persistent disks created by `create_disk` already
survive `delete_vm` via the foreign-VMID guard (`detachForeignActiveDisks`): the guard
detaches every active-slot disk whose embedded VMID differs from the VM's own VMID and
preserves the backing volume. The `retain_on_delete` flag adds explicit provenance — it is
readable from the disk CID alone, independent of VM config — and is available as an audit
signal for inventory tooling (`scripts/disk-audit`).

*Ephemeral-disk retain flag.* `create_vm` cloud_properties accepts
`retain_ephemeral_on_delete: true` (`*bool`). When set, `create_vm` stamps the PVE tag
`bosh-retain-ephemeral` on the VM. This tag survives `set_vm_metadata`'s tag RMW (the tag
is not in `reservedBoshTagPrefixes`). On `delete_vm` — on both the fast path (skiplock
destroy) and the slow path (stop+await+destroy) — the handler reads the VM tags; when
`bosh-retain-ephemeral` is present it finds every ephemeral disk slot whose bare volid
contains `vm-<vmid>-ephemeral-` (the naming convention set by `attachEphemeralDisk`), then:
(1) calls `UpdateQemuUnlink(force=false)` on the slot, which demotes the disk to an
`unusedN` config entry while leaving backing storage intact; (2) re-reads the VM config to
locate the resulting `unusedN` slot; (3) calls `UpdateQemuConfig(Delete: "unusedN")` to
remove that config reference only, without freeing storage; (4) proceeds to `DeleteQemu`
with `purge=true, destroyUnreferencedDisks=false` — the volume is now unreferenced with a
matching VMID, which is exactly the class `destroyUnreferencedDisks=true` would free, so
the flag is forced false on the retain path (and only there; non-retain deletes keep
`true`, byte-identical). The retained volid is logged at WARN for operator recovery.

Tag presence — not unlink success — gates the destroy flag, which makes the path safe to
re-enter: a retried `delete_vm` whose prior attempt already unlinked and swept the disk
finds no active ephemeral slot but still forces `destroyUnreferencedDisks=false`. The
fast-path straggler sweep (`sweepFastDeleteStragglers`) applies the same rule: a
`bosh-deleting` straggler that also carries `bosh-retain-ephemeral` gets the detach re-run
(finishing any pending unlink) and its re-issued destroy forced to
`destroyUnreferencedDisks=false`; if the detach fails, the straggler is deferred to the
next sweep rather than destroyed with the volume in an unknown state.

`force=false` was chosen over alternatives: `force=true` physically destroys the volume
(wrong); reassigning to another VM is out of scope. The two-step unlink+config-delete
sequence is required because `DeleteQemu purge` destroys `unusedN` entries it finds in
config — without the config-delete sweep, the volume would still be destroyed.

*Limits.* On a retain-flagged VM, `destroyUnreferencedDisks=false` means any other
unreferenced own-VMID volumes (for example an orphan from a prior failed create) also
survive the delete — conservative by design. Retained volumes accumulate until operator
prune; use `scripts/disk-audit` to inventory them. The ephemeral retain path reads the VM
config once per `delete_vm` invocation (exits immediately when the tag is absent). The
destroy-flag semantics are live-PVE validated (2026-06-12): with `purge=1`, an unreferenced
own-VMID volume survives `destroy-unreferenced-disks=0` and is freed by
`destroy-unreferenced-disks=1`, exactly as the retain path assumes. Unset flags →
byte-identical delete behavior. Tier: deployment.

#### 7.55 SHIPPED — Router and NAT VM support

*References: AWS, Google.* The AWS CPI supports `source_dest_check: false`, which lets a VM
forward packets not addressed to it (NAT gateways, router VMs), and `advertised_routes`, a
list of `{table_id, destination}` pairs that the CPI upserts into route tables pointing at
the new instance so the routing tier owns its routes declaratively. PVE has no CPI-layer
equivalent, so routing and NAT VMs require manual configuration.

**Shipped:** `ip_forwarding: true` on a NIC's `cloud_properties` causes the CPI to call
`UpdateQemuConfig` post-create with `firewall=0` for that NIC index (implemented in
`create_vm_firewall.go:applyIPForwarding`). The §7.14 ipfilter path (`applyVIPAllowedAddressPairs`)
also excludes `ip_forwarding=true` NICs from ipset seeding and from the step-4 DHCP safety
guard, so enabling ipfilter on non-forwarding NICs is unaffected. `advertised_routes:
[{vnet, destination}]` on the VM's `cloud_properties` calls `CreateSdnVnetsSubnets` (POST
`/cluster/sdn/vnets/{vnet}/subnets`, type=`subnet`) for each entry, then `applySDN` (PUT
`/cluster/sdn`) once to commit all staged changes. On failure, any subnets created before
the error are removed via `DeleteSdnVnetsSubnets`; if removal fails, a warning names the
leftover subnet for operator cleanup. A subnet that already exists is adopted (not an
error, not rolled back); live PVE reports duplicate SDN object creation as HTTP 500 with
"sdn <kind> object ID '<id>' already defined" rather than HTTP 409, and `isSDNConflict`
matches both shapes. Both features are byte-identical when absent. Live-PVE validated
(2026-06-12): subnet `type=subnet` create + `applySDN` commit verified end-to-end
(canonical subnet id `<zone>-<network>-<mask>`), and the NIC firewall read-modify-write
preserved model/MAC/bridge/mtu while applying `firewall=0`; a bare partial `net{i}` write
is rejected by PVE (`net0.model: property is missing`), confirming replace-not-merge.

**Limits.** `advertised_routes` targets OVN vnet subnets only — the SDK exposes no OVN
`nbctl` static logical-router route API, so routes that do not correspond to a full subnet
CIDR require out-of-band OVN commands. When the SDN zone is not OVN (e.g. vxlan/simple),
PVE may accept the subnet create without injecting a logical-router route; this is a
PVE-layer constraint, not a CPI defect. `ip_forwarding=true` explicitly voids per-NIC
ipfilter protection on that NIC. Guest-OS IP forwarding (`sysctl net.ipv4.ip_forward`)
is not managed by the CPI. `delete_vm` does not remove `advertised_routes` SDN subnets;
an operator must clean them up manually (via `pvesh DELETE /cluster/sdn/vnets/{vnet}/subnets/{cidr}`
followed by `pvesh PUT /cluster/sdn`) after destroying a router VM. Tier: deployment.

#### 7.56 SHIPPED — Per-VM `*bool` override pattern and operator retry budget

*References: OpenStack-Go, AWS.* The OpenStack-Go CPI threads `*bool` cloud_properties through
a consistent inherit-from-global-then-default chain (a nil pointer falls back to the global,
which falls back to a hardcoded default), and the public clouds expose retry budgets as config
— AWS requires `aws.max_retries` at validation time. PVE's retry behavior (§7.25) is partly
in place but its budget is not operator-tunable, and the `*bool` inheritance pattern, while
used in spots, is not stated as a uniform convention.

**Build:** document and apply a single `*bool` inheritance rule across optional knobs —
per-call overrides per-disk, which overrides `vm_type`, which overrides global, which defaults
off — so new optional flags behave predictably and stay byte-identical when unset. Expose a
`retry_config` block (attempt counts and backoff bounds) that the existing transient-retry and
lock-retry paths read, defaulting to the current hardcoded values.

**Limits.** A tunable retry budget can mask a persistent fault as a transient one if set too
high, lengthening failure detection; the defaults must remain the shipped values so existing
deployments are unaffected. Tier: hardening.

**Shipped:** `StorageLock *RetryPolicy` added to `RetryConfig` as the fifth retry-policy seam
(`json:"storage_lock,omitempty"`), alongside the existing four (StorageImport, VMIDAlloc,
TaskPoll, Pushback). The `RetryStorageLock()` accessor returns defaults `base_ms=2000`,
`cap_ms=30000`, `jitter_pct=30` (matches the shipped `StorageLockBackoff` curve) when the
block is absent; `max_attempts=0` defers to `pve.DefaultStorageLockMaxAttempts` (10). The
storage-lock backoff curve is now wired to the seam via `ConfigureStorageLockBackoff` (called
at process startup from operator config). The `create_disk` handler's lock-attempt budget reads
`RetryStorageLock().MaxAttempts` first, then `RetryStorageImport().MaxAttempts` (legacy
fallback), then `VMIDAllocAttempts`, then the package constant; `create_vm` follows the same
precedence chain. The existing four retry policies are unmodified. The `*bool` inheritance
convention is formally documented in
[Bool Inheritance Convention](bool-inheritance-convention.md).

### Explicitly not recommended as core CPI work

| Item | Demonstrated by | Why not |
|------|-----------------|---------|
| Per-disk / per-image encryption toggle | AWS, Azure, Alicloud | At-rest encryption is a PVE storage-backend property (ZFS/LUKS/Ceph), not a per-volume API. Select it via §7.8 (point a vm_type at an encrypted storage), mirroring OpenStack/Google delegation. The bounded, delegation-style form is tracked as §7.49. |
| Spot / preemptible / capacity-reservation | AWS, Google, Azure, Alicloud | Cloud-economic constructs with no PVE primitive. AWS exposes `spot_bid_price` with `spot_ondemand_fallback` and an `AbruptlyTerminated` retry loop for mid-flight preemption; Google and Alicloud offer comparable preemptible types. PVE has no spot market and no eviction mechanism: HA priority levels are the nearest analogue, but actual eviction would require a separate watchdog or reaper job that no PVE primitive provides. The only portable fragment — an on-failure fallback retry loop — is already covered by post-selection fallback (§7.31), so the distinctive spot value cannot be delivered on PVE. |
| Floating / elastic IP as a standalone primitive | AWS, OpenStack, Azure, Alicloud | No PVE elastic-IP primitive. Deliver the value via §7.14 (self-hosted VRRP VIP) and §7.19 (external hook). |
| Routes / advertised-routes / `source_dest_check` | AWS, Google | Routing lives in the guest OS, the SDN zone (EVPN/OSPF via frr), or the fabric — not a CPI-layer route table. The bounded SDN-assist form is tracked as §7.55. |
| Multi-region / cross-cluster stemcell manifest | AWS, Alicloud, Azure | A CPI instance targets one PVE cluster; "region" has no PVE primitive. The in-cluster analogue is §7.2. |
| CPI-driven live-migration / rebalance loop | vSphere | Delegated to PVE DLB/CRS; a CPI is a stateless per-call process with no daemon for a control loop. |
| `ClientToken` idempotency keys | Alicloud | VMID allocate-with-retry already gives create-once semantics on PVE's reservation model. |
| In-process distributed tracing (OTel/Jaeger) | (none ship it) | A single-shot per-RPC CLI has no long-lived span; `request_id` is the correct correlation point. |
| First-class metrics/telemetry pipeline | Azure, vSphere | Wrong layer; BOSH (bosh-monitor, Prometheus) owns this. Offer as an optional hook only. |

## 8. Cross-CPI Engineering Lessons

These are the transferable principles behind the specific gaps — the "why," distilled from
reading six mature CPIs against this one.

- **A cross-process lock is required exactly when the platform exposes non-atomic shared
  config — and PVE is in that camp.** vSphere locks DRS rule mutation through vCenter's
  `CustomFieldsManager`, using `create`-raises-`DuplicateName` as an atomic compare-and-swap
  (`drs_rules/drs_lock.rb:30-57`); Azure takes OS `flock` on named files under `/tmp/azure_cpi/`
  for availability-set and gallery-image writes (`utils/helpers.rb:130-138,564-575`). The four
  public-cloud CPIs lock nothing: AWS, Google (`metadata_client.go:147`), OpenStack-Go
  (`loadbalancer_service.go:101`), and Alicloud lean on provider-atomic creates, fingerprint
  CAS, or idempotency tokens. PVE's pmxcfs-replicated HA rules are non-atomic shared config, so
  it resembles vSphere and Azure, not AWS. Its shipped cluster mutex (§7.36) is the correct
  response; do not cargo-cult the lock-free cloud CPIs.

- **Idempotency has two correct responses to a collision, and the platform-provided tier is one
  PVE must supply itself.** Regenerate identity when the collision means "taken" (Alicloud's
  token, PVE's VMID); retry the same identity when it means "in flight" (AWS and Azure tokens).
  Pick by meaning, and state the rule (§7.33) — the wrong choice either loops forever or
  duplicates the resource. Note also what PVE does not get for free: server-side dedup through
  `ClientToken` (Alicloud, AWS, Azure), fingerprint CAS (Google), or a 409-already-exists check
  (OpenStack-Go LB) all live in the platform. The PVE API offers none, so the CPI must
  self-implement idempotency through CID checks and pre-flight status queries (§7.33).

- **Capability-based sizing is the one universal feature PVE still lacks.** Five of six
  references map abstract `cpu`/`ram` to a concrete instance through
  `calculate_vm_cloud_properties`: AWS's static `InstanceTypeMapper` table, Azure's live SKU
  query with a 24-hour on-disk cache, OpenStack-Go's flavor mapper, Google's custom-machine-type
  URL builder, and Alicloud's mapper (vSphere alone has no equivalent). Abstract `vm_resources`
  is a BOSH-native input; not supporting it forces operators to hand-map node specs to PVE
  config. This is the strongest net-new candidate the comparison surfaces (§7.43).

- **Error classification is a control signal, not telemetry.** AWS's
  `VMCreationFailed.new(retryable)` and its two spot-failure classes (bidding → fall back,
  instance → retry) *drive* the fallback decision. PVE's §7.23 classifier should likewise gate
  post-selection fallback (§7.31), not just tag the error. The retriable bit is an instruction to
  the director, so its boundary is a correctness concern (§7.11).

- **Enforce creation-time invariants; do not merely record them.** Azure rejects a caching-mode
  mutation outright. PVE records disk-performance attributes in the CID (§7.9) but no path
  rejects drift on re-attach (§7.26). Metadata is only as good as the code that checks it.

- **Verify before commit beats verify after commit when the platform allows it.** Google
  validates the image SHA server-side before the import writes it; PVE computes the hash *after*
  import (§7.6), so a deterministically corrupt download re-verifies to the same bad hash and
  enters the dedup cache. The same logic motivates boot-path agent integrity (§7.29).

- **A config schema is the antidote to dead-field drift.** OpenStack-Go parses six fields it
  never reads (ConnectionOptions, DefaultVolumeType, WaitResourcePollInterval, EphemeralDisk,
  SchedulerHints, UseNovaNetworking), and Alicloud's surface is sparse by design; both lack a
  declarative schema. vSphere's Membrane DSL and PVE's opt-in strict validation (§7.17) close
  that gap — PVE's unknown-key rejection is in fact stricter than every reference, none of which
  rejects unknown keys. The drift is the cost of validating imperatively and partially.

- **Secret redaction maturity tracks team origin, not language.** The hyperscaler-maintained
  CPIs scrub: AWS's `Bosh::Cpi::Redactor`, Azure's recursive `CREDENTIAL_KEYWORD_LIST` walk, and
  Google's `RedactSecrets` regex. The two community Go ports scrub nothing — OpenStack-Go has no
  redaction anywhere, and Alicloud logs its `AccessKeySecret` and mbus password verbatim. PVE's
  §7.41 scrubbing (userinfo, presigned URLs, signature parameters) is table-stakes for a
  production CPI, not gold-plating.

- **Delegate to the platform — validate, don't orchestrate.** Azure's Compute Gallery
  replication is platform-automatic; the CPI only checks that replicas exist. PVE's §7.2
  correctly *uses* replicas opportunistically rather than driving replication itself, and DLB
  (CRS) is delegated to PVE entirely. Do not reimplement what the platform already does; gate on
  its result.

- **Progress-aware polling beats fixed backoff for long platform operations.** vSphere's
  ETA-proportional interval and Alicloud's per-operation retry budgets adapt to the specific
  operation; fixed curves (§7.25) pay a polling-storm tax exactly when load is highest (§7.28).

- **Per-call re-authentication is the price of stateless purity — pay it deliberately.**
  OpenStack-Go re-authenticates to Keystone on every service build, costing four or more
  round-trips per `create_vm`. The pattern keeps each call self-contained but taxes latency under
  load. PVE's transport tuning and ticket reuse (§7.30) is the better default: keep the connection
  warm rather than re-prove identity per operation.

- **Hooks-vs-hardcoding is a real tradeoff, and on-prem favors hooks.** AWS bakes ELB/ALB
  registration into `create_vm` (fewer failure modes, but create-only, no deregister). PVE made
  LB integration a post-hook with a rollback contract (§7.19) — more failure surface, but the
  right call when the load balancer is an operator-chosen HAProxy/keepalived rather than a managed
  cloud primitive.

- **The wedged-wait hazard has two complementary fixes; keep both.** Bound the wait (the §7.15
  timeout envelope) and skip the wait (the §7.32 fast-path delete) cover different operations
  against the same queue-slot incident class. One is not a substitute for the other.

- **A coarse global bound is worth less than a per-class one.** OpenStack-Go uses a single
  `state_timeout`; PVE's per-method-class envelope (create 30m / delete 15m / has-get 2m, §7.15)
  is the more useful granularity — a strength to preserve, not flatten.

## 9. Cross-Cutting Dimensions

The matrices above map every CPI method and standout feature. This section compares the seven CPIs along the cross-cutting dimensions that decide whether the shipped gaps hold up under load: concurrency safety, observability, test discipline, configuration validation, failure-mode coverage, and the performance envelope. Each subsection states the framing, gives a comparison table with a "PVE (this codebase)" row, and draws the one insight the data supports.

### 9.1 Cross-process concurrency and cluster-config safety

A BOSH director spawns many CPI processes concurrently, and several of them touch the same cluster-wide mutable state. The question is whether each platform forces the CPI to hold an explicit lock, or whether the platform's API makes the mutation atomic. The pattern is sharp: an explicit cross-process lock is required exactly when the platform exposes a non-atomic shared-config surface, and is unnecessary when the platform offers atomic create, compare-and-swap, or an idempotency token.

| CPI | Explicit cross-process lock | Mechanism | Cite |
|-----|------------------------------|-----------|------|
| vSphere | Yes | vCenter `CustomFieldsManager` create, with `DuplicateName` used as a compare-and-swap; locks `drs_lock`, `host_vm_group`, `DISABLE_DRS_LOCK`; 600 s timeout, 0.5 s poll | `drs_rules/drs_lock.rb:30-57` |
| Azure | Yes | OS `flock` on named files under `/tmp/azure_cpi/` for availability set, stemcell copy, and gallery image; `LOCK_EX`, with `LOCK_NB` for availability-set delete | `utils/helpers.rb:130-138,564-575` |
| AWS | No | API issues an atomic unique ID per create; no shared mutable counter | `aws_provider.rb` |
| Google | No | GCE metadata and label fingerprint compare-and-swap (409 on a stale fingerprint); global operation serialization | `metadata_client.go:147` |
| OpenStack-Go | No | API state machine plus poll-until-active; 409 idempotency check on load-balancer member | `loadbalancer_service.go:101` |
| Alicloud | No | `ClientToken` idempotency plus `Invoker.Catcher` conflict retry (`OperationConflict`, 60 attempts) | `invoker.go` |
| PVE (this codebase) | Yes | Cross-process cluster mutex for HA-rule and anti-affinity mutation, backed by pmxcfs | §7.36 |

The two reference CPIs that lock are precisely those whose platform exposes non-atomic shared config: vSphere DRS rules and Azure availability sets and shared galleries. The four public-cloud CPIs avoid locking because their APIs guarantee atomicity through unique-ID create, fingerprint compare-and-swap, or a client-supplied idempotency token. PVE's HA rules and anti-affinity rules live in pmxcfs, a replicated configuration filesystem with no compare-and-swap primitive, so PVE belongs with vSphere and Azure, not with AWS and GCE. The cross-process mutex of §7.36 is the correct response to that platform shape, not borrowed ceremony from the lock-free cloud CPIs.

### 9.2 Observability and operability

Operability turns on three questions: can a stuck deploy be correlated across logs by a request ID, are agent credentials and presigned URLs scrubbed before they reach the log, and does the CPI emit any metric an operator can alert on? The references split cleanly, and the split tracks team origin rather than language.

| CPI | Request-ID correlation | Secret redaction | Metrics |
|-----|------------------------|------------------|---------|
| AWS | `options.aws.request_id` threaded to logger | `Bosh::Cpi::Redactor` over user_data and keys | None |
| vSphere | `vcenters[0].request_id` threaded to logger | XPath over Login and password only (narrow) | None from the CPI; a vCenter extension flag exists |
| Azure | `x-ms-client-request-id` per request (`SecureRandom.uuid`), echoed; MDC slot | `CREDENTIAL_KEYWORD_LIST` recursive walk; `/listKeys` response suppressed | Yes, opt-in telemetry: per-operation `duration_ms` and success emitted via a forked telemetry handler, at most once per 60 s |
| Google | None (operation name serves as an implicit token) | `RedactSecrets` regex over account_key, json_key, password, private_key; `MultiLogger` buffers the full trace into every response `log` field | None |
| OpenStack-Go | None | None anywhere | None |
| Alicloud | None | None (logs `AccessKeySecret` and the mbus password verbatim) | None |
| PVE (this codebase) | `request_id` in the dispatcher | Shipped §7.41 (URL userinfo, presigned signatures, sig query params) | None |

Redaction maturity is bimodal. The three hyperscaler-maintained CPIs — AWS, Azure, and Google — scrub credentials at the log boundary, while the two community Go ports, OpenStack-Go and Alicloud, scrub nothing; Alicloud logs its access-key secret in clear text. The §7.41 work moves PVE into the mature tier. Request-ID correlation is likewise universal among the hyperscaler CPIs and absent from the community Go ports; PVE carries it. Only Azure emits metrics, and PVE does not yet — the one observability item still open and a candidate for future work.

### 9.3 Test strategy

The relevant axes are the test framework, the tiers each suite runs, and whether the fast tier requires live infrastructure. PVE's discipline on these axes is a genuine differentiator that the capability matrices never surface.

| CPI | Framework | Tiers | Notable |
|-----|-----------|-------|---------|
| AWS | RSpec | Unit (mocked), integration (live) | `verify_partial_doubles`; Ruby-version-pin assertion |
| vSphere | RSpec | Unit, integration (live vCenter) | ~50 feature integration specs; Timecop; stemcell reuse across the suite |
| Azure | RSpec plus WebMock | Unit, integration (live), manual performance | Per-cloud-property spec subdirectories; request-header matchers |
| OpenStack-Go | Ginkgo v2 plus Gomega | Unit (counterfeiter fakes), integration (httptest mock endpoints) | No live API; polling-interval package vars set to 0 in tests |
| Google | Ginkgo plus Gomega | Unit (counterfeiter), integration (live GCP) | 1405-line `vm_test.go`; `CPI_ASYNC_DELETE` speedup |
| Alicloud | Ginkgo v1 | Unit (in-memory mocks), integration (live) | `action/` unit tests largely commented out; 90 s hard sleep; no coverage gate |
| PVE (this codebase) | Go `testing` plus testify | Unit, integration, e2e (live PVE) | `-race`, adversarial review, 85%+ coverage gate, TDD with deterministic polling |

PVE's coverage gate, race detector, and deterministic-poll discipline exceed every reference. Only OpenStack-Go and PVE keep live infrastructure out of the fast tier — OpenStack-Go through httptest mock endpoints, PVE through fakes — which keeps the inner loop quick. Alicloud sets the floor: its unit tests are largely commented out, it sleeps 90 seconds in integration, and it ships no coverage gate. Critically, the references that test the hardest concurrency and idempotency paths least are exactly the ones (OpenStack-Go, Alicloud) with the weakest cross-process safety, while PVE's TDD discipline targets those same paths.

### 9.4 Configuration surface and validation

As optional knobs accumulate, the question is whether the CPI rejects misspelled or stale keys and how it expresses cross-field constraints. The references converge on imperative, partial validation; none rejects unknown keys.

| CPI | Schema | Reject unknown keys | Cross-field validation |
|-----|--------|---------------------|------------------------|
| AWS | Imperative Ruby | No | Credential-source mutual exclusion; region xor endpoints |
| vSphere | Membrane DSL (declarative) | No (dictionary passthrough) | Datacenter count; disk_type enum; datastore-pattern xor; NSX-T auth-mode branch |
| Azure | Imperative | No | Zone with managed_disks; zone xor availability set; root-disk placement vs type |
| OpenStack-Go | JSON structs | No (no `DisallowUnknownFields`) | Credential-mode xor; config_drive enum; az xor azs; six dead fields |
| Google | Required-field check only | No | None beyond Project-required |
| Alicloud | Minimal | No | Region and registry port only; sparse by design |
| PVE (this codebase) | Opt-in `strict_config_validation` (§7.17) | Yes (opt-in unknown-key rejection) | Shipped cross-field rules (§7.17, §7.18) |

No reference CPI rejects unknown keys, and vSphere's Membrane DSL is the only declarative schema. PVE's opt-in unknown-key rejection (§7.17) is therefore the strictest configuration validation in the set. The cost of going without a schema is visible: OpenStack-Go carries six parse-but-dead fields (`ConnectionOptions`, `DefaultVolumeType`, `WaitResourcePollInterval`, `EphemeralDisk`, `SchedulerHints`, `UseNovaNetworking`), and Alicloud's surface is sparse by necessity. A schema, declarative or opt-in strict, is the antidote to that drift.

### 9.5 Consolidated failure-mode taxonomy

The body classifies errors as retriable or non-retriable (§7.11, §7.23) but never assembles the failure modes into one table mapping each to the gap that addresses it. The following taxonomy makes the Tier-1 correctness work legible as a single safety story.

| Failure mode | What goes wrong | PVE gap(s) that address it |
|--------------|-----------------|----------------------------|
| Partial create | `create_vm` fails after side effects (LB registration, tags) are applied | §7.19 dispatch rollback contract; §7.31 post-selection fallback |
| Orphaned resource | A template or VM survives a failed or abandoned operation | §7.13 orphaned-template GC; §7.32 fast-path delete |
| Split-brain placement | Two processes place conflicting VMs or rules against one cluster | §7.3 disk fault-domain co-location; §7.36 cross-process cluster mutex |
| Corrupted in-flight operation | A `delete_disk` races a live volume operation | §7.38 pre-delete lock/status guard |
| Unreachable disk | A VM lands on a node that cannot reach its disk | §7.3 VM-disk fault-domain co-location |
| Duplicate IP | A static IP collides with a foreign device or an out-of-range address | §7.5 active IP-conflict probe; §7.18 static-IP-in-range validation |
| Stale LB membership | A failed VM remains registered on the load balancer | §7.19 external LB hook with deregister-on-rollback |
| Secret leak | Credentials or presigned URLs reach the logs | §7.41 dispatcher request/response redaction |
| Racing template clone | Two processes clone the same stemcell concurrently | §7.37 adopt-and-wait on clone-target-exists |
| Undersized ephemeral disk | Ephemeral disk smaller than swap demand | §7.40 ephemeral-disk ≥ 2× RAM invariant |

### 9.6 Performance envelope

The references differ in how aggressively they avoid wasted round-trips. The qualitative comparison favors adaptive and cached strategies over naive per-call work.

| CPI | Polling strategy | Notable per-call overhead | Cite / §ref |
|-----|------------------|---------------------------|-------------|
| vSphere | Adaptive or server-directed | — | (qualitative) |
| Azure | Server-directed (`Retry-After`) | 24 h on-disk SKU cache under `/var/vcap/.../cache` avoids repeated capability queries | (qualitative) |
| AWS | API waiters | — | (qualitative) |
| Google | Global operation polling | — | (qualitative) |
| OpenStack-Go | Poll-until-active | Re-authenticates to Keystone per service build: 4+ round-trips per `create_vm` | qualitative; re-auth observed across `openstack_service.go` service builds |
| Alicloud | Server-directed plus hard sleeps | 90 s hard sleep in tests; conflict-retry loop | `invoker.go` |
| PVE (this codebase) | Progress-aware adaptive poll (§7.28) | Connection-pool and keepalive tuning, ticket reuse (§7.30) | §7.28, §7.30 |

PVE's progress-aware adaptive poll (§7.28) and transport tuning with ticket reuse (§7.30) are the better default than OpenStack-Go's four Keystone re-authentications per `create_vm`, and they spare the cluster the polling-storm tax that fixed-interval polling imposes on a many-node cluster. One dimension remains genuinely open. The steady-state API-call count per `create_vm` and the parallel-deploy throughput on a K-node PVE cluster are still not measured here; the adaptive-poll, transport-tuning, and parallel-replication wins (§7.28, §7.30, §7.35) are argued from mechanism rather than from numbers. Folding the project's e2e timing data into a measured comparison is the one piece of this analysis that remains to be done.

