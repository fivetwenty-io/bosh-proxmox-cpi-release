# Place disks across multiple NFS shares

Define separate storage sets for persistent disks and VM disks. Each set contains PVE storage IDs for existing shared NFS exports and names the algorithm that selects a target. A singleton set pins a role to one export; a larger set distributes new allocations across its eligible members.

The CPI allocates each volume on one backing store. Changing a strategy affects new allocations without moving existing disks. Distribution is approximate and does not reserve capacity against other writers or future guest growth.

[Executable examples](../manifests/examples/multi-storage-placement/README.md) cover separate sets, singleton pinning, alternate strategies, regex membership, root splits, capacity domains, and two cluster contexts.

## Configure separate persistent and ephemeral sets

Register the exports in PVE before enabling the policy. Every disk target must permit `images` content. Keep explicit stemcell and ISO stores with the content types those operations require.

```yaml
properties:
  pve:
    vm_storage: nfs-ephemeral-01
    disk_storage: nfs-persistent-01
    stemcell_storage: nfs-images
    iso_storage: nfs-iso
    iso_storage_follow_vm_storage: false
    storage_placement_namespace: production-director-pve
    storage_allocation_journal_dir: /var/vcap/store/pve_cpi/allocations
    storage_sets:
      ephemeral:
        names: [nfs-ephemeral-01, nfs-ephemeral-02, nfs-ephemeral-03]
        strategy: {name: spread, version: 1}
        min_free_mb: 10240
        max_utilization_pct: 85
      persistent:
        names: [nfs-persistent-01, nfs-persistent-02]
        strategy: {name: weighted_free_space, version: 1}
        min_free_mb: 20480
        max_utilization_pct: 80
      persistent-pinned:
        names: [nfs-persistent-01]
        strategy: {name: spread, version: 1}
    ephemeral_storage_set: ephemeral
    persistent_storage_set: persistent
    require_disjoint_storage_sets: true
```

The ephemeral binding places the root and any dedicated ephemeral disk together by default. Without `ephemeral_disk_size_mb`, the agent retains its existing behavior of deriving ephemeral space from the root disk. A storage-set binding alone does not create another guest disk or change its size.

Both persistent choices use the same placement path. A disk type can select the singleton when a database needs a specific export.

```yaml
vm_types:
- name: worker
  cloud_properties:
    cpu: 4
    ram: 8192
    ephemeral_disk_size_mb: 51200

disk_types:
- name: database-pinned
  disk_size: 102400
  cloud_properties:
    storage_set: persistent-pinned
- name: database-distributed
  disk_size: 102400
  cloud_properties:
    storage_set: persistent
```

Global bindings also constrain overrides. The pinned persistent set must remain inside the global persistent boundary. Root overrides must stay inside the global root boundary when one is configured; otherwise they inherit the ephemeral boundary. Set `root_storage_set` to define a separate root policy when root and dedicated ephemeral disks need separate targets.

## Choose the allocation strategy

Every set requires an explicit name and version.

| Strategy | Selection behavior |
| --- | --- |
| `spread`, version `1` | Uses deterministic rendezvous ranking to distribute allocation keys across eligible backings. |
| `weighted_free_space`, version `1` | Biases selection toward eligible capacity domains and members with more residual free space. It retains a random seed for the operation. |
| `least_utilized`, version `1` | Prefers the lowest projected utilization after accounting for the allocation. |

Two named sets can reference the same exports with different strategies. This lets deployments select different policies through VM and disk types. Duplicate physical backings within one set are rejected.

## Charge in-flight siblings against a placement

A create ranks its candidates against the capacity PVE reports, and PVE reports a volume only once that volume exists. Two creates that started seconds apart therefore saw the same free space and often chose the same member, which is how several VMs of one instance group ended up on one share. Every set-managed create now reads the allocation journal before it ranks, and it charges each in-flight sibling's claim against the member and the capacity domain that sibling chose.

On a `create_vm` that places for the first time, the journal read, the ranking, and the record write all happen inside the journal's exclusive index lock, so a peer that started a moment earlier is always visible and no window is left there. A retry is different. It lists the journal with no index lock held, it re-plans from that listing, and it writes the new attempt under its own record lock, so it keeps a narrow window of its own. On `create_disk` the read happens before planning, but the record is written after it, so a persistent set with more than one member keeps the same kind of narrow window in this release. We accept that for now. The journal already holds the index lock for the whole of its own body on a disk create, even though discovery, ranking, and the PVE calls all run outside that lock, so a later release can close the window the way the first VM placement closes it.

Four record states charge their bytes, and they are the states where the volume is neither absent nor yet counted in what PVE reports.

| Record state | Charged | Why |
| --- | --- | --- |
| `planned`, `submitted`, `observed`, `reconciliation_required` | Yes | The allocation is in flight, so its bytes sit in no storage status yet. |
| `ready_to_return`, `adopted`, `vm_deleted_retained` | No | The volume exists, and the capacity snapshot already carries it. |
| `deleted`, `cleaned` | No | The volume does not exist. |

A wider rule would be worse than the problem it fixes. If every non-terminal record charged its bytes, then every live VM would charge its virtual size forever on top of the bytes PVE already reports. The capacity projection is a hard gate, so the capacity ceiling would quietly turn into a ceiling on the number of VMs.

The bytes come from two sources, and the larger of the two wins for each capacity key, because once a step exists its charges restate the claim the plan made. The first source is the plan's own charges, which a record in `planned` carries even before it writes a step. The second source is the record's step charges. Outstanding bytes always count, since they sit in no storage status yet, and acquired bytes count only when the record was last updated less than two minutes before the capacity snapshot started, or at any time after it.

That two-minute tolerance absorbs clock offset between hosts. A record's timestamp comes from the wall clock of whichever process wrote it, the snapshot's start time comes from the collector's clock, and under several directors those two clocks sit on different hosts. Subtracting the tolerance biases us toward counting, and we do that deliberately. Counting a sibling whose bytes PVE already reports over-counts by one allocation at most, while dropping it reproduces the pile-up we are fixing.

An allocation never charges against itself, so a retry stays on the share it already holds rather than being steered away from its own retained artifacts.

Sibling bytes change feasibility under every strategy, because the capacity projection is a hard gate, but they change the order only under `least_utilized` and `weighted_free_space`. The `spread` strategy ranks by rendezvous hash and never reads free space, so a `spread` set gets its spreading from the anti-affinity preference below rather than from these charges.

Each create decodes a sibling's plan leniently, so a field that a later release adds to the plan does not break an older binary's creates. Every allocation's decode of its own plan stays strict. A charging sibling whose plan is not valid JSON, or that carries a plan version this release does not know, fails the create with an audit-required error, because a namespace we cannot read in full cannot be accounted for in full. A resting sibling with an unreadable plan feeds only the count, so we skip it rather than fail every create in the namespace, and a rollback across a plan version change therefore wedges nothing while the in-flight records read cleanly.

Nothing ages a record out of an in-flight state, so a CPI that dies between writing its record and running its first step leaves a `planned` record that charges its bytes against every later create. [A crash-abandoned allocation keeps charging capacity](troubleshooting.md#a-crash-abandoned-allocation-keeps-charging-capacity) covers the audit that names such a record.

A member the capacity ceiling excludes now leaves a rejection in the plan instead of disappearing silently, so an operator can see which members the ceiling refused and which ones the ranking never saw. That change and the sibling bytes both push more candidates through the rejection path, so the `cpi.storage.candidate_rejections` counter reads higher after this release even when nothing is wrong.

The `pve-cid storage-plan` diagnostic opens no journal for its ranking, so it ranks against the capacity snapshot alone and charges no in-flight sibling. It reads no sibling record either, so every sibling count comes back empty and the band partition is skipped altogether. What it explains is the strategy's own order over all the candidates, rather than the bucketed order a live create produces. A placement it explains can therefore differ from one a live create makes while peers are in flight, and that difference is expected rather than a fault.

## Spread an instance group across members

The planner also prefers a member that does not already hold a sibling of the allocation it is placing. Two allocations are siblings when their groups match under the scope the set configures.

```yaml
storage_sets:
  ephemeral:
    names: [nfs-ephemeral-01, nfs-ephemeral-02, nfs-ephemeral-03]
    strategy: {name: least_utilized, version: 1}
    anti_affinity:
      scope: instance_group
      utilization_band_pct: 20
```

The whole block is optional. A set that declares no `anti_affinity` block behaves exactly like the block above, because the defaults are read where they are used and are never written onto the set. That matters on upgrade, since the set marshals into the policy fingerprint and into the context-override cache key, and neither of those moves for a deployment that leaves the block out. A set that does declare the block has to name a scope, because a blank scope is a configuration error. The band stays optional inside a declared block. Add `anti_affinity: {scope: none}` to a set to turn the preference off there.

| `scope` | Two allocations are siblings when |
| --- | --- |
| `instance_group` (the default) | They name the same deployment and the same instance group. |
| `deployment` | They name the same deployment, whatever instance groups they belong to. |
| `none` | Never, so the strategy alone orders the candidates. |

`utilization_band_pct` is an integer from 0 to 100, and it defaults to 20.

Every `create_vm` plan records its group as `deployment--<name>/instance-group--<name>`, and each half is sanitized to the CPI's tag alphabet before the two halves are joined. The `deployment` scope compares the first half alone. The group is empty whenever the environment names no instance group, which is the usual `bosh create-env` shape, and an allocation with an empty group has no siblings at all. Records written before this release carry no group either, so they count for nothing, and a namespace starts spreading from its first create after the upgrade.

The sibling count reads a wider set of records than the byte charges do. It admits every record whose group matches except the `deleted` and the `cleaned` ones, so a sibling that has finished its work still occupies its member for the partition even though it charges no bytes at all. The count also lands only on the member that a plan's root disk chose, so the other targets a plan carries, such as a dedicated ephemeral disk, add nothing to it.

The band is what keeps the preference from filling the fullest member first. Before it ranks, the planner measures each candidate's projected utilization as a percentage of that member's total capacity, and it finds the least utilized candidate. A candidate within `utilization_band_pct` percentage points of that minimum is in band. The planner buckets the in-band candidates by how many siblings already hold them, ranks each non-empty bucket on its own with the set's strategy, and concatenates the ranked buckets with the bucket holding the fewest siblings first. It ranks the out-of-band candidates once and appends them after all of those, so a member far fuller than the least utilized one never wins on a sibling count alone.

With the default band of 20 points, a member at 10 percent and a member at 15 percent are both in band, and the sibling counts decide between them. A member at 80 percent sits outside that band, so the member at 10 percent wins whatever the counts say.

The preference orders the root disk of a `create_vm` and nothing else. A dedicated ephemeral disk follows its root's placement, and a persistent disk carries no instance group, so `create_disk` is never partitioned. Before any of this runs, capacity excludes a member that cannot hold the charge. The preference itself never fails a placement, because a set whose members all hold a sibling shifts every bucket up together.

Bucketing changes what each strategy sees, because each bucket is ranked as a slice of its own. A bucket's order is therefore not in general a subsequence of the strategy's order over the whole set. The effect is narrow. `spread` compares rendezvous hashes that depend only on the request and the backing key, `least_utilized` compares the projected domain ratio first and the member ratio second, and both of those belong to the candidate alone. Only `weighted_free_space` aggregates across the slice it is handed.

The winning candidate's reason records the evidence, naming the band, whether the member sat inside it, which bucket it came from, and how many siblings already hold it. From that string an operator reads why a member won.

Promoting a member also decides which template a root clones from. A member that holds no replica of the stemcell clones in full rather than linked, and the CPI logs a warning naming the member. So before a deploy lands on a share that has never hosted that stemcell, we check that its replica exists. [Cache templates per member](#cache-templates-per-member) covers how those replicas are built and what happens when one is missing.

## Cache templates per member

A linked clone lands on the storage its template lives on, and the CPI builds one cache template per cluster on `vm_storage`. So with a multi-member root set, a root placed on any other member would be a full copy of the stemcell across the network. To avoid that, `create_stemcell` builds a template on every member of the effective root set as well, tagged `bosh-stemcell-storage-<storage-id>`, and the planner ranks the template on the placed member ahead of every other candidate. Under `clone_mode: auto` every root then comes up as a linked clone.

This is on by default whenever a root or ephemeral set is bound, because that binding is what creates the problem. It costs nothing where there is nothing to fix. A set whose only member is `vm_storage` already has its template there and builds no replica, and a deployment with no set bound, or one on `stemcell_strategy: import`, builds none either. Set `pve.stemcell_replicate_storage_set: false` to turn it off and accept the full clones. Set it to `true` to insist on it, and three things that would otherwise be a quiet skip become a config error instead: a missing set, the `import` strategy, and two member names that sanitize to one tag.

The replicas build after the primary and never fail the upload. A member that is unreachable, full, or node-local is skipped with a warning naming it, and a `create_vm` placed there clones in full with a warning that names the member and suggests `bosh upload-stemcell --fix`, which rebuilds the missing replica through the ordinary dedup path. A member added to the set later gets its replica on the next upload or `--fix`; a member removed keeps its replica until the stemcell is deleted.

Unreachable covers permissions as well as outages. The PVE API token needs `Datastore.AllocateSpace`, `Datastore.Audit`, and `Datastore.Allocate` on every member of the set, not just on the four scalar pools, and a member it cannot write to is skipped like any other.

Each replica is one more image on its member. The cost is the member count times the stemcell count times the image size, the planner does not charge it against capacity, and `least_utilized` sees it as used bytes after each upload. The template VMID band, 30000 to 30999 by default, is shared by every template, so replicas multiply the templates per stemcell by the member count plus one.

Replicas hold no director reference. The primary's reference set decides the stemcell's lifetime, and `delete_stemcell` sweeps every replica of that stemcell when the last reference drops, whichever director built them. A replica that still backs a running linked clone refuses to die, and `delete_stemcell` then returns an error naming it before touching the qcow2, so the base images under those VMs are never orphaned. Nothing here relaxes the `linked-only root requires singleton set` rule. `clone_mode: linked` still needs a one-member set, because a missing replica under a hard linked requirement would fail the create outright.

## Resolve membership and shared capacity

Use exactly one of `names` or `name_pattern`. Patterns follow Go RE2 search semantics, so anchor them when the whole ID must match.

```yaml
storage_sets:
  ephemeral:
    name_pattern: '^nfs-ephemeral-[0-9]+$'
    types: [nfs]
    shared: true
    strategy: {name: least_utilized, version: 1}
```

Membership is resolved once per operation. Missing explicit names fail validation; inactive members can be excluded while healthy members remain eligible. An inventory failure cannot establish an empty set. The initial implementation accepts shared NFS members only, including when `types` and `shared` are omitted.

Declare a capacity domain when different exports consume the same underlying capacity. The CPI cannot discover every NAS alias or quota relationship.

```yaml
storage_capacity_domains:
  nas-persistent-budget:
    members: [nfs-persistent-01, nfs-persistent-02]
```

The domain uses the most conservative capacity observations from its members. It does not add the free space reported by both exports. `_mb` values mean MiB; set limits combine with applicable global headroom and utilization limits. `storage_status_max_age_seconds` defaults to `5`.

## Preserve allocation identity

Provision and enroll a durable journal before set-managed creation. [Journal provisioning](storage-journal-provisioning.md) covers ownership, Director mounts, and create-env paths. Keep a stable namespace for each allocation authority and back up its journal with the Director. Multi-cluster requests need the correct namespace and cluster identity for each context.

A matching VM request consults its recorded generation before making a new plan. An uncertain submitted outcome requires reconciliation and retains its resources and charges. Persistent requests receive independent allocation UUIDs because the CPI API supplies no general disk-create idempotency key.

Removing an export from a set prevents new placement there. Existing CID operations continue to resolve the actual disk and its recorded provenance. Keep historical backings accessible until their resources and journal records have been reconciled.
