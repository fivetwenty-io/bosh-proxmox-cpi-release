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
  PVE's `move_disk` refuses to reassign a volume between VMs on different nodes, even on shared storage. By default (`pve.disk_migration: on_attach`) we work around it. `attach_disk` isolates the disk onto a fresh single-purpose mover parker on the disk's node, offline-migrates the mover to the VM's node, attaches from it, and destroys it. The attach keeps the empty mover instead, and logs how to remove it by hand, when the mover's protection restore was cut off or when a journal-managed attach finds a mover that an earlier request created. Setting `pve.disk_migration: off` restores the hard refusal, which names the knob and the manual `qm migrate` escape. See [Stable disk identity and ownership transfer](persistent-disk-strategy.md#stable-disk-identity-and-ownership-transfer).

- Stable-ID disks always park on detach
  A volume renamed by a reassignment cannot safely be left free-floating (PVE deallocates an owner-named volume when its last config reference is swept), so a stable-ID disk's detached state is parked even under `detached_disk_strategy: free`. Legacy disks keep the configured strategy's behavior. See [Stable disk identity and ownership transfer](persistent-disk-strategy.md#stable-disk-identity-and-ownership-transfer).

## VM ownership

- A bare CID can still destroy another Director's legacy VM
  A VM created before the owner tag existed passes the ownership guard on its `bosh-cpi` tag, whichever Director created it. `set_vm_metadata` compares the instance the Director names with the one the VM records, but `delete_vm` receives no metadata to compare, so a stale bare CID can destroy another Director's legacy VM. The gap closes for a VM only when a deploy that updates its instance, or a `bosh recreate`, writes the owner tag. Until then the VM stays exposed. See [Operations](operations.md#which-director-owns-a-vm).

- A bare CID can't tell which cluster it came from
  A VM created before stamped CIDs keeps its bare CID until a `bosh recreate`. When its VMID is absent, every call answers as if the VM were gone, so a `delete_vm` that reaches the wrong cluster drops the Director's record of a VM that may still be running elsewhere. `pve.vm_cid_format: bare` keeps every new VM in that position too. See [Operations](operations.md#which-cluster-does-a-vm-cid-name).

- Two clusters that share a root CA share a stamp
  The stamp is derived from the root CA, so a cluster built from another cluster's CA backup gets the same stamp. Between those two clusters, a CID whose VMID is absent reads as a proven absence, and only the token still tells a reused VMID apart. A CID with the stamp `00000000`, which a create writes when it can't read the CA, names no cluster and gets the same unprotected answers. When another VM with a different token holds its VMID, on any cluster, the call answers `VMNotFound` and leaves that VM alone, so the Director drops a record whose VM may still run on the cluster that created it. See [Design Decisions](design-decisions.md#d17-stamped-vm-cids-that-name-their-cluster-and-their-vm).

- `bosh delete-vm` on a VM with no instance goes to the default CPI
  The Director sends it to the job-level cluster with no `cpi_config` entry properties. A stamped CID from another cluster is refused there, but a bare CID destroys whatever VM of ours holds that VMID on the job cluster. See [Operations](operations.md#bosh-delete-vm-on-a-vm-with-no-instance-goes-to-the-default-cpi).

- `lb_register` names a bare-CID VM by its VMID alone
  A stamped VM's HAProxy server is `vm-<vmid>-<cluster>`, but a VM with a bare CID keeps `vm-<vmid>`, so two clusters that reuse a VMID behind one backend still collide on those VMs. See [Configuration](configuration.md#lb_register).

## Storage and stemcells

- `:heavy:` files on a shared export are kept, not deleted, under cpi-config
  Reference counts are scoped to one cpi-config entry, so `delete_stemcell` keeps the qcow2 when the request came through an entry and the storage is shared, and `pve-cid stemcells --orphans` reports the leftover file for removal with `pvesm free`. Every entry, including the one for the job's cluster, must set `pve.host`, because an entry with empty properties looks like a Director with no cpi_config and still deletes the file. See [Light Stemcells](light-stemcells.md#heavy-and-a-cross-cluster-shared-export-do-not-mix).

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

- Two entries on one cluster share one stemcell reference
  A stemcell cache template counts its references by Director UUID, so two cpi-config entries that point at the same PVE cluster hold one reference between them on a template they both use. The Director deletes every entry's copy of a stemcell in one task, so an ordinary delete removes the template once, and that is correct. The hazard needs a delete task that removes the stemcell through one entry and then fails before it reaches the other. The template is then gone while the Director still records the stemcell against the second entry, and the next `create_vm` through that entry can fail on the missing stemcell. We recover by running `bosh upload-stemcell --fix` with the same stemcell. See [Stemcell registration across CPI entries](multi-cluster.md#stemcell-registration-across-cpi-entries).

- Two entries on one cluster share a journal namespace and can share a parker band
  The CPI warns about these shapes and does not refuse them. It records the VM prefix, parker prefix, and VMID bands of the entry that last admitted a create into the namespace, warns when a create comes from an entry with different values, and reports the mismatch in `storage-journal audit`. When it lists parkers, it warns about parkers of another prefix inside its parker band and about parkers with its own director tag and prefix outside the band. See [Two cpi-config entries on one cluster](troubleshooting.md#two-cpi-config-entries-on-one-cluster).

## Permissions

- Pool-scoped tokens do not cover pre-existing VMs
  `pve.vm_pool` stamps membership only on VMs created after it takes effect, and parts of the reduced-ACL propagation story are documented PVE behavior rather than live-verified. See [PVE API Permissions](pve-api-permissions.md#caveats).

## Validation status

- The parked disk strategy is live-validated on clusters only
  Validated 2026-08-20 on a two-node Proxmox VE cluster: baseline and current code both ran the full tier 1 lifecycle green, including park, unpark, refusal, and drain coverage. No single-node lab is currently materialized, so single-node validation remains open. See [Persistent Disk Lifecycle Strategy](persistent-disk-strategy.md#residual-risk).
