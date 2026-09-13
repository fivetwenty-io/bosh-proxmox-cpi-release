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
