# CPI Methods Reference

The BOSH PVE CPI implements the full BOSH CPI v2 specification: 21 canonical methods plus `update_disk`, a PVE-specific extension inherited from the prior implementation.

The CPI communicates over JSON-RPC on stdin/stdout. Each invocation handles one request and exits. Logs go to stderr.

---

## BOSH CPI v2 Differences from v1

The following changes apply when using CPI v2 (i.e., when the stemcell's `api_version` field is 2):

- `configure_networks` is removed. Networks are configured only at `create_vm` time.

- `create_vm` returns `[vm_cid, networks_with_mac]` — an array — instead of the v1 bare string `vm_cid`.

- `attach_disk` returns disk hints (e.g., `{"path": "/dev/sdd"}`) instead of void.

- Registry is optional. When a stemcell has `api_version: 2`, the CPI injects agent settings into the VM's cloud-init metadata at `create_vm` time. The agent reads from the IaaS metadata service and falls back to the registry only if configured.

---

## Info

### `info`

**Type:** Required v2

**Args:** none

**Returns:** `Hash` — `{ "api_version": 2, "stemcell_formats": ["openstack-qcow2", "openstack-raw", "pve-qcow2", "general-qcow2", "general-raw"] }`

**Errors:** none

**Notes:** The Director calls `info` first to determine the CPI's API version and supported stemcell formats. This CPI always returns `api_version: 2`.

The CPI advertises `openstack-qcow2` and `openstack-raw` because OpenStack qcow2/raw stemcells are byte-compatible with what PVE imports via `qm importdisk` — PVE treats the format name opaquely and only the on-disk image bytes matter. Operators running existing `bosh-openstack-kvm-*` stemcells can therefore upload them directly with no conversion. The `pve-*` and `general-*` aliases remain accepted for forward compatibility.

---

## Stemcell

### `create_stemcell`

**Type:** Required v2

**Args:**

- `args[0]` (String): `image_path` — absolute path to the extracted stemcell image file on the local filesystem
- `args[1]` (Hash): `cloud_properties` — parsed `cloud_properties` section from `stemcell.MF`

**Returns:** `String` — `stemcell_cid`

**Errors:** `Bosh::Clouds::CloudError` on PVE API failure or storage error

**Notes:** The CPI uploads the disk image to `stemcell_storage` as a qcow2 file under the `import` content type. If the image is a gzip+tar tarball (as produced by the BOSH stemcell builder), the disk image is extracted before upload. The CPI computes a SHA-256 of the disk image and builds a content-addressed filename:

```
bosh-stemcell-<name>-<version>-<sha8>.qcow2
```

After upload, the CPI creates a frozen PVE template VM from the qcow2 in the template VMID range and tags it with `bosh-stemcell-sha-<sha8>`. For CPI-owned images (heavy and light-fetch), the intermediate upload volume is deleted after the template is frozen. For operator-preuploaded light stemcell images, the upload volume is kept.

Template creation is idempotent: if a template VM named `bosh-stemcell-<name>-<version>` already exists, its VMID is reused.

The returned `stemcell_cid` is `template:<vmid>` (e.g. `template:6042`), identifying the frozen template VM.

`cloud_properties` must include `name` and `version`; both are required to build the deterministic template name. `stemcell_storage` must be a shared storage pool accessible from all cluster nodes.

---

### `delete_stemcell`

**Type:** Required v2

**Args:**

- `args[0]` (String): `stemcell_cid` — CID returned by `create_stemcell`

**Returns:** `null`

**Errors:** `Bosh::Clouds::CloudError` on PVE API failure

**Notes:** Routes on the stemcell CID format:

- `template:<vmid>` — destroys the template VM with `purge=true` (removes all associated disks). Idempotent: an absent VM is treated as success.
- `<storage>:import/<filename>` — deletes the qcow2 upload volume. Absent volumes are treated as success.
- `light:...` — no-op (operator-managed image; the CPI never deletes it).
- Integer-only CIDs — no-op (pre-upgrade legacy scrub).

Running VMs have no dependency on the template after cloning. The template can be deleted at any time without affecting running VMs.

---

## VM

### `create_vm`

**Type:** Required v2

**Args:**

- `args[0]` (String): `agent_id` — ID the Director has selected for the BOSH agent
- `args[1]` (String): `stemcell_cid` — CID of the stemcell to clone. `template:<vmid>` for stemcells uploaded by this CPI version; `<storage>:import/<filename>` or `light:...` for pre-upgrade stemcells (the CPI opportunistically upgrades to the clone path)
- `args[2]` (Hash): `cloud_properties` — resource pool properties from the manifest (e.g., `cpu`, `memory`, `ephemeral_disk_size`)
- `args[3]` (Hash): `networks` — NetworkSpec map; each key is a network name, each value has `type`, `ip`, `netmask`, `gateway`, `dns`, and `cloud_properties`
- `args[4]` (Array of String): `disk_cids` — persistent disks likely to be attached (for placement optimization)
- `args[5]` (Hash): `environment` — resource pool env merged with BOSH-appended properties

**Returns:** `Array` — `[vm_cid, networks_with_mac]`

**Errors:**

- `Bosh::Clouds::VMCreationFailed` if VM creation fails (CPI must clean up partial resources)
- `Bosh::Clouds::CloudError` on PVE API failure

**Notes:** Creates a new VM by cloning a stemcell template. The `stemcell_cid` drives dispatch:

- **`template:<vmid>`** — clones the identified template VM directly. On linked-clone–capable storage backends (`dir`, `nfs`, `cifs`, `zfspool`, `lvmthin`, `rbd`, `cephfs`) this is a copy-on-write clone that completes in seconds. On `lvm`-thick storage a full clone is performed. Clone type is controlled by `pve.clone_mode` (default `auto`).
- **Pre-upgrade CID** (`<storage>:import/<file>` or `light:...`) — the CPI extracts the sha8 from the filename and searches for a matching template by PVE tag. If a template is found, it clones it (fast path). If not, it falls back to the original `import-from=` block-copy (slow path, roughly four minutes for a typical stemcell). No re-upload is required for pre-upgrade stemcells.

A VMID is allocated from the range `[vmid_range_start, vmid_range_end]` (default: `[100, 8999]`). After the clone task completes, the CPI configures NICs, attaches any pre-existing persistent disks, writes agent settings, and starts the VM. The returned `networks_with_mac` hash augments the input networks map with MAC addresses assigned by PVE.

`cloud_properties.tags` (map of `key: value`) is applied to the PVE tags field on the new VM as sanitized `<key>--<value>` entries. The BOSH-managed `director--`, `deployment--`, and `job--` triple is not known at create time and is added later by `set_vm_metadata`. See [Custom Tags](configuration.md#custom-tags).

---

### `delete_vm`

**Type:** Required v2

**Args:**

- `args[0]` (String): `vm_cid` — CID returned by `create_vm`

**Returns:** `null`

**Errors:** `Bosh::Clouds::CloudError` if deletion is not certain (to prevent orphaned VMs)

**Notes:** Stops the VM if running, then destroys it. The CPI decodes the destroy task UPID and awaits its completion before returning success, so the Director never observes a still-pending volume on the storage backend. If persistent disks are attached, the CPI detaches them before destroying the VM. If the VM does not exist, the call succeeds without error.

---

### `has_vm`

**Type:** Required v2

**Args:**

- `args[0]` (String): `vm_cid`

**Returns:** `Boolean`

**Errors:** `Bosh::Clouds::CloudError` on PVE API failure

**Notes:** Used by the BOSH cloudcheck consistency tool to detect orphaned or missing VMs.

---

### `reboot_vm`

**Type:** Required v2

**Args:**

- `args[0]` (String): `vm_cid`

**Returns:** `null`

**Errors:** `Bosh::Clouds::CloudError` on PVE API failure

**Notes:** Reboots the VM using the strategy set by `pve.reboot_mode` (default `soft`).

- **soft** (default): sends a graceful ACPI reboot via the PVE API and waits up to `pve.reboot_timeout` seconds (default 60) for the guest to respond. If the guest does not shut down in time, or the reboot call fails for any reason other than a 404, the CPI falls back to a hard reset automatically. A 404 response means the VM was not found and raises `Bosh::Clouds::CloudError`.
- **hard**: issues an immediate hard reset (power cycle) with no grace period. This was the only behavior before the `reboot_mode` option was introduced.

If the VM is stopped when `reboot_vm` is called, the CPI starts it so the VM ends up running, matching the BOSH expectation. The BOSH wire contract is unchanged: `reboot_vm` accepts only `vm_cid`.

---

### `set_vm_metadata`

**Type:** Required v2

**Args:**

- `args[0]` (String): `vm_cid`
- `args[1]` (Hash): `metadata` — arbitrary key-value pairs; the CPI must not require specific keys

**Returns:** `null`

**Errors:** `Bosh::Clouds::CloudError` on PVE API failure

**Notes:** Stores metadata as PVE VM tags and/or description. The Director passes standard keys such as `director`, `deployment`, `instance_group`, `job`, `id`, `name`, `index`, and `created_at`. Do not override or filter these.

The handler reads the VM's existing PVE tags, strips entries with the reserved prefixes `director--`, `deployment--`, and `job--`, rebuilds the triple from the incoming metadata, and merges the result with any operator-supplied custom tags already on the VM. Custom tags from `create_vm` therefore survive director re-syncs without manual reconciliation.

---

### `calculate_vm_cloud_properties`

**Type:** Optional v2

**Args:**

- `args[0]` (Hash): `desired_instance_size`
  - `cpu` (Integer): virtual core count
  - `ram` (Integer): RAM in MiB
  - `ephemeral_disk_size` (Integer): ephemeral disk size in MB
  - `storage` (String, optional): overrides `pve.vm_storage` for this call only; the returned `target_storage` reflects it

**Returns:** `Hash` — `cloud_properties` suitable for use in a BOSH VM type

**Errors:** `Bosh::Clouds::NotSupported` when no node satisfies the request. The message names the requested cpu/ram and storage, and lists CPU/RAM-qualifying nodes that failed the storage check.

**Notes:** Selects a node storage-first: only nodes where the effective storage is active and `images`-capable are considered, then the node with the most free RAM among them wins. This prevents placing a VM on a node where the storage is unavailable (which previously failed later in `create_vm` with an opaque PVE error). Returns the minimum PVE cloud_properties (cores, sockets, memory, target node, target storage) that satisfy the requested size. May oversize. Used by the BOSH CLI `interpolate` and `env` commands.

---

## Disk

### `create_disk`

**Type:** Required v2

**Args:**

- `args[0]` (Integer): `size` — disk size in MiB
- `args[1]` (Hash): `cloud_properties` — from the deployment manifest disk pool
- `args[2]` (String): `vm_cid` — VM where the disk will likely be attached (for placement optimization)

**Returns:** `String` — `disk_cid`

**Errors:** `Bosh::Clouds::CloudError` on storage or PVE API failure

**Notes:** Allocates a disk on `disk_storage`. The disk CID encodes the storage pool and disk identifier. Disks use VMIDs in the `[disk_vmid_range_start, disk_vmid_range_end]` range (default `[9000, 29999]`).

`cloud_properties.tags` (map of `key: value`) is applied to the PVE tags field on the VM identified by `vm_cid`. PVE has no native disk-volume tag field — tags ride on the hosting VM. When `vm_cid` is empty (Director is creating an unattached disk), the tags are deferred and applied on the next `set_disk_metadata` call. See [Custom Tags](configuration.md#custom-tags).

---

### `delete_disk`

**Type:** Required v2

**Args:**

- `args[0]` (String): `disk_cid`

**Returns:** `null`

**Errors:** `Bosh::Clouds::CloudError` if deletion is not certain (to prevent orphaned disks)

**Notes:** The disk must be detached before deletion. The Director ensures `detach_disk` is called first.

---

### `has_disk`

**Type:** Required v2

**Args:**

- `args[0]` (String): `disk_cid`

**Returns:** `Boolean`

**Errors:** `Bosh::Clouds::CloudError` on PVE API failure

**Notes:** Used by cloudcheck to detect orphaned or missing persistent disks.

---

### `attach_disk`

**Type:** Required v2

**Args:**

- `args[0]` (String): `vm_cid`
- `args[1]` (String): `disk_cid`

**Returns:** `Hash` — disk hints, e.g. `{"path": "/dev/sdd"}`

**Errors:** `Bosh::Clouds::CloudError` on PVE API failure

**Notes:** Attaches the disk to the VM's PVE config and awaits the PVE task. Returns the kernel device path assigned to the disk. This is a v2 change: v1 returned void and updated the registry instead. A snapshot pre-flight guard runs first: if the VM has snapshots, attach is rejected with an actionable error, because a disk attached after a snapshot is invisible to that snapshot on rollback. Set `pve.allow_disk_ops_with_snapshots` to bypass. See [Snapshot guard on disk operations](#snapshot-guard-on-disk-operations).

---

### `detach_disk`

**Type:** Required v2

**Args:**

- `args[0]` (String): `vm_cid`
- `args[1]` (String): `disk_cid`

**Returns:** `null`

**Errors:** `Bosh::Clouds::CloudError` on PVE API failure

**Notes:** Removes the disk from the VM's PVE config and awaits the PVE task. In v2 with `api_version: 2` stemcells, the Director sends disk-detach notification to the agent directly; the CPI does not touch the registry. A snapshot pre-flight guard runs first: if the VM has snapshots that reference the disk, detach is rejected with an actionable error naming the blocking snapshots (PVE would otherwise reject it with a raw message). Set `pve.allow_disk_ops_with_snapshots` to bypass. See [Snapshot guard on disk operations](#snapshot-guard-on-disk-operations).

---

### `get_disks`

**Type:** Required v2

**Args:**

- `args[0]` (String): `vm_cid`

**Returns:** `Array of String` — disk CIDs currently attached to the VM

**Errors:** `Bosh::Clouds::CloudError` on PVE API failure

**Notes:** Used by cloudcheck to reconcile disk attachment state.

---

### `snapshot_disk`

**Type:** Required v2

**Args:**

- `args[0]` (String): `disk_cid`
- `args[1]` (Hash): `metadata` — key-value snapshot annotations

**Returns:** `String` — `snapshot_cid`

**Errors:** `Bosh::Clouds::CloudError` on PVE API failure

**Notes:** Creates a PVE snapshot of the disk and returns an identifier encoding the disk CID and snapshot name.

---

### `delete_snapshot`

**Type:** Required v2

**Args:**

- `args[0]` (String): `snapshot_cid`

**Returns:** `null`

**Errors:** `Bosh::Clouds::CloudError` on PVE API failure

**Notes:** Deletes the PVE disk snapshot identified by `snapshot_cid`.

---

### `resize_disk`

**Type:** Required v2

**Args:**

- `args[0]` (String): `disk_cid`
- `args[1]` (Integer): `new_size` — target size in MiB

**Returns:** `null`

**Errors:**

- `Bosh::Clouds::NotSupported` when `new_size` is less than the current disk size (shrink rejected; Director falls back to create-new + copy-data)
- `Bosh::Clouds::CloudError` on PVE API failure

**Notes:** The Director calls this only when `director.enable_cpi_resize_disk: true`. The disk must be attached to a VM at call time; the handler uses `FindVMByDiskVolid` to locate the hosting VM and determine the current disk size before issuing the resize. If the disk is not attached, the call will error. A snapshot pre-flight guard runs first: if the VM has snapshots, resize is rejected with an actionable error. PVE cannot resize disks on LVM-thin or ZFS storage while snapshots exist; on qcow2/raw the resize would succeed but leave snapshot data inconsistent. Set `pve.allow_disk_ops_with_snapshots` to bypass. See [Snapshot guard on disk operations](#snapshot-guard-on-disk-operations).

---

### Snapshot guard on disk operations

`attach_disk`, `detach_disk`, and `resize_disk` run a snapshot pre-flight check before mutating the VM. If the VM has one or more snapshots, the operation is rejected with an error that names the VM, node, and snapshot names, and states the remediation: delete the snapshots first, or set `pve.allow_disk_ops_with_snapshots`. This converts opaque PVE rejections (detach, resize) and silent data-integrity hazards (attach) into clear, actionable failures.

Two settings tune the behavior:

- `pve.allow_disk_ops_with_snapshots` (default `false`) — when `true`, the guard is bypassed and the operation proceeds despite snapshots. For emergency recovery only; snapshot state becomes inconsistent afterward.

- `pve.require_snapshot_check_pass` (default `false`) — controls what happens when the snapshot check itself cannot reach PVE. Default is fail-open: log a warning and proceed. Set `true` to fail-closed: abort the operation if the snapshot list cannot be fetched.

---

### `set_disk_metadata`

**Type:** Required v2

**Args:**

- `args[0]` (String): `disk_cid`
- `args[1]` (Hash): `metadata` — key-value pairs; reserved keys include `instance_id`, `instance_index`, `attached_at`, `instance_group`, `deployment`, `director`

**Returns:** `null`

**Errors:** `Bosh::Clouds::CloudError` on PVE API failure

**Notes:** PVE has no native disk metadata. This CPI stashes metadata as a JSON block in the description of the VM that currently has the disk attached. If the disk is detached at call time, the CPI logs a warning and returns success without storing the metadata.

If `metadata` contains a `tags` sub-object (map of `key: value`), the entries are extracted from the regular `bosh_disk_metadata` payload, written to the hosting VM's PVE tags field as sanitized `<key>--<value>` entries, and recorded separately in the description sentinel under `bosh_disk_tags[<disk_cid>]`. Existing tag entries whose key prefix collides with a new tag are replaced; other entries are preserved. See [Custom Tags](configuration.md#custom-tags).

---

### `update_disk`

**Type:** Extension (PVE-specific)

**Args:**

- `args[0]` (String): `disk_cid`
- `args[1]` (Hash): `update_spec` — optional fields:
  - `size` (Integer): new size in MiB
  - `iothread` (Boolean): enable per-disk I/O thread
  - `cache` (String): PVE cache mode (`none`, `writeback`, `writethrough`, etc.)
  - `discard` (String): discard/TRIM mode (`ignore` or `on`)
  - `ssd` (Boolean): expose disk as SSD
  - `backup` (Boolean): include in backups
  - `iops_rd` (Integer): read IOPS limit
  - `iops_wr` (Integer): write IOPS limit

**Returns:** `null`

**Errors:** `Bosh::Clouds::CloudError` on PVE API failure

**Notes:** Not part of the canonical BOSH CPI v2 specification. Provides full PVE disk option updates beyond what `resize_disk` covers. If `size` is provided, the resize follows the same shrink-rejection rule as `resize_disk`.

---

## Network

The CPI implements `create_network` and `delete_network` for BOSH managed
networks. Two paths are available: **SDN** (PVE SDN vnets via the cluster API)
and **bridge** (Linux bridges via the nodes API). The active path is selected
by `network_mode` in config and by the `cloud_properties` keys present in the
network spec. Most deployments that pre-configure networks in PVE do not use
managed networks and these methods are never called; they are only invoked when
the BOSH cloud-config marks a network as `managed: true`.

For the full `cloud_properties` schema, zone/vnet/subnet semantics, naming
rules, and worked manifest examples, see [Network configuration](networks.md).

---

### `create_network`

**Type:** Optional v2

**Args:**

- `args[0]` (Hash): `network_spec`
  - `type` (String): `"manual"` or `"dynamic"` — required
  - `range` (String): CIDR, e.g. `"10.0.0.0/24"` — optional
  - `gateway` (String): gateway IP — optional
  - `netmask_bits` (Integer): prefix length — optional
  - `cloud_properties` (Hash): PVE-specific keys:
    - `zone` (String): PVE SDN zone name; overrides `pve.sdn_zone` config
    - `zone_type` (String): zone type used when the CPI creates the zone (`simple` | `vlan` | `qinq` | `vxlan` | `evpn`); overrides `pve.sdn_zone_type` config
    - `vnet` (String): PVE vnet name — max 8 chars, `[a-z0-9]`; required for the SDN path
    - `bridge` (String): Linux bridge interface name, e.g. `"vmbr1"`; required for the bridge path when `pve.network_bridge` is not set
    - `node` (String): PVE node name for bridge operations; falls back to `pve.node` config

**Returns:** `[network_id, address_properties, cloud_properties_out]`

| Element | SDN path | Bridge path |
|---|---|---|
| `network_id` | vnet name | bridge interface name |
| `address_properties` | `{range, gateway, reserved: []}` | `{range, gateway, reserved: []}` |
| `cloud_properties_out` | `{zone, vnet, bridge: <vnet>}` | `{bridge, node}` |

Note: on the SDN path `bridge` equals `vnet` because PVE realizes a simple-zone
vnet as a Linux bridge with the same name. The `bridge` key in
`cloud_properties_out` is present so `create_vm` NIC attachment works without
additional config.

**Path selection:**

The handler picks a path based on `pve.network_mode` (default `"auto"`):

| `network_mode` | Path taken |
|---|---|
| `"sdn"` | Always SDN |
| `"bridge"` | Always bridge |
| `"auto"` | SDN when `cloud_properties.zone` or `cloud_properties.vnet` is set, or `pve.sdn_zone` is configured; bridge otherwise |

**Behavior — SDN path:**

1. Resolve zone: `cloud_properties.zone` → `pve.sdn_zone` → error if neither is set.
2. Probe `GetSdnZones` for the zone. If absent and `pve.sdn_auto_manage_zone=true`, create the zone using the resolved `zone_type`. If absent and `sdn_auto_manage_zone=false`, return a `CloudError`.
3. Probe `GetSdnVnets` for the vnet. If absent, call `CreateSdnVnets`. A 409 conflict (concurrent create) is treated as success.
4. If `network_spec.range` is set, call `CreateSdnVnetsSubnets` with the CIDR and gateway. A 409 conflict is treated as success.
5. Call `UpdateSdn` to commit staged SDN changes to the data plane.
6. Return `[vnet, {range, gateway, reserved:[]}, {zone, vnet, bridge: vnet}]`.

On apply failure the handler makes best-effort rollback (delete subnet, vnet, and zone if created in this call) before returning the error.

**Behavior — bridge path:**

1. Resolve node: `cloud_properties.node` → `pve.node` → error if neither is set.
2. Call `CreateNetwork(node, {iface: bridge, type: "bridge", autostart: true})`. A 409 conflict (bridge already exists) is treated as success.
3. Call `UpdateNetwork(node)` to reload the node's network configuration.
4. Return `[bridge, {range, gateway, reserved:[]}, {bridge, node}]`.

**Idempotency:**

- SDN path: `GetSdnVnets` is probed before each create. Re-calling with the same `vnet` and `zone` returns the same result without error.
- Bridge path: a 409 response from `CreateNetwork` is treated as success. `UpdateNetwork` is always called.

**Zone lifecycle (SDN path, `sdn_auto_manage_zone=true`):**

When `pve.sdn_auto_manage_zone=true` the CPI creates the SDN zone if it does
not exist. The CPI does not track zone ownership between calls; instead it
applies a stateless safety rule on deletion (see `delete_network` below).
`create_network` is safe to retry regardless of the zone state.

**Errors:**

- `CloudError`: zone not found and `sdn_auto_manage_zone=false`
- `CloudError`: `cloud_properties.vnet` missing or invalid (>8 chars, non-`[a-z0-9]` characters) on the SDN path
- `CloudError`: neither `cloud_properties.zone`/`pve.sdn_zone` nor `cloud_properties.bridge`/`pve.network_bridge` is set and path cannot be determined
- `CloudError`: target node not set for bridge path
- `CloudError`: PVE API failure (zone create, vnet create, subnet create, `UpdateSdn`, bridge create, node reload)

---

### `delete_network`

**Type:** Optional v2

**Args:**

- `args[0]` (String): `network_id` — the value returned as `network_id` by `create_network` (vnet name for SDN, bridge name for bridge)

**Returns:** `null`

**Path selection:**

`delete_network` receives only the `network_id` string. It probes
`GetSdnVnets(network_id)` to determine the path: if the vnet is found the SDN
path runs; if the probe returns 404 the bridge path runs.

**Behavior — SDN path:**

1. Probe `GetSdnVnets(network_id)`. If 404, return `null` (idempotent).
2. Extract zone name from the vnet response.
3. List and delete all subnets under the vnet (PVE requires subnets removed first). 404 on a subnet is treated as already-gone.
4. Delete the vnet. 404 is treated as already-gone.
5. Call `UpdateSdn` to commit the change.
6. Evaluate zone auto-delete (see rule below). If conditions hold, delete the zone and call `UpdateSdn` again.
7. Return `null`.

**Behavior — bridge path:**

1. Call `DeleteNetwork2(config.node, network_id)`. If 404, return `null` (idempotent).
2. Call `UpdateNetwork(config.node)` to reload.
3. Return `null`.

The bridge path uses `pve.node` from config. Per-bridge node assignment is not
stored between the `create_network` and `delete_network` calls; if `pve.node`
is unset, `delete_network` returns a `CloudError` rather than silently
succeeding with the bridge still present.

**Idempotency:**

Both paths are no-ops when the resource is already absent. `delete_network`
can be called multiple times safely.

**Zone auto-delete safety rule:**

The CPI deletes the parent SDN zone during `delete_network` only when **all**
of the following conditions hold:

1. `pve.sdn_auto_manage_zone=true` (explicit opt-in; default `false`).
2. The zone name does not equal `pve.sdn_zone` (the configured default zone is never auto-deleted).
3. After the vnet is removed, `ListSdnVnets` filtered by zone returns zero remaining vnets.

If `ListSdnVnets` fails, the zone is left intact rather than risking deletion
of a zone that may still contain vnets.

Residual risk: with `sdn_auto_manage_zone=true`, any zone supplied via
`cloud_properties.zone` that differs from `pve.sdn_zone` will be deleted when
emptied. Operators who share a zone across deployments must either set
`pve.sdn_zone` to pin it or leave `sdn_auto_manage_zone=false` (the default).

**Errors:**

- `CloudError`: `pve.node` unset on the bridge path
- `CloudError`: unexpected PVE API failure probing, deleting, or applying SDN changes
- `CloudError`: unexpected PVE API failure deleting bridge or reloading node network

---

## Method Summary

| # | Method | Group | Returns | v1→v2 Change |
|---|---|---|---|---|
| 1 | `info` | Info | Hash | Required; must return `api_version: 2` |
| 2 | `create_stemcell` | Stemcell | stemcell\_cid | No change |
| 3 | `delete_stemcell` | Stemcell | null | No change |
| 4 | `create_vm` | VM | [vm\_cid, networks] | Returns array (was bare string) |
| 5 | `delete_vm` | VM | null | Registry skip in v2 |
| 6 | `has_vm` | VM | Boolean | No change |
| 7 | `reboot_vm` | VM | null | No change |
| 8 | `set_vm_metadata` | VM | null | No change |
| 9 | `calculate_vm_cloud_properties` | VM | Hash | New in v2 |
| 10 | `create_disk` | Disk | disk\_cid | No change |
| 11 | `delete_disk` | Disk | null | No change |
| 12 | `has_disk` | Disk | Boolean | No change |
| 13 | `attach_disk` | Disk | disk\_hints Hash | Returns hints (was void) |
| 14 | `detach_disk` | Disk | null | Registry skip in v2 |
| 15 | `get_disks` | Disk | Array of String | No change |
| 16 | `snapshot_disk` | Disk | snapshot\_cid | No change |
| 17 | `delete_snapshot` | Disk | null | No change |
| 18 | `resize_disk` | Disk | null | No change |
| 19 | `set_disk_metadata` | Disk | null | No change |
| 20 | `update_disk` | Disk | null | Extension (PVE-specific) |
| 21 | `create_network` | Network | [id, addrs, props] | New in v2 (optional). Implemented — see [Network configuration](networks.md). |
| 22 | `delete_network` | Network | null | New in v2 (optional). Implemented — see [Network configuration](networks.md). |
| — | `configure_networks` | REMOVED | — | Removed in v2 |
