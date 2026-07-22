# Persistent disk lifecycle strategy

This document explains how the BOSH PVE CPI manages persistent disks in the
detached state — the period between `detach_disk` and the next `attach_disk`
or `delete_disk`. It covers the root cause that makes the choice necessary,
both available strategies, their trade-offs, configuration, and the operator
tools that keep the system observable.

See also:

- [Persistent disks](persistent-disks.md) — node selection, storage types, cloud-properties, and CID encoding.
- [Configuration](configuration.md) — full property reference.

---

## Why a detachment strategy is needed

Proxmox VE has no first-class volume object. AWS EBS, OpenStack Cinder, and
vSphere VMDK descriptors each provide a native volume entity that exists
independently of any virtual machine. PVE does not: a storage volume's only
identity is the string `<storage>:<name>` (for example, `local-lvm:vm-9003-disk-0`).
PVE provides no volume metadata API, no volume tag API, and no ownership field in
the storage layer. A volume not attached to a VM config slot exists
as bytes on the backing storage with no record of who created it, what
it is for, or whether it is safe to delete.

The CPI works around this by embedding volume identity and placement metadata
into the disk CID (the `pvd-` envelope — or its opt-in compressed `pvz-`
variant — described in
[Persistent disks — Disk CID encoding](persistent-disks.md#disk-cid-encoding)).
That encoding is opaque to PVE; the cluster still presents the volume as a raw
storage object. When a disk is detached and waiting for its next owner, any
PVE administrator who sees an unfamiliar volume in the storage browser has no
PVE-native signal that the volume belongs to BOSH.

The two strategies below make different choices about that exposure window.

---

## Free-floating strategy (default)

### Mechanics

When `detached_disk_strategy` is `free` (or absent), a detached disk floats as
an unattached volume in PVE storage. Its CID encodes the storage pool, the
node (for local backends), the availability zone, and any per-disk performance
options. The BOSH Director holds the CID and re-presents it on the next
`attach_disk` call.

The CPI never holds a durable in-process index of detached disks. On each
`attach_disk` the storage backend locates the volume by its pool-and-name from
the CID, confirms it still exists on the expected node, and attaches it.

### Risks

- **No PVE-side ownership protection.** The volume has no tag, no description,
  and no `protection=1` flag. A PVE administrator viewing the storage browser
  sees a volume such as `vm-9003-disk-0` with no indication that it is a live
  BOSH persistent disk. They can delete it without warning.

- **Detached disks look like orphans.** PVE's storage-cleanup scripts and
  third-party management tools may classify unattached volumes as candidates
  for garbage collection. The CPI offers no hook to prevent this.

- **`set_disk_metadata` is a no-op.** BOSH calls `set_disk_metadata` after
  attaching a disk to record deployment, job, and instance metadata. That
  metadata is stored in the hosting VM's description field. A free-floating
  detached disk has no hosting VM, so the call logs a warning and persists
  nothing. Metadata is re-applied on the next `attach_disk` cycle.

### Mitigations

- **CID-embedded placement metadata.** The `pool` and `node` fields in the CID
  keep the CPI routing correct across operator renames and storage topology
  changes, even without PVE-side markers.

- **`disk-audit` script.** The `scripts/disk-audit` tool classifies every
  BOSH-managed volume as `attached`, `parked`, `free-floating`, or `unknown`.
  Free-floating volumes are flagged with a non-zero exit code. Run this script
  before any storage-cleanup operation. Pass `--json` for machine-readable
  output.

- **Documentation.** Inform PVE administrators of the VMID band reserved for
  persistent-disk containers (`9000–29999` by default) and instruct them not
  to delete VMs or volumes in that range without consulting BOSH.

---

## Parked strategy

### Overview

When `detached_disk_strategy` is `parked`, the CPI maintains a fleet of
dedicated "parker" VMs. A detached disk is immediately attached to a parker VM
in an active `scsiN` slot. The parker VM is never started, carries
`protection=1`, and has `onboot=0`. Its sole purpose is to give every detached
disk a PVE-visible, protected home.

### Parker VM properties

Each parker VM is created with the following fixed properties:

| Property | Value |
| --- | --- |
| Name | `bosh-parker-<vmid>` |
| Tags | `bosh-parker` (always); `director--<id>` when `pve.stemcell.director_id` is set |
| `onboot` | `0` — never auto-started |
| `protection` | `1` — PVE blocks deletion while protection is set |
| `memory` | 16 MiB |
| `cores` | `1` |
| `scsihw` | `virtio-scsi-pci` |
| Disk slots | `scsi0` through `scsi30` (31 slots per parker VM) |

When a parker VM fills all 31 slots, the CPI creates a second parker VM in the
same VMID band and attaches subsequent disks there. Each new park reuses the
lowest existing parker that still has a free slot before creating another,
so the VMID band fills densely rather than one parker per disk. Each
parker VM is node-scoped: one parker (or chain of parkers) per PVE cluster
node.

### Provenance sentinel

Each park operation records a provenance entry in the parker VM's PVE
description field inside a structured sentinel comment:

```
<!--BOSH:{"bosh_parked_disks":{"<bare-volid>":{"disk_cid":"...","source_vm_cid":"...","parked_at":"<RFC3339>","node":"<node>","director_id":"..."}}}-->
```

Fields:

| Field | Description |
| --- | --- |
| `disk_cid` | Full encoded disk CID as the Director knows it, in whichever format was current when the disk was parked (`pvd-` envelope, its compressed `pvz-` variant, or a legacy bare/pipe-annotated form). |
| `source_vm_cid` | VMID of the VM the disk was detached from. |
| `parked_at` | RFC 3339 timestamp of the park operation. |
| `node` | PVE cluster node the disk lives on. |
| `director_id` | Optional BOSH director identifier from `pve.stemcell.director_id`. |

Provenance writes are best-effort: a failure logs a warning but does not block
the park. Because PVE has no atomic read-modify-write on VM descriptions,
two concurrent park operations targeting the same parker VM may overwrite each
other's provenance entry. The disk remains correctly attached in its `scsiN`
slot; only the advisory provenance record may be incomplete.

The `disk-audit` script reads these sentinel entries to build its inventory.
Free-floating disks have no provenance entry because PVE provides no field to
write one. That gap is why `disk-audit` classifies free-floating volumes
separately: their presence is inferred from the CID band rather than a recorded
origin.

### Detach lifecycle (parked strategy)

1. BOSH calls `detach_disk`.
2. The CPI detaches the disk from its VM via the PVE config PUT (synchronous).
3. The CPI calls `ParkDisk`: checks whether the disk is already parked
   (idempotent); if not, calls `EnsureParker` to find or create a parker VM
   on the disk's node; reads the parker's config to find a free `scsiN` slot;
   attaches the disk with an explicit `DiskID`; writes the provenance sentinel.
4. Park failure is fail-closed retriable: the CPI returns a retriable error to
   the Director, which retries `detach_disk`. On retry the disk is
   free-floating, so `ParkDisk`'s idempotency check detects it and re-parks
   without repeating the detach.

### Attach lifecycle (parked strategy)

1. BOSH calls `attach_disk`.
2. The CPI calls `UnparkDisk`: scans the cluster for the disk's holder; if the
   holder VMID falls in the parker band and carries the `bosh-parker` tag,
   detaches the disk from the parker's `scsiN` slot and removes the provenance
   entry.
3. Unpark failure returns a retriable error; the disk stays safely parked and
   the Director retries.
4. Normal attach proceeds: snapshot guard, slot selection, `AttachDisk`,
   device-path derivation, agent disk-hints update.

### Delete lifecycle (parked strategy)

`delete_disk` calls `UnparkDisk` before deleting the volume. If the disk is
parked, the CPI detaches it from its parker first. Unpark failure returns a
retriable error so the Director retries; the volume is never deleted while it
is still attached to a parker slot.

### `set_disk_metadata` and parked disks

`findVMsHostingDisk` skips parker VMs when the parked strategy is active
(using a VMID-range check followed by a tag check, with no extra API calls).
A disk held by a parker VM produces zero matches, which flows into the
existing warn-and-return-nil path. This is correct: `set_disk_metadata` for
a parked disk is irrelevant until the disk is re-attached to a workload VM.

### `snapshot_disk` and parked disks

`snapshot_disk` is refused for a disk held by a parker VM with a clear error:
"disk is not attached to a workload VM (disk is parked as detached)". PVE
snapshots target the entire VM rather than individual volumes; snapshotting a
parker VM would bundle all disks from all BOSH deployments into a single
snapshot. The guard fires only when the VMID falls in the configured parker
band; it adds one config read when triggered and zero reads otherwise.

### `resize_disk` and parked disks

`resize_disk` is permitted for a parked disk. Parker VMs are stopped (`onboot=0`,
never started). PVE's resize API operates on the storage layer directly and
does not require the VM to be running, so the resize passes through the normal
path with no parker-specific gate.

### `delete_vm` and parker VMs

`delete_vm` refuses to destroy a VM whose VMID falls in the parker band and
whose config carries the `bosh-parker` tag. This guard fires before any mutating
call. Its purpose is a belt-and-braces backstop: bypassing `protection=1` via
`skiplock` would destroy all disks in every `scsiN` slot. An in-band VMID that
cannot be read (transient PVE error) is also refused and the caller is directed
to retry when PVE recovers.

### Parker teardown procedure

The CPI never auto-destroys parker VMs. To remove a parker manually:

1. Run `scripts/disk-audit` and confirm the parker carries zero disks (`parked`
   count = 0 for that parker VMID).

2. Remove the `protection=1` flag: `qm set <vmid> --protection 0`, or via the
   PVE API `PUT /nodes/<node>/qemu/<vmid>/config` with `{"protection":0}`.

3. Destroy the VM: `qm destroy <vmid> --purge`.

Do not skip step 1. Destroying a parker that still holds disks deletes all
volumes in its `scsiN` slots.

---

## Strategy comparison

| Dimension | Free-floating | Parked |
| --- | --- | --- |
| PVE-side visibility | None — bare storage entry | Parker VM visible in PVE UI with name and tags |
| Accident protection | None | `protection=1` blocks PVE-level deletion |
| API ops per `detach_disk` | 1 (DetachDisk config PUT) | 3–5 (DetachDisk + IsDiskParked + EnsureParker + slot-read + AttachDisk) |
| API ops per `attach_disk` | 1 (AttachDisk config PUT) | 3–4 (IsDiskParked + DetachDisk on parker + AttachDisk + config read) |
| `set_disk_metadata` while detached | Warning logged; nothing persisted | Warning logged; nothing persisted |
| `snapshot_disk` while detached | Fails (no hosting VM found) | Refused with explicit "parked" error |
| `resize_disk` while detached | Proceeds via storage backend | Proceeds via storage backend (parker stopped; no extra gate) |
| Provenance | None natively; CID encodes pool/node/AZ | Sentinel entry in parker description; `disk-audit` reads it |
| Capacity limit | Unlimited | 31 disks per parker VM; additional parkers created automatically |
| Concurrency | No extra synchronization | Concurrent parks on the same parker may overwrite each other's provenance entry; disks themselves are safe |
| Blast radius on parker accidental delete | n/a | All disks attached to that parker VM are destroyed |
| Migration from free-floating | Existing disks remain free-floating | New detaches park; first `attach_disk` or `delete_disk` finds the free-floating disk and operates normally |

---

## Configuration reference

### `pve.detached_disk_strategy`

Selects the lifecycle strategy for detached persistent disks.

| Value | Behavior |
| --- | --- |
| `""` or `free` | Default. Detached disks float as unattached storage volumes. |
| `parked` | Detached disks are attached to a dedicated parker VM with `protection=1`. |

### `pve.parked_disk_vmid_range_start`

Inclusive lower bound of the VMID band reserved for parker VMs. When
`detached_disk_strategy=parked` and both range fields are zero, `ApplyDefaults`
fills in `90000`. Ignored when the strategy is `free`.

### `pve.parked_disk_vmid_range_end`

Inclusive upper bound of the parker VMID band. When `detached_disk_strategy=parked`
and both range fields are zero, `ApplyDefaults` fills in `90999`. Must be greater
than `parked_disk_vmid_range_start`.

### Range defaults and validation

The default parker band `90000–90999` is applied only when
`detached_disk_strategy=parked` and both range fields are `0`. Explicitly setting
either field to a non-zero value disables the auto-fill for both fields; the
operator must provide a complete valid range.

At config load time, the CPI validates that the parker band does not overlap:

- The VM VMID range (`vmid_range_start`–`vmid_range_end`, defaults `100–8999`).
- The persistent-disk range (`disk_vmid_range_start`–`disk_vmid_range_end`, defaults `9000–29999`).
- The stemcell-template range (`stemcell_template_vmid_range_start`–`stemcell_template_vmid_range_end`, defaults `30000–30999`).

Overlap causes a hard validation error at startup.

`ParkedStrategyActive()` returns `true` when either range field is explicitly
set to a non-zero value, even if `detached_disk_strategy` is not `parked`. This
allows operators to pre-configure the parker band without enabling the strategy,
and means `IsParkerVM` and related checks are active whenever the band is defined.

### When to enable the parked strategy

Enable `parked` when:

- PVE administrators are not BOSH-aware and may act on storage objects directly.
- Cluster-level automation or monitoring scans for unattached volumes.
- Compliance requirements call for explicit ownership records on all storage objects.
- The cluster uses a dedicated PVE role for the BOSH API token and that role does not
  grant storage-delete rights to other users.

Keep `free` (the default) when:

- All PVE administrators are BOSH-aware and understand the VMID naming bands.
- API latency per `detach_disk` and `attach_disk` is a concern (shared or high-latency storage).
- The deployment is a single-operator environment with no separate PVE admin team.

### Migrating between strategies

**Free to parked:** existing detached disks remain free-floating. New `detach_disk`
calls park immediately. When the Director next calls `attach_disk` or
`delete_disk` for a free-floating disk, the unpark check finds the disk is not
parked and the operation proceeds normally. The two states coexist transparently;
`disk-audit` reports them separately.

**Parked to free:** existing parked disks self-heal on the next `attach_disk` or
`delete_disk`. `UnparkDisk` runs before each operation and detaches the disk
from its parker VM. Once detached, it becomes free-floating. No bulk migration step
is required. Parker VMs that become empty remain in PVE; remove them manually
following the teardown procedure above.

**Mixed state is safe.** `ParkedStrategyActive()` controls whether unpark probes run.
When the parked range is configured, the CPI always runs unpark probes before
`attach_disk` and `delete_disk`, regardless of the current `detached_disk_strategy`
setting. Flipping the strategy back to `free` while parked disks exist is therefore
handled gracefully: the probes still fire, unpark the disks, and the operations
complete.

> **Warning: never remove the `parked_disk_vmid_range_start` /
> `parked_disk_vmid_range_end` knobs while any disk remains parked.** Clearing the
> range disables `ParkedStrategyActive()`, which turns off every unpark probe and
> parker guard. With the guards gone the CPI will attach a still-parked volid to a
> workload VM (double reference), snapshot the parker VM during `snapshot_disk`, and
> delete a volume the parker still references during `delete_disk`. Before removing
> the range knobs, run `disk-audit` and confirm it reports zero parked disks. To
> drain parked disks first, leave the range configured and let the next
> `attach_disk` or `delete_disk` on each disk unpark it (the "Parked to free"
> procedure above), then remove the knobs once the audit is clean.

---

## Provenance story

### What is recorded and where

When a disk is parked, the CPI writes a provenance entry into the parker VM's
PVE description field using the `<!--BOSH:{...}-->` sentinel format. The entry
is keyed by the bare `<storage>:<volid>` string and records the full Director
disk CID, the source VM CID, a timestamp, the node name, and the director ID
(optional).

The sentinel format is shared with `set_disk_metadata` (which uses
`bosh_disk_metadata`) and `bosh_parked_disks` as distinct top-level JSON keys.
Both can coexist in the same `<!--BOSH:{...}-->` block; the parsers preserve
unknown keys so new codecs do not corrupt existing data.

### What is not recorded

Free-floating disks have no PVE-native field in which to write provenance.
PVE storage volumes carry no description, tag, or metadata field that survives
independently of a VM config slot. The CID suffix is the only provenance record
for a free-floating disk, and it lives in the BOSH Director database, not in PVE.

### How `disk-audit` consumes provenance

`scripts/disk-audit` reads parker VM descriptions cluster-wide and extracts
`bosh_parked_disks` entries from the sentinel. When a parked disk's provenance
entry is present the audit displays its `disk_cid`, `source_vm_cid`,
`parked_at`, node, and `director_id` columns. When the entry is absent
(possible after a concurrent park race that overwrote the sentinel) those
columns are empty, but the disk still appears as `parked` because holder
classification is based on the actual `scsiN` slot scan, not the sentinel.

The audit also scans all VMs in the configured disk VMID band for unattached
volumes not held by any parker. Those are reported as `free-floating` and cause
a non-zero exit code. Parkers with zero disk slots occupied are reported as
empty, which is the expected state after a full strategy migration.

---

## Residual risk

The parked strategy has not been validated against a live Proxmox VE cluster.
The implementation is code-complete and unit-tested. Before enabling in
production:

- Run `scripts/disk-audit` after a `detach_disk` cycle to confirm that parker
  VMs appear and that provenance entries match the attached slots.
- Verify that `attach_disk` and `delete_disk` complete cleanly on a parked disk.
- Confirm that `protection=1` on the parker VM prevents accidental deletion
  through the PVE UI and API.
- Confirm that the parker VMID band does not collide with any existing VMs in
  the target cluster before enabling.
