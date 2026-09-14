# Multiple NFS shares: research, updated design, and implementation plan

The implementation follows this design, and the [implementation plan](../plans/multi-storage-placement-plan.md) records the work packages and gates. The research and adversarial revision dated 2026-09-08 establish complete ephemeral coverage and replaceable operator-selected algorithms as first-release requirements.

We recommend adding **named storage sets**. Each set names existing PVE storage IDs through an explicit list or a regex. The operator chooses a versioned placement strategy for that set. The base configuration binds one set to persistent disks and a separate set to VM-lifetime storage: root disks and any dedicated ephemeral disks. Root placement and cloning are required in the first release; otherwise VMs that obtain guest ephemeral space from their root disk would escape the policy. Operators may explicitly bind root disks to another set when they want that separation.

This supports a deployment with ephemeral disks spread across three exports and persistent disks confined to one export, or spread across a separate set. A disk remains wholly on one export. Distribution of disk allocations does not stripe the blocks of an individual disk, combine share capacity into one larger disk, or replicate its data. Multiple exports on one appliance also do not necessarily provide independent performance or failure domains.

The release acceptance case is **P = one or more persistent NFS exports; E = a separate group of ephemeral NFS exports**. Every new persistent disk is allocated in P; every new VM root and dedicated ephemeral disk is allocated in E. A VM's root and dedicated ephemeral disk are co-located by default, so the strategy distributes VM storage bundles across E. Existing persistent disks retain their identity and location. Stemcell source/base images and ConfigDrive ISO files are infrastructure artifacts with explicit storage settings, not deployment data disks. Their placement and reachability still require validation.

The proposal extends the storage-tier and capacity work recorded in [the CPI comparison](../cpi-comparison-matrix-and-analysis.md), especially its single-pool headroom limitation, while preserving the current scalar configuration and disk lifecycle. The [implementation plan](../plans/multi-storage-placement-plan.md) defines the ordered work packages and release gates for this design.

## 1. What vSphere actually does

We inspected upstream `cloudfoundry/bosh-vsphere-cpi-release` commit `2f09844150ccceec13ff9aaaf030c05185e826f2`, downloaded to `~/w/fivetwenty/studios/cloudfoundry/src/bosh-vsphere-cpi-release`. Source links below are pinned to that revision. This is an inspected upstream snapshot, not a claim about every released CPI version.

| Mechanism | Observed behavior | Lesson for this CPI |
| --- | --- | --- |
| Global datastore patterns | Separate `datastore_pattern` and `persistent_datastore_pattern` select eligible names. | Keep VM and persistent placement independently configurable. |
| Per-resource `datastores` | VM types/extensions and disk types accept exact datastore names, plus datastore-cluster entries. Exact names are escaped when converted into a regex. | Offer explicit lists as well as regex discovery; do not treat literal names as regexes. |
| Global datastore-cluster patterns | `datastore_cluster_pattern` and `persistent_datastore_cluster_pattern` discover StoragePods. The picker expands SDRS-enabled pods into member datastore names and unions them with the respective datastore pattern. | A named set can provide a grouping abstraction over existing PVE storage IDs. |
| Explicit datastore clusters | The ephemeral picker expands all eligible pods. The persistent picker first chooses one eligible pod using randomized free space, then adds its members to explicit datastore names. Disabled SDRS pods are excluded. | These are distinct upstream paths, not one universal cluster-selection algorithm. |
| Allocation | Matching, accessible, non-maintenance datastores with adequate free space are ranked by a stable random value multiplied by free space. | Separate eligibility from strategy, and retain ranked alternatives. |
| VM placement | The VM pipeline evaluates disk feasibility within each compute cluster, prefers less persistent-disk migration, then storage balance and host-group memory. | Select storage with compute accessibility and existing disk locality in view. |

The configuration surface is documented in [BOSH vSphere usage](https://bosh.io/docs/vsphere-cpi/). The exact cluster-expansion and precedence behavior comes from [StoragePicker](https://github.com/cloudfoundry/bosh-vsphere-cpi-release/blob/2f09844150ccceec13ff9aaaf030c05185e826f2/src/vsphere_cpi/lib/cloud/vsphere/storage_picker.rb) and [StorageList](https://github.com/cloudfoundry/bosh-vsphere-cpi-release/blob/2f09844150ccceec13ff9aaaf030c05185e826f2/src/vsphere_cpi/lib/cloud/vsphere/storage_list.rb).

The normal allocation pipeline in this snapshot performs CPI-side selection over concrete datastores. It is not simply a call asking Storage DRS to place every disk. [DiskPlacementSelectionPipeline](https://github.com/cloudfoundry/bosh-vsphere-cpi-release/blob/2f09844150ccceec13ff9aaaf030c05185e826f2/src/vsphere_cpi/lib/cloud/vsphere/disk_placement_selection_pipeline.rb) requires free space greater than disk size plus 1 GiB of headroom, with an additional maximum-swapfile term for ephemeral placement. Its `random * free_space` ranking is capacity-biased randomness, not round-robin and not a categorical draw with probabilities exactly proportional to free space. [VmPlacementSelectionPipeline](https://github.com/cloudfoundry/bosh-vsphere-cpi-release/blob/2f09844150ccceec13ff9aaaf030c05185e826f2/src/vsphere_cpi/lib/cloud/vsphere/vm_placement_selection_pipeline.rb) accounts for the disks considered within a candidate VM placement, including existing disks and alternatives.

For persistent creation, [cloud.rb](https://github.com/cloudfoundry/bosh-vsphere-cpi-release/blob/2f09844150ccceec13ff9aaaf030c05185e826f2/src/vsphere_cpi/lib/cloud/vsphere/cloud.rb#L775) restricts candidates to VM-accessible datastores when given a VM hint, otherwise to datacenter-accessible datastores. Explicit placement intent is encoded in the Director disk CID. There are also actual SDRS recommendation calls in the [stemcell path](https://github.com/cloudfoundry/bosh-vsphere-cpi-release/blob/2f09844150ccceec13ff9aaaf030c05185e826f2/src/vsphere_cpi/lib/cloud/vsphere/stemcell.rb); that is separate from the ordinary disk picker.

Storage DRS/vMotion may subsequently move attached disks. BOSH documents both that support and a recovery limitation when the VM holding a moved disk is accidentally deleted. This is a separate lifecycle feature from initial distribution. [BOSH Storage DRS and vMotion support](https://bosh.io/docs/vsphere-vmotion-support/).

**Design conclusion.** Copy the separation between membership, feasibility, placement, and durable identity. Implement initial allocation policies first. Automatic movement of existing disks needs its own migration design.

## 2. PVE model and current CPI baseline

PVE already supports multiple NFS storage definitions. Each has a storage ID, server/export, and optional mount path; its NFS backend mounts the export. The CPI should select those IDs rather than mount exports inside the CPI process. NFS cloning/snapshot support depends on image format. [Official PVE NFS documentation source](https://github.com/proxmox/pve-docs/blob/master/pve-storage-nfs.adoc).

Cluster-wide storage configuration does not guarantee access from every node. The `nodes` property restricts use, `disable` disables a storage, and content types constrain its purpose. PVE warns against multiple storage IDs aliasing the same image storage. [Official PVE storage documentation source](https://github.com/proxmox/pve-docs/blob/master/pvesm.adoc).

We inspected local baseline `ed12da155940d6d03ac422b6b78f0e91c73b9669`.

| Existing component | Current behavior / change required |
| --- | --- |
| [config.go](../../src/pve_cpi/internal/config/config.go) | `vm_storage`, `disk_storage`, `stemcell_storage`, and `iso_storage` are scalars. `storage_tiers` matches attributes. Keep these settings compatible. |
| [storage_tier.go](../../src/pve_cpi/internal/cpi/handlers/storage_tier.go) | Resolves a tier to the lexicographically first matching storage. It is discovery plus deterministic selection, not distribution. Reuse matching predicates without silently changing old tier behavior. |
| [create_vm_shape.go](../../src/pve_cpi/internal/cpi/handlers/create_vm_shape.go) | Root storage is resolved after node selection. Explicit `storage_pool` precedes tier/global fallback. Move set feasibility before node commitment. |
| [create_vm_disk.go](../../src/pve_cpi/internal/cpi/handlers/create_vm_disk.go) | Dedicated ephemeral disk has separate pool/tier settings. Its legacy unencrypted path checks tier before pool and defaults to global `VMStorage`, which can differ from resolved root storage. Preserve this on the legacy path. |
| [create_vm_placement.go](../../src/pve_cpi/internal/cpi/handlers/create_vm_placement.go), [placement/facts.go](../../src/pve_cpi/internal/placement/facts.go) | Storage facts/headroom are centered on global `VMStorage`. Replace the single-pool assumption for set-enabled requests with per-node, per-storage facts. |
| [create_disk.go](../../src/pve_cpi/internal/cpi/handlers/create_disk.go) | `storage_pool` / legacy `storage`, tier, then global persistent default. Resolve a set together with the VM/node hint before allocation. |
| [storage_utilization.go](../../src/pve_cpi/internal/cpi/handlers/storage_utilization.go) | Existing ceiling checks can fail open on unavailable facts. Preserve legacy behavior, but require usable facts for new automatic set placement. |
| [storage_info.go](../../src/pve_cpi/internal/pve/storage_info.go) | Already provides shared classification, node restrictions, content, and backing identity. Extend canonical parsing for disabled state and use one inventory snapshot. |
| [disk_identity.go](../../src/pve_cpi/internal/cpi/handlers/disk_identity.go), [parker.go](../../src/pve_cpi/internal/pve/parker.go) | Stable identity already resolves current volume location. Parker allocation still receives a single `DiskStorage` for collision scanning; multi-storage support must cover this too. |
| [configdrive.go](../../src/pve_cpi/internal/agent/configdrive.go) | ISO-following currently resolves against global VM storage. Make resolution request-local using the selected root backing, while retaining explicit fixed ISO support. |

## 3. Options and recommendation

| Option | Benefits | Costs / decision |
| --- | --- | --- |
| Add plural lists and patterns beside every existing scalar | Small surface for one deployment; familiar vSphere shape. | Repeats membership and policy across roles/profiles. Reasonable small implementation, but harder to operate at scale. |
| Extend existing storage tiers with membership and strategies | Reuses a known name and selection interface. | Current tier semantics include alphabetical choice and encryption assertions. Requires careful versioning to avoid moving allocations merely on upgrade. |
| Add named storage sets, reuse tier predicates internally | Separates physical membership from class attributes; reusable across VM/disk types; explicit opt-in. | A small new abstraction and resolver. **Recommended.** |
| Build a storage-balancing controller with persistent reservations | Can coordinate strict distribution and move existing allocations. | We reject this option because it introduces a continuously running controller and migration behavior that the base case does not need. |

Use the same storage-set type for all roles. A singleton set is a valid pin. A set is a CPI configuration object, not a PVE resource pool, storage plugin, RAID group, or Storage DRS service.

## 4. Proposed configuration contract

The implementation of these keys is under review on the `multi-storage-placement` branch. Live certification remains required before release. Existing global scalars remain required in the first release for compatibility and explicit stemcell/legacy consumers. A global role-set binding defines both the allocation default and the allowed placement boundary. It overrides the corresponding global scalar for new allocations. Unbound roles retain legacy behavior.

```yaml
properties:
  pve:
    vm_storage: nfs-ephemeral-01 # legacy default and shared stemcell base target
    disk_storage: nfs-persistent-01
    stemcell_storage: nfs-images
    iso_storage: nfs-iso
    iso_storage_follow_vm_storage: false
    storage_placement_namespace: production-director-pve # stable across restarts
    storage_allocation_journal_dir: /var/vcap/store/pve_cpi/allocations

    storage_sets:
      ephemeral-nfs:
        names: [nfs-ephemeral-01, nfs-ephemeral-02, nfs-ephemeral-03]
        types: [nfs]
        shared: true
        strategy: {name: spread, version: 1}
        min_free_mb: 10240
        max_utilization_pct: 85
      persistent-nfs:
        names: [nfs-persistent-01, nfs-persistent-02]
        types: [nfs]
        shared: true
        strategy: {name: weighted_free_space, version: 1}
        min_free_mb: 20480
        max_utilization_pct: 80
      persistent-pinned:
        names: [nfs-persistent-01]
        types: [nfs]
        shared: true
        strategy: {name: spread, version: 1}

    ephemeral_storage_set: ephemeral-nfs # root + dedicated ephemeral, together
    persistent_storage_set: persistent-nfs
    require_disjoint_storage_sets: true
```

Operators register these exports in PVE first. Disk targets require `images`; the chosen import/ISO stores need their respective content capabilities. The numbers above illustrate explicit reserves and ceilings, not performance sizing advice.

An alternative membership definition replaces `names` with `name_pattern: '^nfs-ephemeral-[0-9]+$'`. Require exactly one of `names` or `name_pattern`; reject empty lists, duplicate names, invalid regexes, and unknown strategy names/versions. Require an explicit strategy object on each set so upgrades cannot change an implicit default. Reject unknown new configuration keys, wrong types, blank selectors, and fractional/negative sizes instead of allowing the legacy resolver to treat malformed values as absent. `_mb` means MiB; validate positive bounded ceilings (1–100), nonnegative reserves, integer overflow, and allocation rounding. Omitted set reserve is zero; omitted set ceiling adds no limit beyond applicable global constraints.

Match case-sensitive PVE storage IDs with Go RE2 syntax. Explicit names are literal. Regexes use Go search semantics, so examples are anchored; Ruby-specific vSphere regex syntax is not automatically portable. Predicates `types` and `shared` narrow membership by intersection. First-release sets support shared NFS only; omitted predicates effectively require NFS/shared and contradictory values are errors. Missing explicitly named storage is a configuration error with all missing IDs listed. A present but temporarily inactive member can be excluded while others remain eligible. No eligible member returns a diagnostic error; an API failure is never a successful empty discovery.

Resolve patterns from live cluster inventory once per CPI operation. Freeze and log the resulting membership for that operation. A new matching export becomes eligible on the next operation; removing an export from membership prevents new placement there but does not change existing disk identity.

`require_disjoint_storage_sets` defaults to true when both persistent and ephemeral global bindings are present. Validate their resolved memberships by storage ID and known physical backing before new allocations; include any explicit root binding in the VM-lifetime side. A regex that starts matching the opposite role's backing fails validation. Explicit false permits intentional overlap with combined capacity accounting. Neither setting can discover unknown NAS-level aliases; see the capacity-domain limitation below. A singleton persistent override inside P is valid and does not conflict with this rule.

Deployments choose policies through normal cloud-config VM types/extensions and disk types:

```yaml
vm_types:
- name: worker-spread
  cloud_properties:
    cpu: 4
    ram: 8192
    ephemeral_disk_size_mb: 51200
    ephemeral_storage_set: ephemeral-nfs

disk_types:
- name: database-pinned
  disk_size: 102400
  cloud_properties:
    storage_set: persistent-pinned
- name: database-spread
  disk_size: 102400
  cloud_properties:
    storage_set: persistent-nfs
```

An instance group selects `vm_type: worker-spread` and either disk type. Both persistent choices use the same resolver. No new deployment-manifest top-level property is needed. Spread distributes new allocations selected by that deployment; it does not require the CPI to discover a complete deployment topology or guarantee an exact per-instance-group count.

The first release uses the following role rules:

| Input / resource | Effective behavior |
| --- | --- |
| Global or VM `ephemeral_storage_set` | Select a root-plus-dedicated-ephemeral bundle from this set unless root is explicitly overridden. |
| Global or VM `root_storage_set` | Optional separate root selection; dedicated ephemeral selection still uses its own set. Defining this opts out of bundled placement. |
| Global `persistent_storage_set` | Default and placement boundary for every new persistent allocation, including calls without a VM hint. |
| Disk cloud property `storage_set` | Choose another operator-defined set within the global persistent boundary; a singleton pins the disk. |
| No dedicated ephemeral disk requested | Select the root through the ephemeral binding; keep current guest disk layout and sizing. |
| Stemcell imports/base templates and ISO | Explicit infrastructure targets; never reinterpret their existing identifiers as deployment disk selectors. |

If an explicit root override is used while the guest obtains ephemeral space from root, that space follows the root override. Diagnostics must state this exception. The two-set base example uses no root override and therefore covers both disk layouts. The existing VM `storage_pool` remains a root selector; existing `ephemeral_storage_pool` remains a dedicated-disk selector. Such explicit selectors split the bundle when allowed by the boundary check.

Resolve role selectors as an atomic choice at each existing configuration layer, using [cloudprops_resolver.go](../../src/pve_cpi/internal/cpi/handlers/cloudprops_resolver.go)'s established layer ordering. A higher-layer explicit scalar must override a lower-layer set, and a higher-layer set must override a lower-layer scalar. Do not independently search all layers for each key, which can let a lower-layer scalar defeat a higher-layer set. Reject multiple competing selectors for the same role at one layer when a new set selector participates. Legacy-only calls retain their current precedence, including the ephemeral tier-before-pool behavior. Global role set precedes that role's global scalar. A failed explicit set never falls back to an unrelated scalar.

After resolving precedence, enforce the global boundary on every effective scalar, tier, or set selection for a bound role. Overrides may narrow membership or use a different strategy through another named set whose resolved members are a subset of that boundary; they cannot expand it. Diagnose an out-of-bound legacy profile instead of ignoring it or silently weakening the boundary. A globally configured root set establishes a separate root boundary; otherwise root inherits E. Cloud-config merging performed by BOSH happens before the CPI sees the request; CPI validation must operate on that effective request and its own local profile layers, not pretend to reconstruct the original manifest layers. This is placement policy, not an authorization mechanism against someone who can edit CPI configuration.

The global boundary also supplies minimum reserve/maximum utilization constraints that resource-level sets cannot relax. Every role with no set binding keeps legacy behavior. During opt-in, preflight existing VM/disk types, extensions, compilation settings, errands, resurrection, and `create-env` configuration for conflicting selectors. Global scope covers every call served by that CPI configuration; a separate `create-env` or other CPI configuration must carry the same bindings to get the same behavior.

**Partial opt-in and validation scope.** A creation operation enters set mode when any disk it creates resolves to a set or is constrained by a global role boundary. Validate and journal the complete creation operation, including its legacy-selected companion disks and infrastructure artifacts. Shared-NFS membership restrictions apply to set-constrained disk roles; they do not silently prohibit a supported legacy companion backend. Joint capacity accounting still includes every planned write. A persistent-only global binding adds the future-access check to `create_vm`, but does not by itself change an otherwise legacy VM allocation into a set-managed allocation. An unused set definition does not activate set mode. Static schema errors fail configuration validation; live inventory, set discovery, disjointness, and allocation-journal readiness checks belong to allocation or explicit preflight. Existing CID lifecycle operations must remain available when an unrelated current set is empty, unavailable, or requires journal recovery.

**Cluster configuration and identity.** A cpi-config entry can replace the complete storage-set and capacity-domain maps and supply its role bindings, status-age policy, and stable placement namespace. Deep-copy the effective policy before validation. When changing clusters with active inherited bindings, require explicit set definitions and a namespace for that cluster. Resource cloud properties cannot change these cluster-level boundaries. Keep the journal directory process-level and reject request attempts to redirect it. Use the operator's stable namespace as the allocation authority; record a verified cluster identity separately from API endpoint aliases. A connection URL change must not create a new allocation namespace. Reject a namespace that is rebound to another cluster while live or unresolved records remain. If stable cluster identity cannot be established, require explicit audit rather than infer a new authority from an endpoint string.

Encryption requirements must remain enforceable. Sets may declare `encrypted: true` as the same operator assertion used by current tiers; this does not ask the CPI to encrypt storage or prove NAS encryption. Effective `encrypted: true` on persistent/ephemeral allocation requires both the selected set and any global role-boundary set to carry that assertion. A scalar/tier override inside a bound role must pass its existing encryption checks and remain within that asserted boundary. Reject conflicting encryption assertions on policy aliases for the same storage ID. Do not infer encryption from NFS, accept an unverified arbitrary scalar, or silently weaken existing checks. Log the existing operator-responsibility notice. Root inherits placement membership but does not gain a new encryption guarantee merely from bundling. Add encrypted-set success, contradiction, and override tests to milestone 1.

## 5. Strategies and their guarantees

| Strategy | Definition | Intended use |
| --- | --- | --- |
| `spread`, v1 | Use rendezvous hashing to rank unique eligible backings by descending SHA-256 of a length-prefixed tuple `(algorithm/version, CPI namespace, allocation key, allocation group, backing key)`; canonical backing key breaks exact ties. | Approximately equal counts of VM bundles or persistent disks over many allocations; stable ordering within retries and limited remapping when membership changes. Recommended ephemeral choice. |
| `weighted_free_space`, v1 | Let usable bytes be the smaller of physical free bytes after reserve and bytes allowed by the utilization ceiling. Require room for the allocation group. Set each weight to remaining usable bytes after allocation, floored at one byte for exact fits. Sample a full ordering without replacement proportional to these weights. | Prefer shares with room to grow; recommended persistent choice. Explicitly different from vSphere's random-times-free-space formula. |
| `least_utilized`, v1 | Choose the lowest projected used/total ratio after allocation; rendezvous hash breaks ties. | First-release alternative for operators prioritizing fill ratios; deterministic choice can concentrate concurrent arrivals. |
| Strict round-robin / least-count / failure-domain anti-affinity | Requires synchronized counters or reservations and defined deployment/group identity. | Not supported by this stateless allocation design; reject these strategy names. Do not simulate exact balance with process-local counters. |

Implement all three stateless strategies in the first release. This gives the operator a real choice between count distribution, capacity-biased distribution, and projected utilization. They select the algorithm in `storage_sets.<name>.strategy` and can change it without changing membership or handlers. A deployment that needs a different strategy on the same exports can reference another named set with the same members and different strategy. Duplicate backings are forbidden within a single set, not across such policy aliases; allocation still operates on unique physical candidates.

For weighted sampling, use an exponential race. Generate one pseudorandom `u` strictly between 0 and 1 for each eligible backing, rank ascending by `-ln(u)/weight`, and break exact ties canonically. V1 uses a 256-bit seed generated once from `crypto/rand`, then HMAC-SHA-256 keyed by that seed over the canonical strategy/namespace/allocation/backing tuple. Convert its leading 52 bits `x` to `u = (x + 1) / (2^52 + 1)` using float64. Inject the seed in tests. Freeze weights and draws for that planning attempt; rechecks may reject candidates but cannot redraw until a safe new planning attempt. Golden tests fix encoding and ordering; never rely on Go's process-global RNG or its default seed. Reject nonfinite/invalid calculations. Log the plan seed/fingerprint for replay in diagnostics, not metric labels.

V1 encodes tuple fields as UTF-8 bytes, each preceded by its unsigned 32-bit big-endian byte length. Include the allocation group in weighted draws as well as spread scores. Hierarchical draws use distinct domain and member tags, with the domain or canonical backing identity in the final field. Reject oversized fields. Freeze exact field order and algorithm/version strings in shared golden fixtures so adding an algorithm cannot silently change existing encodings.

Use BOSH `agent_id` as the VM allocation key within the required `storage_placement_namespace`, with allocation group `vm_bundle` for co-located root and ephemeral disks. Explicitly split roles get `root` and `ephemeral` keys. Namespace must distinguish CPI instances sharing exports and remain stable across restarts. A recreation with a new agent ID can map differently. For persistent creation, generate one allocation UUID before selection and retain it across internal attempts. Derive the journal's disk correlation token from that UUID, and use it as the stable disk ID only on the existing parked-disk path. Free-floating disks retain their existing volume-based CID semantics. A fresh `create_disk` call supplies size, cloud properties, and an optional VM hint, not a general idempotency key. Do not deduplicate independent same-size calls by VM ID. [BOSH create_disk contract](https://bosh.io/docs/cpi-api-v2-method/create-disk/).

None of these policies promises equal bytes, IOPS, latency, or an exact 1/N share for a small deployment. Default bundled placement measures VM bundle count on E; a VM with two transient disks is not two independent spreading decisions. No guest RAID or multipath configuration is implied. If one member cannot fit a whole bundle, reject it even when root and ephemeral would fit separately on different members; independent placement requires an explicit root or dedicated-ephemeral selector.

### Strategy extension and change contract

The planner owns feasibility and recovery; strategies only order its eligible candidates. Implement an internal registry keyed by `(name, version)` with the following narrow interface:

```go
type Strategy interface {
    Rank(RequestSnapshot, []EligibleCandidate) ([]RankedCandidate, error)
}
```

The snapshot identifies the allocation and its group. It also carries the total requested bytes, capacity facts, reserves, ceilings, and deterministic entropy. Candidates have canonical identities and immutable facts. Rank returns every input candidate exactly once, ordered, with diagnostic score/reason. Validate that invariant centrally. Strategies cannot add storage, remove policy constraints, call PVE, mutate inventory, allocate disks, migrate data, or decide whether a task is safe to retry. Strategy errors fail the operation rather than substituting another algorithm.

Adding an algorithm means registering an implementation and schema, adding golden/property/strategy-contract tests, and documenting its guarantees. Handlers must have no switch on strategy names. Built-in algorithms require a CPI binary update; operator selection among installed algorithms is configuration-only. Arbitrary executable scripts and dynamically loaded Go plugins are out of scope.

Changing the strategy or membership applies to new operations after the configuration is rendered/deployed. Read configuration once per operation and retain an immutable policy fingerprint through task completion and reconciliation; no in-flight switch. Log configured name/version, fingerprint, chosen storage, and any fallback. Existing disks never move because a strategy changes. Rollback restores the earlier strategy for future allocations. A changed algorithm meaning requires a new version; reject unknown versions before mutation, and include downgrade validation. Do not use a mutable set label alone to reconstruct an interrupted operation.

Store separate fingerprints for the caller's creation intent and the effective placement policy. The intent includes normalized creation arguments, including explicit selectors, but excludes secrets and mutable global strategy definitions. A retry with the same intent can recover the recorded operation after a global policy edit. Recovery first returns or reconciles already-created resources using the recorded policy and concrete identities. Before submitting any remaining mutation, require that the recorded target still satisfies the current global boundary and infrastructure safety constraints. A newly prohibited target blocks continuation for audit; recovery must neither switch targets nor ignore a newly restrictive boundary. Retain the recorded ranking and seed for allowed continuation.

Golden tests must fix canonical encoding, input-order independence, strategy version behavior, tie handling, one-to-one rankings, and repeatability. Distribution tests distinguish approximate count balance from utilization and use injected deterministic inputs. Strict round-robin remains a separate coordinated feature; operators must see that distinction in configuration documentation and dry-run output.

## 6. Placement and capacity algorithm

Introduce a pure `internal/storageplacement` package for selector expansion, eligibility, ranking, and per-backing capacity accounting; keep API calls and mutations in adapters/handlers.

1. Normalize all effective role selectors and sizes before VM node selection. Parse existing disk identities and their actual storage locations. Preserve AZ, explicit-node, local-disk, network, PCI, memory, and HA constraints.

2. Fetch cluster storage definitions plus node-local storage status for relevant candidate nodes. Collect definitions once for initial discovery and all relevant status entries per node, with bounded concurrency. Revalidate selected definitions before mutation as specified below. Record active/enabled state, content, accessibility, available/total bytes, and observation time. Authorization or inventory failures must not masquerade as empty successful discovery.

3. Resolve set membership, intersect predicates, then require enabled, active, `images`-capable NFS on the candidate node. Unknown capacity/accessibility makes a new-set candidate ineligible. Report permanent configuration errors separately from transient observation failures. Legacy single-storage behavior remains unchanged.

4. Group candidate IDs by canonical `BackingKey`. Reject known duplicate backings within a set with the conflicting IDs listed. Across roles, account against the same backing once. Canonical server/export equality catches known aliases, but cannot prove that DNS aliases, nested exports, or exports on one NAS share capacity. Provide the explicit capacity-domain configuration below in this implementation; never infer independence merely from different export paths.

5. Build feasible `(node, root target, ephemeral target)` plans, co-locating the VM bundle unless an explicit root/dedicated selector splits it. Existing persistent disks must be reachable from the node; they are not newly charged capacity. Validate the fixed ISO and stemcell path too. Rank storages without multiplying a share's weight by the number of nodes that can see it. Within each AZ in the existing AZ order, apply storage ranking among feasible targets and then existing compute scoring. For split roles, rank root targets first. For each root choice, subtract its capacity charge before ranking ephemeral targets. Skip a root choice if no feasible ephemeral target remains. This explicit lexicographic rule prevents arbitrary map iteration or combining unrelated strategy scores. Generate candidate plans as needed and bound the search, rather than constructing every combination of nodes and storage pairs. Hard constraints always precede scoring. Reuse one rank per backing across nodes.

6. Aggregate new allocation bytes per physical backing: root footprint, dedicated ephemeral size, import/full-clone needs, and auxiliary writes on that backing. Use effective rounded sizes. Where root and ephemeral share a backing, validate their sum. A conservative estimate is preferable to treating a thin disk as free. Future guest growth and unrelated writers remain outside an instantaneous free-space guarantee.

7. Enforce `available - new_bytes >= reserve_bytes` and `(total - available + new_bytes) / total <= ceiling / 100`. Reserve is the maximum applicable configured minimum/headroom floor for that backing; ceiling is the strictest enabled global/set ceiling, including the global role boundary. Apply these to each concrete backing, never summed set capacity. Keep byte arithmetic checked and compute percentage comparisons without overflow. New-set constraints are hard, including when a legacy global gate is in warning mode. Do not add the same reserve separately for every role. Preserve legacy headroom semantics when the new feature is absent and document the opt-in difference. Account for actual PVE/guest requirements. The vSphere swapfile term provides research context; it does not establish that PVE creates the same per-VM swapfile.

8. Return a request-local plan containing concrete targets, exact size charges, policy source, ranking, and exclusions. Pass it into shape creation, clone/import, ephemeral allocation, and cleanup. Never mutate shared `deps.Config` to change the selected storage.

9. Recheck selected targets immediately before allocation. Retain the plan during a submitted task. Rechecks must subtract only the remaining unallocated portion. After root creation, observed usage may already include root, so charging the full bundle again double counts it. Track acquired resources and outstanding charges, accepting that thin-provisioned growth is still not reserved. Try another eligible plan only after establishing that the earlier task failed without an allocation, or after verified cleanup of its owned resources. Unknown task outcome requires reconciliation, not a second allocation elsewhere. Bound attempts by existing retry/fallback budgets.

**Definition changes and observation order.** Freeze each selected storage ID's backing identity and safety-relevant definition fields with the plan. Before each new mutation, reread those definitions as well as live status and compare them with the recorded values. A changed server/export, disabled flag, content capability, or node restriction invalidates continuation and requires a new safe plan or reconciliation. Do not reinterpret an existing volid against an ID that has been repointed. Operators must drain and audit a storage definition before repurposing its ID. These checks cannot eliminate an external configuration change between validation and API execution; no transactional lock on operator storage edits is claimed.

Retire an acquired resource's immediate allocation charge only against a status response fetched after successful task completion and volume readback. A cached response that predates allocation cannot be combined with a reduced outstanding ledger. Sparse or linked volumes may consume less than their virtual size; later growth remains outside this admission guarantee. Preserve a conservative charge or fail when the required post-completion observation is unavailable. Cleanup must remove verified owned resources before a fallback can reuse their budget. If retention policy intentionally keeps a resource from a failed attempt, stop the attempt and expose that retained resource for audit rather than silently allocating a replacement elsewhere.

For `create_disk`, use the VM's actual current node when a VM hint exists, respecting explicit constraints and migration races. Without a VM hint, choose a node that can access an eligible shared target within configured nodes/AZ restrictions. Reject an empty feasible intersection before allocating a VMID or volume. Do not assume global storage registration proves accessibility.

**Future persistent disks are a separate call.** `create_vm` receives existing disk locality hints, not the future `create_disk` request and its size/type. Therefore it cannot guarantee feasibility for an arbitrary later disk-type override. [BOSH create_vm contract](https://bosh.io/docs/cpi-api-v2-method/create-vm/). When global P exists, filter VM nodes for access to at least one eligible P member, even if no persistent disk exists yet. For the base-case rollout, preflight that every configured deployment/HA node can access every P and E member and the infrastructure targets. Later disk creation rechecks actual size and its effective subset on the VM's current node. If that request is infeasible, fail clearly instead of allocating on unreachable storage or migrating the VM implicitly. Disk-specific HA eligibility must include every permitted HA node. Only the operator may change that permitted node set; the placement strategy must not rewrite HA policy to make a target eligible.

**Capacity observations across nodes.** A shared backing may be reported multiple times with different timestamps/free space. Do not sum those values or turn each report into another candidate. Use the minimum available bytes among fresh, healthy observations for conservative ranking/admission; inconsistent total capacity for the same backing is an observation error requiring refresh. A failed node observation removes that node/backing pair, not every healthy observation. Fetch planning status live; `storage_status_max_age_seconds` defaults to 5 and accepts integers 1–60. Re-fetch after expiration, including during long cloning operations before another allocation. Diagnostics show observation ages and exclusions.

**Capacity domains.** Add optional global `storage_capacity_domains: {nas-volume-a: {members: [nfs-ephemeral-01, nfs-ephemeral-02]}}`. A domain groups different exports consuming a shared filesystem/quota budget, not aliases of the exact same image namespace. Exact image aliases remain invalid within a set. Members are literal IDs; each backing belongs to at most one declared domain. Undeclared backings each form their own domain. Reject empty domains, missing members, duplicate membership, and contradictory aliases. Preflight asks operators to declare exports sharing a capacity budget; the CPI cannot discover an unreported NAS relationship reliably.

For each declared domain, calculate conservative live limits after consolidating observations of the same backing. Use the minimum observed member total for total capacity and the minimum observed member available bytes for available capacity. Require usable observations for all declared members contributing to that envelope, or exclude the domain from new allocations. Keep per-member quota/capacity checks as well. Aggregate all planned writes to the domain, taking maximum reserve and strictest ceiling across contributing role policies. This can reject a feasible placement when export quotas differ, but does not manufacture additive capacity from a shared budget. Diagnostics show both member and domain limits.

For `weighted_free_space`, rank capacity domains first by the exponential-race formula using domain residual budget, then members within a domain by the same formula using their individual residual budget. Thus exporting one filesystem twice does not double its capacity weight. For `least_utilized`, compare projected domain ratio first and member ratio second. For `spread`, rank unique backings equally but enforce the same domain admission limit; its objective remains share/bundle counts. Missing domain annotations mean operator-declared independent budgets, not a claim of detected independence. Domain support and its quota/overlap tests are included in milestone 2.

A candidate's clone mechanism can change its required bytes. For weighted ranking, compute the residual domain budget for each feasible member plan after its candidate-specific charges, then use the minimum of those residuals as the single domain weight, with the exact-fit floor. Rank members using their own residual budgets. Recompute this domain input only for a new planning attempt or a distinct split-role continuation. This conservative rule avoids choosing an arbitrary representative member or multiplying the domain weight by its number of alternatives. Golden tests must cover linked and full-clone candidates with different charges in the same domain.

PVE capacity observations are not atomic reservations across CPI processes, directors, or external writers. These checks reduce bad choices but do not prevent all concurrent overcommit. Strict admission would need a reservation coordinator respected by all participants, and even that cannot reserve capacity against unrelated NAS writers without backend support.

## 7. Full lifecycle requirements

**Cloning and root placement.** Current clone code already distinguishes physical backing mismatch and uses full clones for supported automatic-mode mismatches; linked-clone requests cannot specify another target store. Set mode validates actual template backing and uses full clone/import when the ranked target requires it. Reject multi-member root or ephemeral sets with explicit linked-only mode. Allow linked-only placement only on a singleton with a compatible base. Otherwise the requested spreading algorithm could be bypassed. Automatic mode may use a linked clone on the selected template backing and full clones elsewhere. Determine each candidate's compatible clone mechanism and capacity charges before ranking. After ranking selects a target, execute that target's already-validated mechanism. Do not use the legacy unknown-template fail-open path for a set constraint.

Superseded in part by D15 in `docs/design-decisions.md`, which adds a per-member replica cache that is on by default. The text below is the 0.6.0 record. The implementation uses the existing shared base and full-clone/import mechanisms to serve every eligible target; it does not require a new per-storage replica cache. Keep the scalar template/base target inside E as in the example, and explicitly configure stemcell import and ISO targets. Changing the base default does not move existing bases. Before claiming export drain completion, inspect linked-clone dependencies, templates, imports, ISOs, and snapshots as well as deployment disks. Do not permit forceful base deletion while linked clones depend on it. Explicit linked-only mode is supported for a singleton backed by a compatible base; multi-member linked-only sets are rejected rather than relying on an unimplemented replica mechanism.

**Ephemeral semantics.** `ephemeral_storage_set` covers the VM's root and any disk requested with `ephemeral_disk_size_mb` unless explicitly overridden. If no dedicated disk exists, guest ephemeral space remains on the selected root's layout; a set alone must not add or resize a disk. Verify both actual PVE volume placement and the agent's mount mapping. Compilation VMs, errands, and resurrected VMs receive the same global policy. Retained ephemeral volumes remain on their actual backing and count as consumed space even after VM deletion; retention does not turn them into tracked persistent disks or authorize automatic reuse.

**Persistent identity and recovery.** Select only during creation. Existing CIDs and stable-ID resolution determine attach, detach, snapshot, resize, delete, and recovery targets. Never run current regex membership to locate or delete an old disk. A disk removed from a set must still attach if reachable, resize on its current backing if capacity allows, and delete correctly. Record selected set/strategy as diagnostic provenance without making that label authoritative for location or breaking old CID decoding.

**Parker and ownership.** Thread the actual disk storage into park/transfer operations and collision scans. A single global `disk_storage` scan is insufficient once disks are allocated elsewhere. If a parker may hold disks from several stores, allocation/reuse must validate its access to those stores and scan the relevant backing set. Preserve cross-cluster VMID-band rules and foreign-volume protection; never turn a selector into an unrestricted storage-wide deletion sweep.

**ISO, HA and DLB.** Resolve ISO placement per request. When following is disabled, use the configured ISO target. When enabled, preserve the existing explicit ISO override and `local`-sentinel semantics, substituting the selected root storage for global `vm_storage`; follow it only if it is shared, accessible, and supports `iso`. Otherwise validate and use the configured ISO fallback. If neither is eligible, fail before VM allocation. The base example disables following and uses an explicit shared ISO target. Pass the resolved ISO through ConfigDrive upload, attachment, and cleanup without mutating global config; cleanup uses the actual attached/provenance volume, not a freshly resolved default. These changes are required in milestone 3.

Check every attached disk and ISO, including dedicated ephemeral disks, before treating a VM as HA/DLB migration-safe. Exclude storage candidates not accessible throughout the already permitted HA node set; do not silently widen or rewrite HA policy. NFS's shared classification alone does not override node restrictions or a missing mount. Treat these settings as placement-time checks; subsequent infrastructure mount outages still require operational monitoring.

**Policy changes.** Adding/removing members affects future allocations. Recreating a VM can change new root/ephemeral placement; existing persistent data stays at its resolved location. Removing the PVE storage definition itself before draining disks breaks management and is not a supported policy change. This feature performs allocation only. Ordinary `attach_disk` must not copy a disk because a regex changed, and no strategy runs a continuous rebalancing loop. Preserve independently supported existing migration behavior; do not introduce new policy-driven moves.

**Interrupted creates.** Set mode requires a durable allocation journal in `storage_allocation_journal_dir`. The BOSH job creates this directory on the Director's persistent store; `create-env` must supply a durable local path that survives CPI invocation cleanup. Validate ownership, writability, and exclusive process locking before allocation. Store one versioned record per allocation UUID, using atomic replacement, file and directory fsync, and private permissions. Generate the persistent correlation token from that allocation UUID before selecting storage. Records contain namespace, allocation key, intent and policy fingerprints, concrete targets, anticipated volume names, owned VMID, returned UPID, discovered volids, and outcome. Do not store credentials or agent secrets.

Preserve the existing `bpd-` plus 16 lowercase hexadecimal digit format when a parked disk receives a stable ID. Derive those 8 bytes from SHA-256 over a versioned domain separator and the full allocation UUID, and retain the full UUID in the journal and ownership provenance. The shortened token is a locator, not sufficient proof of ownership. Reject a detected collision without adopting the other disk, and allocate a new UUID only before any mutation. Keep set names, strategy details, seeds, and journal state outside the CID so they cannot exceed the existing 255-character disk-CID limit. Free-floating CIDs remain volume-based, and recovery cross-checks their actual volume identity against the full allocation record. The current distinction is implemented in [stableIDForNewDisk](../../src/pve_cpi/internal/cpi/handlers/create_disk.go) and the [stable-ID format](../../src/pve_cpi/internal/pve/disk_stable_id.go).

Write `planned` before a mutation, `submitted` after obtaining a task ID, and `observed` after reading back the allocated resources. Before returning the CID, durably record `ready_to_return` and the exact CID. The CPI cannot know whether the Director received that response, so this state is not an acknowledgement. A crash between submission and recording the UPID requires checking the reserved VMID, anticipated volumes, and existing ownership markers; it must not be treated as no submission. An indeterminate result becomes `reconciliation_required` and cannot trigger another allocation for that journal record. Extend existing PVE provenance so ownership can be cross-checked against journal records; the journal alone cannot authorize deleting an unrelated volume.

On restart, reconcile incomplete records before reusing their identities. For VM calls, matching namespace/agent ID and creation-intent fingerprint can resume the recorded allocation. For fresh persistent-disk calls, never deduplicate by VM, size, or cloud properties because the API supplies no general idempotency token. Report unreturned disk allocations through `pve-cid storage-plan` and the disk audit tooling with their exact CID and ownership evidence. Keep unresolved and ready-to-return records until an explicit audit confirms adoption or approved cleanup; never age-delete a live or uncertain allocation. Document journal backup and recovery alongside Director backup. If the journal is lost or restored stale, block automatic reuse of implicated allocation identities until the audit reconciles PVE state. Journal entries do not change existing disk identity resolution or make global capacity reservations.

**Record lifecycle and concurrent creates.** Atomically index active VM allocations by namespace and agent ID, independently of the creation-intent fingerprint. Two simultaneous identical requests must acquire the same record lock; two different intents with that key must conflict before mutation. Under the lock, reconcile the record and verify the returned VM still exists and belongs to that allocation before reusing a `ready_to_return` CID. Completed deletion records a `deleted` tombstone after verified absence of the VM and disposition of its artifacts. Verified failed-attempt cleanup records `cleaned`; audit-confirmed adoption records `adopted` without making the record disposable. Normal deletion that retains ephemeral storage records `vm_deleted_retained` after proving that the VM is absent and that the surviving volume belongs to its recorded holding VM. This state closes the VM generation while preserving authority over the retained volume; it does not claim that all artifacts are absent. A later creation with the same agent ID can open a new generation after verified deletion, cleanup, or this verified retained-volume disposition. Retained resources still prevent rebinding the namespace to another cluster. An unexpected external disappearance requires reconciliation before starting that generation. Never return a stale CID solely because a prior journal record contains it. Keep terminal evidence for audit; it must not block legitimate recreation or authorize reuse of retained volumes.

One namespace has one active journal authority. Use a durable filesystem with tested atomic replacement, fsync, and cross-process lock behavior. A Director failover or journal relocation requires fencing the previous writer and reconciling a consistent backup before activating the replacement. Two independent directories or hosts claiming the same namespace are not coordinated by local locks. Audit scope must include recorded targets and known historical provenance outside current regex membership; an incomplete scan cannot certify that an allocation is absent. These rules protect allocation identity, not global capacity.

**Disk size, format and existing operations.** Validate NFS image format against the requested snapshot/clone behavior. Root clone capacity must use the actual base virtual size and any requested expansion, not just the hard-coded default root size. Include scratch/full-copy peaks where the chosen path needs them. Persistent resize stays on the current backing, checks its incremental requested growth and format/backend capabilities, and does not treat a new set's capacity as permission to move it. An update-disk request that changes placement returns the existing unsupported result; allocation strategy changes do not add a migration operation. Snapshot growth, retained disks, thin disk growth, and NAS quotas require ongoing capacity monitoring; a create-time check cannot reserve their future bytes.

**Role separation and topology.** Disjoint set membership prevents knowingly mixing transient and persistent targets; it does not isolate traffic, controllers, spindles, capacity domains, or NAS failure domains. Place both exports on one backend only with that understood. No availability claim follows from count spreading. An API `active` flag does not establish that a storage can accept an allocation. Bound API/task deadlines and expose allocation errors, including quota or inode exhaustion and read-only exports. Preserve reconciliation of tasks whose outcome remains unknown. A higher-performance weighting policy would need measured performance inputs; free bytes are not a proxy for IOPS.

**Diagnostics.** Emit chosen node/storage/backing, role, selector source, strategy, capacity observation, and rejection reasons. Add bounded-cardinality allocation/fallback/rejection metrics by configured set/role/strategy; keep UUIDs and regex text out of metric labels. Proposed read-only `pve-cid storage-plan` should explain discovery and feasibility without allocating or mounting anything; its output is a snapshot, not a reservation.

## 8. Implementation sequence and acceptance gates

| Milestone | Concrete changes | Exit criteria |
| --- | --- | --- |
| 1. Schema and strategy contract | Add set structs/validation and namespace in `internal/config/config.go`; atomic role resolution, global boundaries, and disjoint checks; new `internal/storageplacement` with versioned registry; render through `jobs/pve_cpi/spec` and `templates/cpi.json.erb`. | Three selectable strategies pass contract/golden tests; changing name/version changes only ranking; invalid keys, versions, boundary escapes, and conflicting selectors fail before mutation; legacy renders unchanged. |
| 2. Inventory and feasible plans | Extend canonical storage parsing; add per-node/per-storage status alongside `internal/placement/facts.go`; bundle root/ephemeral by default; include template/ISO, existing and default future persistent access; aggregate capacity. | No node-count weight bias, duplicate shared capacity, or double-debit on recheck; combined-size, unavailable/wrong-node, alias, overflow, stale/inconsistent facts, and split-role ordering cases pass. |
| 3. All disk allocation paths | Wire `create_vm_placement.go`, `create_vm_shape.go`, `create_vm_disk.go`, `create_disk.go`; root clone/import and dedicated disk paths; pass plans through normal and node-fallback execution, utilization checks, parkers, VMID scans, and cleanup. | Root-only and root-plus-ephemeral VMs land in E; persistent disks land in P; full clone/import follows chosen target; linked-only never silently defeats distribution; no fallback leaves a role boundary. |
| 4. Recovery and policy changes | Extend durable ownership/provenance where needed; exercise disk handlers, template dependencies, and HA/DLB; add plan explanation, metrics, and operator examples. | Strategy edits affect next operations only; existing CIDs survive set/strategy removal; crash-window tests reconcile known tasks and report unreturned allocations; retained volumes and bases are protected. |
| 5. Base-case deployment certification | Run the two-set scenario and the adversarial matrix below in a disposable multi-node NFS lab; update configuration, lifecycle, limitations, and rollout docs. | All required scenarios pass with recorded actual volume locations, agent mounts, policy fingerprints, and recovery outcomes. This is the first-release completion gate. |

Milestones 1–5 together deliver the requested base case, including capacity domains, request-local ISO resolution, and durable allocation reconciliation. A dedicated-disk-only intermediate build is not feature-complete and must not be documented as supporting all ephemeral placement. The feature is complete only when every review finding has an implemented change and passing acceptance evidence. A stub, TODO, or backlog entry does not close a finding.

Testing should extend existing storage-tier, headroom/utilization, clone-backing, disk-identity, parker, and rollback suites, plus focused new pure planner tests. Run applicable repository Go build/vet/race gates when code is implemented, and job-template rendering checks. No runtime tests were run for this research-only change.

Use a disposable lab with at least three distinct ephemeral exports, two persistent exports, explicit shared import/ISO targets, and at least two PVE nodes. The shared base-template target is one E member. Exercise both free-floating and parked disk strategies. Keep injected distribution tests deterministic; use larger repeated samples and documented statistical tolerances for capacity weighting, while live certification checks eligibility and lifecycle rather than flaky exact balancing.

| Adversarial scenario | Required outcome |
| --- | --- |
| Global E/P only; no per-type set selectors | All new root/dedicated ephemeral disks remain in E; persistent disks remain in P, including no-VM-hint creation. |
| No dedicated ephemeral size | Root lands in E; agent gets its expected ephemeral mount from root. No hidden scalar fallback. |
| Root plus dedicated ephemeral requested | One bundle selection, same E backing, combined capacity check; audit actual device/mount mapping. |
| P singleton and P plural | Same schema and lifecycle; singleton always pins, plural uses operator-selected algorithm. |
| Each of three strategies on both roles; edit strategy only | Eligible membership unchanged; deterministic test fixture shows different choices where algorithm objectives differ; existing disks stay put. |
| One share reachable from more nodes | It receives no extra probability simply because it has more node observations. |
| Three equal shares, many VMs; large/small unequal shares | Spread assessed as bundle/disk counts; weighted/least-utilized assessed against their formulas, without claiming equal IOPS. |
| Old VM extension/profile points outside E/P | Preflight or allocation fails with selector source and boundary error; no quiet override. |
| Regex becomes overlapping, misses all members, or names a nonexistent share | Clear validation error; no fallback to `vm_storage` or `disk_storage`. |
| Share partly mounted, disabled, read-only, quota/inodes exhausted, or nearly full | Feasibility rejects known unusable targets; allocation failures follow bounded reconciliation/fallback rules. |
| Root allocated, ephemeral allocation fails or status becomes stale | Recheck charges outstanding bytes only; owned root cleaned before changing bundle target; no double allocation on unknown outcome. |
| Create VM before persistent request; later disk type is a restricted subset | Base full-access topology succeeds; incompatible topology fails with a precise locality error, not an inaccessible volume. |
| Compilers, errands, recreation, resurrection, and a configured `create-env` | Same role rules, both import and template paths, including all node fallback paths. |
| Policy/strategy edited while allocation task runs | In-flight operation keeps its plan; next operation uses new fingerprint. |
| Remove set/member after persistent creation; detach/attach/resize/snapshot/delete | Resolve actual disk identity independently of membership; resize checks growth on actual backing. |
| Retain ephemeral; delete VM; parker holds disks on multiple stores | No foreign/base/retained volume deletion; collision/access checks cover actual stores. |
| API timeout or CPI process crash at each allocation/provenance boundary | Reconcile known UPID/ownership; expose orphan cases explicitly; never claim absence from a timeout. |
| Linked-only with multiple targets but one base; automatic clone mode | Linked-only configuration rejected; automatic mode honors placement with supported clone/import mechanisms. |
| HA/DLB movement; binary/config rollback | All actual disks and ISO reachable on permitted nodes; old metadata readable; future policy rollback does not move current disks. |
| Partial opt-in, unused sets, or unrelated unhealthy sets during lifecycle operations | Set-constrained creates use complete-operation recovery; unrelated existing CIDs remain manageable. |
| Same agent ID after deletion, concurrent creates, or changed global policy during recovery | One active generation exists; verified deletion permits recreation; recovery preserves identity and checks current boundaries before new mutations. |
| Cached capacity predates a completed root clone; storage ID is repointed | Do not release the root charge against the old observation or mutate the repointed backing. |
| Free-floating and parked CIDs; shortened-ID collision; long policy names | Preserve each existing CID mode and length limit; never adopt a colliding foreign identity. |
| Journal authority moves or history falls outside current membership | Fence the old writer and audit historical targets before declaring recovery complete. |

Roll out by retaining current scalars, configuring sets without selecting them, inspecting the read-only plan, and then opting a canary VM type/disk type into placement. To restore legacy allocation behavior, remove the applicable global role-set bindings and all effective set selectors in the affected request/profile layers. Removing only a resource-level selector restores its global set default, not legacy placement. Already-created disks remain at their actual locations, and their journal/provenance records remain available for recovery. Certify a binary downgrade against any new metadata before declaring downgrade support.

## 9. Decisions and remaining validation

We recommend named sets with two bindings in the base case. One binding places VM storage bundles, and the other independently places persistent disks. Operators explicitly opt into shared-NFS placement and select a versioned `spread` / `weighted_free_space` / `least_utilized` strategy. Global role boundaries and strict feasibility checks constrain every new automatic placement. The feature performs no implicit migration. Existing single-storage configurations remain stable when no new bindings are selected.

Implementation must validate actual API permissions/status fields on the supported PVE versions, full-clone/import capacity and cleanup behavior, and HA access across the permitted node set. Confirm NAS quotas/capacity domains in the target environment; two exports may report the same underlying free space. These are lab and operator-topology checks, not prerequisites for accepting the configuration model.

We inspected upstream and local source code and official documentation. We did not change or test live PVE or vSphere infrastructure. Published vSphere usage simplifies placement as weighted random; the pinned source details above take precedence for algorithm comparisons. PVE website fetches were blocked, so the research used the official `proxmox/pve-docs` source repository for NFS and storage semantics.

## 10. Adversarial review disposition

The earlier draft handled dedicated ephemeral and persistent sets, but did not fully handle the operator's stated all-ephemeral scope. The following design defects were remediated in this revision; runtime verification remains work for implementation.

| Severity | Finding | Remediation |
| --- | --- | --- |
| High | Root-derived guest ephemeral space escaped E because root was deferred. | Root/bundle allocation is first-release scope, with tests for both disk layouts. |
| High | A strategy enum alone did not define algorithm replaceability or upgrade behavior. | Versioned registry, pure ranking interface, change semantics, fingerprints, three operator choices, and contract tests. |
| High | Global sets were defaults that a legacy scalar/profile could bypass. | Global role boundaries validated after precedence; overrides can narrow/change strategy, not escape the binding. |
| High | Linked-only mode could reduce spreading to one template share. | Reject incompatible multi-target linked-only placement; automatic mode selects targets before clone mechanism. |
| High | Future persistent-disk feasibility was implicitly treated as knowable at VM creation. | State the CPI call boundary; preflight full P/E reachability and revalidate actual disk requests on the current node. |
| High | Cross-process retries and unreturned CIDs could be mistaken for exactly-once placement. | Explicit crash-window/provenance work, task reconciliation, and orphan handling; no unsupported guarantee. |
| Medium | Per-role independent placement increased VM share dependencies and left strategy composition unspecified. | Default co-located bundle with aggregate bytes; explicit split selectors and deterministic root-first planning. |
| Medium | Duplicate node observations, incremental rechecks, and full-clone sizing could miscount capacity. | Conservative shared observations, checked arithmetic, outstanding allocation accounting, and actual base virtual size. |
| Medium | Regex overlap and hidden profile/compilation paths could undermine persistent/ephemeral separation. | Default disjoint validation, full call-path matrix, and source-aware diagnostics. |
| Medium | Infrastructure artifacts, retention, snapshots, and HA reachability were easy to overlook. | Explicit infrastructure settings, dependency-aware drain/lifecycle checks, and certification cases. |
| Medium | HA eligibility and rollback wording allowed conflicting interpretations. | Only operators change permitted HA nodes. Legacy rollback removes both global bindings and resource-level set selectors. |
| High | Completed VM records could return deleted CIDs or prevent recreation with the same agent ID. | Atomically index active generations, verify live ownership, and record deletion/cleanup tombstones before permitting a new generation. |
| High | One fingerprint conflated caller intent with mutable global policy during recovery. | Separate intent and policy fingerprints; recover existing resources from the recorded plan and check current boundaries before submitting remaining mutations. |
| High | Stable disk identity was prescribed for free-floating disks despite their existing volume-based CID path. | Preserve both CID modes, define the parked token derivation and collision behavior, and keep placement metadata outside the CID length budget. |
| High | Capacity could be released against a pre-allocation observation, or an ID could be repointed between steps. | Require post-completion status for charge retirement and compare backing definitions before every new mutation. |
| High | Local journal locks were insufficient for duplicate authorities, failover, and incomplete historical scans. | Require one fenced journal authority per namespace and audit recorded and historical targets independently of current set membership. |
| Medium | Partial opt-in and live validation could change unrelated legacy allocations or block existing disk management. | Define activation per creation operation, preserve unconstrained companion backends, and keep allocation-only live checks out of existing-CID lifecycle paths. |
| Medium | Cluster overrides and API aliases could inherit the wrong policy or create duplicate allocation authorities. | Replace and validate effective cluster policy explicitly; keep namespace authority stable across endpoint changes and protect the process-level journal directory. |
| Medium | Retaining a failed attempt's volume conflicted with cleanup-before-fallback. | Stop and expose retained artifacts for audit instead of silently allocating a replacement. |
| Medium | Domain weights were undefined when candidate clone mechanisms required different bytes. | Use the minimum residual domain budget across feasible member plans and freeze versioned encoding and golden fixtures. |
| Medium | Clone selection order could be read as postponing required size information until after ranking. | Determine feasible mechanisms and charges per candidate before ranking, then execute the selected target's validated mechanism. |

The design's guarantees match the available information. Spreading is approximate; operators declare shared capacity domains; and the allocation journal records uncertain outcomes without inventing a Director acknowledgement. Capacity checks cannot reserve storage against future guest growth or external writers. This feature offers the three documented stateless placement strategies. It does not offer exact count balance, IOPS-based scheduling, active rebalancing, or policy-driven persistent migration, and it must reject unsupported strategy names explicitly.
