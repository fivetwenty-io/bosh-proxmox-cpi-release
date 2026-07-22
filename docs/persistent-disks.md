# Persistent disks

The BOSH PVE CPI maps BOSH persistent disks to PVE storage volumes. Each
disk lives on a single PVE storage and attaches to one VM at a time. This
page covers how the CPI selects the target node for each disk operation,
which PVE storage types are supported, the cloud-properties an operator
can set per disk pool, disk CID encoding, and the delete safety guard.

## Backend auto-classification

The CPI inspects the target PVE storage via `GET /storage` and classifies it
as **shared** (cluster-visible) or **local** (single-node). This classification
drives node selection for every disk operation.

| PVE storage type | Backend  | Notes                                                             |
| ---------------- | -------- | ----------------------------------------------------------------- |
| `rbd`            | shared   | Ceph RBD; any cluster node may run the disk op.                   |
| `cephfs`         | shared   | Ceph file system.                                                 |
| `nfs`            | shared   | Network file system export.                                       |
| `cifs`           | shared   | SMB/CIFS export.                                                  |
| `glusterfs`      | shared   | Glusterfs volume.                                                 |
| `pbs`            | shared   | Proxmox Backup Server.                                            |
| `lvm`            | local    | LVM volume group. Volume lives on the node that owns the VG.      |
| `lvmthin`        | local    | LVM thin pool. Same constraint as `lvm`.                          |
| `zfspool`        | local    | ZFS dataset; node-pinned.                                         |
| `dir`            | local    | Local directory.                                                  |
| `btrfs`          | local    | Local btrfs subvolume.                                            |
| any with `shared=1` | shared | Any storage explicitly flagged shared in `storage.cfg`.           |
| anything else    | local    | Safe default — forces explicit node selection via cloud_props.    |

Cache TTL: 60 seconds. Edits to `/etc/pve/storage.cfg` take effect within
that window.

## Cloud-properties reference

All keys are optional. Unset keys fall back to CPI defaults
(`disk_storage` / `vm_disk_format` from the CPI config).

| Key                  | Type    | Effect                                                                                                                                                                                     |
| -------------------- | ------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `storage`            | String  | Overrides the global `disk_storage` for this pool.                                                                                                                                         |
| `disk_format`        | String  | PVE disk format: `qcow2` (file storages) or `raw` (required for `lvm`/`lvmthin`/`zfspool`).                                                                                               |
| `node`               | String  | Pins disks to a specific PVE node. Required when no `vm_cid` hint is available and the disk pool maps to a local-backend storage. Ignored by shared backends.                              |
| `iothread`           | Boolean | Enables a dedicated I/O thread for the disk controller. Improves throughput on multi-core VMs. Overrides `pve.disk_performance.iothread`, which defaults to `true` — set `false` here to opt this disk out of the global default. |
| `cache`              | String  | PVE cache mode for the disk. Accepted values: `none`, `writethrough`, `writeback`, `unsafe`, `directsync`. Omit to use PVE's default.                                                      |
| `aio`                | String  | PVE AsyncIO backend for the disk. Accepted values: `native`, `io_uring`, `threads`. Omit to use PVE's default (`io_uring` on modern PVE hosts). Overrides `pve.disk_performance.aio`. `native` paired with `cache: none` suits block-backed pools; see `pve.disk_performance.aio` for full guidance. |
| `discard`            | Tri-state (`true`\|`false`\|unset) | Passes discard/TRIM commands through to the backing storage, reclaiming space on thin-provisioned volumes. Unset (default) auto-resolves from this disk's actual resolved storage pool's TRIM capability — `pve.disk_performance.discard` documents the full matrix. `true`/`false` force the value regardless of backend. |
| `ssd`                | Tri-state (`true`\|`false`\|unset) | Marks the disk as an SSD for guest OS detection. Same auto-resolution as `discard`. Not applicable to `virtio-blk` buses (dropped there unconditionally, even when auto-resolved true). |
| `mbps_rd`            | Integer | Read throughput cap in MB/s. Set to `0` to remove the limit.                                                                                                                               |
| `mbps_wr`            | Integer | Write throughput cap in MB/s. Set to `0` to remove the limit.                                                                                                                              |
| `iops_rd`            | Integer | Read IOPS limit. Set to `0` to remove the limit.                                                                                                                                           |
| `iops_wr`            | Integer | Write IOPS limit. Set to `0` to remove the limit.                                                                                                                                          |
| `virtio_scsi_single` | Boolean | Enables `virtio-scsi-single` controller mode (one controller per disk). Overrides `pve.disk_performance.virtio_scsi_single`, which defaults to `true` (a shared `virtio-scsi-pci` controller was the earlier default) — set `false` here to opt this VM out of the global default. |
| `retain_on_delete`   | Boolean | When `true`, opts this disk out of deletion during `delete_disk` and `delete_vm`. The volume is preserved on storage; the CPI skips its deletion step and logs the retained volume. Default: omitted (deletion proceeds normally). |

Per-disk performance settings take precedence over the global `pve.disk_performance.*` defaults
in the CPI config. See [configuration.md](configuration.md) for the full property reference.

### Retain-on-delete

Setting `retain_on_delete: true` on a `create_disk` cloud_properties block encodes a retention flag into the disk CID at creation time. The flag is carried in `DiskCIDMeta.Opts` as `retain_on_delete=1` and survives `attach_disk`, `detach_disk`, and `update_disk` operations — the CID is preserved throughout the disk's lifetime.

Both `delete_disk` and `delete_vm` check this flag before destroying a volume:

- **`delete_disk`:** if the flag is present, the CPI skips the `DELETE /nodes/{n}/storage/{s}/content/{volume}` call and returns success without destroying the volume. The volume remains on storage.

- **`delete_vm`:** persistent disks flagged `retain_on_delete` that are still attached to the VM at delete time are detached and preserved, following the same foreign-disk guard described below.

Use this flag for volumes whose lifecycle the Director should not control — for example, a shared data volume or a manually managed backup disk. Volumes retained this way are invisible to subsequent BOSH deployments; coordinate their cleanup outside BOSH.

Example:

```yaml
disk_pools:
- name: retained-data
  disk_size: 102400
  cloud_properties:
    storage: ceph-rbd
    disk_format: raw
    retain_on_delete: true
```

## Disk delete state guard

The `pve.disk_delete_state_guard` config property (default: on) blocks `delete_disk`
when the disk's hosting VM is in a transient state (such as locked, migrating, or snapshotting) —
closing the race window against nightly vzdump/PBS backups and other in-flight operations. With
the guard active, `delete_disk` defers deletion and returns a retriable error; the Director retries
on the next cycle. The guard fails open on resolution uncertainty so that disks attached to no VM
pass through without delay. Set `pve.disk_delete_state_guard: "off"` to restore the earlier unguarded
behavior (no attachment lookup).

## Node selection precedence

**create_disk** (new volume):

- Shared backend: `cloud_properties.node` → VM's current node (via `vm_cid`) → `config.node`.

- Local backend: VM's current node (via `vm_cid`) → `cloud_properties.node` → `config.node`. The `vm_cid` hint takes priority because a local-storage disk must live on the same node as its owner VM.

**delete_disk / has_disk / attach_disk / detach_disk / resize_disk / snapshot_disk / update_disk** (existing volume):

- Shared backend: `config.node` (any node works).

- Local backend: cluster-wide scan via `Storage.Exists()` on every candidate node; the first node that reports the volume present wins.

**attach_disk** also verifies co-location when the backend is local: if the VM
runs on `pve-01` but the disk lives on `pve-02`, the call fails with a clear
"local-storage disks cannot cross nodes" error rather than producing a stale
or unattachable disk.

## Disk CID encoding

Disk CIDs produced by `create_disk` use the `pvd-` envelope format:

```
pvd-<base64url(JSON payload)>
```

The payload is a JSON object with two fields:

| Field | Description                                                                            |
| ----- | -------------------------------------------------------------------------------------- |
| `v`   | The exact PVE volid (`<storage>:<volume>`) — what every PVE storage API call receives. |
| `m`   | Optional `DiskCIDMeta` placement metadata; omitted when all fields are zero-valued.    |

`DiskCIDMeta` carries:

| Field  | Description                                                                               |
| ------ | ----------------------------------------------------------------------------------------- |
| `pool` | PVE storage pool name (e.g. `local-lvm`). Populated from the resolved storage.           |
| `node` | PVE node that owns the disk. Set for local-backend volumes; empty for shared storage.     |
| `az`   | Availability-zone label at create time. Empty when no AZ was resolved.                   |
| `opts` | Per-disk performance settings map (iothread, cache, discard, ssd, mbps_rd, etc.).        |

The envelope uses RFC 4648 §5 base64url encoding with no padding, so an emitted
CID contains only `[A-Za-z0-9_-]`. That charset is the point: PVE's own volids
are REST-hostile as Director-visible identifiers. A path-form volid
(`local:9001/vm-9001-disk-0.qcow2`) embeds `/`, which 404s the Director's
`/disks/<cid>/attachments` route, and the earlier `|` metadata separator was
mangled by `bosh` CLI argument passthrough — together they made
`bosh attach-disk` unusable. The CPI decodes the envelope back to the exact
bare volid before any PVE API call.

Example CID for `local-lvm:vm-300-disk-0` with an `iothread` option:

```
pvd-eyJ2IjoibG9jYWwtbHZtOnZtLTMwMC1kaXNrLTAiLCJtIjp7InBvb2wiOiJsb2NhbC1sdm0iLCJvcHRzIjp7ImlvdGhyZWFkIjoidHJ1ZSJ9fX0
```

`attach_disk` decodes the `opts` map and merges it with the global `pve.disk_performance.*`
config; per-disk values win over global defaults.

Two legacy formats from earlier releases decode forever, because the Director
replays stored CIDs indefinitely: the bare volid (`<storage>:<volume>`) and the
pipe-annotated form (`<storage>:<volume>|<base64url-JSON DiskCIDMeta>`). A PVE
storage whose name literally starts with `pvd-` still parses correctly — its
bare CID contains `:`, which can never appear in a valid base64url payload, so
the decoder falls back to the legacy paths.

`create_disk` logs a warning when an emitted CID exceeds 255 characters:
MySQL-backed Directors store `disk_cid` in a `VARCHAR(255)` column, and the
envelope can cross that bound when long storage names combine with many
per-disk performance options.

### Compressed CIDs (`pvz-`, opt-in)

Setting `pve.disk_cid_compression: true` opts `create_disk` into a compressed
variant for exactly the over-limit case above. When the standard `pvd-` form
would exceed 255 characters, the CID is emitted as:

```
pvz-<base64url(gzip(JSON payload))>
```

The JSON payload, the charset guarantee, and the decode rules are identical to
`pvd-`; only the container changes. CIDs that fit 255 characters are emitted as
`pvd-` unchanged, byte-identical to the flag-off encoding, so enabling the flag
never alters the common case. If gzip cannot shorten an unusually high-entropy
payload, the plain form is kept (both overflow; the length warning fires either
way).

The flag exists for MySQL-backed Directors, whose `disk_cid` column is a hard
`VARCHAR(255)` (strict mode rejects longer values; legacy non-strict mode
silently truncates them). PostgreSQL-backed Directors store CIDs in unbounded
`text` columns and gain nothing from compression — leave the flag off there.
(The one PostgreSQL exception is the newer `dynamic_disks` Director table,
which is `varchar(255)` on every backend.)

The CPI decodes `pvz-` unconditionally and forever, regardless of the flag —
like every format it has ever emitted — so the flag can be enabled or disabled
at any time without migrating existing disks. A storage literally named
`pvz-…` falls back to the legacy paths by the same `:`-based rule as `pvd-`,
and a hostile CID cannot decompression-bomb the CPI: inflation is capped at
64 KiB.

To inspect a compressed CID by hand (restores the base64 padding, then
un-gzips the payload):

```sh
cid="pvz-H4sIAAAAAAAC_..."           # the emitted CID
payload="${cid#pvz-}"
python3 -c "import sys,base64,gzip; p=sys.argv[1]; print(gzip.decompress(base64.urlsafe_b64decode(p + '=' * (-len(p) % 4))).decode())" "$payload"
```

`get_disks` intentionally returns bare volids, matching what PVE's config scan
reports. It has never returned the annotated form — metadata exists only in
CIDs minted by `create_disk` — and that asymmetry is unchanged by the envelope.

## Worked examples

### Ceph RBD on a multi-node cluster

```yaml
disk_pools:
- name: disks
  disk_size: 10240
  cloud_properties:
    storage: ceph-rbd        # type=rbd → shared backend, any node attaches
    disk_format: raw         # rbd does not support qcow2
```

No `node` field is needed. The CPI uses `config.node` for the storage call; the disk
attaches to whichever node the BOSH VM runs on.

### Local ZFS on a single-node deployment

```yaml
disk_pools:
- name: disks
  disk_size: 10240
  cloud_properties:
    storage: local-zfs       # type=zfspool → local backend
    disk_format: raw
    # node not required: config.node is the only node
```

### Local ZFS on a multi-node cluster with availability zones

```yaml
azs:
- name: z1
  cloud_properties: {node: pve-01}
- name: z2
  cloud_properties: {node: pve-02}

disk_pools:
- name: disks
  disk_size: 10240
  cloud_properties:
    storage: local-zfs
    disk_format: raw
    # 'node' here is optional: when the AZ is wired through to vm_cid, the
    # CPI co-locates the disk with the VM automatically. Set 'node' only
    # when a disk must be pinned independently of any owner VM.
```

### High-performance disk with per-disk options

```yaml
disk_pools:
- name: fast-disks
  disk_size: 51200
  cloud_properties:
    storage: local-lvm
    disk_format: raw
    iothread: true
    cache: none
    discard: true
    mbps_rd: 500
    mbps_wr: 300
```

These options are encoded into the disk CID at `create_disk` time and applied
automatically when `attach_disk` runs. They also override the global
`pve.disk_performance.*` defaults for this pool.

## delete_vm and persistent-disk safety

`delete_vm` destroys the VM along with its root and ephemeral disks, but must never
destroy a persistent disk that the BOSH Director has not explicitly released.

Persistent disks are identified by the VMID embedded in their PVE volid.
`create_disk` allocates volumes under a synthetic free VMID chosen at creation
time, so a persistent disk volid such as `zfs-1:vm-15689-disk-0` carries VMID
15689 even while attached to VM 6031. Any active-slot disk whose embedded VMID
differs from the owning VM's VMID is a foreign persistent disk.

If a persistent disk is still attached to an active bus slot when `delete_vm`
runs — for example, after an interrupted Director recreate that skipped
`detach_disk` — the CPI detaches it automatically. The volume is preserved on
storage before the VM is destroyed.

If the detach cannot complete (for example, because PVE returns a lock-timeout
error), `delete_vm` refuses to destroy the VM and returns a retriable error.
The Director retries; the next attempt re-detaches before proceeding. No volume
is lost to a transient PVE failure.

A persistent volume can also linger in an `unusedN` config slot when a
snapshot reference prevents PVE from fully sweeping it during the detach. The CPI
probes the configured `pve.disk_storage` for the volume and refuses to destroy the VM
while any such volume still exists. This refusal is not retriable: remove the
snapshot (or the unused slot) before deleting the VM. An `unusedN` slot whose
volume has already been deleted from storage does not block the destroy.

**Operator note:** An interrupted `create-env` recreate sequence no longer risks
the Director database disk. The guard runs on both the synchronous delete path
and the fast-path (`fast_path_delete: true`) delete path.

## Known limitations

- **Snapshots require the disk to be attached.** PVE provides no per-volume snapshot primitive; `snapshot_disk` takes a VM snapshot of the host VM. Detached-disk snapshots would require a worker-VM workaround (tracked separately).

- **No cross-node move.** Once a local-storage disk lives on `pve-01`, recreating its owner VM on `pve-02` is rejected by `attach_disk`. Use `pvesm move` manually, or switch to a shared backend.

- **`set_disk_metadata`** stashes BOSH metadata in the host VM's description (sentinel comment block). Detached disks log a warning and persist nothing.

- **Shrink not supported.** PVE's resize endpoint is additive only; requesting a smaller size returns `NotSupported`. Enable `pve.resize_wait_for_convergence` to poll until the guest filesystem reports the new size after an additive resize.

- **Detached disks have no PVE-side ownership protection by default.** A detached disk floats as an unattached volume inside its synthetic VMID container. PVE has no first-class volume object, so an administrator scanning for unused VMs can delete that container and destroy the disk. The opt-in `detached_disk_strategy: parked` mode attaches detached disks to a dedicated parker VM with `protection=1`, making ownership visible in the PVE UI and blocking accidental deletion. See [Persistent Disk Strategy](persistent-disk-strategy.md) for the full analysis and configuration details.

Refer to [configuration.md](configuration.md) for the full property reference.
