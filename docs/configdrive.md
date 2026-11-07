# ConfigDrive

When `pve.agent_mode` is `cloudinit`, the CPI delivers BOSH agent settings to a newly created VM via an OpenStack ConfigDrive ISO attached as a CD-ROM. This is the only cloudinit bootstrap path.

## How it works

The CPI builds an ISO 9660 + Rock Ridge volume labeled `config-2`. The ISO contains two layout trees:

- **Primary (OpenStack):** `/openstack/latest/user_data` carries the raw BOSH settings JSON. `/openstack/latest/meta_data.json` is written alongside it as a minimal JSON stub. BOSH openstack-kvm stemcell agents read this path first.

- **Fallback (EC2):** `/ec2/latest/user-data` and `/ec2/latest/meta-data.json` duplicate the content for stemcells whose datasource is configured for EC2.

ISO size is fixed at 10 MiB. The CPI uploads the ISO to the PVE storage pool, then attaches it to the VM as a CD-ROM on `scsi30` (see [SCSI slot usage](#scsi-slot-usage)).

Stock OpenStack stemcells from bosh.io (for example `bosh-openstack-kvm-ubuntu-noble-go_agent`) recognize the primary layout on first boot without any `#cloud-config` parsing, `runcmd`, or `systemctl` step.

## Why no PVE-native sub-mode

An earlier `cloudinit_mode: native` setting attempted to deliver settings via PVE's `cicustom` + snippets mechanism. PVE's storage upload REST endpoint only accepts content types `iso`, `vztmpl`, and `import` — `snippets` requires SSH/filesystem placement to `/var/lib/vz/snippets/`. The CPI therefore drops the native sub-mode. ConfigDrive via CD-ROM works on stock OpenStack stemcells without any cloud-init service running in the guest.

## Migration and HA interaction

The ConfigDrive ISO stays attached to the VM's CD-ROM slot (`scsi30`) for the VM's whole life — not only at boot. It is removed only by `delete_vm`. This matters for anything that moves the VM between nodes: PVE refuses to live-migrate a VM whose CD-ROM volume sits on storage that is not accessible from the destination node, and PVE HA recovery on another node fails at start for the same reason — the ISO file does not exist there.

That would silently defeat every feature that registers a VM as a PVE HA resource, which means the HA decision under `pve.ha.mode` and the features it drives, such as `pve.ha.dlb`, `pve.ha.pin_az`, and `pve.ha.anti_affinity`. A VM whose `pve.iso_storage` is node-local (for example the `local` default) could be HA-registered and appear to work, until the first rebalance, failover, or manual migration attempt fails.

The ISO therefore counts toward a VM's HA eligibility, like any other volume. A node-local ISO makes the VM a VM with node-local volumes, so it follows `pve.ha.local_policy`. With `pve.ha.mode: auto`, `create_vm` runs the VM without HA under `none`, or under `hold` with a strict pin to its node, and it logs a warning that names the pool. With `pve.ha.mode: managed`, or with `pve.require_shared_iso_for_ha: true`, a node-local ISO fails `create_vm` on a VM that would otherwise get full HA, and on a VM under the `replicate` policy. The error tells the operator to point `pve.iso_storage` at shared storage. Replication copies only the volumes a VM owns, so a node-local ISO never travels with a `replicate` VM. See [DLB-Aware Placement, Shared Storage Required](dlb-aware-placement.md#shared-storage-required) for the detail and for the `pve.iso_storage_follow_vm_storage` opt-in that can point the ISO pool at an already-shared `pve.vm_storage` pool automatically. A cluster whose volumes all live on one NFS export resolves every VM to shared, and `pve.iso_storage` then only needs to name that same storage.

The ISO also matters when several clusters mount one export. A config-drive ISO is named `vm-<N>-config.iso` for the VM that owns it, so VMID allocation reserves the ID of every such file it finds, and the CPI refuses to delete one whose owner ID lies outside the cluster's VMID bands, except for the VM it has proven it owns. See [Multi-Cluster Deployments](multi-cluster.md#shared-storage-rules).

This check is independent of, and in addition to, the [ISO storage readability warning](operations.md#configdrive-iso-storage) the CPI logs when `iso_storage` is the node-local `local` default — that warning covers a credential-exposure risk, this one covers a migration/HA-availability risk. Both point at the same fix: move `pve.iso_storage` off the node-local default.

## SCSI slot usage

PVE exposes `scsi0` through `scsi30` (31 slots). The CPI reserves slots as follows:

| Slot          | Reserved for                                                  |
|---------------|---------------------------------------------------------------|
| `scsi0`       | System disk (cloned from stemcell template).                  |
| `scsi1`–`scsi28` | Ephemeral and persistent disks (attached by `create_vm` and `attach_disk`). |
| `scsi29`      | Headroom — unallocated to leave space for future use.         |
| `scsi30`      | ConfigDrive CD-ROM.                                           |

`create_vm` rejects deployments that supply more than 28 persistent `disk_cids` at creation time, keeping the headroom and ConfigDrive slots free.

## See also

- [Configuration reference](configuration.md)
- [Architecture overview](architecture.md)
- [DLB-Aware Placement](dlb-aware-placement.md)
- [Operations — ConfigDrive ISO storage](operations.md#configdrive-iso-storage)

## ConfigDrive with storage sets

Set-managed VM planning includes the fixed 10 MiB ConfigDrive allocation and verifies that its target supports ISO content. An explicit `iso_storage` remains an infrastructure choice. When the original ISO policy permits following VM storage, the selected root target must satisfy the ISO requirements. Every permitted HA node must reach the planned ISO store.

The CPI records upload and attachment evidence with the VM allocation. An uncertain upload retains that evidence for reconciliation. See [multi-storage placement](multi-storage-placement.md) for the role bindings and [journal provisioning](storage-journal-provisioning.md) for durable storage requirements.
