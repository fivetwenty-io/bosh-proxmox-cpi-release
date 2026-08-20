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

## Free-floating strategy (opt-in)

### Mechanics

When `detached_disk_strategy` is `free`, a detached disk floats as
an unattached volume in PVE storage. This was the default before parking took
over that role; it is now the opt-out for operators who want the older
behavior back. Its CID encodes the storage pool, the
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

## Parked strategy (default)

### Overview

When `detached_disk_strategy` is `parked` (or absent), the CPI maintains a fleet of
dedicated "parker" VMs. A detached disk is immediately attached to a parker VM
in an active `scsiN` slot, and a freshly created disk is parked the same way
before `create_disk` returns its CID, so a volume is never exposed unowned in
the window between creation and its first attach. The parker VM is never
started, carries `protection=1`, and has `onboot=0`. Its sole purpose is to
give every detached disk a PVE-visible, protected home.

### Parker VM properties

Each parker VM is created with the following fixed properties:

| Property | Value |
| --- | --- |
| Name | `bosh-parker-<vmid>` |
| Tags | `bosh-cpi` and `bosh-parker` (always, in that order); `director--<id>` when the calling director's identity is present in the request context (automatic — no configuration) |
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
| `director_id` | BOSH director UUID from the calling director's request context. Empty when the request carries no director UUID. Not a config property — no manifest setting controls it. |

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

### Create lifecycle (parked strategy)

1. BOSH calls `create_disk`.
2. The CPI creates the volume and encodes its CID.
3. The CPI calls `ParkDisk` on the fresh volume before returning the CID, so
   the disk already sits on a protected parker when the Director first learns
   its name. The provenance entry carries the disk CID but no
   `source_vm_cid` (the disk was never attached to a VM).
4. Park failure is fail-closed: `create_disk` returns the error, unparks
   best-effort in case the park half-committed, deletes the volume, and the
   Director's retry re-creates from scratch.

The first `attach_disk` (or a `create_vm` carrying `disk_cids`) unparks the
disk through the same holder guard every attach path uses, exactly as it
would for a disk parked by `detach_disk`.

### Anchor invariant

A disk created under the parked strategy carries a promise in its CID
envelope (`anchor`) that a parker VM holds it whenever it is detached. The
CPI never deletes parkers, so a promised disk with no holder anywhere in the
cluster means the parker was deleted out-of-band. Under
`pve.parked_anchor_strict` (unset or `true`, the default), `attach_disk`,
`create_vm` with `disk_cids`, and `delete_disk` refuse that state with an
error naming the recovery; the same applies when a parker the holder scan
identified vanishes before its config can be read or before the unpark
detach runs. Setting the property to `false` restores the permissive
treat-as-free-floating behavior for labs that intentionally delete parkers.
Disks created before this release, or under the `free` strategy, carry no
promise and are always handled permissively. See
[Troubleshooting](troubleshooting.md#parker-anchor-missing-parked-disk-with-no-holder).

### Detach lifecycle (parked strategy)

1. BOSH calls `detach_disk`.
2. The CPI detaches the disk from its VM via the PVE config PUT (synchronous).
3. The CPI calls `ParkDisk`: checks whether the disk is already parked
   (idempotent); if not, calls `EnsureParker` to find or create a parker VM
   on the disk's node; reads the parker's config to find a free `scsiN` slot;
   attaches the disk with an explicit `DiskID`; writes the provenance sentinel.
4. Park failure is fail-closed and keeps the class the park chose: most
   failures are retriable, and the Director's retry finds the disk
   free-floating, so `ParkDisk`'s idempotency check re-parks it without
   repeating the detach. A denied PVE grant, an exhausted parker VMID band,
   and a reference the CPI could not sweep out of a parker's `unusedN` keys
   are permanent, since retrying repairs none of them; each names what to fix.

### Attach lifecycle (parked strategy)

1. BOSH calls `attach_disk`.

2. The CPI resolves the volume's holder with one cluster scan. A holder that is
   neither the target VM nor a parker is refused outright, naming the holding VM
   and its node: PVE permits two configs referencing one volume, nothing
   downstream detects it, and the volume dies with whichever holder is destroyed
   first. A holder carrying the `bosh-parker` tag from outside the configured
   band is refused too, naming the property to set.

3. The CPI calls `UnparkDisk`: when the holder is a parker, it detaches the disk
   from the parker's `scsiN` slot and removes the provenance entry. PVE documents `protection=1` as disabling "the remove VM and remove
   disk operations", so the CPI clears the flag immediately before the detach
   and restores it immediately after. The unprotected window spans one detach
   call, and the flag is restored even when the detach fails; a restore failure
   is logged with the `qm set <vmid> --protection 1` command that repairs it.

4. Unpark failure leaves the disk safely parked and keeps the class the unpark
   chose. Most failures are retriable and the Director retries. A denied PVE
   grant and a reference the CPI could not sweep out of the parker's `unusedN`
   keys are permanent, since retrying neither repairs them; each names the
   command that does.

5. Normal attach proceeds: snapshot guard, slot selection, `AttachDisk`,
   device-path derivation, agent disk-hints update.

### Delete lifecycle (parked strategy)

`delete_disk` calls `UnparkDisk` before deleting the volume. If the disk is
parked, the CPI detaches it from its parker first. The volume is never deleted
while it is still attached to a parker slot. Unpark failure keeps the class the
unpark chose: retriable for the transient cases, permanent for a denied grant or
a reference the sweep could not clear.

### `set_disk_metadata` and parked disks

`findVMsHostingDisk` skips any VM carrying the `bosh-parker` tag, using only the
tags the cluster-resources scan already carried and no extra API calls. The tag
alone decides, band-independent and whatever the configured strategy is: an
operator who moved the band away while parkers were still standing would
otherwise have deployment metadata merged into a parker's description, the field that
carries the provenance sentinel for every disk it holds. A disk held by a parker
VM produces zero matches, which flows into the existing warn-and-return-nil path. This is correct: `set_disk_metadata` for
a parked disk is irrelevant until the disk is re-attached to a workload VM.

### `snapshot_disk` and parked disks

`snapshot_disk` is refused for a disk held by a parker VM with a clear error:
"disk is not attached to a workload VM (disk is parked as detached)". PVE
snapshots target the entire VM rather than individual volumes; snapshotting a
parker VM would bundle all disks from all BOSH deployments into a single
snapshot. The holder is classified by the `bosh-parker` tag alone, band-
independent, from tags the holder scan already read, so the guard costs no
additional API call.

### `resize_disk` and parked disks

`resize_disk` is permitted for a parked disk. Parker VMs are stopped (`onboot=0`,
never started). PVE's resize API operates on the storage layer directly and
does not require the VM to be running, so the resize passes through the normal
path with no parker-specific gate.

### `delete_vm` and parker VMs

`delete_vm` refuses to destroy any VM carrying the `bosh-parker` tag, whatever
its VMID. This guard fires before any mutating call. Its purpose is a
belt-and-braces backstop: bypassing `protection=1` via `skiplock` would destroy
all disks in every `scsiN` slot. Classification reads the tags on the
cluster-resources row, which are authoritative when non-empty. An empty tags
field proves nothing, since PVE may simply not populate it, so the CPI falls
back to reading the VM config; a config read that fails is refused retriably and
the caller is directed to retry when PVE recovers. Every parker the CPI creates
is tagged, so on a CPI-managed cluster that fallback never fires.

### Parker teardown procedure

The CPI never auto-destroys parker VMs. To remove a parker manually:

1. Run `scripts/disk-audit` and confirm that parker's row shows `0` under both
   `DISKS` and `UNUSED`, and that the audit emitted no `unusedN` warning naming
   it. Both columns matter: an `unusedN` entry is a reference to a live volume
   that no active-bus probe reports.

2. Remove the `protection=1` flag: `qm set <vmid> --protection 0`, or via the
   PVE API `PUT /nodes/<node>/qemu/<vmid>/config` with `{"protection":0}`.

3. Destroy the VM: `qm destroy <vmid> --purge`.

Do not skip step 1. Destroying a parker that still holds disks deletes all of
them, and `qm destroy --purge` frees the volume behind an `unusedN` entry as
readily as one in a `scsiN` slot.

---

## Strategy comparison

| Dimension | Free-floating | Parked |
| --- | --- | --- |
| PVE-side visibility | None — bare storage entry | Parker VM visible in PVE UI with name and tags |
| Accident protection | None | `protection=1` blocks PVE-level deletion |
| API ops per `detach_disk` | 1 (DetachDisk config PUT) | 1 holder scan + 4–7 (EnsureParker, slot read, AttachDisk, provenance write) |
| API ops per `attach_disk` | 1 holder scan + 1 (AttachDisk config PUT) | 1 holder scan + 7–10 (lock, protection clear, DetachDisk on parker, unused-slot sweep, protection restore, unlock, AttachDisk, config read) |
| Cost of one holder scan | One `ListResources` **plus one config read per VM in the cluster** — O(cluster VM count), not a constant. On a 150-VM cluster that is ~151 calls. Both strategies pay it on `attach_disk` and `delete_disk`, because the refusal that keeps a stranded parker's volume safe has to know who holds the volume. | Same |
| `set_disk_metadata` while detached | Warning logged; nothing persisted | Warning logged; nothing persisted. A `bosh-parker`-tagged holder is skipped whatever the band says, so the parker's description — which carries the provenance sentinel — is never written to |
| `snapshot_disk` while detached | Fails (no hosting VM found) | Refused with explicit "parked" error |
| `resize_disk` while detached | Proceeds via storage backend | Proceeds via storage backend (parker stopped; no extra gate) |
| Provenance | None natively; CID encodes pool/node/AZ | Sentinel entry in parker description; `disk-audit` reads it |
| Capacity limit | Unlimited | 31 disks per parker VM; additional parkers created automatically |
| Concurrency | No extra synchronization | Parks and unparks on one parker are serialized against each other by a per-VMID sentinel-pool lock, advisory in the sense that a lock the CPI cannot take does not stop the work: a nil pool service, a denied `Pool.Allocate`, or a transport fault runs the window unserialized and warns. An acquire *timeout* is the exception, and is returned retriably, because it means another park or unpark is inside the window right now. Work inside the window runs under a deadline derived from the lock TTL, so a window cannot outlive the claim that entitles it; concurrent parks may still overwrite each other's provenance entry, and the disks themselves are safe either way |
| Blast radius on parker accidental delete | n/a | All disks attached to that parker VM are destroyed |
| Migration from free-floating | Existing disks remain free-floating | New detaches park; first `attach_disk` or `delete_disk` finds the free-floating disk and operates normally |

---

## Configuration reference

### `pve.detached_disk_strategy`

Selects the lifecycle strategy for detached persistent disks.

| Value | Behavior |
| --- | --- |
| `""` or `parked` | Default. Detached disks are attached to a dedicated parker VM with `protection=1`. |
| `free` | Opt-out. Detached disks float as unattached storage volumes. |

### `pve.parked_disk_vmid_range_start`

Inclusive lower bound of the VMID band reserved for parker VMs. The band
resolves under every strategy, at the job level and in every cpi-config entry
alike: under `parked` it is where parker VMs are allocated, and under `free` it
is read-only, letting the holder scans recognize and unpark disks parked
earlier. A zero value resolves to the built-in bound.

### `pve.parked_disk_vmid_range_end`

Inclusive upper bound of the parker VMID band, resolving on the same terms as
the lower bound. Must be greater than `parked_disk_vmid_range_start`.

### Range defaults and validation

Each bound is filled independently, so moving one and leaving the other at the
`0` the job spec documents produces a complete band rather than a half-open one.
That matters most in a cpi-config entry, which overrides one key at a time.

The missing bound comes from the built-in band when that yields a window no wider
than the built-in one — narrowing `90000-90999` from either side. A bound outside
that window derives its partner from the bound we named, at the same 1000-VMID
width: `parked_disk_vmid_range_start: 50000` gives `[50000,50999]`, not
`[50000,90999]`. A band we did not describe should not be wider than the one we
did.

At config load time, the CPI validates that the parker band does not overlap:

- The VM VMID range (`vmid_range_start`–`vmid_range_end`, defaults `100–8999`).

- The persistent-disk range (`disk_vmid_range_start`–`disk_vmid_range_end`, defaults `9000–29999`).

- The stemcell-template range (`stemcell_template_vmid_range_start`–`stemcell_template_vmid_range_end`, defaults `30000–30999`).

Overlap is a hard validation error only under the parked strategy, which is the
only strategy that allocates a parker VMID for another band to collide with.
Under `free` the band is read-only — it exists to keep the unpark probes running
— and every parker classification also requires the `bosh-parker` tag, so an
overlap there cannot mistake a workload VM for a parker and is accepted.

The bands are validated against each other, never against VMIDs already
allocated. A cluster that ran with a wider `vmid_range` may hold workload VMs
inside a parker band the CPI later accepts; those VMs are never misread as
parkers, because every classification requires the tag as well.

An unchanged config upgrading into the new default is treated differently. If the
strategy was never set and no parker band was configured, and the built-in band
`90000–90999` overlaps a band the operator did configure, the CPI stands the
parked default down for that load, keeps the previous free-floating behavior for
new detaches, and warns:

> `config: the default detached_disk_strategy "parked" is standing down for this load: its built-in parker band [90000,90999] overlaps the configured vmid_range [40000,200000]. Detached persistent disks stay free-floating for as long as that is true. The band itself remains in force read-only, so any disk parked before the overlap existed is still recognized and unparked on its next attach_disk or delete_disk. To park disks here, set parked_disk_vmid_range_start/end to a 1000-wide window that does not overlap any other VMID band. To silence this notice, set detached_disk_strategy to "free" explicitly`

The alternative is worse than the warning. The CPI binary is executed once per
JSON-RPC request, so a config that fails to load is not a deploy-time error: it is
every subsequent CPI call failing, and in the `bosh create-env` case it lands after
the old Director VM has already been stopped. Set `parked_disk_vmid_range_start`
and `parked_disk_vmid_range_end` to a free window to turn parking on for such a
deployment, or set `detached_disk_strategy: free` to accept the pre-parking
behavior and silence the notice.

### When to opt out

Parking is the default because a detached disk with no PVE-side owner is the
sharper edge: the failure it prevents (an administrator deleting what looks like
an unused volume) destroys data, while the cost it adds is a handful of API calls
per detach and attach on top of a holder scan both strategies now pay.

Keep the default `parked` when:

- PVE administrators are not BOSH-aware and may act on storage objects directly.

- Cluster-level automation or monitoring scans for unattached volumes.

- Compliance requirements call for explicit ownership records on all storage objects.

- The cluster uses a dedicated PVE role for the BOSH API token and that role does not
  grant storage-delete rights to other users.

Set `free` when:

- All PVE administrators are BOSH-aware and understand the VMID naming bands.

- API latency per `detach_disk` and `attach_disk` is a concern (shared or high-latency storage).

- The deployment is a single-operator environment with no separate PVE admin team.

- The VMID layout cannot give up a band for parker VMs.

### Migrating between strategies

**Free to parked** (which is also what an upgrade onto this default does for a
deployment that never set the property): existing detached disks remain free-floating. New `detach_disk`
calls park immediately. When the Director next calls `attach_disk` or
`delete_disk` for a free-floating disk, the unpark check finds the disk is not
parked and the operation proceeds normally. The two states coexist transparently;
`disk-audit` reports them separately.

**Parked to free:** existing parked disks self-heal on the next `attach_disk`
or `delete_disk`. Both handlers resolve the volume's current holder before they
act; when that holder is a parker they unpark first, and the disk becomes
free-floating. The band resolves to `90000–90999` even when unset, so no
property needs to be carried forward for the drain to work — switching the
strategy is the whole migration. Parker VMs that become empty remain in PVE;
remove them manually following the teardown procedure above.

**Mixed state is safe.** `attach_disk` and `delete_disk` resolve the holder on
every call, whatever `detached_disk_strategy` says, so free-floating and parked
disks coexist and each is handled on its own terms.

**Moving the band away from live parkers:** the band is what tells the CPI
which VMIDs are parkers, so pointing it at a different window while disks are
still parked in the old one leaves those parkers unrecognized. The CPI does not
proceed blindly there. `attach_disk` and `delete_disk` still resolve the
holder, read its tags, and refuse the call with an error naming
`parked_disk_vmid_range_start` / `parked_disk_vmid_range_end` when the holder
turns out to be a `bosh-parker` VM; `delete_vm` refuses a parker VM on the tag
its cluster-resources row carries, whatever the band says, falling back to the
VM config when that row carries no tags at all. The refusals are non-retriable
and say what to restore, so a disk is never double-referenced and a parker is
never purged with disks in its slots.

> **Drain before moving the band — removing a custom band is a move too.**
> Changing `parked_disk_vmid_range_start` / `parked_disk_vmid_range_end` while
> disks remain parked in the old window does not lose data, but it does stop
> those disks being attachable or deletable until the old band comes back.
> Removing the knobs counts when the old band was not `90000`–`90999`: unset
> bounds resolve to the built-in window, which is a move away from wherever the
> custom band's parkers live. Run `disk-audit` first and confirm it reports
> zero parked disks. To drain, leave the band where the parkers live and let
> the next `attach_disk` or `delete_disk` on each disk unpark it (the "Parked
> to free" procedure above), then move or remove the knobs once the audit is
> clean.

## Three consequences worth knowing

**`attach_disk` and `delete_disk` depend on a readable cluster.** Both resolve
the volume's current holder before they act, and that scan reads the config of
every VM in the cluster. A node that is down answers its guests' config reads
with a transport error rather than a 404, so both calls fail retriably until the
node is back, including for disks and VMs on healthy nodes. The alternative is to
skip the VMs we cannot read and conclude the volume is free, which is how a
volume ends up attached to two VMs at once. Under `free` this is new: before
parking became the default, those two calls made no cluster call at all.

**Unparking re-reads the parker before it detaches.** PVE's detach names a slot,
not a volume, so a slot resolved before the protection lock is held would be a
blind write by the time it runs. The unpark re-resolves the volume's slot inside
the window, sweeps it if PVE has already demoted it to an `unusedN` key, and
does nothing at all if another caller got there first.

**A failed unused-slot sweep leaves an invisible reference, and nothing clears it
on its own.** PVE does not free a detached volume; it demotes it to an `unusedN`
key, and the CPI clears that key as a second step inside the same protection
window. If the clear fails, the volume is referenced by a key no holder probe
matches on.

The retry does not fix it. By that point the detach itself has succeeded, so the
retry's holder scan finds nothing on the parker and returns without sweeping,
and the attach it guards goes ahead. That is why the CPI fails the call as
permanent rather than retriable: a retriable failure would send the Director
straight down the path that attaches a volume the parker still references.

The error names the parker and carries the `qm unlink` sequence that clears the
reference, the same condition is logged at ERROR, and `disk-audit` counts the
`unusedN` key: the parker is not reported as empty, it is not offered as a
teardown candidate, and it draws a warning of its own naming the same sequence. Destroying that parker by hand before the reference is
cleared frees the live volume with it, since `qm destroy` walks the config's
`unusedN` entries too. Treat it as an action item, not a warning.

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
a non-zero exit code. A parker is reported as empty, and offered as a teardown
candidate, only when its config was read and holds neither a bus disk nor an
`unusedN` reference. An `unusedN` entry gets its own warning naming the `qm
unlink` sequence that clears it, because `qm destroy --purge` frees the volume
behind such an entry as readily as one in a `scsiN` slot. An empty parker is the
expected state after a full strategy migration.

---

## Residual risk

Parking is now the default, so every deployment that does not name a strategy
gets it on upgrade. It has not been validated against a live Proxmox VE cluster.
The implementation is code-complete and unit-tested, and `scripts/lifecycle`
carries park and unpark coverage, but that coverage is itself new and has not
been run against a live cluster either.

There is no "before enabling" moment left, so treat the list below as
post-upgrade verification, on the first cluster to take the new release:

- Confirm the parker VMID band does not collide with any existing VMs in the
  target cluster. This is the one item worth doing before the upgrade rather
  than after, and setting `pve.detached_disk_strategy: free` defers the rest.

- Run `scripts/disk-audit` after a `detach_disk` cycle to confirm that parker
  VMs appear and that provenance entries match the attached slots.

- Verify that `attach_disk` and `delete_disk` complete cleanly on a parked disk.

- Confirm that `protection=1` on the parker VM prevents accidental deletion
  through the PVE UI and API.
