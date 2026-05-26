# Configuration

The CPI is configured via properties in a BOSH deployment manifest. The job template renders the manifest properties into a JSON document that the binary reads with the `--config` flag. The properties below match `jobs/pve_cpi/spec`.

| Property | Description | Default | Required |
|---|---|---|---|
| `pve.host` | PVE host (IP or FQDN) | - | yes |
| `pve.port` | PVE API port | `8006` | no |
| `pve.user` | PVE username | - | yes |
| `pve.password` | PVE password | - | one of password or api_token |
| `pve.api_token` | PVE API token | - | one of password or api_token |
| `pve.realm` | Authentication realm | `pam` | no |
| `pve.node` | Default node | - | yes |
| `pve.vm_storage` | Storage pool for VM root disks | - | yes |
| `pve.disk_storage` | Storage pool for persistent disks | - | yes |
| `pve.stemcell_storage` | Storage pool for stemcell qcow2 images. Must be a file-based PVE storage (dir, nfs, cifs, glusterfs, cephfs) — block-based storages (lvm, lvmthin, zfspool, rbd) cannot accept qcow2 uploads. Must also be shared across cluster nodes when the cluster has more than one node. Defaults to `vm_storage`; in that case `vm_storage` must satisfy the same constraints. | `vm_storage` | no |
| `pve.network_bridge` | Default network bridge | `vmbr0` | no |
| `pve.verify_ssl` | Verify TLS certificates | `true` | no |
| `pve.agent_mode` | Agent bootstrap mode (`cloudinit`, `registry`, `noagent`) | `cloudinit` | no |
| `pve.vm_disk_format` | Disk image format (`qcow2`, `raw`, `vmdk`) | `qcow2` | no |
| `pve.reboot_mode` | `reboot_vm` strategy: `soft` (graceful ACPI reboot, hard-reset fallback) or `hard` (immediate reset) | `soft` | no |
| `pve.reboot_timeout` | Seconds to wait for graceful shutdown before hard-reset fallback (soft mode only) | `60` | no |
| `pve.log_level` | Log level (`debug`, `info`, `warn`, `error`) | `info` | no |
| `pve.vmid_range_start` | First VMID used for VM allocation. VMs use `[vmid_range_start, vmid_range_end]`. Persistent disks use `[9000, 9999]`. | `100` | no |
| `pve.vmid_range_end` | Inclusive upper bound of the VM VMID range. Must be greater than `vmid_range_start` and at most `9999`. The allocator scans this range from a randomized start so concurrent CPI invocations rarely pick the same VMID; a retry-on-conflict loop backstops the rare collision. | `5999` | no |
| `pve.allow_disk_ops_with_snapshots` | When `true`, bypasses the snapshot pre-flight guard in `attach_disk`, `detach_disk`, and `resize_disk`. Use only for emergency disk recovery — snapshot state becomes inconsistent after the operation. | `false` | no |
| `pve.require_snapshot_check_pass` | Controls behavior when the snapshot pre-flight check itself cannot reach PVE. `false` (default) logs a warning and proceeds (fail-open); `true` aborts the disk operation if the snapshot list cannot be fetched (fail-closed). | `false` | no |
| `registry.endpoint` | BOSH registry URL | - | yes when `agent_mode = registry` |
| `registry.user` | Registry username | - | yes when `agent_mode = registry` |
| `registry.password` | Registry password | - | yes when `agent_mode = registry` |

## Stemcell Storage

`stemcell_storage` must be a **file-based** PVE storage pool. The CPI uploads the qcow2 image via the PVE upload API, which only accepts file-based storages (`dir`, `nfs`, `cifs`, `glusterfs`, `cephfs`). Block-based storages (`lvm`, `lvmthin`, `zfspool`, `rbd`) reject uploads with `can't upload to storage type '<type>'` and are unusable for stemcells regardless of cluster topology.

For multi-node clusters, `stemcell_storage` must additionally be shared. The `create_stemcell` call enforces this: if the storage is local-only and the cluster has more than one node, the call fails immediately with a descriptive error. Single-node clusters may use local file-based storage (e.g. the default `local` dir at `/var/lib/vz`); the shared check is skipped when the cluster reports exactly one node.

Recommended shared backends: NFS, CIFS, CephFS, GlusterFS, or any other PVE storage type configured with `shared=1` in `/etc/pve/storage.cfg`.

The storage pool must have the `import` content type enabled. See [Proxmox VE Settings](pve-settings.md) for the steps to enable it.

## Authentication

Exactly one of `pve.password` or `pve.api_token` must be set. API tokens are preferred for production deployments; they support per-token revocation and per-token privilege separation in PVE 9.

See [pve-api-permissions.md](pve-api-permissions.md) for token creation and the minimum-privilege `bosh@pve` user setup.

## MBus fallback

When `agent.mbus` is empty but a blobstore endpoint is configured, the CPI derives `nats://<blobstore-host>:4222` from the blobstore endpoint host and uses that as the agent's NATS URL. Explicit `agent.mbus` values always win. Loopback hosts (`127.0.0.1`, `localhost`, `::1`, `0.0.0.0`) are rejected — the MBus stays empty so the misconfiguration fails loudly instead of being silently misrouted to a non-routable URL.

This convention matches the typical BOSH topology where NATS and the DAV blobstore are colocated on the director (or on the create-env machine during initial bootstrap, when the director does not yet exist to advertise an MBus URL).

## Example

```yaml
properties:
  pve:
    host: pve.example.com
    port: 8006
    user: root
    realm: pam
    password: ((pve_password))
    node: pve1
    vm_storage: local-lvm
    disk_storage: local-lvm
    stemcell_storage: nfs-shared
    network_bridge: vmbr0
    verify_ssl: true
    agent_mode: cloudinit
    vm_disk_format: qcow2
    log_level: info
```

In the example above, `nfs-shared` is a PVE NFS storage pool with the `import` content type enabled and `shared=1`. Both `vm_storage` and `stemcell_storage` must be accessible from all cluster nodes when operating a multi-node cluster.

## Custom Tags

Operators may attach arbitrary tags to VMs and persistent disks via the `tags` cloud-property on `vm_types` and `disk_types`. Tags surface in the PVE UI for filtering, cost-allocation, ownership tracking, and ad-hoc grouping.

The `tags` cloud-property is a map of `key: value` pairs. Each pair is sanitized and emitted as a `<key>--<value>` entry in the PVE tags field (PVE allows only `[A-Za-z0-9-]` in tag values; other bytes are replaced with `-`). Multiple entries are joined with `;`.

Example cloud-config snippet:

```yaml
vm_types:
- name: tagged
  cloud_properties:
    cpu: 2
    memory: 1024
    tags:
      env: prod
      owner: platform-team

disk_types:
- name: small
  disk_size: 1024
  cloud_properties:
    tags:
      tier: bronze
```

Notes:

- Tags are sanitized: a key like `bad key` becomes `bad-key`, a value like `with spaces` becomes `with-spaces`.

- The combined tag string is capped at 255 bytes; entries past the cap are dropped at a `;` boundary so partial entries are never emitted.

- The CPI reserves three tag-key prefixes for its own use: `director--`, `deployment--`, and `job--`. These are rebuilt from BOSH-supplied metadata on every `set_vm_metadata` call. Custom tags survive those re-syncs.

- PVE has no native disk-volume tag field. Tags supplied on a `disk_type` are written to the tags field of the VM the disk is attached to and recorded in the VM description sentinel under `bosh_disk_tags`. Disk tags only become visible once the disk is attached to a VM; if `create_disk` is called without a `vm_cid` hint, the tags are deferred and applied on the next `set_disk_metadata` call.
