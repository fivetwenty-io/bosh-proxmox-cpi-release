# Proxmox VE CPI — Cross-CPI Comparison Matrix and Improvement Analysis

A comprehensive re-analysis of the Proxmox VE (PVE) BOSH CPI against six upstream
CPI implementations, measured against the canonical BOSH CPI API v2 contract. The
goal is to mine proven patterns from a far broader reference set than the prior
report covered, confirm which earlier recommendations have shipped, and rank the
capabilities still worth adding — with a PVE-specific rationale and a concrete
build approach for each.

## 1. Reference Set and Method

Six reference CPIs were inventoried in depth (real handler source, not just READMEs),
then compared against the current PVE CPI through six cross-cutting capability themes.

| CPI | Language | API/SDK surface |
|-----|----------|-----------------|
| AWS | Ruby | EC2/EBS/ELB/ALB, v1/v2/v3 API versions |
| vSphere | Ruby | vSphere SDK + NSX-T Manager/Policy, cpi_plugins |
| OpenStack (Go) | Go | Gophercloud — Nova/Cinder/Neutron/Octavia |
| Google | Go | GCE/Compute, target pools + backend services |
| Azure | Ruby | ARM — managed disks, Compute Gallery, LB/App Gateway |
| Alicloud | Go | ECS/SLB/NLB, legacy SDK + Tea OpenAPI SDK |
| **Proxmox (PVE)** | **Go** | **`pve-apiclient-go` v3 — this repository** |

Each reference CPI received a dedicated deep-read producing a method-by-method status
inventory plus a standout-feature list. The PVE CPI received the same treatment, with
explicit cross-checks of which prior-roadmap items are now implemented. Six thematic
analyses (placement, networking, storage, stemcell/agent, resiliency/observability,
extensibility/ops-UX) then compared the reference behaviors against the current PVE
code and classified each capability as already-done, or as a Tier 1–4 / not-recommended
gap. Every Tier-1 headline below was re-verified directly against PVE source.

## 2. Executive Summary

**Status as of this round.** Every gap §7.1–§7.35 below is now **shipped and
source-verified**: a feature-group-by-feature-group re-read of `src/pve_cpi` confirmed each
"Shipped." claim against the real control flow, with file-and-function citations now folded
into each section (the "In this codebase" / "Shipped" blocks). Two things changed as a
result of going deeper. First, the verification surfaced honest caveats the original
DONE-prose had glossed — fail-open windows, method-class-global (not per-call) timeouts,
text-pattern pushback fragility, post-import (not pre-commit) checksum, reactive-only orphan
GC — now recorded as "Limits" under each feature. Second, the wider reference re-read turned
up **ten new gaps (§7.26–§7.35)**, none of which appeared in the prior report; **all ten are now
shipped** — they are extensions of the shipped work (enforce the invariants §7.9 records, monitor
the resizes §7.24 sizes, make the polling §7.25 fixed adaptive, and so on). **This round went
further:** a fresh, independent re-extraction of all six references against the now-shipped
§7.1–§7.35 surfaced **six more genuinely new gaps (§7.36–§7.41), all still open**, clustered on a
theme the prior rounds under-weighted — *cross-process and in-flight-operation safety*: an
unguarded HA-rule read-modify-write under parallel deploys (§7.36), a racing concurrent template
clone (§7.37), and an unguarded `delete_disk` against a locked volume (§7.38), plus three
operability/hardening items (SDN eventual-consistency §7.39, an ephemeral ≥2×RAM invariant §7.40,
and dispatcher log redaction §7.41). The §3 matrix gained one correction (Azure `update_disk` is a
full method, now `Y`, with a third documented refusal — account-type conversion), and the §6
standout list was corrected against source in several places this round: Azure's per-backend-pool
LB/App-Gateway assignment is **restored** (it is real, in `vm_manager_network.rb`); the §7.30
transport-tuning model is **re-attributed to Alicloud** (the OpenStack *Go* CPI parses
`ConnectionOptions` but never applies it to an `http.Transport`); and §7.29's agent-checksum model
is the **Ruby** OpenStack CPI, not the Go one in this reference set. (The earlier-round finding that
process-level panic recovery (§7.4) is *not* unique to OpenStack-Go — Google has it too — still
holds.) A new **§9** records the cross-cutting dimensions this analysis still under-covers
(concurrency model, observability, config surface, measured performance, test strategy,
failure-mode taxonomy). The remainder of this summary is the original framing that motivated
§7.1–§7.25, retained for provenance.

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

¹ **Corrected this round.** Azure `update_disk` was previously marked `~` (partial). A
source re-read shows a full implementation (`cloud.rb:460`, `disk_manager2.rb:49`) for
managed disks — size grow, IOPS, and MBPS — with three deliberate, documented refusals
(caching mode is creation-time-only, disk shrink is rejected, and account-type/tier
conversion raises `NotSupported`, `cloud.rb:494-500`). It is a complete method with
constraints, not a stub, so it is now `Y`.

Two other re-reads were checked against source and the existing cells held: AWS
`calculate_vm_cloud_properties` is real (`cloud_v1.rb:419`, maps `cpu`/`ram`/
`ephemeral_disk_size` → `instance_type` + `ephemeral_disk`), so it stays `Y`; Alicloud
`calculate_vm_cloud_properties` is genuinely an empty stub (`action/calculate_vm_properties.go`
returns `NewVMCloudPropsFromMap(nil)`), so it stays `~ (empty)`. OpenStack-Go `get_disks` was
confirmed a real handler (`cpi/methods/get_disks.go:26`). Google `set_disk_metadata` and
`update_disk` were confirmed absent in source (previously inferred).

Takeaway, reconfirmed across six references and re-verified against source: **surface
coverage is a settled strength.** PVE implements all 22 canonical methods with real logic
plus the `update_disk` extension, and is one of only two CPIs (with vSphere) to implement
network lifecycle. Every remaining improvement is depth within methods, not new method stubs.

## 4. What the PVE CPI Already Does Well

Stated explicitly so the roadmap does not regress these. Items marked **(new since
prior report)** were recommendations in the previous roadmap that have since shipped.

- **Live node scoring at `create_vm` (new).** A weighted filter-and-scorer over live
  cluster facts (free-memory fraction, free-storage fraction, CPU headroom, inverse
  guest density) with repeatable-random tie-break and AZ filtering — the direct
  analogue of the vSphere placement pipeline, and richer than any cloud CPI's
  flavor/AZ selection.

- **Dual anti-affinity (new).** Both a soft scoring penalty and cluster-enforced PVE HA
  negative resource-affinity rules keyed on the BOSH instance group, self-cleaning on
  delete, with an optional strict mode — the analogue of vSphere DRS anti-affinity.

- **AZ-to-node mapping (new).** BOSH `availability_zone` maps to a restricted candidate
  node set; a missing AZ is a hard error, matching AWS/Google/Azure AZ selection.

- **DLB / CRS integration (new).** Opt-in PVE 9.2+ Dynamic Load Balancer registration
  with version, cluster-size, and shared-storage guards — a continuous-rebalance
  equivalent of vSphere DRS that no cloud CPI has, correctly delegated to the platform.

- **Pre-create IP-conflict detection (new, partial coverage).** Parallel cluster scan
  for duplicate static `ipconfig` IPs on the target bridge. (Covers CPI-managed static
  IPs; see §6 for the DHCP/foreign-device half still open.)

- **Per-VM/per-NIC firewall and security groups (new).** Cluster-level firewall group
  references attached to VMs via the PVE firewall API, with per-NIC override.

- **Dispatcher hook middleware (new).** A zero-cost-when-unused `Before`/`After`
  middleware chain with a static registry and config validation — the vSphere
  cpi_plugin analogue (currently one built-in hook: audit logging).

- **Full v2 contract** including `info` with `api_version=2`, `disk_hint` return on
  `attach_disk`, and `network_info` return on `create_vm`.

- **Three agent-delivery modes** — cloudinit/configdrive, registry, noagent — matching
  AWS's registry-optional design and exceeding vSphere (env ISO only).

- **Three stemcell modes** — heavy tarball, light pre-uploaded, light fetch over
  HTTPS/S3/BOSH-blobstore/OCI — with magic-byte format detection and `os.Root`
  extraction sandboxing; a broader fetch surface than AWS light AMIs.

- **Mature, differentiated retry infrastructure.** Three distinct backoff curves
  (transient, storage-lock, combined) with exponential growth, ±30% jitter, caps, and
  `ctx.Done()` short-circuit; VMID-conflict allocate-retry; async UPID task polling
  with retriable poll-fault handling. At parity with Azure and beyond OpenStack-Go.

- **PVE-aware fault classification.** The error mapper classifies SDK 4xx/5xx, net
  timeouts, and — uniquely — Perl `die()` strings inside UPID task bodies (storage-lock
  timeout, pmxcfs race, LVM timeout, VMID conflict, clone-source-missing,
  snapshot-blocked, volume-missing). No reference CPI is this platform-aware because
  PVE leaks failure detail as strings in task bodies rather than HTTP codes.

- **Richest error taxonomy in the set.** `TypeCloud`/`TypeRetriableCloud` plus 14
  specialized typed errors — richer than AWS, Google, or OpenStack-Go.

- **`create_vm` rollback.** A rollback defer (stop + purge, idempotent on 404,
  transient-retry-wrapped, survives caller cancel via context-without-cancel) — matching
  Azure parallel cleanup, vSphere delete-on-NSX-failure, and OpenStack delete-ports.

- **Per-call `request_id` propagation** threaded through context into every structured
  log line and the dispatcher — matching vSphere and Azure.

- **Clone-mode intelligence.** Auto linked-vs-full clone by storage-backend capability,
  with cross-node shared-storage handling and per-disk format negotiation
  (qcow2/raw/vmdk with block-storage auto-omit) — exceeds the reference set's clone
  handling.

- **Snapshot-aware disk guards.** Attach/detach/resize pre-flight checks for active
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
  pattern), state-free placement stickiness; on-demand DRS anti-affinity (`AntiAffinityRuleSpec`)
  and VM-host affinity (`VmHostRuleInfo`) rule automation; storage-policy (PBM)
  compatible-datastore discovery; **`cpi_plugins` pre/post hooks on every method with plugin
  rollback on post-hook failure** (the model §7.19 adopted); IP-conflict pre-detection;
  **adaptive, progress-aware task polling** — interval `= (elapsed·100/progress − elapsed)/5`
  clamped 1–10s, so a slow clone is polled less often (the model for new gap §7.28);
  **HA/vSAN-aware delete delay (a 15s sleep before removal to avoid quorum alarms)**;
  primary-plus-fallback disk placement with retry on `GenericVmConfigFault` (the model for
  new gap §7.31); multi-cluster placement with datastore fallback; a vCenter custom-field
  created as a distributed mutex (platform-native locking).

- **AWS** — three-way AZ consensus (volume + subnet + vm_type), but applied *piecemeal*
  per operation (`create_disk` picks an AZ from the instance, `create_vm` re-checks via
  `common_availability_zone`), not as a single monolithic gate; per-disk `iops`/`throughput`/
  type and KMS encryption; **`VMCreationFailed.new(retryable)` signaling that gates fallback
  decisions** (a bidding failure is non-retriable→fall back to on-demand, an instance failure
  is retriable→retry) — `DiskNotAttached(retryable)` exists in the broader taxonomy but is
  not actively raised by the AWS CPI; ELB + ALB target-group registration **at create only,
  with no delete-side deregistration** (contrast §7.19's rollback contract);
  `wait_until_running` is a **waiter that raises `VMCreationFailed(retryable)` on timeout**
  (not Azure-style serial-console capture); an `AbruptlyTerminated` retry loop (up to 2×) for
  launch races (the model for new gap §7.31); **`fast_path_delete`** that tags and returns
  without polling for terminated state (the model for new gap §7.32); a volume-modification
  wait loop (`ResourceWait.for_volume_modification`, the model for new gap §7.27).

- **Azure** — managed-disk caching/tier baked into the disk CID; **Compute Gallery
  stemcell replication that is automatic at the platform layer** — the CPI calls
  `ensure_gallery_image_in_target_location` only to *validate* replicas exist, it does not
  orchestrate the push (the "validate, don't orchestrate" lesson §7.2 follows); the gallery
  path also computes a streaming SHA-256 of the image, stores it as an `image_sha256` tag, and
  hard-errors on a same-version content mismatch (`compute_gallery_manager.rb:129-169`) — a
  version-collision guard §7.13's provenance work parallels; three-level
  Gallery namespacing (gallery/image-definition/version) with stemcell reference-counting
  tags for idempotent re-upload; `keep_failed_vms` forensic mode (the §7.20 model); parallel
  rollback on create failure; telemetry + per-request-ID correlation; **native `update_disk`
  (size grow / IOPS / MBPS) that explicitly rejects caching-mode changes, disk shrink, and
  account-type/tier conversion as `NotSupported`** (the creation-time-invariant model behind
  §7.26); optional Disk Encryption Set (BYOK) per managed disk; and **per-backend-pool
  load-balancer and Application-Gateway assignment at create** (`vms/vm_manager_network.rb:91-147`
  — `_get_load_balancers` maps each vm_type LB config to a single LB backend pool and joins it
  to the NIC), with **no deregistration on delete** (`vm_manager.rb:288-387` deletes the NIC and
  IPs but never detaches the pool) — the create-only LB posture §7.19's rollback contract is the
  on-prem answer to. *(This restores a claim an earlier round withdrew for lack of a citation;
  the logic lives in `vm_manager_network.rb`, not `cloud.rb`/`vm_manager.rb`.)*

- **Google** — LB target-pool/backend-service registration at create; cross-project
  (XPN) networking; **operator-controlled local-SSD opt-in (`ephemeral_disk_type: local-ssd`)
  plus CPU-aware custom machine types** (corrected — there is no automatic SSD auto-scaling
  by machine series); remote-tarball SHA-1 verification before import (the verify-before-write
  model §7.6's caveat aspires to); an operation waiter with exponential backoff capped at
  **~782s (≈13 min: `maxTries=100`, `maxSleepExponent=3`)** — corrected from "~1100s";
  multi-zone disk-set conflict rejection; **process-level panic recovery
  (`main/main.go:29`, `defer logger.HandlePanic`)** — see §7.4; VM `cloud_properties` that
  override network-level defaults (the cross-property-override model for new gap §7.34).

- **OpenStack-Go** — `allowed_address_pairs` + VRRP port check for in-deployment VIPs
  (the §7.14 model), re-applied idempotently on re-attach (a refinement §7.14 does not yet
  cover); LB pool membership automation with metadata tracking and cleanup; shuffled
  multi-AZ with per-failure next-AZ fallback (§7.10); stale Neutron-port cleanup on retry
  (a cloud-specific network-orphan pattern with no direct PVE analogue, since PVE networks
  are cluster-wide); process-level panic handler (§7.4 — *not* unique to OpenStack-Go);
  **a single global `state_timeout`** on async operations — coarser than PVE's per-method-class
  envelope (§7.15), which is a PVE strength to claim. *(Two earlier attributions to this CPI are
  withdrawn after a source re-read: it does **not** implement transport/connection-pool tuning —
  `ConnectionOptions` is parsed at `config.go:51` but is never consumed by any `http.Transport` —
  so §7.30's model is Alicloud, not OpenStack-Go; and it injects **no** agent checksum into
  user-data — that capability lives in the Ruby OpenStack CPI, outside this Go reference set — so
  §7.29 is a PVE-originated mechanism.)*

- **Alicloud** — **`ClientToken` that is *regenerated* on `IdempotentFailed`** (a new token
  on collision), distinct from AWS/Azure same-token retry — the two-response idempotency model
  feeding new gap §7.33's contrast; classic SLB + modern NLB dual integration (both registered
  at create, both deregistered at delete) with backward-compatible structured/flat manifest
  forms; KMS image-copy encryption; capacity-reservation tag mapping from `env.Bosh.Group`;
  proactive ENI cleanup on delete; an `Invoker`/`Catcher` abstraction giving **per-operation
  retry budgets** (15–60 retries × 5–15s, tuned per call) rather than a few global curves (the
  finer-grained take on the shipped §7.25); multipart parallel image upload (`oss.Routines(5)`,
  5 MB parts — the model for new gap §7.35); callback-based state-machine polling that embeds per-operation
  recovery (auto-stop, cleanup) inside the poll loop; and explicit SDK transport tuning — a
  shared `http.Transport` with `MaxIdleConns: 500` and an env-tunable `TLSHandshakeTimeout`
  (`alicloud/common.go:99-110`, `config.go:278`) — the actual model for new gap §7.30,
  corrected from OpenStack-Go.

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
in a lab where maintenance windows are frequent. **Limits.** Fail-open means a flapping HA
API can momentarily leave a draining node eligible; the tag list is maintained separately
from PVE HA state and has no auto-cleanup when a tag is removed.

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
time substantially. **Limits.** Use is opportunistic — the CPI consumes a replica if one
exists but does not itself create it on every node; there is no replica lifecycle
(re-sync after a stemcell change, periodic GC) beyond the sha-tag sweep on delete.

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
VM-disk fault-domain co-location as a hard invariant (AWS three-way AZ consensus, Google
rejects multi-zone disk sets, Azure migrates a regional disk into the VM's zone).

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
hard fault-domain invariant every cloud CPI enforces. **Limits.** Bare legacy CIDs carry no
metadata and therefore impose no constraint (backward-compatible, but unprotected);
backend classification is best-effort (a `/cluster/storage` fetch error fails open, dropping
the AZ constraint); and there is no runtime check that the disk volume still physically
exists on the chosen node.

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
**Comparative note (corrected this round).** PVE is *not* the only-or-second CPI with this:
Google also installs process-level recovery (`main/main.go:29`, `defer logger.HandlePanic`),
and OpenStack-Go has it too. PVE's placement at the *handler* boundary is architecturally
stronger — the process survives and the director can retry *other* methods — but the
recovered value is returned non-retriable, so panic recovery prevents process death; it
does not make the failed operation succeed.

#### 7.5 PARTIAL — Active IP-conflict probe (ARP / guest-agent) for DHCP and foreign devices

*References: vSphere, OpenStack-Go, AWS, Alicloud.* The shipped detector scans static
`ipconfig{N}` entries only; its source notes it cannot detect DHCP-assigned addresses
and does not see physical hosts, containers, or non-PVE devices. The documented CF
NATS-churn incident was caused by exactly that uncovered half — a BOSH VM IP that also
answered ARP from a physical device on the shared LAN. The detector covers the easy half
and leaves the half that bit the deployment uncovered.

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
mitigated by the isolated-SDN migration.

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
silent corruption into a clean, retriable failure. **Limits (verified this round).**
Verification is client-side and *after* download/extraction, so a deterministically corrupt
source re-verifies to the same bad hash on retry (unlike Google's server-side
verify-before-write); and the retriable network-mismatch path has no internal exponential
backoff — it relies entirely on director-level retry. This is the open half that new gap
§7.29 (boot-path agent integrity) and the verify-before-commit lesson address.

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
relocate to a slow pool after a node failure or migration. It is the exact vSphere
`DirectorDiskCID` (`<uuid>.<base64url-json>`) pattern. **Limits.** Legacy bare CIDs still
parse, carry no metadata, and therefore impose no §7.3 constraint; the metadata is advisory —
PVE API calls always use the bare volid extracted from the encoded form.

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
reference CPIs ship the chain (vSphere PBM, OpenStack default volume type, Azure vm_type
root/ephemeral, Google per-root/per-disk).

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
on a shared pool. The CID-carried encoding (`disk_performance.go` resolves at create, attach
decodes) is the load-bearing design choice: `attach_disk` receives only `vm_cid`/`disk_cid` under
CPI v2, so options that must survive a detach/re-attach cycle have nowhere else to live but the
disk CID metadata.

#### 7.10 DONE — Multi-AZ candidate spread with next-AZ fallback

*References: OpenStack-Go, AWS, vSphere.* `availability_zone` is a single string yielding
one candidate set; if that AZ is full, all in maintenance (§7.1), or yields no viable
candidate, `create_vm` fails rather than spilling to a sibling AZ. Small on-prem clusters
(three nodes modeled as three single-node AZs) are exactly where one AZ being full or
drained is routine; OpenStack demonstrates shuffled multi-AZ with next-AZ retry.

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
to a sibling AZ instead of failing the deploy, with no sequential per-AZ retry delay.
**Limits.** The legacy singular form yields no AZ fallback (only the ultimate `config.node`
fallback); the `config.node` fallback is silent (debug log) and steps outside the AZ
topology; the transient-vs-permanent decision on an exhausted run is the §7.23 heuristic.

#### 7.11 DONE — Retryability-flag boundary audit across all 22 handlers

*References: AWS, Google, Alicloud.* The taxonomy and the error mapper exist, but not
every error return is confirmed to set the retriable bit on the correct boundary
(prior-roadmap item, still open). Mis-signaling is deploy-cost-bearing: a transient
fault mis-classified as permanent fails a whole CF deploy; a permanent fault
mis-classified as retriable burns the director's retry budget. Directly relevant given
the documented NATS-churn, wedged-task, and orphan-VM fragility.

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
**Limits (verified this round).** Retryability is a single binary flag: an intra-CPI retry
loop (`RetryOnTransient`, `AllocateWithRetry`) that exhausts its budget surfaces the *last*
attempt's classification — which may have flipped from retriable to non-retriable — and
there is no coordination between the CPI's own retry budget and the director's.

#### 7.12 DONE — Post-boot guest-agent / VM health verification

*References: AWS, Azure.* `create_vm` returns once the start task completes; it never
verifies the VM booted or that the QEMU/BOSH agent is reachable. The documented emptyvm
pre-start NATS hazard (agent dead from a long synchronous apt wedging the pre-start
canary) and the orphan-VM duplicate-IP incident are exactly the class a post-boot health
gate would surface earlier with actionable diagnostics. AWS babysits with
`wait_until_running`; Azure captures boot diagnostics/serial console.

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
cluster accumulates cruft from interrupted operations.

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
template config is byte-identical to prior releases.

`delete_stemcell` now always performs a best-effort cross-node sweep: it resolves the
stemcell sha8 from the primary template, then deletes every template across the cluster
carrying `bosh-stemcell-sha-<sha8>` (discovered via `/cluster/resources`), covering
replicas created by §7.2. Errors are warned, never fatal.

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
exactly this.

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
rather than silently leaving an unprotected VIP.

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
finer-grained than OpenStack-Go's single global `state_timeout`. **Limits (verified this
round).** The budget is method-class *global* (all `create_vm` share one timeout, no
per-stemcell override); inner retry loops (`AllocateWithRetry`) are not deadline-aware; and
a nil-error success is returned even if the deadline just fired — the envelope only acts on
a non-nil error.

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
already-saturated node. A limit of 0 is unlimited and byte-identical, so the throttle is purely
additive insurance for high-fan-out deploys.

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
MTTR for a misconfigured cluster. **Limits.** Strict mode is opt-in (default off for
backward compatibility); unknown-key detection is top-level only, because `cloud_properties`
are intentionally free-form maps the CPI does not prescribe.

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
reference hosts by domain rather than bare IP. **Limits.** Containment is an `IP ∈ CIDR`
check only — it does not confirm the netmask and gateway are mutually consistent with the
range — and is skipped entirely when no range is declared (dynamic networks).

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
into failure. **Limits.** Every hook except the rollback contract is best-effort (failures
are logged, not propagated); `external_command` is synchronous with no shell or stdin
(quick actions only); and the catalog is static — hooks are registered from config names at
start-up, with no dynamic discovery.

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
stranding a second failed VM. **Limits.** It preserves only the final post-clone attempt
(VMID-allocation retries still clean up their throwaways), tagging is best-effort, and there
is no automated reaper — preserved VMs accumulate until an operator deletes them.

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
failure) versus a preferred pin (HA may relocate off-AZ to keep the VM running). Because it
reuses the existing HA-rule plumbing and is default-off, the guarantee is opt-in and
byte-identical when unset.

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
`create_vm` failure at create time rather than as a task-join hang. **Limits.** The MBus
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
runs. **Limits (verified this round).** Classification is heuristic — a rejection string not
in the whitelist is conservatively treated as permanent; there is no internal timeout, so
retries continue for as long as the director (or the §7.15 envelope) allows.

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
means a template that is not 5 GiB no longer gets mis-grown. **Limits.** The dedicated ephemeral
disk is local to its node, so it is not part of any fault-domain or live-migration set — a guest
with a local-NVMe ephemeral disk cannot be migrated off its node without dropping that disk.

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
it for a fast lab, without recompiling. **Limits.** Config is read once at startup (no live
re-tune); `max_attempts ≤ 0` silently reverts to the built-in default;
curves are pure and deterministic except for seeded jitter.

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
(§7.26–§7.35) have since been shipped** (the still-open work this round is §7.36–§7.41, below).
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
code path enforces it. Azure makes this explicit: `update_disk` rejects a caching-mode change
as `NotSupported` (caching is creation-time-only), and AWS waits out a volume modification
before treating it as applied.

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
power-loss event. **Limits.** Only the three structural keys (`diskPerfInvariantKeys`,
`disk_performance.go:194`) are enforced — throttle and discard drift is deliberately allowed since
PVE can change those on a live device — and because the §7.9 merge pins CID-recorded values, the
only divergence that ever fires in practice is a global config newly introducing a structural
option the disk lacked at creation.

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
a subsequent BOSH disk operation observes a settled size. **Limits.** It is strictly best-effort
and non-failing by design — the helper is void and on timeout merely logs a warning and returns
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
round-trips on parallel deploys without lengthening tail latency. **Limits.** The benefit only
materializes for PVE operations that actually populate the UPID `progress` field (clone,
move-disk) — short or progress-less tasks fall back to the exact §7.25 fixed cadence — and the
feature depends on a vendored-SDK addition (`Status.Progress` + single-shot `GetStatus`), guarded
only by a compile-time assertion that breaks the build, rather than silently degrading, if a
vendor refresh drops it.

#### 7.29 DONE — Boot-path agent integrity / checksum verification

*Reference: the Ruby OpenStack CPI (mechanism PVE-originated).* §7.12 pings the guest agent
and §7.6 verifies the *stemcell* digest (post-import, per its own caveat). Neither verified
that the BOSH **agent binary** inside the booted guest is the expected one — a tampered or
partially-written agent passed both checks. The Ruby OpenStack CPI injects an expected agent
checksum into the configdrive for boot-time self-verification (the OpenStack **Go** CPI in this
reference set does not, so PVE's approach below is its own).

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
compromised or corrupt agent never enters a deployment. The pin is a single SHA-256 string the
operator updates whenever the expected agent changes, and it is a no-op until set.

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
into a bounded, retriable error instead of a long hang. Defaults are the SDK no-op, so a
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

This is the post-selection analogue of the §7.10 pre-selection multi-AZ fallback, closing
the gap observed in vSphere (primary-plus-fallback placement list, retries on
`GenericVmConfigFault`) and AWS (up to two retries on `AbruptlyTerminated`).

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
different operations.

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
resource. The classification rule is: regenerate when the collision means *taken*; retry
the same identity when it means *in flight*. PVE's model falls firmly in the first
category.

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
the escape hatch for one-off placement onto a bridge the resolver would not otherwise pick.

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
clean.

### Newly identified gaps (latest round)

These six surfaced from a fresh, independent re-extraction of all six reference CPIs against the
now-shipped §7.1–§7.35 work. None duplicates an existing item; each maps to a real PVE primitive
and remains **open**. They cluster around one theme the prior rounds under-weighted — *cross-process
and in-flight-operation safety* — which §8 names directly. They follow the same additive-optional
convention: validate only when set, omit from VM config when empty, zero behavior change for
existing manifests. Ordered by value, not severity.

#### 7.36 DONE — Cross-process cluster mutex for HA-rule and anti-affinity mutation

*References: vSphere, Azure.* vSphere serializes every DRS-group / anti-affinity reconfiguration
behind a platform-native distributed mutex: `DrsLock#with_drs_lock` creates a vCenter custom field
by name and relies on vCenter raising `DuplicateName` if it already exists, polling every 0.5 s for
up to 600 s and releasing by deleting the field (`drs_rules/drs_lock.rb:16-58`). Azure takes the
same posture with an OS advisory `flock` on `/tmp/azure_cpi/<lock>` wrapped (EX/SH/NB) around
availability-set get-or-create, gallery-image create/update, and user-image create
(`helpers.rb:564-575`, `vm_manager_availability_set.rb:35,65`,
`compute_gallery_manager.rb:149,262,270,340`). Both treat shared cluster-config edits as a critical
section across concurrent CPI processes.

The shipped PVE HA work (§7.21 node-affinity pin, dual anti-affinity rules) mutates cluster-wide
`/etc/pve` HA state with an unguarded read-modify-write: `placement_antiaffinity.go` lists HA rules,
deletes the group rule, then re-registers it, and `placement_nodeaffinity.go` likewise lists then
rewrites the pin. There is no cross-process lock anywhere on this path — the only mutexes in the
tree (`dispatcher.mu`, `rollback.mu`, `inflightSems`) are in-process, and `max_inflight_per_node`
(§7.16) is a per-process semaphore. Under a parallel deploy the BOSH director runs many
`create_vm`/`delete_vm` CPI invocations as **separate processes**, each racing the same
`bosh-aa-<group>` rule: process A reads the member set, process B reads the same stale set, both
delete-and-recreate, and the last writer wins — silently dropping a VM from its negative-affinity
rule. pmxcfs is a replicated filesystem with last-write-wins semantics, so this is a real data-race
on safety-critical placement state, not a theoretical one (the lab notes already record a pmxcfs-race
and duplicate-stemcell-template class of bug).

**Build:** add an opt-in `pve.cluster_lock_mode` (`off` default → byte-identical; `flock` to enable).
When enabled, wrap HA-rule and anti-affinity mutation (`ensureAntiAffinityRule`,
`ensureNodeAffinityPin`, and their delete-side cleanup) in a cluster-wide critical section keyed on a
stable lock name (e.g. `bosh-cpi-ha`). The natural PVE primitive is a create-or-fail sentinel under
`/cluster`: take a config-object whose creation PVE rejects with a conflict if it already exists
(mirroring vCenter's `DuplicateName`), poll-acquire with jittered backoff up to a bounded
`cluster_lock_timeout_sec` (default ~60 s), and release on a `defer`. Because pmxcfs serializes config
writes cluster-wide, an alternative is a lock file under `/etc/pve` taken via the API with a
TTL-stamped owner token so a crashed holder self-expires. Keep the unguarded path as the default so
existing deployments are unchanged.

**Shipped.** Both mechanisms landed, both opt-in and byte-identical when unset. A pool-sentinel
cluster lock (`internal/pve/cluster_lock.go`: `AcquireClusterLock`/`Release` over POST/DELETE/GET
`/pools`, poolid `bosh-lock-<name>`, comment `owner=<token> exp=<unix>`) wraps the per-group
anti-affinity read-modify-write: create-or-fail is the test-and-set, a dup whose recorded expiry has
passed is stolen (delete+recreate) with a post-steal owner re-read to refuse a displaced handle, and
release is deferred so it fires even on a mid-RMW error. The matcher is **fail-closed** — an error
that cannot be positively classified as duplicate is mapped to a retriable acquire failure rather
than wrongly assumed to mean "lock held". A read-after-write **verify** re-lists the rule and asserts
the VMID is present; an absent member returns `TypeRetriableCloud`. Both the anti-affinity and the
node-affinity create_vm call sites now propagate that retriable class to the director (selectively —
a generic HA-API blip stays fail-open per §7.21), so the spread/pin guarantee is re-driven rather
than silently lost. Knobs: `pve.cluster_lock_mode` (`off`|`pool`), `pve.cluster_lock_timeout_sec`,
`pve.antiaffinity_verify`. The node-affinity pin is per-VMID (no cross-group RMW), so it takes the
verify but intentionally skips the coarse lock. *Live-validation caveat:* the exact PVE status/text
for a duplicate poolid and the comment round-trip are inferred from the API shape and pmxcfs
serialization; unit tests assert the contract against a fake `PoolService`, and a true multi-process
race must be validated on a live cluster.

#### 7.37 DONE — Adopt-and-wait on a racing concurrent template clone (clone-target-exists)

*References: vSphere, Azure.* vSphere's `Stemcell#replicate` clones a per-datastore replica and, when
a parallel CPI is already replicating the same stemcell, catches the resulting `DuplicateName` and
converts it to *find-by-path + wait-for-snapshot* of the in-progress replica rather than erroring
(`stemcell.rb:90-100`). Azure's `_get_user_image` takes an EX `flock` around **both** the existence
GET and the create specifically so process 2 never VM-creates against an image process 1 is still
building (`stemcell_manager2.rb:138-169`). Both convert a concurrent-create collision into a safe
wait on the winner's artifact.

This is distinct from the shipped §7.35 (which parallelizes one CPI's own replication loop) — §7.35
governs intra-process concurrency, not the cross-process race. On PVE, when two independent
`create_vm` invocations target a node lacking the stemcell replica, both consult the per-node replica
tag (`ResolveTemplateVMIDForNode`, `template.go:441`), both find it absent, and both issue a clone for
the same template onto the same node. The second clone hits a VMID conflict, and today the create_vm
allocation loop treats VMID conflicts as retriable jitter — it allocates a *fresh* VMID and clones
again, producing a duplicate, half-built replica template instead of waiting for the winner. The
orphan-template and duplicate-stemcell-template hazards in the lab notes are exactly this failure
mode.

**Build:** in the replica-ensure path (`ensureTemplate` / the per-node replica build consumed by the
scorer), when a clone or template-create returns a target-exists conflict for the *replica name/tag
we were about to create* (as opposed to an unrelated guest VMID collision), branch to adopt-and-wait:
poll for the per-node replica tag (`bosh-stemcell-node-<node>` + `bosh-stemcell-sha-<sha8>`) to appear
and the template to leave clone-in-progress state, bounded by `replica_adopt_timeout_sec` (default
~300 s), then return the adopted replica. Distinguish a replica-name collision (adopt) from a guest
VMID collision (the existing retry-jitter path) so create_vm's allocation loop is unchanged. With a
single CPI process the conflict never fires, so behavior is byte-identical; the new code only changes
the multi-process race outcome from "duplicate orphan template" to "wait for the winner".

**Shipped.** PVE allocates a fresh VMID for every clone, so a duplicate-replica collision is invisible
at the VMID-allocation layer (two losers pick different VMIDs and both succeed) — the Azure/vSphere
"catch DuplicateName" hook has no PVE analogue. Instead the in-flight winner is observed directly: a
replica VM carries its identity tags (`bosh-stemcell-sha-<sha8>` + `bosh-stemcell-node-<node>`) from
creation, but `Template` flips true only after the freeze and the guest config `lock` reads
`clone`/`create` while the build is in flight. A new primitive `pve.AdoptReplicaTemplate`
(`internal/pve/replica_adopt.go`) scans for that mid-build VM via `findReplicaCandidate` — which, unlike
the settled-only `ResolveTemplateVMIDForNode`, does *not* require the `Template` flag — and polls it to a
settled template (frozen and unlocked), bounded by `pve.replica_adopt_timeout_sec`. The scan prefers a
settled candidate over any lower-VMID unsettled orphan, so a crashed-mid-build remnant cannot shadow a
genuine adoptable template. The probe is wired into the per-node replica build (`replicateOneNode`,
`create_stemcell.go`) **before** the qcow2 upload: on adoption the node skips upload + clone entirely
(no duplicate, no orphaned upload); a winner that never settles within the bound yields a
`TypeRetriableCloud` that the best-effort replication loop logs and skips (re-driven next deploy).
Distinguishing a replica collision from a guest-VMID collision is structural — the adopt probe keys on
the replica tag set, while the create_vm allocation loop's VMID-conflict jitter is untouched. The knob
defaults to 0 (disabled): the probe call site is skipped entirely, so single-process and pre-existing
behavior is byte-identical. The residual sub-second TOCTOU window between a not-found probe and the
caller's own clone is shrunk but not eliminated (no cross-process lock is taken on this path; the
optional `cluster_lock_mode=pool` primitive from §7.36 could close it but is not wired here).

*Live-validation caveat:* the `clone`/`create` lock strings and the list endpoint's per-VM `lock`/`tags`
fields are exercised against a fake PVE client asserting the contract; a true multi-process replica
race and a stuck-lock winner (which requires operator `qm unlock` recovery, noted in the spec) need a
live cluster to validate.

#### 7.38 DONE — Pre-delete lock/status guard on `delete_disk` against in-flight volume operations

*References: Google, OpenStack.* Google's disk delete refuses when `disk.Status` is neither `READY`
nor `FAILED`, returning an error rather than racing a `CREATING`/`RESTORING` disk
(`google/disk/google_disk_service_delete.go:19-21`). OpenStack's `delete_disk` rejects unless the
volume status is `available`, 404-skips an already-gone volume, then waits for terminal `deleted`
(`delete_disk.go:42-66`). Both gate destruction on the resource being quiescent.

PVE's `delete_disk` has no such precondition: `HandleDeleteDisk` goes straight to the `imgdel` call
wrapped only in `RetryOnTransientOrLock` (`delete_disk.go:94`). Retry-on-lock recovers from a
*transient* storage lock, but it does not guard against freeing a volume whose owning VM is
mid-operation — a `qm clone`, a `qm disk move`/`storage migrate`, a backup, or a snapshot rollback
that holds the VM `lock` config field. Freeing the backing image out from under such an operation can
corrupt the in-flight task or leave storage inconsistent. PVE exposes the signal directly: the owning
guest's config carries a `lock` field (`backup`, `clone`, `migrate`, `snapshot`, `rollback`).

**Build:** add an opt-in `pve.disk_delete_state_guard` (`off` default → byte-identical; `on` to
enable). When enabled and the volume resolves to an owning VM, read that VM's config `lock` field
before freeing; if it is set to a destructive/in-flight value, treat it as *retriable* so the
director re-drives the delete after the lock clears (mirroring §7.27's convergence posture), or
fail-fast with a clear non-retriable error. Keep the existing 404-idempotent skip. With the guard off,
current behavior is preserved exactly.

**Shipped.** Opt-in `pve.disk_delete_state_guard` (`off` default → byte-identical; `on` to enable).
When enabled, `HandleDeleteDisk` runs `pve.GuardDiskDeleteState` between node resolution and the
`imgdel` call. The critical design point: the VMID baked into a managed volid name
(`<storage>:vm-<VMID>-disk-<N>`) is only the allocation-time placeholder this CPI assigns at
`create_disk` — BOSH attaches the volume to a different guest without renaming it, so the guard
resolves the *currently-attached* VM by scanning VM configs for the volid (`FindVMByDiskVolid`),
**not** by parsing the name. It then reads that VM's `lock` config field; a destructive/in-flight
value (`backup`, `clone`, `migrate`, `snapshot`, `rollback`, `create`) yields a `TypeRetriableCloud`
error so the director re-drives the delete after the lock clears (mirroring §7.27's convergence
posture). The guard is best-effort and fails open on every uncertainty: a disk attached to no VM (the
normal pre-delete state), an attachment-resolution failure, a config-read error, or a 404 all pass
straight through, so an enabled guard can never convert a hiccup into a delete failure. The existing
404-idempotent skip and the `RetryOnTransientOrLock`-wrapped `imgdel` are unchanged; with the guard
off (the default) no attachment lookup runs and behavior is byte-identical. Residual: the
check-then-delete window is inherently best-effort (a lock taken between the guard read and the
`imgdel` is not caught), and if the attached VM is left with a stuck config lock the delete defers
until an operator runs `qm unlock <vmid>` — the deferral log names the VM, node, and lock.
Live-validation caveat: exercised against fakes asserting the resolution + lock-classification
contract; the real attached-VM-mid-operation race needs a live PVE cluster.

#### 7.39 OPEN — Eventual-consistency retry resolving a freshly-created SDN vnet/bridge

*Reference: vSphere.* vSphere wraps network lookup in `find_network_retryably` — `Bosh::Retryable`
with 62 tries (~10 min) on `NetworkNotFoundError` — explicitly to tolerate the lag between a portgroup
being created and becoming queryable cluster-wide (`vcenter_client.rb:225-239`). The CPI assumes a
freshly-created network is not immediately resolvable on every host and polls until it converges.

PVE's SDN has the identical eventual-consistency property but no corresponding wait. `create_network`
stages a zone/vnet/subnet and calls `applySDN` to push the config to the data plane
(`create_network.go:266,378`), but returns as soon as the apply task is accepted — it does **not**
poll for the vnet to materialize as a usable bridge on the node where the next `create_vm` lands. On
the consume side, `create_vm` takes the bridge name purely from config/cloud_properties and writes it
into `netN=` with no existence check. SDN apply is asynchronous and per-node (`ifupdown2` reload plus
pmxcfs propagation), so a `create_vm` immediately following a `create_network` on a different node can
attach a NIC to a bridge that does not yet exist there — the VM boots with a dead NIC, or `qm start`
fails, surfacing as a flaky, deploy-order-dependent failure that disappears on retry. This is exactly
the lab reality the SDN-network gap note flags.

**Build:** add an opt-in `pve.network_resolve_retries` (default 0 → byte-identical) and a companion
`network_resolve_timeout_sec`. When set, after `applySDN` succeeds in `create_network`, poll
`/cluster/sdn` (status `available`, no `pending`) and/or `/nodes/{node}/network` until the target
vnet/bridge is resolvable, bounded by the timeout. On the consume side, optionally gate `configureNICs`
in `create_vm` with the same bounded retry resolving the per-NIC bridge before writing `netN=`,
classifying a not-yet-present bridge as retriable so the director re-drives rather than booting a NIC
into the void. With retries at 0 the apply-and-return behavior is unchanged.

#### 7.40 OPEN — Ephemeral-disk minimum-size invariant (≥ 2× RAM) on `create_vm`

*Reference: OpenStack.* OpenStack's flavor resolver rejects an `instance_type` whose
`flavor.Ephemeral > 0` but is smaller than `(RAM/1024)*2`, enforcing that the ephemeral disk has
headroom for agent swap plus `/var/vcap/data` (`flavor_resolver.go:78-86`). The invariant encodes a
hard truth: the BOSH agent places swap (sized to RAM) and the data partition on the ephemeral disk,
so an ephemeral disk smaller than ~2× RAM cannot satisfy the agent's own layout.

PVE's §7.24 ephemeral path (`resolveEphemeralShape`, `create_vm.go`) sizes the dedicated ephemeral
disk straight from `ephemeral_disk_size_mb` (rounded up to GiB) and resolves the pool, but asserts
**nothing** about the size relative to VM RAM. An operator who configures a 2 GiB ephemeral disk on an
8 GiB-RAM job gets a VM whose agent cannot lay down its 8 GiB swap file — the agent's ephemeral-disk
setup fails at boot, or swap silently does not activate, producing the exact ephemeral-space boot
failure §7.24's root-resize logic already guards against on the *root* disk but not on the *ephemeral*
disk. This is a cheap, high-signal pre-flight invariant the shipped code omits.

**Build:** add an opt-in `pve.ephemeral_disk_min_ratio` (default 0 → no check, byte-identical;
conventional value 2). When set and an ephemeral disk is being created, compute the resolved ephemeral
GiB against the VM's configured memory (already available in the create_vm shape) and, if
`ephemeral_gib < ratio * (memory_mb/1024)`, either fail-fast with a clear non-retriable cloud error
naming the deficit, or warn — operator's choice via an `enforce|warn` knob mirroring §7.26's
`disk_perf_invariant_mode`. This reuses the §7.26 enforce/warn pattern verbatim. With the ratio at 0
nothing changes.

#### 7.41 OPEN — Secret redaction over the dispatcher request/response log path

*References: AWS, Google.* AWS clones-and-redacts instance params and spot specs (`user_data`, access
keys) before logging (`instance_manager.rb:36-41`, `spot_manager.rb:28`). Google wraps every
request/response/error byte stream in `redactor.RedactSecrets` before debug logging at the dispatch
boundary (`api/dispatcher/json.go:70,112,142,161,180`). Both treat the CPI's own structured logs as an
untrusted sink for agent settings and credentials.

The PVE CPI handles the same sensitive payloads — `create_vm` receives the agent env (an mbus URL with
embedded NATS credentials, blobstore secrets, a registry endpoint), and the configdrive/cloud-init
user-data is assembled from it. Today the dispatcher logs only `method`, `request_id`, and
`duration_ms` (`dispatcher.go`), not the argument tree, so nothing leaks at the default level — but
there is **no redaction primitive anywhere in the `log`/dispatcher layer**. The CPI is therefore one
debug-level `log` statement (or one well-meaning "log the request to triage a stuck deploy" change)
away from writing the mbus password and blobstore credentials to a log BOSH ships to syslog and the
director's debug bundle. This is a latent hazard, not a live leak — which is exactly when a cheap
guardrail is worth adding ahead of need.

**Build:** add a structured redaction helper in the `log` package and call it at the dispatcher
boundary for any request/response payload logging, gated by an opt-in `pve.redact_logs` (default off →
byte-identical; recommend on). The scrubber masks known-sensitive keys/paths in the CPI argument tree
— `mbus`, `blobstore.options.secret_access_key`/`password`, `registry` credentials, and any
`env`/`agent` settings blob — replacing values with `<redacted>` while preserving structure. No PVE
primitive is required; this is pure log hygiene. Validate-only-when-set, omit from the ERB when empty.

### Explicitly not recommended as core CPI work

| Item | Demonstrated by | Why not |
|------|-----------------|---------|
| Per-disk / per-image encryption toggle | AWS, Azure, Alicloud | At-rest encryption is a PVE storage-backend property (ZFS/LUKS/Ceph), not a per-volume API. Select it via §7.8 (point a vm_type at an encrypted storage), mirroring OpenStack/Google delegation. |
| Spot / preemptible / capacity-reservation | AWS, Google, Azure, Alicloud | Cloud-economic constructs with no PVE primitive. |
| Floating / elastic IP as a standalone primitive | AWS, OpenStack, Azure, Alicloud | No PVE elastic-IP primitive. Deliver the value via §7.14 (self-hosted VRRP VIP) and §7.19 (external hook). |
| Routes / advertised-routes / `source_dest_check` | AWS, Google | Routing lives in the guest OS, the SDN zone (EVPN/OSPF via frr), or the fabric — not a CPI-layer route table. |
| Multi-region / cross-cluster stemcell manifest | AWS, Alicloud, Azure | A CPI instance targets one PVE cluster; "region" has no PVE primitive. The in-cluster analogue is §7.2. |
| CPI-driven live-migration / rebalance loop | vSphere | Delegated to PVE DLB/CRS; a CPI is a stateless per-call process with no daemon for a control loop. |
| `ClientToken` idempotency keys | Alicloud | VMID allocate-with-retry already gives create-once semantics on PVE's reservation model. |
| In-process distributed tracing (OTel/Jaeger) | (none ship it) | A single-shot per-RPC CLI has no long-lived span; `request_id` is the correct correlation point. |
| First-class metrics/telemetry pipeline | Azure, vSphere | Wrong layer; BOSH (bosh-monitor, Prometheus) owns this. Offer as an optional hook only. |

## 8. Cross-CPI Engineering Lessons

These are the transferable principles behind the specific gaps — the "why," distilled from
reading six mature CPIs against this one. They are worth keeping in view independent of any
single feature.

- **Enforce creation-time invariants; do not merely record them.** Azure rejects a
  caching-mode mutation outright. PVE records disk-performance attributes in the CID (§7.9)
  but no path rejects drift on re-attach (§7.26). Metadata is only as good as the code that
  checks it.

- **Error classification is a control signal, not telemetry.** AWS's
  `VMCreationFailed.new(retryable)` and its two spot-failure classes (bidding → fall back,
  instance → retry) *drive* the fallback decision. PVE's §7.23 classifier should likewise
  gate post-selection fallback (§7.31), not just tag the error. The retriable bit is an
  instruction to the director, so its boundary is a correctness concern (§7.11).

- **Idempotency has two correct responses to a collision.** Regenerate identity when the
  collision means "taken" (Alicloud's token, PVE's VMID); retry the same identity when it
  means "in flight" (AWS/Azure tokens). Pick by meaning, and state the rule (§7.33) — the
  wrong choice either loops forever or duplicates the resource.

- **Verify before commit beats verify after commit when the platform allows it.** Google
  validates the image SHA server-side before the import writes it; PVE computes the hash
  *after* import (§7.6), so a deterministically corrupt download re-verifies to the same bad
  hash and enters the dedup cache. The same logic motivates boot-path agent integrity
  (§7.29).

- **Progress-aware polling beats fixed backoff for long platform operations.** vSphere's
  ETA-proportional interval and Alicloud's per-operation retry budgets adapt to the specific
  operation; fixed curves (§7.25) pay a polling-storm tax exactly when load is highest
  (§7.28).

- **Delegate to the platform — validate, don't orchestrate.** Azure's Compute Gallery
  replication is platform-automatic; the CPI only checks that replicas exist. PVE's §7.2
  correctly *uses* replicas opportunistically rather than driving replication itself, and DLB
  (CRS) is delegated to PVE entirely. Do not reimplement what the platform already does; gate
  on its result.

- **Hooks-vs-hardcoding is a real tradeoff, and on-prem favors hooks.** AWS bakes ELB/ALB
  registration into `create_vm` (fewer failure modes, but create-only, no deregister). PVE
  made LB integration a post-hook with a rollback contract (§7.19) — more failure surface,
  but the right call when the load balancer is an operator-chosen HAProxy/keepalived rather
  than a managed cloud primitive.

- **The wedged-wait hazard has two complementary fixes; keep both.** Bound the wait (the
  §7.15 timeout envelope) and skip the wait (the §7.32 fast-path delete) cover different
  operations against the same queue-slot incident class. One is not a substitute for the
  other.

- **A coarse global bound is worth less than a per-class one.** OpenStack-Go uses a single
  `state_timeout`; PVE's per-method-class envelope (create 30m / delete 15m / has-get 2m,
  §7.15) is the more useful granularity — a strength to preserve, not flatten.

## 9. Dimensions Still Under-Compared (what this analysis is missing)

The report is exhaustive on the *capability/feature* axis — every CPI method and every standout
feature has been mapped. It is thin on several cross-cutting dimensions that determine whether the
shipped gaps actually hold up under load. These are the honest blind spots of the current analysis,
recorded so the next round can close them.

- **Cross-process concurrency and cluster-config safety.** The feature-by-feature framing missed
  two distinct multi-process races (§7.36 HA-rule read-modify-write, §7.37 racing template clone)
  because they are not features — they are emergent properties of running N director-spawned CPI
  processes against one pmxcfs-replicated cluster. Five of the six references ship an explicit
  cross-process lock (vCenter custom-field, Azure `flock`); the report scatters the concern across
  §7.16/§7.21/§7.35 instead of walking every cluster-wide mutable resource (HA rules, anti-affinity
  rules, node-affinity pins, replica templates, VMID allocation, SDN config) and asking "what happens
  when two CPI processes touch this at once?" A dedicated concurrency-model subsection is the missing
  artifact.

- **Observability and operability.** There is no observability column in the matrix. The references
  uniformly carry request-ID correlation, secret-redaction at the log boundary (AWS, Google),
  telemetry emission (Azure), and structured retry-audit logs, yet the report never compares what
  each CPI logs at `create_vm`, how it redacts agent credentials, whether it emits metrics, or how an
  operator debugs a stuck deploy. The §7.41 redaction gap fell straight out of this blind spot.

- **Config-surface growth.** The optional knobs are described one feature at a time but never as a
  whole. With ~40 additive-optional properties now accumulated, there is no enumeration of the total
  config surface, no comparison of how each CPI structures cross-field validation, and no assessment
  of the discoverability/maintenance cost as the surface grows — a real risk worth an explicit
  subsection and possibly a generated config reference.

- **Performance envelope, measured.** Adaptive polling (§7.28), transport tuning (§7.30), and
  parallel replication (§7.35) are asserted as wins but not *measured* comparatively. There is no
  steady-state API-call count per `create_vm`, no parallel-deploy throughput figure, and no
  quantification of the polling-storm tax on a K-node cluster — the e2e timing data captured in the
  project notes is not folded into this analysis.

- **Test strategy.** The comparison is silent on how each CPI is tested (Ruby rspec vs Go
  table-tests vs live integration). The PVE CPI's own discipline — TDD, `-race`, adversarial review,
  an 85%+ coverage gate — is a genuine differentiator the report never states, nor does it compare
  how each CPI tests the hard concurrency/idempotency paths this very synthesis shows are the
  highest-risk surface.

- **Consolidated failure-mode taxonomy.** Errors are classified retriable/non-retriable
  (§7.11/§7.23) but never assembled into one table mapping each failure mode (partial create,
  orphaned resource, split-brain placement, corrupted in-flight operation) to the gap that addresses
  it. Such a table would make the Tier-1 correctness items legible as a coherent safety story rather
  than a list — and would have surfaced §7.36–§7.38 a round earlier.

