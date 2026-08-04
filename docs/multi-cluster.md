# Multi-Cluster Deployments

A single BOSH director can target more than one independent PVE cluster at once — one named `type: pve` cpi-config entry per cluster, with each availability zone bound to the CPI entry that serves it. This document covers the full walkthrough: the cpi-config manifest, AZ binding, stemcell registration across entries, and — the part that matters most once storage is shared between clusters — how to keep two independent clusters from corrupting each other's data.

If you are running a single PVE cluster, none of this applies to you; skip straight to [Configuration](configuration.md).

## How multi-CPI works

BOSH resolves the executable to run for a cpi-config entry from its `type` field, not its `name`. Every `type: pve` entry — no matter how many you declare — dispatches to the same colocated `pve_cpi` job binary already installed on the Director VM. No release code change, no extra job colocation, and no extra release upload is required to add a second entry. Each entry carries its own independent `properties:` block (host, credentials, node, storage pools, VMID ranges, and so on), so each named CPI targets a genuinely independent PVE cluster.

### Worked `cpi-config.yml` example

```yaml
cpis:
- name: pve-az1
  type: pve
  properties:
    pve:
      host: pve-az1.example.com
      user: bosh@pve
      api_token: ((pve_az1_api_token))
      node: pve-az1-n1
      vm_storage: data
      disk_storage: data
      stemcell_storage: nfs-shared
      iso_storage: data
      network_bridge: vmbr0
      vmid_range_start: 100
      vmid_range_end: 4999
      disk_vmid_range_start: 9000
      disk_vmid_range_end: 19999
      stemcell_template_vmid_range_start: 30000
      stemcell_template_vmid_range_end: 30499

- name: pve-az2
  type: pve
  properties:
    pve:
      host: pve-az2.example.com
      user: bosh@pve
      api_token: ((pve_az2_api_token))
      node: pve-az2-n1
      vm_storage: data
      disk_storage: data
      stemcell_storage: nfs-shared
      iso_storage: data
      network_bridge: vmbr0
      vmid_range_start: 5000
      vmid_range_end: 9999
      disk_vmid_range_start: 20000
      disk_vmid_range_end: 29999
      stemcell_template_vmid_range_start: 30500
      stemcell_template_vmid_range_end: 30999
```

Apply it the same way as any other cpi-config:

```bash
bosh update-cpi-config cpi-config.yml
```

The repository ships a parameterized version of this manifest at `manifests/cpi-config.yml`, applied via `scripts/bosh update-cpi-config` — which layers the active env's vars (`BOSH_PVE_ENV`), then re-registers the active light stemcell against every entry with `--fix`. An AZ binding of this shape ships as the ops layer `manifests/cf/multi-cpi-azs.yml`, which `scripts/cf cloud-config` applies automatically whenever the Director reports an active cpi-config. That layer binds three AZs against the two shipped entries — `z1` and `z2` to `pve-az1`, `z3` to `pve-az2` — so the CF deployment keeps three AZs while the second cluster carries one of them.

Note the two entries above share `stemcell_storage: nfs-shared` — a shared NFS export both clusters can reach — while every VMID range is disjoint between the two entries. That combination is the multi-cluster safety pattern this document exists to explain; see [Disjoint VMID banding](#disjoint-vmid-banding-the-multi-cluster-safety-pattern) below.

### Per-entry `placement`

An entry may also carry its own `placement` block. This matters most for `az_map`, whose values are PVE *node* names — names that mean nothing outside the one cluster that has them, so a single job-level map cannot serve two clusters:

```yaml
- name: pve-az2
  type: pve
  properties:
    pve:
      host: pve-az2.example.com
      # ... connection and VMID settings as above ...
      placement:
        az_map:
          z3: [pve-az2-n1, pve-az2-n2, pve-az2-n3]
        pin_az_via_ha_rules: true
```

An entry's `placement` block replaces the job-level one entirely rather than merging into it, so each entry states its own complete placement policy. Omit the key to inherit the job-level block; send `null` to run with no placement policy at all. A typo inside the block is rejected rather than ignored, so a misspelled field fails the request instead of silently disabling the feature it was meant to enable.

The three HA features (`placement.dlb`, `placement.anti_affinity.use_ha_rules`, `placement.pin_az_via_ha_rules`) are reachable only this way in a multi-cluster deployment. Enabling any of them also means disabling the BOSH resurrector for the affected deployment, and `pin_az_via_ha_rules` additionally requires `cloud_properties.availability_zone` on each `vm_type` — see [HA and Resurrection](ha-and-resurrection.md).

## AZ-to-CPI binding in cloud-config

BOSH's `azs[].cpi` field binds an availability zone to a named cpi-config entry:

```yaml
azs:
- name: z1
  cpi: pve-az1
- name: z2
  cpi: pve-az2
```

Every operation BOSH performs against an instance in a given AZ — create, delete, cloud-check, resurrection — is dispatched exclusively through that AZ's bound CPI entry, and so exclusively against that AZ's PVE cluster.

## Stemcell registration across CPI entries

A stemcell upload binds to a specific cpi-config entry — uploading it once does **not** automatically register it against every entry. Upload normally against the first entry, then re-register the same content against each additional entry with `--fix`:

```bash
bosh -e <alias> upload-stemcell <stemcell>.tgz          # registers against the first-seen entry
bosh -e <alias> upload-stemcell <stemcell>.tgz --fix    # re-registers against every other named entry (e.g. pve-az2)
```

Each `--fix` invocation runs `create_stemcell` against that entry's own cluster, which — per the eager-cache design in [Design Decisions — D1](design-decisions.md#the-refs-anchor-rule) — builds that cluster's own per-cluster cache template immediately, tagged `bosh-stemcell-cache`, and registers that entry's director UUID as a reference on it.

### The AZ-reassignment trap

An instance group's AZ assignment determines which named CPI — and so which PVE cluster — BOSH uses for every operation against that instance, including cloud-check and resurrection. **Persistent disks do not transfer automatically when an instance's AZ is reassigned across two independent clusters.** A redeploy after an `azs:` change from `z1` to `z2` succeeds with no error at all — but BOSH recreates the instance fresh on the new cluster with an empty disk, and leaves the old disk orphaned on the original cluster (visible only in that director's orphaned-disk listing). There is no in-place cross-cluster disk move, because the two clusters share no storage state the CPI can move between them.

Treat a cross-cluster AZ reassignment as a blue/green instance-group change with an explicit, out-of-band data migration — never as an in-place `azs:` edit on an already-deployed instance group.

### `bosh clean-up --all` fails the first time, then succeeds

On a multi-entry director the first `bosh clean-up --all` reliably fails while deleting stemcells:

```
Task NNN | Deleting stemcells: bosh-proxmox-kvm-ubuntu-noble-go_agent-light/1.383
          L Error: Attempt to delete object did not result in a single row modification
            (Rows Deleted: 0, SQL: DELETE FROM "stemcells" WHERE ("id" = 1))
```

This is a Director-side bug, not a CPI failure. A stemcell is registered once per cpi-config entry, so the Director's cleanup loop walks it once per entry and issues the same `DELETE FROM "stemcells" WHERE ("id" = 1)` twice; the second matches zero rows and Sequel raises. The stack lands in `cleanup_artifact_manager.rb`'s `delete_stemcells`.

Every CPI call in that same task succeeds — `delete_stemcell` returns `ok` for each entry, including the Director's own retry against an entry whose reference was already released, since the CPI is idempotent there. The Director simply cannot commit its own database row twice.

Re-run the command: the second `bosh clean-up --all` completes normally, because the first run already removed the row. The window between the two runs is safe — the artifacts the failed run deleted stay deleted, and nothing is left half-referenced.

## Disjoint VMID banding: the multi-cluster safety pattern

Two safety mechanisms exist inside a single CPI process, and neither one extends across independent CPI processes:

- **VMID-allocation storage scanning** checks the shared storage pool for volumes belonging to VMIDs *outside* this cluster's own resource list before allocating a new one, closing the allocation-time collision for the one pool it scans. It covers VM, stemcell-template, and parker-VM allocation (via `WithStorageScan`), disk allocation (built into `NextDiskVMID`), and the ISO pool (via `WithExtraStorageScan`, and only when `iso_storage` is a distinct third pool — a scan of `vm_storage` or `disk_storage` under another name would be redundant). But it is a read at allocation time with no cross-cluster lock: two clusters allocating concurrently can still pick the same VMID (a genuine time-of-check-to-time-of-use race), and it cannot repair a collision that already existed before this guard was added, or one created by non-CPI tooling.
- **The pool-comment cluster lock** (used for VM metadata, disk metadata, stemcell-reference read-modify-write, and delete serialization) relies on PVE's `pmxcfs`, which is a per-cluster, corosync-backed filesystem. Two independent PVE clusters run two independent `pmxcfs` instances, so the same sentinel pool name can be "locked" simultaneously in both clusters — each cluster's lock is real, but neither serializes against the other.

Because of this, **disjoint VMID banding across CPI entries sharing storage is not optional — it is the mechanism that makes shared storage safe at all.** Give every CPI entry that shares any storage pool with another entry a non-overlapping range for every band it uses:

| Band | Config keys |
|---|---|
| VM | `vmid_range_start` / `vmid_range_end` |
| Persistent disk | `disk_vmid_range_start` / `disk_vmid_range_end` |
| Stemcell template cache | `stemcell_template_vmid_range_start` / `stemcell_template_vmid_range_end` |
| Parker VM (only when `detached_disk_strategy: parked`) | `parked_disk_vmid_range_start` / `parked_disk_vmid_range_end` |

The repository's `manifests/envs/lab/vmid-range.yml` is the existing single-cluster precedent for this pattern (reserving 100–199 for hand-managed bastions); apply the same idea per CPI entry when clusters share storage, as in the worked example above.

The CPI validates that its own four bands are mutually disjoint *within one config*. It cannot validate a second, independent cpi-config entry's bands against the first — there is no in-process visibility into a sibling CPI's configuration. Banding across entries is an operator convention this document exists to make explicit, not something the CPI can enforce for you.

## Shared-storage rules

When two or more CPI entries share a storage pool, three rules apply.

**`destroy_unreferenced_disks` must stay `false`.** This is the default (see [Design Decisions — D9](design-decisions.md#d9--cross-cluster-delete-safety-destroy_unreferenced_disks-defaults-false)), and on shared storage it must remain the default. PVE's `DestroyUnreferencedDisks` flag frees every volume matching the destroyed VM's VMID that is not referenced in *this* cluster's view of that VM's config — with overlapping or accidentally-colliding VMID bands, that can free another cluster's live disks. Leave it off on any storage a second cluster can reach.

**Watch for the duplicate-backing warning.** On its first storage lookup, each CPI entry independently checks every storage ID in *its own* cluster's `/storage` index for two IDs that resolve to the same physical export or path, and warns once per entry:

> `storage_info: two or more storage IDs share one physical backing — two names, one export; prefer a single storage ID to avoid silent full-clone downgrades, cross-cluster VMID collisions, and split placement decisions`

This check runs per CPI entry, against that entry's own cluster's storage index — it does not, and cannot, compare storage IDs across two separate cpi-config entries, even when both point at the same physical export under matching names. Seeing this warning means you registered the same export twice under different names *within one cluster's storage.cfg*; it is not a signal about cross-cluster sharing, which is expected and by design when you deliberately point two CPI entries at one shared pool.

**A duplicate qcow2 filename is benign; a template VMID collision is not.** Stemcell qcow2 filenames are deterministic (name, version, and content hash), so two clusters uploading the identical stemcell to the same shared pool write the same path — harmless, since the content is identical. Cache templates, however, are built independently per cluster (each cluster needs its own guest configuration), so each cluster consumes a template VMID from its own stemcell-template band; that's exactly why the bands must stay disjoint.

## `:light:` stemcells: one file, every cluster

The headline scenario this whole design serves: upload a stemcell once as `:light:` — a preuploaded, operator-managed qcow2 (`cloud_properties.image_id`) — onto storage every cluster's `stemcell_storage` can reach, and every cluster consumes it from that single file with no duplicate upload traffic and no cross-cluster refcounting complexity, because `:light:` files are never deleted by the CPI at all (see [Design Decisions — D10](design-decisions.md#d10--qcow2-deletion-policy-light-never-heavy-at-last-cluster-reference)).

**This safety is specific to `:light:`.** A `:heavy:` stemcell on an export shared across clusters is the opposite case: the CPI owns that file and deletes it at last reference, reference counts are scoped per entry, and so the first cluster to reach zero removes a file the other cluster's templates still point at. Put `:heavy:` stemcells on storage only one entry can reach — see [Light Stemcells — `:heavy:` and a cross-cluster shared export do not mix](light-stemcells.md#heavy-and-a-cross-cluster-shared-export-do-not-mix) for the mechanism. This is not hypothetical bookkeeping: we have run a full teardown against a pair of clusters mounting one NFS export with identical content listings, and only the `:light:` rule kept one cluster's clean-up from removing the other's file.

Each cluster still builds and owns its own cache template. `create_stemcell` (run once per entry via the upload-plus-`--fix` sequence above) builds that cluster's cache template *eagerly*, at upload time — it is not deferred to the first `create_vm`. If a cluster's cache template is later missing (manually deleted, or never built there), a `create_vm` under `stemcell_strategy: template` does **not** rebuild it inline: it logs a warning and falls back to `stemcell_strategy: import` for that one VM, importing directly from the shared qcow2. Run `--fix` again against that entry to rebuild the cache, or set `stemcell_strategy: import` deliberately if you never want a given deployment to depend on the cache.

## Cross-cluster stemcell inventory

`pve-cid stemcells --orphans` (see [Design Decisions — D4](design-decisions.md#d4--disk-cids-keep-the-envelope-ship-a-tool)) gives you the "what is safe to delete" view of one cluster's stemcell storage: qcow2 files correlated against cache templates and their director references. It is a single-cluster tool by construction — each run loads one CPI entry's `cpi.json`. `pve-cid` ships on the Director VM at `/var/vcap/packages/pve_cpi/bin/pve-cid` (not on `PATH` by default). For a full multi-cluster inventory, run it once per entry:

```bash
pve-cid stemcells --orphans --config /path/to/pve-az1/cpi.json
pve-cid stemcells --orphans --config /path/to/pve-az2/cpi.json
```

An entry showing an orphan on shared `:light:` storage that another entry still actively references is expected and correct — orphan status is always relative to the one cluster the command was run against, exactly matching how director references are scoped.

The same applies after a teardown. Once the last deployment is gone, every `:light:` stemcell is reported as an orphan with the reason *"qcow2 file has no correlated cache template"* — which is precisely the steady state a light stemcell is designed for: the operator's file outlives the cache templates built from it. Do not read that as a delete list. The CPI never deletes an operator-managed file, and neither should a post-teardown sweep.

## Network reachability

A multi-CPI CF deployment spreading instance groups across AZs bound to different clusters needs the two clusters to have reachable L2/L3 connectivity between them for those instance groups to communicate. The single-node `simple` SDN zone shape used by this repository's lab manifests does not span clusters; a routed fabric, or a vxlan/EVPN overlay reaching both clusters, is required instead. See [Networks](networks.md) for the bridge-versus-SDN patterns this CPI supports.

## See also

- [Design Decisions](design-decisions.md) — the full rationale behind D9 (cross-cluster delete safety) and D10 (qcow2 deletion policy) referenced throughout this document.
- [Operations Runbook](operations.md) — stemcell lifecycle across directors and safe teardown ordering for multi-director environments.
- [Configuration](configuration.md) — the complete `pve.*` property reference.
