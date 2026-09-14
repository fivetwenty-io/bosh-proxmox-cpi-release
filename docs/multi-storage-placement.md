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
