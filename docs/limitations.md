# Known Limitations

This page collects every known limitation of the CPI in one place. Each entry is a single sentence with a link to the page that owns the detail; the owning pages remain the authoritative reference. When a limitation is closed by later work, its entry is removed here.

## Disks

- Snapshots require the disk to be attached
  PVE provides no per-volume snapshot primitive, so `snapshot_disk` takes a VM snapshot of the host VM and a detached disk cannot be snapshotted. See [Persistent Disks](persistent-disks.md#known-limitations).

- Legacy disks cannot cross nodes
  `attach_disk` migrates a stranded stable-ID disk to the VM's node by default (`pve.disk_migration: on_attach`), but a disk created before stable disk identities existed is still refused: the migration renames the volume, and a legacy CID is the volume name. Move such a disk by hand or recreate the VM on its node. See [Persistent Disks](persistent-disks.md#known-limitations).

- Cross-node migration needs the source node online
  The migration runs as a PVE task on the node holding the disk, so a disk stranded on an offline node cannot move until that node returns; `attach_disk` surfaces PVE's own error for that case. The placement-time refusal when a target node is under maintenance is likewise unchanged. See [Persistent Disks](persistent-disks.md#known-limitations).

- `set_disk_metadata` persists nothing for detached disks
  Metadata lives in the host VM's description sentinel, so a detached disk logs a warning and stores nothing. See [Persistent Disks](persistent-disks.md#known-limitations).

- Disk shrink is not supported
  PVE's resize endpoint is additive only; a smaller size returns `NotSupported`. See [Persistent Disks](persistent-disks.md#known-limitations).

- Free-floating disks carry no PVE-side provenance
  PVE volumes have no metadata field independent of a VM config slot, so under the free-floating strategy the CID suffix in the Director database is the only provenance record. See [Persistent Disk Lifecycle Strategy](persistent-disk-strategy.md#what-is-not-recorded).

- Reassignment transfer is same-node only, so cross-node attaches ride a mover
  PVE's `move_disk` refuses to reassign a volume between VMs on different nodes, even on shared storage. By default (`pve.disk_migration: on_attach`) we work around it: `attach_disk` isolates the disk onto a fresh single-purpose mover parker on the disk's node, offline-migrates the mover to the VM's node, attaches from it, and destroys it. Setting `pve.disk_migration: off` restores the hard refusal, which names the knob and the manual `qm migrate` escape. See [Stable disk identity and ownership transfer](persistent-disk-strategy.md#stable-disk-identity-and-ownership-transfer).

- Stable-ID disks always park on detach
  A volume renamed by a reassignment cannot safely be left free-floating (PVE deallocates an owner-named volume when its last config reference is swept), so a stable-ID disk's detached state is parked even under `detached_disk_strategy: free`. Legacy disks keep the configured strategy's behavior. See [Stable disk identity and ownership transfer](persistent-disk-strategy.md#stable-disk-identity-and-ownership-transfer).

## Storage and stemcells

- `:heavy:` stemcells and a cross-cluster shared export do not mix
  Reference counts are scoped to one cpi-config entry, so two entries pointing `stemcell_storage` at one shared export delete each other's files; keep `:heavy:` on single-entry storage or use `:light:`. See [Light Stemcells](light-stemcells.md#heavy-and-a-cross-cluster-shared-export-do-not-mix).

- Fast-path delete skips the pool reaper
  With `pve.fast_path_delete: true`, a resource pool emptied by the fast path is reaped only by a later synchronous-path delete or by hand, even when `pve.pool_reap_empty` is on. See [Operations Runbook](operations.md#known-limitation-fast-path-delete-skips-the-pool-reaper).

## Networking

- Bridge networks are node-local
  The CPI creates a bridge on one node and `delete_network` targets `config.node`, so changing `config.node` between create and delete strands the bridge; multi-node bridge provisioning is explicitly out of scope, and the documented multi-node pattern is `managed: false` against a pre-provisioned bridge. See [PVE Settings](pve-settings.md#cluster-topology-limitations) and [Design Decisions](design-decisions.md#out-of-scope).

## Cluster topology and multi-cluster

- Strict AZ pins can wedge on small node sets
  With `pin_az_via_ha_rules` and the default strict pin, an AZ mapped to one or two nodes can leave PVE HA with no legal failover target when those nodes are down together. See [HA and Resurrection](ha-and-resurrection.md#the-small-node-set-az-pin-wedge).

- DLB placement has hard prerequisites
  DLB registration silently skips VMs on node-local storage or single-node clusters, and the Director itself must never be DLB- or HA-managed. See [HA and Resurrection](ha-and-resurrection.md#dlb-caveats).

- `has_vm` is not HA-aware
  The CPI does not consult PVE HA status to distinguish a genuinely gone VM from one mid-recovery; the design fails toward retry instead. See [Design Decisions](design-decisions.md#out-of-scope) and [HA and Resurrection](ha-and-resurrection.md).

- No cross-cluster VMID coordination
  The allocation-time storage scan and the pool-comment lock both stop at cluster boundaries, so disjoint VMID banding across cpi-config entries that share storage is an operator obligation, not something the CPI can enforce. See [Multi-Cluster Deployments](multi-cluster.md#disjoint-vmid-banding-the-multi-cluster-safety-pattern).

## Permissions

- Pool-scoped tokens do not cover pre-existing VMs
  `pve.vm_pool` stamps membership only on VMs created after it takes effect, and parts of the reduced-ACL propagation story are documented PVE behavior rather than live-verified. See [PVE API Permissions](pve-api-permissions.md#caveats).

## Validation status

- The parked disk strategy is live-validated on clusters only
  Validated 2026-08-20 on a two-node Proxmox VE cluster: baseline and current code both ran the full tier 1 lifecycle green, including park, unpark, refusal, and drain coverage. No single-node lab is currently materialized, so single-node validation remains open. See [Persistent Disk Lifecycle Strategy](persistent-disk-strategy.md#residual-risk).
