# Persistent disks

The BOSH PVE CPI exposes BOSH persistent disks as PVE storage volumes. Each
disk is created on a single PVE storage and attached to one VM at a time. This
page documents how the CPI chooses the target node for every disk operation,
which PVE storage types are supported, and the cloud-properties knobs an
operator can set per disk pool.

## Backend auto-classification

The CPI inspects the target PVE storage via `GET /storage` and classifies it
as **shared** (cluster-visible) or **local** (single-node). Classification
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

Cache TTL: 60 seconds. Edits to `/etc/pve/storage.cfg` are picked up within
that window.

## Cloud-properties reference

All keys are optional. Missing keys fall back to CPI defaults
(`disk_storage` / `vm_disk_format` from the CPI config).

| Key           | Type   | Backend  | Effect                                                                                          |
| ------------- | ------ | -------- | ----------------------------------------------------------------------------------------------- |
| `storage`     | string | both     | Overrides the global `disk_storage` for this pool.                                              |
| `disk_format` | string | both     | PVE disk format: `qcow2` (file storages) or `raw` (required for `lvm`/`lvmthin`/`zfspool`).     |
| `node`        | string | local    | Pins disks to a specific PVE node. Required when no `vm_cid` hint is available and the disk pool maps to a local-backend storage. Ignored by shared backends. |

### Node selection precedence

**create_disk** (new volume):

- Shared backend: `cloud_properties.node` → VM's current node (via `vm_cid`) → `config.node`.
- Local backend: VM's current node (via `vm_cid`) → `cloud_properties.node` → `config.node`. The vm_cid hint comes first because a local-storage disk MUST live on the same node as its owner VM.

**delete_disk / has_disk / attach_disk / detach_disk / resize_disk / snapshot_disk / update_disk** (existing volume):

- Shared backend: `config.node` (any node works).
- Local backend: cluster-wide scan via `Storage.Exists()` on every candidate node; the first node that reports the volume present wins.

**attach_disk** also verifies co-location when the backend is local: if the VM
runs on `pve-01` but the disk lives on `pve-02`, the call fails with a clear
"local-storage disks cannot cross nodes" error rather than producing a stale
or unattachable disk.

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

No `node` field. The CPI uses `config.node` for the storage call; the disk
attaches to whichever node the BOSH VM lives on.

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

## Known limitations

- **Snapshots require the disk to be attached.** PVE does not provide a per-volume snapshot primitive; `snapshot_disk` takes a VM snapshot of the host VM. Detached-disk snapshots would require a worker-VM workaround (tracked separately).
- **No cross-node move.** Once a local-storage disk lives on `pve-01`, recreating its owner VM on `pve-02` is rejected by `attach_disk`. Use `pvesm move` manually, or use a shared backend.
- **`set_disk_metadata`** stashes BOSH metadata in the host VM's description (sentinel comment block). Detached disks log a warning and persist nothing.
- **Shrink not supported.** PVE's resize endpoint is additive only; requesting a smaller size returns `NotSupported`.

See `~/.agents/specs/pve-disk-api-research.md` for the underlying PVE API
constraints and `~/.agents/specs/bosh-cpi-persistent-disk-contract.md` for the
BOSH-side contract this implementation satisfies.
